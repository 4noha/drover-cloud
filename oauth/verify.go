package oauth

// Bearer トークン検証（mcp パッケージが /mcp への各リクエストで呼ぶ）。
// 検証は Firestore 1 read/リクエスト（インスタンス内キャッシュは呼び手の
// 責務＝mcp パッケージ側に置く。ここは「正しく検証する」ことだけを担う）。

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// VerifiedToken は検証に成功した access token の中身（mcp ツールの認可判定に使う）。
type VerifiedToken struct {
	Email  string
	Scopes map[string]bool
}

// HasScope は指定スコープを持つか。
func (v VerifiedToken) HasScope(scope string) bool { return v.Scopes[scope] }

// Verify は Authorization: Bearer <token> を検証する。owner 判定（email が
// ALLOWED_EMAILS に含まれるか）は**毎リクエスト**再検査する（env から
// 外したら即失効。drover-cloud-mcp-design.md §4.3）。
func (s *Server) Verify(ctx context.Context, authHeader string) (VerifiedToken, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(authHeader, prefix) {
		return VerifiedToken{}, false
	}
	raw := strings.TrimSpace(strings.TrimPrefix(authHeader, prefix))
	if raw == "" {
		return VerifiedToken{}, false
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	t, ok, err := s.st.GetOAuthToken(cctx, sha256Hex(raw))
	if err != nil || !ok || t.Kind != "access" {
		return VerifiedToken{}, false
	}
	if !s.allowed[strings.ToLower(t.Email)] {
		// 発行後に ALLOWED_EMAILS から外れた owner のトークンを拒否する
		// （§4.3「毎リクエストで再判定」）。
		return VerifiedToken{}, false
	}
	sc := map[string]bool{}
	for _, x := range t.Scopes {
		sc[x] = true
	}
	return VerifiedToken{Email: t.Email, Scopes: sc}, true
}

// RequireBearer は http.HandlerFunc をラップし、未認証/不正トークンに
// RFC 9728 の 401 + WWW-Authenticate を返す。認可済みなら ctx へ
// VerifiedToken を詰めて次へ渡す（mcp パッケージの各ツールハンドラが
// TokenFromContext で取り出す）。
type verifiedTokenCtxKey struct{}

func TokenFromContext(ctx context.Context) (VerifiedToken, bool) {
	v, ok := ctx.Value(verifiedTokenCtxKey{}).(VerifiedToken)
	return v, ok
}

func (s *Server) RequireBearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vt, ok := s.Verify(r.Context(), r.Header.Get("Authorization"))
		if !ok {
			w.Header().Set("WWW-Authenticate",
				`Bearer resource_metadata="`+s.selfBase+`/.well-known/oauth-protected-resource"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), verifiedTokenCtxKey{}, vt)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
