package oauth

// GET/POST /oauth/authorize。PKCE(S256) 必須、resource パラメータ（RFC 8707）
// が自 /mcp と一致することを検証する。既存 cm_session cookie が有効ならそれを
// 本人確認として再利用し、無ければ /oauth/login（login.go）へ return_to 付きで
// 飛ばす。同意画面でクライアント名・スコープを明示し、owner が承認したら
// 認可コード（60秒・一回消費）を発行する。drover-cloud-mcp-design.md §4.1。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/4noha/drover-cloud/state"
)

const authCodeTTL = 60 * time.Second

// authParams は /oauth/authorize のクエリ（GET）/フォーム値（POST、consent
// 画面の hidden フィールドで素通しされる）。
type authParams struct {
	ClientID            string
	RedirectURI         string
	Scope               string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
	Resource            string
}

func parseAuthParams(q url.Values) authParams {
	return authParams{
		ClientID:            q.Get("client_id"),
		RedirectURI:         q.Get("redirect_uri"),
		Scope:               q.Get("scope"),
		State:               q.Get("state"),
		CodeChallenge:       q.Get("code_challenge"),
		CodeChallengeMethod: q.Get("code_challenge_method"),
		Resource:            q.Get("resource"),
	}
}

// consentHTMLTmpl: %[1]s=ClientName, %[2]s=scope一覧(エスケープ済HTML),
// %[3]s=hidden fields（全 authParams を素通しする input 群）。
const consentHTMLTmpl = `<!doctype html><html lang="ja"><meta charset="utf-8">
<title>drover-cloud MCP — 連携の確認</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<body style="font-family:system-ui;max-width:480px;margin:10vh auto;padding:0 16px">
<h1 style="font-size:18px">%[1]s が drover-cloud への接続を要求しています</h1>
<p style="color:#b4401f;font-weight:bold">全 PC のコーディングエージェントセッションを操作できるようになります。</p>
<ul style="font-size:14px;color:#444">%[2]s</ul>
<form method="post">
%[3]s
<button type="submit" name="decision" value="accept" style="padding:10px 20px;margin-right:8px">許可する</button>
<button type="submit" name="decision" value="deny" style="padding:10px 20px">拒否する</button>
</form>
</body></html>`

func scopeListHTML(scope string) string {
	var b strings.Builder
	for _, sc := range strings.Fields(scope) {
		b.WriteString("<li>" + html.EscapeString(sc) + "</li>")
	}
	if b.Len() == 0 {
		b.WriteString("<li>(スコープ指定なし)</li>")
	}
	return b.String()
}

func hiddenFieldsHTML(p authParams) string {
	f := func(name, val string) string {
		return `<input type="hidden" name="` + name + `" value="` + html.EscapeString(val) + `">`
	}
	return f("client_id", p.ClientID) + f("redirect_uri", p.RedirectURI) +
		f("scope", p.Scope) + f("state", p.State) +
		f("code_challenge", p.CodeChallenge) + f("code_challenge_method", p.CodeChallengeMethod) +
		f("resource", p.Resource)
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	var p authParams
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", "form 解析失敗")
			return
		}
		p = parseAuthParams(r.PostForm)
	} else if r.Method == http.MethodGet {
		if r.URL.Query().Get("response_type") != "code" {
			writeOAuthError(w, http.StatusBadRequest, "unsupported_response_type", "response_type は code のみ対応")
			return
		}
		p = parseAuthParams(r.URL.Query())
	} else {
		http.Error(w, "GET/POST のみ", http.StatusMethodNotAllowed)
		return
	}

	if p.CodeChallengeMethod != "S256" || p.CodeChallenge == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "PKCE(S256) が必須")
		return
	}
	if p.Resource != s.selfBase+"/mcp" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_target", "resource は "+s.selfBase+"/mcp のみ対応")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	client, ok, err := s.st.GetOAuthClient(ctx, p.ClientID)
	if err != nil {
		http.Error(w, "server_error", http.StatusInternalServerError)
		return
	}
	if !ok {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client", "未登録の client_id")
		return
	}
	redirectOK := false
	for _, u := range client.RedirectURIs {
		if u == p.RedirectURI {
			redirectOK = true
			break
		}
	}
	if !redirectOK {
		// redirect_uri 自体が信用できないので、ここへは絶対にリダイレクトしない
		// （エラーをこの画面で直接返す。RFC 6749 の既定挙動）。register() の
		// ホスト拒否と同じ理由でログに出す（owner が登録済み redirect_uris と
		// 実際に送られてきた値のズレを追えるように）。
		log.Printf("oauth/authorize: 拒否 client_id=%q redirect_uri=%q（登録済み redirect_uris に無い: %v）",
			p.ClientID, p.RedirectURI, client.RedirectURIs)
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "redirect_uri が未登録")
		return
	}

	tok, authed := s.authCookie(r)
	if !authed {
		// 元のリクエストを return_to としてそのまま login へ渡す（GET/POST
		// どちらで来ても、ログイン後は GET 相当の authorize へ戻す＝
		// authorize は毎回 consent 画面から再開する設計なので POST 版の
		// return_to は作らない）。
		returnTo := "/oauth/authorize?" + authorizeQuery(p).Encode()
		http.Redirect(w, r, "/oauth/login?return_to="+url.QueryEscape(returnTo), http.StatusFound)
		return
	}

	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, consentHTMLTmpl, html.EscapeString(client.Name), scopeListHTML(p.Scope), hiddenFieldsHTML(p))
		return
	}

	// POST: 同意の確定。
	decision := r.PostForm.Get("decision")
	if decision != "accept" {
		redirectWithError(w, r, p.RedirectURI, p.State, "access_denied")
		return
	}

	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		http.Error(w, "server_error", http.StatusInternalServerError)
		return
	}
	code := hex.EncodeToString(b[:])
	codeHash := sha256Hex(code)
	oc := state.OAuthCode{
		ClientID: p.ClientID, Email: tok.PC, Scopes: strings.Fields(p.Scope),
		CodeChallenge: p.CodeChallenge, RedirectURI: p.RedirectURI, Resource: p.Resource,
	}
	if err := s.st.CreateOAuthCode(ctx, codeHash, oc, authCodeTTL); err != nil {
		http.Error(w, "server_error", http.StatusInternalServerError)
		return
	}
	dest, _ := url.Parse(p.RedirectURI)
	q := dest.Query()
	q.Set("code", code)
	if p.State != "" {
		q.Set("state", p.State)
	}
	dest.RawQuery = q.Encode()
	http.Redirect(w, r, dest.String(), http.StatusFound)
}

func authorizeQuery(p authParams) url.Values {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", p.ClientID)
	q.Set("redirect_uri", p.RedirectURI)
	q.Set("scope", p.Scope)
	q.Set("state", p.State)
	q.Set("code_challenge", p.CodeChallenge)
	q.Set("code_challenge_method", p.CodeChallengeMethod)
	q.Set("resource", p.Resource)
	return q
}

// redirectWithError は RFC 6749 §4.1.2.1（認可拒否）に沿い client の
// redirect_uri へエラーを返す（redirect_uri 自体は上で検証済みのため安全）。
func redirectWithError(w http.ResponseWriter, r *http.Request, redirectURI, state, errCode string) {
	dest, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, "server_error", http.StatusInternalServerError)
		return
	}
	q := dest.Query()
	q.Set("error", errCode)
	if state != "" {
		q.Set("state", state)
	}
	dest.RawQuery = q.Encode()
	http.Redirect(w, r, dest.String(), http.StatusFound)
}
