package web

// /api/webhook（セッション状態 Webhook 通知設定）の検証。実 Firestore
// エミュレータ（slave_emulator_test.go の TestMain・needEmu を共有）。
// HeaderSecret が応答 JSON に絶対出ないこと・accountScope 必須・https 検証・
// secret 空送信で既存値維持、を確認する。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/4noha/drover-cloud/relay"
	"github.com/4noha/drover-cloud/state"
	"github.com/4noha/drover-cloud/webauth"
	"github.com/4noha/drover-cloud/webhook"
)

func newWebhookTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	needEmu(t)
	st, err := state.New(context.Background(), emuProject, "webhook-api-test-pc")
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ws := New(relay.NewServer(), st, webauth.NewSigner("k"), "cid", allowEmail, fakeGV{}, emuProject, "")
	ts := httptest.NewServer(ws.Handler())
	t.Cleanup(ts.Close)
	return ws, ts
}

func doWebhookReq(t *testing.T, ts *httptest.Server, cookie, method string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, ts.URL+"/api/webhook", r)
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func TestAPIWebhookUnauthenticated(t *testing.T) {
	_, ts := newWebhookTestServer(t)
	resp, _ := doWebhookReq(t, ts, "", http.MethodGet, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未認証が %d", resp.StatusCode)
	}
}

func TestAPIWebhookGetDefaultDisabled(t *testing.T) {
	ws, ts := newWebhookTestServer(t)
	// settings/webhook は account 全体の singleton doc（pc でスコープしない）
	// なので他テストの書込みが残っていることがある＝ここで明示的に disabled
	// へ戻してから「既定値」を検証する（state/webhook_test.go と同じ注意）。
	if err := ws.st.SetWebhookConfig(context.Background(), webhook.Config{}); err != nil {
		t.Fatalf("前提のクリア失敗: %v", err)
	}
	resp, out := doWebhookReq(t, ts, authCookie(ws), http.MethodGet, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if out["enabled"] != false || out["has_secret"] != false {
		t.Fatalf("既定値が想定外: %+v", out)
	}
	if _, has := out["header_secret"]; has {
		t.Fatalf("header_secret が応答に含まれている: %+v", out)
	}
}

func TestAPIWebhookSaveRejectsHTTP(t *testing.T) {
	ws, ts := newWebhookTestServer(t)
	resp, out := doWebhookReq(t, ts, authCookie(ws), http.MethodPost, map[string]any{
		"enabled": true, "url": "http://example.com/hook",
		"header_name": "X-Secret", "header_secret": "s3cr3t",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("http URL が %d で拒否されない: %+v", resp.StatusCode, out)
	}
	if !strings.Contains(out["error"].(string), "https") {
		t.Fatalf("エラー文言が https 不正を示していない: %+v", out)
	}
}

func TestAPIWebhookSaveAndSecretNeverEchoed(t *testing.T) {
	ws, ts := newWebhookTestServer(t)
	cookie := authCookie(ws)

	resp, out := doWebhookReq(t, ts, cookie, http.MethodPost, map[string]any{
		"enabled": true, "url": "https://example.com/hook",
		"header_name": "X-Secret", "header_secret": "s3cr3t",
	})
	if resp.StatusCode != http.StatusOK || out["ok"] != true {
		t.Fatalf("保存失敗: status=%d body=%+v", resp.StatusCode, out)
	}

	resp, out = doWebhookReq(t, ts, cookie, http.MethodGet, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if out["enabled"] != true || out["url"] != "https://example.com/hook" ||
		out["header_name"] != "X-Secret" || out["has_secret"] != true {
		t.Fatalf("保存後の取得値が想定外: %+v", out)
	}
	if _, has := out["header_secret"]; has {
		t.Fatalf("header_secret が応答に含まれている（漏洩）: %+v", out)
	}

	// secret を空で再送 → 既存値（s3cr3t）を維持したまま URL だけ変わる。
	resp, out = doWebhookReq(t, ts, cookie, http.MethodPost, map[string]any{
		"enabled": true, "url": "https://example.com/hook2",
		"header_name": "X-Secret", "header_secret": "",
	})
	if resp.StatusCode != http.StatusOK || out["ok"] != true {
		t.Fatalf("再保存失敗: status=%d body=%+v", resp.StatusCode, out)
	}
	cfg, err := ws.st.GetWebhookConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.URL != "https://example.com/hook2" || cfg.HeaderSecret != "s3cr3t" {
		t.Fatalf("secret 空送信で既存値が維持されていない: %+v", cfg)
	}
}

func TestAPIWebhookScopeForbidden(t *testing.T) {
	ws, ts := newWebhookTestServer(t)
	// PC スコープ（accountScope でない）トークンは forbidden。
	tok := ws.signer.Sign(webauth.Token{PC: "some-pc", Scope: "some-pc", Exp: time.Now().Add(time.Hour).Unix()})
	resp, _ := doWebhookReq(t, ts, cookieName+"="+tok, http.MethodGet, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("PCスコープトークンが %d（forbidden のはず）", resp.StatusCode)
	}
}
