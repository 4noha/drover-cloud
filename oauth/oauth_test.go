//go:build !windows

package oauth

// 実 Firestore エミュレータ + 実 HTTP（httptest）で OAuth フロー全体を検証する
// （鉄則2: 合成で緑にしない）。GIS の実 Google 検証だけは fakeGoogleVerifier に
// 差し替える（webauth.GoogleVerifier インターフェースはテスト注入用に設計
// 済み）。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/4noha/drover-cloud/state"
	"github.com/4noha/drover-cloud/webauth"
)

const emuProject = "demo-cm-oauth"

var emuUp bool

func java21Bin() string {
	for _, d := range []string{
		"/opt/homebrew/opt/openjdk/bin",
		"/opt/homebrew/opt/openjdk@25/bin",
		"/opt/homebrew/opt/openjdk@21/bin",
	} {
		j := d + "/java"
		if fi, err := os.Stat(j); err == nil && !fi.IsDir() {
			out, _ := exec.Command(j, "-version").CombinedOutput()
			s := string(out)
			for _, v := range []string{"\"21", "\"22", "\"23", "\"24", "\"25", "\"26"} {
				if strings.Contains(s, v) {
					return d
				}
			}
		}
	}
	return ""
}

func freePort() int {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestMain(m *testing.M) {
	jbin := java21Bin()
	if _, err := exec.LookPath("gcloud"); err == nil && jbin != "" {
		port := freePort()
		host := fmt.Sprintf("127.0.0.1:%d", port)
		cmd := exec.Command("gcloud", "beta", "emulators", "firestore", "start",
			"--host-port="+host, "--quiet")
		cmd.Env = append(os.Environ(),
			"PATH="+jbin+":"+os.Getenv("PATH"), "CLOUDSDK_CORE_DISABLE_PROMPTS=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if cmd.Start() == nil {
			ready := false
			for i := 0; i < 80; i++ {
				if c, e := http.Get("http://" + host + "/"); e == nil {
					c.Body.Close()
					ready = true
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
			if ready {
				os.Setenv("FIRESTORE_EMULATOR_HOST", host)
				emuUp = true
				code := m.Run()
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				os.Exit(code)
			}
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	os.Exit(m.Run())
}

func needEmu(t *testing.T) {
	t.Helper()
	if !emuUp {
		t.Skip("Firestore emulator 不在（gcloud / Java21+ 無し）")
	}
}

func newState(t *testing.T) *state.Client {
	t.Helper()
	st, err := state.New(context.Background(), emuProject, "relay")
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// fakeGoogleVerifier: credential 文字列をそのままメールとして扱う
// （本物の idtoken 検証はしない＝テスト注入。webauth.GoogleVerifier を満たす）。
type fakeGoogleVerifier struct{ fail bool }

func (f fakeGoogleVerifier) Verify(ctx context.Context, idToken, audience string) (string, bool, error) {
	if f.fail || idToken == "" {
		return "", false, nil
	}
	return idToken, true, nil // credential=email という単純な fake
}

func newTestServer(t *testing.T, allowedEmails, redirectHosts string) (*Server, *httptest.Server) {
	t.Helper()
	st := newState(t)
	signer := webauth.NewSigner("test-secret")
	srv := New(st, signer, "test-client-id", allowedEmails, fakeGoogleVerifier{}, redirectHosts, "")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	srv.selfBase = ts.URL // httptest URL が決まってから self を確定
	return srv, ts
}

func TestPKCEVerify(t *testing.T) {
	verifier := "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHI"
	challenge := pkceChallengeFromVerifier(verifier)
	if !pkceVerify(challenge, verifier) {
		t.Fatal("正しい verifier が検証を通らない")
	}
	if pkceVerify(challenge, "wrong-verifier") {
		t.Fatal("間違った verifier が検証を通った")
	}
	if pkceVerify("", verifier) || pkceVerify(challenge, "") {
		t.Fatal("空文字が検証を通った")
	}
}

func TestHostAllowedDefaultDeny(t *testing.T) {
	s := &Server{allowedRedirectHosts: map[string]bool{}}
	if s.hostAllowed("https://grok.example.com/cb") {
		t.Fatal("allowlist 空なのに許可された（デフォルト deny が壊れている）")
	}
	s2 := &Server{allowedRedirectHosts: map[string]bool{"grok.example.com": true}}
	if !s2.hostAllowed("https://grok.example.com/cb") {
		t.Fatal("allowlist 一致のホストが拒否された")
	}
	if s2.hostAllowed("http://grok.example.com/cb") { // https 必須
		t.Fatal("http（非TLS）が許可された")
	}
	if s2.hostAllowed("https://evil.example.com/cb") {
		t.Fatal("allowlist に無いホストが許可された")
	}
}

func TestMetadataEndpoints(t *testing.T) {
	needEmu(t)
	_, ts := newTestServer(t, "owner@example.com", "grok.example.com")

	resp, err := http.Get(ts.URL + "/.well-known/oauth-protected-resource")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("protected-resource: %v status=%v", err, resp)
	}
	resp2, err := http.Get(ts.URL + "/.well-known/oauth-authorization-server")
	if err != nil || resp2.StatusCode != 200 {
		t.Fatalf("authorization-server: %v status=%v", err, resp2)
	}
}

func TestRegisterDCR(t *testing.T) {
	needEmu(t)
	_, ts := newTestServer(t, "owner@example.com", "grok.example.com")

	ok := strings.NewReader(`{"client_name":"Grok Bot","redirect_uris":["https://grok.example.com/cb"]}`)
	resp, err := http.Post(ts.URL+"/oauth/register", "application/json", ok)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("正常な DCR が失敗: err=%v status=%v", err, resp)
	}

	bad := strings.NewReader(`{"client_name":"Evil","redirect_uris":["https://evil.example.com/cb"]}`)
	resp2, err := http.Post(ts.URL+"/oauth/register", "application/json", bad)
	if err != nil || resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("allowlist 外ホストの DCR が通った: err=%v status=%v", err, resp2)
	}
}

// TestFullAuthorizeAndTokenFlow: DCR → authorize(未ログイン→login誘導) →
// GIS ログイン(fake) → authorize(consent) → 同意 → code 発行 →
// token 交換(PKCE検証) → access token が Verify を通る、までを実 HTTP で通す。
func TestFullAuthorizeAndTokenFlow(t *testing.T) {
	needEmu(t)
	const email = "owner@example.com"
	s, ts := newTestServer(t, email, "127.0.0.1")

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return nil // 自動フォロー（デフォルト）。必要に応じ個別に止める。
	}}

	// 1) DCR でクライアント登録。
	redirectURI := "https://127.0.0.1/cb" // 実際には接続しない。コード交換まで到達させる検証用
	dcrResp, err := client.Post(ts.URL+"/oauth/register", "application/json",
		strings.NewReader(`{"client_name":"Test Client","redirect_uris":["`+redirectURI+`"]}`))
	if err != nil {
		t.Fatalf("DCR: %v", err)
	}
	var dcr registerResponse
	mustDecode(t, dcrResp, &dcr)

	// 2) PKCE verifier/challenge を用意。
	verifier := "test-verifier-0123456789abcdefghijklmnopqrstuvwxyz"
	challenge := pkceChallengeFromVerifier(verifier)

	authQ := url.Values{
		"response_type": {"code"}, "client_id": {dcr.ClientID}, "redirect_uri": {redirectURI},
		"scope": {"drover.read drover.input"}, "state": {"xyz123"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"},
		"resource": {s.selfBase + "/mcp"},
	}

	// 3) 未ログインで authorize → login ページへ飛ばされる。
	resp, err := client.Get(ts.URL + "/oauth/authorize?" + authQ.Encode())
	if err != nil {
		t.Fatalf("authorize(未ログイン): %v", err)
	}
	if !strings.Contains(resp.Request.URL.Path, "/oauth/login") {
		t.Fatalf("未ログインで login へ飛んでいない: 最終パス=%s", resp.Request.URL.Path)
	}

	// 4) login ページから login-callback（GIS fake）を直接叩き、cookie を得る。
	//    login ページが生成する data-login_uri をそのまま使うのが筋だが、ここでは
	//    return_to だけ引き継いで直接 POST する（CSRF 二重送信はこちらで用意）。
	returnTo := resp.Request.URL.Query().Get("return_to")
	if returnTo == "" {
		t.Fatalf("login ページに return_to が無い: %s", resp.Request.URL.String())
	}
	csrf := "csrf-token-1"
	loginURL := ts.URL + "/oauth/login-callback?return_to=" + url.QueryEscape(returnTo)
	req, _ := http.NewRequest(http.MethodPost, loginURL,
		strings.NewReader(url.Values{"credential": {email}, "g_csrf_token": {csrf}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "g_csrf_token", Value: csrf})
	resp2, err := client.Do(req)
	if err != nil {
		t.Fatalf("login-callback: %v", err)
	}

	// 5) login 後は authorize の consent 画面に戻っているはず。
	if !strings.Contains(resp2.Request.URL.Path, "/oauth/authorize") {
		t.Fatalf("login 後に authorize へ戻っていない: %s", resp2.Request.URL.String())
	}
	body := readBody(t, resp2)
	if !strings.Contains(body, "許可する") {
		t.Fatalf("consent 画面になっていない: %s", body[:min(200, len(body))])
	}

	// 6) 同意を POST（decision=accept）→ client の redirect_uri へ code 付きで
	//    飛ばされるはず。実際には接続できない URL なので CheckRedirect で止める。
	noFollow := &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	form := authQ
	form.Set("decision", "accept")
	resp3, err := noFollow.PostForm(ts.URL+"/oauth/authorize", form)
	if err != nil {
		t.Fatalf("authorize(accept): %v", err)
	}
	if resp3.StatusCode != http.StatusFound {
		t.Fatalf("同意後のリダイレクトが無い: status=%d body=%s", resp3.StatusCode, readBody(t, resp3))
	}
	loc, err := url.Parse(resp3.Header.Get("Location"))
	if err != nil {
		t.Fatalf("Location パース失敗: %v", err)
	}
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("code が発行されていない: %s", loc.String())
	}
	if loc.Query().Get("state") != "xyz123" {
		t.Fatalf("state が往復していない: %s", loc.String())
	}

	// 7) token エンドポイントで code→access/refresh 交換。
	tokResp, err := client.PostForm(ts.URL+"/oauth/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier},
		"redirect_uri": {redirectURI},
	})
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	var tok tokenResponse
	mustDecode(t, tokResp, &tok)
	if tok.AccessToken == "" || tok.RefreshToken == "" {
		t.Fatalf("token 応答不正: %+v", tok)
	}

	// 8) 発行された access token が Verify を通る。
	vt, ok := s.Verify(context.Background(), "Bearer "+tok.AccessToken)
	if !ok || vt.Email != email {
		t.Fatalf("発行済み access token が Verify を通らない: ok=%v vt=%+v", ok, vt)
	}
	if !vt.HasScope("drover.read") || !vt.HasScope("drover.input") {
		t.Fatalf("scope が反映されていない: %+v", vt)
	}

	// 9) 同じ code を再利用しようとすると失敗する（一回消費）。
	tokResp2, _ := client.PostForm(ts.URL+"/oauth/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier},
	})
	if tokResp2.StatusCode == http.StatusOK {
		t.Fatal("同じ code で2回トークン発行できた（一回消費が壊れている）")
	}

	// 10) refresh ローテーション: 新しい access/refresh が発行され、古い
	//     refresh は再利用で reuse 検知（family 一括失効）される。
	refResp, err := client.PostForm(ts.URL+"/oauth/token", url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken},
	})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	var tok2 tokenResponse
	mustDecode(t, refResp, &tok2)
	if tok2.AccessToken == "" || tok2.RefreshToken == "" || tok2.RefreshToken == tok.RefreshToken {
		t.Fatalf("refresh ローテーション不正: %+v", tok2)
	}
	// 古い access token も新しいローテーションでは失効させない設計
	// （access は access 自身の TTL のみで管理。refresh 失効は refresh 系列のみ）
	// ので、ここでは古い refresh の再利用だけを検証する。
	reuseResp, _ := client.PostForm(ts.URL+"/oauth/token", url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken}, // 古い方を再提示
	})
	if reuseResp.StatusCode == http.StatusOK {
		t.Fatal("ローテーション済み refresh_token の再提示が成功した（reuse 検知が壊れている）")
	}
	// family 一括失効の証明: ローテーションで新しく得た refresh もこの時点で
	// 失効しているはず。
	famResp, _ := client.PostForm(ts.URL+"/oauth/token", url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {tok2.RefreshToken},
	})
	if famResp.StatusCode == http.StatusOK {
		t.Fatal("reuse 検知後も同一 family の新 refresh が生きている（family 失効が壊れている）")
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("body 読取失敗: %v", err)
	}
	return string(b)
}

func mustDecode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("JSON decode 失敗（status=%d）: %v", resp.StatusCode, err)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
