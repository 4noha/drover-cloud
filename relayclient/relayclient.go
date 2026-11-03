// Package relayclient は cm（claude-master-go）の relay クライアント側
// （source 役の dial と idle 付き双方向ポンプ）のコピー/適応。
// cm internal/cloud/relay/relay.go の Dial / pump / idlePump /
// BridgeSourceIdle が原典（同一作者＝コピー自由・cm リポジトリは無改変）。
//
// ワイヤ契約（cm コード実読の抽出・relay 側は既デプロイ Cloud Run を無改変
// 共有するため、この形式から一切逸脱しない）:
//   - URL = baseURL + "/session?sid=" + sid + "&role=" + role
//     （パス /session 固定・query は sid/role の 2 つだけ・カスタムヘッダ
//     無し・subprotocol 無し・DialOptions{} 既定。sid は URL エスケープ
//     しない＝cm と同じ前提。pane_id の `:` は RFC 3986 で query に生の
//     まま合法）
//   - websocket.NetConn(ctx, c, MessageBinary) でバイトストリーム化＝
//     WS message 境界は意味を持たない（既存 RESIZE/SCROLL/frame protocol
//     を無改変トンネル）
//   - quiescence（無通信 idle）切断は「**両方向とも** idle の時のみ」。
//     どちらか一方向でも流れていればリセット（cm idlePump と同一意味論）
//
// cm との差分は 1 点のみ: BridgeSourceIdle のローカル端が cm では
// `net.Dial("unix", <pid>.sock)`（ptyproxy）だったのに対し、herdr-drover
// には常駐 proxy socket が無いので呼び出し側が用意する io.ReadWriteCloser
// （bridge 側 pipe 端など）を受ける。
package relayclient

import (
	"context"
	"io"
	"net"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Dial は relay へ WSS 接続して net.Conn（バイトストリーム）を返す。
// baseURL 例: ws://host:port / wss://…（/session は付けない）。
// cm relay.Dial（relay.go:257-266）のバイト同等コピー。
// conn の寿命は ctx に束縛される（cancel で read/write が死ぬ）＝呼び出し側
// は接続生存期間の ctx を渡すこと。
func Dial(ctx context.Context, baseURL, sid, role string) (net.Conn, error) {
	// sid は URL エスケープする。relay 側は Query().Get で自動デコードするので
	// 既存の `:` 含む pane_id（w1:p2 → w1%3Ap2 → デコードで w1:p2）は透過的に
	// 同一だが、リモート pane 注入の派生 sid `<sid>#inj` の `#` は URL の
	// フラグメント区切りとして解釈され role が届かず relay が 400 を返す（実障害で
	// 確認）。エスケープでフラグメント化を防ぐ。role は source/viewer 固定＝安全。
	u := baseURL + "/session?sid=" + url.QueryEscape(sid) + "&role=" + role
	c, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{CompressionMode: viewerCompression(role)})
	if err != nil {
		return nil, err
	}
	return &Conn{Conn: websocket.NetConn(ctx, c, websocket.MessageBinary), ws: c}, nil
}

// Conn は relay へのデータ線（net.Conn）に WebSocket の Ping を足したもの。
//
// TCP が黙って死んだ接続（スリープ復帰・NAT テーブル消失・relay VM の再起動）は
// ESTABLISHED のまま read/write からは区別できない。無通信自切断を 30 秒から
// 15 分へ伸ばした（2026-11-03・relay が VM に移り常時接続の課金が無くなった）
// ことで「死んだ接続を 15 分抱える」余地ができたため、生きているかを
// ping/pong で確かめる手段を持つ。相手（relay の serve／viewer の読み手）が
// 読んでいれば pong は自動で返る（coder/websocket は読み手が制御フレームへ応答する）。
type Conn struct {
	net.Conn
	ws *websocket.Conn
}

// Ping は ping を送って pong を ctx の期限まで待つ。呼び手は別 goroutine で
// この conn を読んでいること（pong は読み手が受ける）。読み手が居ない相手には
// pong が返らず期限で失敗する＝「相手が読んでいない」もここで検出できる。
func (c *Conn) Ping(ctx context.Context) error { return c.ws.Ping(ctx) }

