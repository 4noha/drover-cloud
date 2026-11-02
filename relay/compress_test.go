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
