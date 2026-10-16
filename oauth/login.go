package oauth

// GIS（Google Identity Services）ログインページの複製。web/ui.go の
// loginHTMLTmpl と同じ仕組みだが、data-login_uri を /oauth/login-callback に
// 向け、return_to（= 元の /oauth/authorize?... の完全な相対 URL）を素通しする。
// web.go（cm とバイト等価対象）に return_to 対応を足さずに済ませるための
// 代替経路（drover-cloud-mcp-design.md §9 決定事項1）。発行する cookie は
// web 側と同じ Signer・同じ cm_session フォーマットなので相互運用できる
// （どちらでログインしても両方に効く）。

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/4noha/drover-cloud/webauth"
)

// cookieName/cookieTTL は web.Server のものと同じ値を独立に持つ（web package
// は unexported const のため import できない＝値のコピーで固定。どちらかを
// 変えたらもう片方も直すこと）。
const cookieName = "cm_session"
const cookieTTL = 12 * time.Hour
const accountScope = "*"

// isValidReturnTo は return_to が自サーバーの /oauth/authorize への相対パス
// であることを検証する（open redirect 対策。GIS ログインを経由して任意の
// 外部 URL へ飛ばされないようにする）。
func isValidReturnTo(returnTo string) bool {
	if !strings.HasPrefix(returnTo, "/oauth/authorize?") && returnTo != "/oauth/authorize" {
		return false
	}
	// パス区切りを装った外部 URL（"//evil.com/..."）を弾く。
	u, err := url.Parse(returnTo)
	if err != nil || u.IsAbs() || u.Host != "" {
		return false
	}
	return true
}

const loginHTMLTmpl = `<!doctype html><html lang="ja"><meta charset="utf-8">
<title>drover-cloud MCP — ログイン</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<body style="font-family:system-ui;max-width:420px;margin:16vh auto;padding:0 16px;text-align:center">
<h1 style="font-size:20px">drover-cloud MCP 連携</h1>
<p style="color:#666;font-size:14px">Google アカウントでログインして、MCP クライアントの連携を許可してください。</p>
<script src="https://accounts.google.com/gsi/client" async></script>
<div id="g_id_onload"
     data-client_id="%s"
     data-login_uri="%s"
     data-ux_mode="redirect"></div>
<div class="g_id_signin" data-type="standard" data-size="large"
     data-text="signin_with" data-shape="pill"
     style="display:inline-block;margin-top:16px"></div>
<noscript>JavaScript を有効にしてください。</noscript>
</body></html>`

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	returnTo := r.URL.Query().Get("return_to")
	if !isValidReturnTo(returnTo) {
		http.Error(w, "invalid return_to", http.StatusBadRequest)
		return
	}
	loginURI := "/oauth/login-callback?return_to=" + url.QueryEscape(returnTo)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, loginHTMLTmpl, html.EscapeString(s.clientID), html.EscapeString(loginURI))
}

func (s *Server) setCookie(w http.ResponseWriter, r *http.Request, tok string) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: tok, Path: "/",
		HttpOnly: true, Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(cookieTTL.Seconds()),
	})
}

// loginCallback は web.authGoogle と同じ検証（GIS 二重送信 CSRF・idtoken・
// allowlist）を行い、同じ cookie フォーマットで発行する。既存 /auth/google
// との違いは、成功後に return_to（元の /oauth/authorize リクエスト）へ戻す点。
func (s *Server) loginCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST のみ", http.StatusMethodNotAllowed)
		return
	}
	returnTo := r.URL.Query().Get("return_to")
	if !isValidReturnTo(returnTo) {
		http.Error(w, "invalid return_to", http.StatusBadRequest)
		return
	}
	cc, _ := r.Cookie("g_csrf_token")
	if cc == nil || cc.Value == "" || cc.Value != r.FormValue("g_csrf_token") {
		http.Error(w, "CSRF 検証失敗", http.StatusForbidden)
		return
	}
	cred := r.FormValue("credential")
	if cred == "" {
		http.Error(w, "credential が必要", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	email, verified, err := s.gv.Verify(ctx, cred, s.clientID)
	if err != nil || email == "" || !verified {
		http.Error(w, "Google 認証に失敗しました", http.StatusUnauthorized)
		return
	}
	if !s.allowed[strings.ToLower(email)] {
		http.Error(w, "このアカウントは許可されていません", http.StatusForbidden)
		return
	}
	tok := s.signer.Sign(webauth.Token{
		PC: email, Scope: accountScope, Exp: time.Now().Add(cookieTTL).Unix(),
	})
	s.setCookie(w, r, tok)
	http.Redirect(w, r, returnTo, http.StatusFound)
}
