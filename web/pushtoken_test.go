package web

// /api/push-token: cookie 必須・POST 限定・token を state へ保存する。
// 実 relay + 実 Firestore エミュレータで検証（slave_emulator_test.go の
// TestMain/needEmu/newState を共有）。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/4noha/drover-cloud/relay"
	"github.com/4noha/drover-cloud/webauth"
)

func TestAPIPushTokenEndpoint(t *testing.T) {
	needEmu(t)
	st := newState(t)
	rl := relay.NewServer()
	ws := New(rl, st, webauth.NewSigner("test-key"), "cid", allowEmail, fakeGV{}, "demo-cm", "")
	ts := httptest.NewServer(ws.Handler())
	defer ts.Close()

	post := func(cookie, body string) *http.Response {
		req, _ := http.NewRequest("POST", ts.URL+"/api/push-token", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if cookie != "" {
			req.Header.Set("Cookie", cookie)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	// cookie 無しは 401（apiGuard）。
	if r := post("", "token=tok-x"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cookie 無しで status=%d, want 401", r.StatusCode)
	}

	cookie := authCookie(ws)

	// token 空は 400。
	if r := post(cookie, "token="); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("token 空で status=%d, want 400", r.StatusCode)
	}

	// 正常系: 保存され ListPushTokens に現れる。
	if r := post(cookie, "token=tok-abc"); r.StatusCode != http.StatusOK {
		t.Fatalf("正常登録で status=%d, want 200", r.StatusCode)
	}
	toks, err := st.ListPushTokens(t.Context())
	if err != nil {
		t.Fatalf("ListPushTokens: %v", err)
	}
	found := false
	for _, tk := range toks {
		if tk == "tok-abc" {
			found = true
		}
	}
	if !found {
		t.Fatalf("登録した token が見当たらない: %v", toks)
	}

	// GET は 405。
	req, _ := http.NewRequest("GET", ts.URL+"/api/push-token", nil)
	req.Header.Set("Cookie", cookie)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET で status=%d, want 405", r.StatusCode)
	}
}
