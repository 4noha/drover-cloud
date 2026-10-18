package main

// GLASSES（眼鏡の回答受け口）のマウント条件の検証。
// GLASSES_ENABLED が無いときは /glasses/ が従来の web/relay 経路に落ちる
// （＝新しいマウントが既存挙動を変えない）。トークンや GCP_PROJECT が
// 欠けるときも fail closed でマウントされない。

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// clearGlassesEnv は glasses/web の判定に効く env を消す（テスト環境の
// 残留 env で挙動が変わらないようにする）。
func clearGlassesEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"GLASSES_ENABLED", "GLASSES_PULL_TOKEN", "GCP_PROJECT",
		"WEB_SIGNING_KEY", "GOOGLE_OAUTH_CLIENT_ID", "ALLOWED_EMAILS", "MCP_ENABLED",
	} {
		t.Setenv(k, "")
	}
}

func getStatus(t *testing.T, url string) int {
	t.Helper()
	r, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer r.Body.Close()
	return r.StatusCode
}

func TestGlassesNotMountedByDefault(t *testing.T) {
	clearGlassesEnv(t)
	ts := httptest.NewServer(handler())
	defer ts.Close()
	if code := getStatus(t, ts.URL+"/glasses/answers?pc=pc-a"); code != http.StatusNotFound {
		t.Fatalf("GLASSES 未設定の /glasses/answers=%d, want 404（従来の経路）", code)
	}
	// 従来のヘルスは不変
	if code := getStatus(t, ts.URL+"/"); code != http.StatusOK {
		t.Fatalf("ヘルス=%d, want 200", code)
	}
}

func TestGlassesEnabledWithoutTokenFallsThrough(t *testing.T) {
	clearGlassesEnv(t)
	t.Setenv("GLASSES_ENABLED", "1")
	// トークン無し（GCP_PROJECT も無い）＝Firestore も作れない → マウントしない
	ts := httptest.NewServer(handler())
	defer ts.Close()
	if code := getStatus(t, ts.URL+"/glasses/answers?pc=pc-a"); code != http.StatusNotFound {
		t.Fatalf("トークン無しの /glasses/answers=%d, want 404（fail closed）", code)
	}
}

func TestGlassesEnabledShortTokenFallsThrough(t *testing.T) {
	clearGlassesEnv(t)
	t.Setenv("GLASSES_ENABLED", "1")
	t.Setenv("GLASSES_PULL_TOKEN", "short-token")
	ts := httptest.NewServer(handler())
	defer ts.Close()
	if code := getStatus(t, ts.URL+"/glasses/answers?pc=pc-a"); code != http.StatusNotFound {
		t.Fatalf("短いトークンの /glasses/answers=%d, want 404（fail closed）", code)
	}
}
