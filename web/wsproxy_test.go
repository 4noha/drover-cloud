package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/4noha/drover-cloud/relay"
	"github.com/4noha/drover-cloud/relayclient"
	"github.com/4noha/drover-cloud/webauth"
	"github.com/coder/websocket"
)

// Web の /ws は、上流 relay（VM）が設定されていれば自分の relay ではなく上流へ viewer と
// して繋ぎ、ブラウザと source の間を両方向に中継する。上流は実 relay（別 httptest）。
func TestWSViewerProxiesToUpstreamRelay(t *testing.T) {
	needEmu(t)
	st := newState(t)
	upstream := httptest.NewServer(relay.NewServer()) // 上流（Grant 無し＝ローカル検証）
	defer upstream.Close()
	upURL := "ws" + strings.TrimPrefix(upstream.URL, "http")
	t.Setenv("RELAY_UPSTREAM_URL", upURL)

	local := relay.NewServer()
	ws := New(local, st, webauth.NewSigner("k"), "cid", allowEmail, fakeGV{}, emuProject, "")
	ts := httptest.NewServer(ws.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const pc, sid = "proxy-src-herdr", "w1:p1"
	// source は上流にだけ居る。
	src, err := relayclient.Dial(ctx, upURL, sid, "source")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	// ブラウザ役: Web の /ws（cookie 認証）。
	h := http.Header{}
	h.Set("Cookie", authCookie(ws))
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/ws?pc="+pc+"&sid="+sid, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatalf("/ws dial: %v", err)
	}
	defer c.CloseNow()
	bc := websocket.NetConn(ctx, c, websocket.MessageBinary)

	if _, err := src.Write([]byte("FRAME-FROM-SOURCE")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := bc.Read(buf)
	if err != nil || string(buf[:n]) != "FRAME-FROM-SOURCE" {
		t.Fatalf("ブラウザに source のバイトが届かない: %q %v", buf[:n], err)
	}
	if _, err := bc.Write([]byte("KEY-FROM-BROWSER")); err != nil {
		t.Fatal(err)
	}
	n, err = src.Read(buf)
	if err != nil || string(buf[:n]) != "KEY-FROM-BROWSER" {
		t.Fatalf("source にブラウザの入力が届かない: %q %v", buf[:n], err)
	}
	// 上流の Grant 検証用に viewer グラントが書かれている（attach と同じ規律）。
	if !st.CheckRelayGrant(ctx, sid, "viewer") {
		t.Fatal("上流用の viewer グラントが無い")
	}
}
