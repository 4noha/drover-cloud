package relay

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// countListener は accept した接続の読み書きバイト数（＝TCP に流れた実バイト）を数える。
type countListener struct {
	net.Listener
	wrote *atomic.Int64
}

func (l countListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &countConn{Conn: c, wrote: l.wrote}, nil
}

type countConn struct {
	net.Conn
	wrote *atomic.Int64
}

func (c *countConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.wrote.Add(int64(n))
	return n, err
}

// realTerminalBytes は実コマンドの色付き出力（端末描画に近い反復の多いテキスト）。
func realTerminalBytes(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	for _, args := range [][]string{
		{"git", "-C", ".", "log", "-p", "--color=always", "-n", "40"},
		{"ls", "-lR", "--color=always", "/usr/share"},
		{"ls", "-lRG", "/usr/share"},
	} {
		out, _ := exec.Command(args[0], args[1:]...).Output()
		b.Write(out)
	}
	if b.Len() < 200<<10 {
		t.Skipf("試料が小さすぎる(%dB)", b.Len())
	}
	if b.Len() > 4<<20 {
		b.Truncate(4 << 20)
	}
	return b.Bytes()
}

// relay→viewer の実バイト数を、圧縮あり/なしで比べる。内容は常にバイト一致。
func relayToViewerWire(t *testing.T, comp websocket.CompressionMode, viewerOffers websocket.CompressionMode, data []byte) int64 {
	t.Helper()
	srv := NewServer()
	srv.Compression = comp
	var wrote atomic.Int64
	ts := httptest.NewUnstartedServer(srv)
	ts.Listener = countListener{Listener: ts.Listener, wrote: &wrote}
	ts.Start()
	defer ts.Close()
	base := "ws" + strings.TrimPrefix(ts.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	v, _, err := websocket.Dial(ctx, base+"/session?sid=c1&role=viewer", &websocket.DialOptions{CompressionMode: viewerOffers})
	if err != nil {
		t.Fatal(err)
	}
	vc := websocket.NetConn(ctx, v, websocket.MessageBinary)
	s, _, err := websocket.Dial(ctx, base+"/session?sid=c1&role=source", &websocket.DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sc := websocket.NetConn(ctx, s, websocket.MessageBinary)
	go func() { _, _ = sc.Write(data) }()
	got := make([]byte, len(data))
	if _, err := io.ReadFull(vc, got); err != nil {
		t.Fatalf("viewer 読取: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("圧縮を通した内容がバイト一致しない")
	}
	return wrote.Load() // relay が TCP へ書いた全量（viewer 宛が主）
}

// relayToViewerWireChunks は data を chunk バイトずつの WS メッセージで流す（herdr の observe が
// 出す 100 バイト級の差分フレームの連続を、実コマンド出力で模す）。
func relayToViewerWireChunks(t *testing.T, comp, viewerOffers websocket.CompressionMode, threshold int, data []byte, chunk int) int64 {
	t.Helper()
	srv := NewServer()
	srv.Compression = comp
	srv.CompressionThreshold = threshold
	var wrote atomic.Int64
	ts := httptest.NewUnstartedServer(srv)
	ts.Listener = countListener{Listener: ts.Listener, wrote: &wrote}
	ts.Start()
	defer ts.Close()
	base := "ws" + strings.TrimPrefix(ts.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	v, _, err := websocket.Dial(ctx, base+"/session?sid=c2&role=viewer", &websocket.DialOptions{CompressionMode: viewerOffers})
	if err != nil {
		t.Fatal(err)
	}
	vc := websocket.NetConn(ctx, v, websocket.MessageBinary)
	s, _, err := websocket.Dial(ctx, base+"/session?sid=c2&role=source", &websocket.DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sc := websocket.NetConn(ctx, s, websocket.MessageBinary)
	go func() {
		for i := 0; i < len(data); i += chunk {
			j := i + chunk
			if j > len(data) {
				j = len(data)
			}
			if _, err := sc.Write(data[i:j]); err != nil {
				return
			}
		}
	}()
	got := make([]byte, len(data))
	if _, err := io.ReadFull(vc, got); err != nil {
		t.Fatalf("viewer 読取: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("圧縮を通した内容がバイト一致しない")
	}
	return wrote.Load()
}

// 小さなメッセージの連続で、圧縮設定ごとの wire を実測して固定する（2026-11-03）。
// 分かっていること（coder/websocket v1.8.14）:
//   - ライブラリ既定の閾値（NoContext 512B・Context 128B）では 100B のメッセージは**圧縮されない**。
//   - 閾値 1 にしても 100B 単位では 95% 程度にしか縮まない。ContextTakeover でも前メッセージの
//     窓は実質使われず NoContext と同じ（測定値が一致）。小さな差分フレームを縮めるには、
//     source 側でフレームをまとめて大きいメッセージにするしかない（herdr-drover 側の課題）。
//   - 1000B 単位なら 20〜24% に縮み、ContextTakeover の方が少し小さい。
func TestSmallMessageCompressionFacts(t *testing.T) {
	data := realTerminalBytes(t)
	if len(data) > 1<<20 {
		data = data[:1<<20]
	}
	def := NewServer()
	if def.Compression != websocket.CompressionContextTakeover || def.CompressionThreshold != 0 {
		t.Fatalf("relay の既定は ContextTakeover・閾値はライブラリ既定のはず: %v %d", def.Compression, def.CompressionThreshold)
	}
	off100 := relayToViewerWireChunks(t, websocket.CompressionDisabled, websocket.CompressionDisabled, 0, data, 100)
	old100 := relayToViewerWireChunks(t, websocket.CompressionNoContextTakeover, websocket.CompressionNoContextTakeover, 0, data, 100)
	new100 := relayToViewerWireChunks(t, websocket.CompressionContextTakeover, websocket.CompressionContextTakeover, 0, data, 100)
	thr1 := relayToViewerWireChunks(t, websocket.CompressionContextTakeover, websocket.CompressionContextTakeover, 1, data, 100)
	off1k := relayToViewerWireChunks(t, websocket.CompressionDisabled, websocket.CompressionDisabled, 0, data, 1000)
	old1k := relayToViewerWireChunks(t, websocket.CompressionNoContextTakeover, websocket.CompressionNoContextTakeover, 0, data, 1000)
	new1k := relayToViewerWireChunks(t, websocket.CompressionContextTakeover, websocket.CompressionContextTakeover, 0, data, 1000)
	t.Logf("100B 単位: 無圧縮 %d / 旧設定 %d (%.0f%%) / 新設定 %d (%.0f%%) / 閾値 1 %d (%.0f%%)", off100, old100, 100*float64(old100)/float64(off100), new100, 100*float64(new100)/float64(off100), thr1, 100*float64(thr1)/float64(off100))
	t.Logf("1000B 単位: 無圧縮 %d / 旧設定 %d (%.0f%%) / 新設定 %d (%.0f%%)", off1k, old1k, 100*float64(old1k)/float64(off1k), new1k, 100*float64(new1k)/float64(off1k))
	if old100 < off100 {
		t.Fatalf("前提が崩れている: 旧設定で 100B メッセージが圧縮されている old=%d off=%d", old100, off100)
	}
	// 100B 単位: 新設定でも旧設定と同じく圧縮されない（閾値未満＝±1%）。閾値 1 は逆に増える＝採らない。
	if d := new100 - old100; d > old100/100 || d < -old100/100 {
		t.Fatalf("100B 単位は旧設定と同等のはず: new=%d old=%d", new100, old100)
	}
	if thr1 <= new100 {
		t.Fatalf("前提が崩れている: 閾値 1 が 100B 単位で小さくなっている（なら既定を変えるべき）: thr1=%d new=%d", thr1, new100)
	}
	if new1k >= old1k || new1k*2 > off1k {
		t.Fatalf("新設定は 1000B 単位で旧設定より小さく、無圧縮の半分以下のはず: new=%d old=%d off=%d", new1k, old1k, off1k)
	}
}

func TestCompressionShrinksRelayToViewerWire(t *testing.T) {
	data := realTerminalBytes(t)
	off := relayToViewerWire(t, websocket.CompressionDisabled, websocket.CompressionDisabled, data)
	on := relayToViewerWire(t, websocket.CompressionNoContextTakeover, websocket.CompressionNoContextTakeover, data)
	// 古い viewer（圧縮を申し出ない）には relay が圧縮を強制しない＝従来どおり無圧縮。
	legacy := relayToViewerWire(t, websocket.CompressionNoContextTakeover, websocket.CompressionDisabled, data)
	t.Logf("試料 %dB: 無圧縮 wire=%dB / 圧縮 wire=%dB (%.1f%%) / 旧 viewer wire=%dB", len(data), off, on, 100*float64(on)/float64(off), legacy)
	if on*3 > off {
		t.Fatalf("圧縮で 1/3 以下になるはず: off=%d on=%d", off, on)
	}
	if legacy < int64(len(data)) {
		t.Fatalf("旧 viewer には無圧縮のはず: legacy=%d data=%d", legacy, len(data))
	}
}
