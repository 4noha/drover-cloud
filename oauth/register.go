package oauth

// POST /oauth/register（RFC 7591 Dynamic Client Registration）。
// redirect_uris は MCP_ALLOWED_REDIRECT_HOSTS に一致するホストのみ受理する
// （任意クライアント登録で owner をフィッシングさせないため。
// drover-cloud-mcp-design.md §4.1）。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type registerRequest struct {
	ClientName   string   `json:"client_name"`
	RedirectURIs []string `json:"redirect_uris"`
}

type registerResponse struct {
	ClientID                string   `json:"client_id"`
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

// registerBodyMax は DCR リクエスト本文の上限（client_name/redirect_uris の
// 数個の文字列で十分収まる。異常に大きい入力を先に撥ねる）。
const registerBodyMax = 64 * 1024

// redirectRule は allowlist 1エントリの制約（oauth.go の New がパースする）。
// port/path は空文字列なら無制限（任意のポート/パスを許可）。
type redirectRule struct {
	port string
	path string
}

// loopbackHosts はスキーム http を例外的に許可するホスト（ネイティブ/CLI
// アプリが OS に一時 HTTP サーバーを立ててコールバックを受ける RFC 8252
// パターン。ループバックは外部ネットワークから到達不可のため https 不要）。
var loopbackHosts = map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}

// schemeAllowed はホストに対して許容するスキームを判定する。ループバック
// 以外は https 必須のまま（デフォルト deny の例外を増やさない）。
func schemeAllowed(scheme, host string) bool {
	if loopbackHosts[host] {
		return scheme == "http" || scheme == "https"
	}
	return scheme == "https"
}

// hostAllowed は redirect_uri のホスト・スキーム（・allowlist エントリが
// ポート/パスも指定していればそれらにも完全一致）が allowlist に一致するか
// を検証する。空の allowlist（未設定）は常に拒否＝デフォルト deny。
func (s *Server) hostAllowed(rawURL string) bool {
	if len(s.allowedRedirectHosts) == 0 {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	rule, ok := s.allowedRedirectHosts[host]
	if !ok {
		return false
	}
	if !schemeAllowed(u.Scheme, host) {
		return false
	}
	if rule.port != "" && u.Port() != rule.port {
		return false
	}
	return rule.path == "" || rule.path == u.Path
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST のみ", http.StatusMethodNotAllowed)
		return
	}
	var req registerRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, registerBodyMax)).Decode(&req); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "request body の JSON 解析に失敗")
		return
	}
	if len(req.RedirectURIs) == 0 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris が空")
		return
	}
	for _, u := range req.RedirectURIs {
		if !s.hostAllowed(u) {
			// MCP_ALLOWED_REDIRECT_HOSTS は未決事項7（コネクタのコールバック
			// ホストが事前に分からない）のため空運用がある。拒否した
			// redirect_uri を Cloud Run ログに出し、owner が実ホストを
			// 確認してから allowlist へ追加できるようにする。
			log.Printf("oauth/register: 拒否 client_name=%q redirect_uri=%q（MCP_ALLOWED_REDIRECT_HOSTS に無い）",
				req.ClientName, u)
			writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri",
				"redirect_uri のホストが MCP_ALLOWED_REDIRECT_HOSTS に無い: "+u)
			return
		}
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		http.Error(w, "server_error", http.StatusInternalServerError)
		return
	}
	clientID := "mcp_" + hex.EncodeToString(b[:])
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := s.st.CreateOAuthClient(ctx, clientID, req.ClientName, req.RedirectURIs); err != nil {
		http.Error(w, "server_error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(registerResponse{
		ClientID: clientID, ClientName: req.ClientName, RedirectURIs: req.RedirectURIs,
		TokenEndpointAuthMethod: "none",
	})
}

// writeOAuthError は RFC 6749 §5.2 形式のエラー応答。
func writeOAuthError(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": desc})
}

// writeJSON は成功応答の共通ヘルパ。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store") // RFC 6749 §5.1: token 応答はキャッシュさせない
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
