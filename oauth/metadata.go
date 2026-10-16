package oauth

// RFC 9728（OAuth 2.0 Protected Resource Metadata）と RFC 8414（OAuth 2.0
// Authorization Server Metadata）の応答。drover-cloud-mcp-design.md §4.1。

import (
	"encoding/json"
	"net/http"
)

// scopesSupported は §4.2 のスコープ一覧（4 段階）。
var scopesSupported = []string{"drover.read", "drover.input", "drover.control", "drover.admin"}

func (s *Server) protectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resource":              s.selfBase + "/mcp",
		"authorization_servers": []string{s.selfBase},
		"scopes_supported":      scopesSupported,
	})
}

func (s *Server) authServerMetadata(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"issuer":                                s.selfBase,
		"authorization_endpoint":                s.selfBase + "/oauth/authorize",
		"token_endpoint":                        s.selfBase + "/oauth/token",
		"registration_endpoint":                 s.selfBase + "/oauth/register",
		"scopes_supported":                      scopesSupported,
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"}, // public client + PKCE
	})
}