// Pinger は Ping できる conn（Dial/DialViewerFrom が返す *Conn）。呼び手は
// net.Conn からこれへ型アサートして使う（Ping できない conn は「不明」＝
// 生きている扱いにする側に倒す）。
type Pinger interface {
	Ping(ctx context.Context) error
}

// DialSource は source 役の Dial（webterm 配線の dialRelay 委譲先）。
func DialSource(ctx context.Context, baseURL, sid string) (net.Conn, error) {
	return Dial(ctx, baseURL, sid, "source")
}

// DialViewerFrom は viewer 役で dial し、source PC を `spc` パラメータで relay へ
// 渡す（リモート pane 注入の viewer 用）。relay の KeyFor が spc==slave PC の
// 時だけ pc 名前空間キーで viewer を Accept し slave source とペアさせる。
// sourcePC が空 or master PC なら relay は spc を無視＝Dial(role=viewer) と
// **同一 wire**（master→master 注入は byte-identical）。
func DialViewerFrom(ctx context.Context, baseURL, sid, sourcePC string) (net.Conn, error) {
	u := baseURL + "/session?sid=" + url.QueryEscape(sid) + "&role=viewer"
	if sourcePC != "" {
		u += "&spc=" + url.QueryEscape(sourcePC)
	}
	c, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{CompressionMode: viewerCompression("viewer")})
	if err != nil {
		return nil, err
	}
	return &Conn{Conn: websocket.NetConn(ctx, c, websocket.MessageBinary), ws: c}, nil
}

// BridgeSourceIdle は relay へ source として dial し、local と双方向ポンプ
// する。idle 秒 無通信（両方向とも）でデータ線を閉じて戻る（quiescence
// 切断＝次の wake まで解放）。cm relay.BridgeSourceIdle の適応コピー
// （unix socket dial の代わりに呼び出し側の local 端を使う）。
// ctx 終了／どちらか切断でも戻る。戻る時には両端とも close 済み。
func BridgeSourceIdle(ctx context.Context, baseURL, sid string, local io.ReadWriteCloser, idle time.Duration) error {
	ws, err := Dial(ctx, baseURL, sid, "source")
	if err != nil {
		return err
	}
	defer ws.Close()
	defer local.Close()
	idlePump(ws, local, idle)
	return nil
}

// pump は a⇄b をバイト透過で双方向中継。片方が閉じたら戻る。
// cm relay.pump のコピー（型だけ io.ReadWriteCloser へ一般化）。
func pump(a, b io.ReadWriteCloser) {
	d := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); d <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); d <- struct{}{} }()
	<-d
}

// idlePump は a⇄b を透過中継しつつ、両方向で idle 秒バイトが流れなければ
// 両端を閉じて戻る（= データ線の quiescence 切断）。idle<=0 なら通常 pump
// と同じ（無期限）。cm relay.idlePump のコピー（32KB バッファ・どちら向き
// でも read で bump・ticker(idle/2) 判定・read/write エラーでも両端 close、
// 全て同一）。
func idlePump(a, b io.ReadWriteCloser, idle time.Duration) {
	if idle <= 0 {
		pump(a, b)
		return
	}
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	bump := func() { last.Store(time.Now().UnixNano()) }
	d := make(chan struct{}, 2)
	cp := func(dst, src io.ReadWriteCloser) {
		buf := make([]byte, 32*1024)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				bump()
				if _, werr := dst.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		d <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	tick := time.NewTicker(idle / 2)
	defer tick.Stop()
	for {
		select {
		case <-d:
			a.Close()
			b.Close()
			return
		case <-tick.C:
			if time.Since(time.Unix(0, last.Load())) >= idle {
				a.Close() // 静止 → データ線解放
				b.Close()
				<-d
				return
			}
		}
	}
}

// viewerCompression は viewer だけ permessage-deflate を申し出る。課金される外向き
// 転送は relay→viewer（画面描画）で、source→relay（入り）は無料＝source 側
// （各 PC の agent）は CPU を使って圧縮しない。relay が対応しなければ無圧縮に落ちる。
func viewerCompression(role string) websocket.CompressionMode {
	if role == "viewer" {
		return websocket.CompressionContextTakeover // 小さな差分フレームの連続に効く（relay.NewServer 参照）
	}
	return websocket.CompressionDisabled
}
