package relay

// ServeHTTP の SlaveGate seam の分岐を実 HTTP/実 WSS で検証（合成の相手では
// なく本番と同じ Accept/serve を経由）。Firestore 不要（fake gate closure）。
// 主張:
//   - bearer 無し ⇒ handled=false ⇒ 既存 Grant 経路（byte-identical・§8.1）
//   - slave viewer ⇒ 403（覗き見の構造的停止）
//   - slave source（許可）⇒ 101（Accept）
//   - 不正 slave ⇒ 403
//   - SlaveGate=nil ⇒ 完全に従来挙動

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

// dialAuth は sid/role/optional bearer で /session を dial し HTTP ステータスを
// 判定に使う（101=成功 nil err、403 等は err）。成功時は conn を閉じる。
func dialAuth(t *testing.T, baseWS, sid, role, bearer string) error {
	t.Helper()
	u := baseWS + "/session?sid=" + sid + "&role=" + role
	h := http.Header{}
	if bearer != "" {
		h.Set("Authorization", "Bearer "+bearer)
	}
	c, _, err := websocket.Dial(context.Background(), u, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		return err
	}
	c.Close(websocket.StatusNormalClosure, "")
	return nil
}

func TestServeHTTPSlaveGateWiring(t *testing.T) {
	rl := NewServer()
	// master 経路: sid=="granted" のみ許可。
	rl.Grant = func(_ context.Context, sid, role string) bool { return sid == "granted" }
	// fake slave gate: bearer 無し=フォールスルー / viewer=deny /
	// "good"=allow(pc 名前空間) / それ以外の bearer=deny。
	rl.SlaveGate = func(r *http.Request, sid, role string) (bool, bool, string) {
		tok := r.Header.Get("Authorization")
		if tok == "" {
			return false, false, ""
		}
		if role != "source" {
			return true, false, ""
		}
		if tok == "Bearer good" {
			return true, true, "ns\x00" + sid
		}
		return true, false, ""
	}
	ts := httptest.NewServer(rl)
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http")

	// (§8.1 不変条件) bearer 無し + 有効 grant の source ⇒ 101（master 経路）。
	if err := dialAuth(t, ws, "granted", "source", ""); err != nil {
		t.Fatalf("bearer 無し master source が通らない（invariant 破れ）: %v", err)
	}
	// bearer 無し + 有効 grant の viewer ⇒ 101。
	if err := dialAuth(t, ws, "granted", "viewer", ""); err != nil {
		t.Fatalf("bearer 無し master viewer が通らない: %v", err)
	}
	// bearer 無し + grant 無し ⇒ 403。
	if err := dialAuth(t, ws, "nogrant", "source", ""); err == nil {
		t.Fatal("grant 無しの master source が通った（Grant 経路破れ）")
	}
	// slave viewer ⇒ 403（bearer 有り・role viewer）。
	if err := dialAuth(t, ws, "granted", "viewer", "good"); err == nil {
		t.Fatal("slave viewer が通った（覗き見停止が効いていない）")
	}
	// slave source（許可）⇒ 101（pc 名前空間キーで Accept）。
	if err := dialAuth(t, ws, "sidX", "source", "good"); err != nil {
		t.Fatalf("許可された slave source が 403: %v", err)
	}
	// 不正 slave bearer の source ⇒ 403。
	if err := dialAuth(t, ws, "sidX", "source", "bad"); err == nil {
		t.Fatal("不正 slave source が通った")
	}
}

func TestServeHTTPSlaveGateNilIsLegacy(t *testing.T) {
	// SlaveGate=nil なら bearer が在っても完全に従来挙動（Grant のみ）。
	rl := NewServer()
	rl.Grant = func(_ context.Context, sid, role string) bool { return sid == "ok" }
	ts := httptest.NewServer(rl)
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http")

	if err := dialAuth(t, ws, "ok", "source", "irrelevant-bearer"); err != nil {
		t.Fatalf("nil gate で grant 通過 source が 403: %v", err)
	}
	if err := dialAuth(t, ws, "no", "source", "irrelevant-bearer"); err == nil {
		t.Fatal("nil gate で grant 無し source が通った")
	}
}
