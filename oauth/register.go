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

// hostAllowed は redirect_uri が https かつホストが allowlist に一致するかを
// 検証する。空の allowlist（未設定）は常に拒否＝デフォルト deny。
func (s *Server) hostAllowed(rawURL string) bool {
	if len(s.allowedRedirectHosts) == 0 {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return false
	}
	return s.allowedRedirectHosts[strings.ToLower(u.Hostname())]
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
