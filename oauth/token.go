package oauth

// POST /oauth/token。authorization_code（code+verifier → access+refresh）と
// refresh_token（ローテーション。reuse 検知で family 一括失効）の2 grant を
// 扱う。drover-cloud-mcp-design.md §4.1・§9 決定事項8（access 1h、
// drover.admin を含む場合のみ 15 分／refresh 30 日）。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/4noha/drover-cloud/state"
)

const (
	accessTokenTTL      = time.Hour
	accessTokenTTLAdmin = 15 * time.Minute // drover.admin を含むスコープのみ短縮
	refreshTokenTTL     = 30 * 24 * time.Hour
)

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

func accessTTLFor(scopes []string) time.Duration {
	for _, s := range scopes {
		if s == "drover.admin" {
			return accessTokenTTLAdmin
		}
	}
	return accessTokenTTL
}

func genOpaqueToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST のみ", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "form 解析失敗")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		s.tokenFromCode(ctx, w, r)
	case "refresh_token":
		s.tokenFromRefresh(ctx, w, r)
	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type",
			"authorization_code か refresh_token のみ対応")
	}
}

func (s *Server) tokenFromCode(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	code := r.PostForm.Get("code")
	verifier := r.PostForm.Get("code_verifier")
	redirectURI := r.PostForm.Get("redirect_uri")
	if code == "" || verifier == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "code/code_verifier が必要")
		return
	}
	oc, ok, err := s.st.ConsumeOAuthCode(ctx, sha256Hex(code))
	if err != nil {
		http.Error(w, "server_error", http.StatusInternalServerError)
		return
	}
	if !ok {
		// 期限切れ・既消費・不在のいずれも同一エラー（情報を漏らさない）。
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "code が無効です")
		return
	}
	if redirectURI != "" && redirectURI != oc.RedirectURI {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri が一致しません")
		return
	}
	if !pkceVerify(oc.CodeChallenge, verifier) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "PKCE 検証失敗")
		return
	}
	s.issueTokenPair(ctx, w, oc.Email, oc.ClientID, oc.Scopes, "")
}

func (s *Server) tokenFromRefresh(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	raw := r.PostForm.Get("refresh_token")
	if raw == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "refresh_token が必要")
		return
	}
	hash := sha256Hex(raw)
	t, ok, err := s.st.GetOAuthTokenRaw(ctx, hash)
	if err != nil {
		http.Error(w, "server_error", http.StatusInternalServerError)
		return
	}
	if !ok || t.Kind != "refresh" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "refresh_token が無効です")
		return
	}
	if t.Superseded {
		// 既にローテーション済みの refresh token が再提示された＝盗難された
		// refresh が後から使われた可能性（reuse）。正規側・攻撃側のどちらが
		// 先に使ったか区別できないため、family ごと道連れに失効させる
		// （drover-cloud-mcp-design.md §4.1 のローテーション失効）。
		_ = s.st.RevokeOAuthTokenFamily(ctx, t.Family)
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "refresh_token は既に使用されています")
		return
	}
	if exp, perr := time.Parse(time.RFC3339Nano, t.ExpiresAt); perr != nil || time.Now().After(exp) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "refresh_token の有効期限切れ")
		return
	}
	if err := s.st.MarkOAuthTokenSuperseded(ctx, hash); err != nil {
		http.Error(w, "server_error", http.StatusInternalServerError)
		return
	}
	s.issueTokenPair(ctx, w, t.Email, t.ClientID, t.Scopes, t.Family)
}

// issueTokenPair は access+refresh を同じ family で発行する。family が空なら
// 新規発行（authorization_code grant）、非空ならローテーション継続
// （refresh_token grant）。
func (s *Server) issueTokenPair(ctx context.Context, w http.ResponseWriter, email, clientID string,
	scopes []string, family string) {
	if family == "" {
		fb, err := genOpaqueToken()
		if err != nil {
			http.Error(w, "server_error", http.StatusInternalServerError)
			return
		}
		family = fb
	}
	access, err := genOpaqueToken()
	if err != nil {
		http.Error(w, "server_error", http.StatusInternalServerError)
		return
	}
	refresh, err := genOpaqueToken()
	if err != nil {
		http.Error(w, "server_error", http.StatusInternalServerError)
		return
	}
	accessTTL := accessTTLFor(scopes)
	now := time.Now()
	if err := s.st.CreateOAuthToken(ctx, sha256Hex(access), state.OAuthToken{
		Kind: "access", Email: email, ClientID: clientID, Scopes: scopes, Family: family,
		ExpiresAt: now.Add(accessTTL).UTC().Format(time.RFC3339Nano),
	}); err != nil {
		http.Error(w, "server_error", http.StatusInternalServerError)
		return
	}
	if err := s.st.CreateOAuthToken(ctx, sha256Hex(refresh), state.OAuthToken{
		Kind: "refresh", Email: email, ClientID: clientID, Scopes: scopes, Family: family,
		ExpiresAt: now.Add(refreshTokenTTL).UTC().Format(time.RFC3339Nano),
	}); err != nil {
		http.Error(w, "server_error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, tokenResponse{
		AccessToken: access, TokenType: "Bearer", ExpiresIn: int(accessTTL.Seconds()),
		RefreshToken: refresh, Scope: strings.Join(scopes, " "),
	})
}
