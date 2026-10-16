// Package oauth は MCP（drover-cloud-mcp-design.md §4）向けの認可サーバー。
// MCP 認可仕様（2025-06-18）に沿い、drover-cloud 自身が Authorization Server
// を兼ね、上流 IdP として既存の Google ログイン（webauth/web と同じ
// GIS + ALLOWED_EMAILS）を使う（「ID 連携型 AS」）。
//
// web パッケージ（relay/web/webauth/state と並ぶ「cm とバイト等価」対象の
// 4 パッケージの1つ）は無改変に保つ。/login の return_to 対応は web.go を
// 直接拡張せず、この package 側に GIS ログインページを複製する
// （login.go）。cookie フォーマット（webauth.Token・cm_session と同じ
// Signer）は共有するので、どちらでログインしても同じ cookie が両方に効く。
//
// ハッシュ化の責務: state 層と同じ分担（state/oauth.go のコメント参照）。
// 平文の認可コード・トークンは Firestore に保存しない。
package oauth

import (
	"net/http"
	"strings"

	"github.com/4noha/drover-cloud/state"
	"github.com/4noha/drover-cloud/webauth"
)

// Server は MCP 認可サーバー本体。
type Server struct {
	st       *state.Client
	signer   *webauth.Signer
	clientID string // Google OAuth Web Client ID（既存 web と共用）
	allowed  map[string]bool
	gv       webauth.GoogleVerifier
	// allowedRedirectHosts は DCR で受理する redirect_uri のホスト allowlist
	// （env MCP_ALLOWED_REDIRECT_HOSTS、カンマ区切り）。空なら DCR は全拒否
	// （フィッシング対策のデフォルト deny。未決事項7＝コネクタのコールバック
	// ホストが判明するまではプレースホルダのまま運用する）。
	// key はホスト名（小文字）、value は redirectRule（任意のポート/パス制限）。
	// 各エントリは env 側で "host"・"host/path"・"host:port"・"host:port/path"
	// のいずれかで指定する（例 "www.cursor.com/agents/mcp/oauth/callback"・
	// "localhost:8787/callback"）。スキームは hostAllowed が判定する
	// （ループバック host=localhost/127.0.0.1/::1 のみ http も許可。
	// RFC 8252 のネイティブアプリ・パターン。それ以外は https 必須）。
	allowedRedirectHosts map[string]redirectRule
	// selfBase はこのサーバー自身の外部 URL（例 "https://host"、末尾 / 無し）。
	// メタデータ応答・resource パラメータ照合に使う。
	selfBase string
}

// New は MCP 認可サーバーを組み立てる。signer/clientID/allowedEmails/gv は
// 既存 web.New と同じ値を渡す（cookie とログイン許可判定を共有するため）。
// allowedRedirectHosts は "MCP_ALLOWED_REDIRECT_HOSTS" の値をそのまま渡す
// （カンマ区切り。各エントリは "host"・"host/path"・"host:port"・
// "host:port/path"）。
func New(st *state.Client, signer *webauth.Signer, clientID, allowedEmails string,
	gv webauth.GoogleVerifier, allowedRedirectHosts, selfBase string) *Server {
	am := map[string]bool{}
	for _, e := range strings.Split(allowedEmails, ",") {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			am[e] = true
		}
	}
	rh := map[string]redirectRule{}
	for _, h := range strings.Split(allowedRedirectHosts, ",") {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		hostPort, path := h, ""
		if i := strings.Index(h, "/"); i >= 0 {
			hostPort, path = h[:i], h[i:]
		}
		host, port := hostPort, ""
		if i := strings.LastIndex(hostPort, ":"); i >= 0 {
			host, port = hostPort[:i], hostPort[i+1:]
		}
		if host = strings.ToLower(host); host != "" {
			rh[host] = redirectRule{port: port, path: path}
		}
	}
	if gv == nil {
		gv = webauth.DefaultGoogleVerifier
	}
	return &Server{st: st, signer: signer, clientID: clientID, allowed: am,
		gv: gv, allowedRedirectHosts: rh, selfBase: strings.TrimRight(selfBase, "/")}
}

// Handler は /.well-known/* と /oauth/* をまとめて返す（main.go が /mcp と
// 同じ mux に Handle する。MCP_ENABLED 時のみマウントされる＝未設定なら
// 既存構成に一切影響しない）。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource", s.protectedResourceMetadata)
	mux.HandleFunc("/.well-known/oauth-authorization-server", s.authServerMetadata)
	mux.HandleFunc("/oauth/register", s.register)
	mux.HandleFunc("/oauth/authorize", s.authorize)
	mux.HandleFunc("/oauth/login", s.loginPage)
	mux.HandleFunc("/oauth/login-callback", s.loginCallback)
	mux.HandleFunc("/oauth/token", s.token)
	return mux
}

// authCookie は cm_session cookie を検証する（web.Server.auth と同じロジック・
// 同じ Signer を共有するので cookie フォーマットは完全互換）。
func (s *Server) authCookie(r *http.Request) (webauth.Token, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return webauth.Token{}, false
	}
	tok, ok := s.signer.Verify(c.Value)
	if !ok || !s.allowed[strings.ToLower(tok.PC)] {
		// Token.PC はアカウント cookie では email を保持する（web.authGoogle と
		// 同じ用法）。allowed から後で外れた email は毎リクエストで再判定し
		// 即座に通さない。
		return webauth.Token{}, false
	}
	return tok, true
}
