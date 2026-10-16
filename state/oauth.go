package state

// MCP 認可サーバ（drover-cloud-mcp-design.md §4）の永続化層。
// oauth_clients / oauth_codes / oauth_tokens の CRUD のみを扱う（PKCE 検証・
// JWT 署名・cookie 再利用等のプロトコル判断は呼び手の oauth/ package の責務
// ＝CreatePairing/ConsumePairing と同じ分担）。
//
// ハッシュ化の責務分担: この package は「すでにハッシュ済みのキー文字列」を
// 受け取る（codeHash/tokenHash）。平文の code/token を計算する sha256 ロジックは
// 呼び手（oauth/ package、Phase 1）に置く＝CreatePairing(codeHash, ...) と同じ
// 既存の分担をそのまま踏襲。
//
// 一回消費パターンは ConsumePairing と同一（get→削除→期限チェック）。
// token は repeated 検証が要るため削除しない（明示 Revoke/期限切れでのみ消える）。

import (
	"context"
	"time"

	"cloud.google.com/go/firestore"
)

// ── oauth_clients ──────────────────────────────────────────────

// OAuthClient は oauth_clients/{client_id} の1件（RFC 7591 DCR で登録）。
type OAuthClient struct {
	Name         string   `firestore:"name" json:"name"`
	RedirectURIs []string `firestore:"redirect_uris" json:"redirect_uris"`
	CreatedAt    string   `firestore:"created_at" json:"created_at"`
}

func (c *Client) oauthClientsCol() *firestore.CollectionRef {
	return c.fs.Collection("oauth_clients")
}

// CreateOAuthClient は DCR 登録を保存する。clientID は呼び手が生成する
// （ランダム ID。衝突時は呼び手が再試行する＝PushCommand の id 生成と同じ分担）。
func (c *Client) CreateOAuthClient(ctx context.Context, clientID, name string, redirectURIs []string) error {
	_, err := c.oauthClientsCol().Doc(clientID).Set(ctx, map[string]any{
		"name":          name,
		"redirect_uris": redirectURIs,
		"created_at":    time.Now().UTC().Format(time.RFC3339Nano),
	})
	return err
}

// GetOAuthClient は登録済みクライアントを読む（authorize 時の redirect_uri 照合用）。
func (c *Client) GetOAuthClient(ctx context.Context, clientID string) (OAuthClient, bool, error) {
	snap, err := c.oauthClientsCol().Doc(clientID).Get(ctx)
	if err != nil || !snap.Exists() {
		return OAuthClient{}, false, nil
	}
	var oc OAuthClient
	if e := snap.DataTo(&oc); e != nil {
		return OAuthClient{}, false, e
	}
	return oc, true, nil
}

// ── oauth_codes（一回消費・ConsumePairing と同型）──────────────────

// OAuthCode は oauth_codes/{sha256} の1件（認可コード、60秒・一回消費）。
type OAuthCode struct {
	ClientID      string   `firestore:"client_id" json:"client_id"`
	Email         string   `firestore:"email" json:"email"`
	Scopes        []string `firestore:"scopes" json:"scopes"`
	CodeChallenge string   `firestore:"code_challenge" json:"code_challenge"`
	RedirectURI   string   `firestore:"redirect_uri" json:"redirect_uri"`
	Resource      string   `firestore:"resource" json:"resource"`
	ExpiresAt     string   `firestore:"exp" json:"exp"`
}

func (c *Client) oauthCodesCol() *firestore.CollectionRef {
	return c.fs.Collection("oauth_codes")
}

// CreateOAuthCode は認可コード発行時に保存する（codeHash はコード平文の
// sha256。平文はここでは一切扱わない＝CreatePairing と同じ理由）。
func (c *Client) CreateOAuthCode(ctx context.Context, codeHash string, oc OAuthCode, ttl time.Duration) error {
	_, err := c.oauthCodesCol().Doc(codeHash).Set(ctx, map[string]any{
		"client_id":      oc.ClientID,
		"email":          oc.Email,
		"scopes":         oc.Scopes,
		"code_challenge": oc.CodeChallenge,
		"redirect_uri":   oc.RedirectURI,
		"resource":       oc.Resource,
		"exp":            time.Now().Add(ttl).UTC().Format(time.RFC3339Nano),
	})
	return err
}

// ConsumeOAuthCode は codeHash を検索し、期限内なら中身を返して**doc を削除**
// （一回消費。ConsumePairing と同一パターン）。期限切れ/不在は ok=false
// （期限切れも掃除のため削除する）。
func (c *Client) ConsumeOAuthCode(ctx context.Context, codeHash string) (OAuthCode, bool, error) {
	ref := c.oauthCodesCol().Doc(codeHash)
	snap, gerr := ref.Get(ctx)
	if gerr != nil || !snap.Exists() {
		return OAuthCode{}, false, nil
	}
	var oc OAuthCode
	derr := snap.DataTo(&oc)
	_, _ = ref.Delete(ctx) // 一回消費（成否・パース可否問わず掃除）
	if derr != nil {
		return OAuthCode{}, false, nil
	}
	if t, perr := time.Parse(time.RFC3339Nano, oc.ExpiresAt); perr != nil || time.Now().After(t) {
		return OAuthCode{}, false, nil
	}
	return oc, true, nil
}

// ── oauth_tokens（repeated 検証・明示失効）──────────────────────────

// OAuthToken は oauth_tokens/{sha256} の1件（access/refresh 共通スキーマ）。
type OAuthToken struct {
	Kind      string   `firestore:"kind" json:"kind"` // "access" | "refresh"
	Email     string   `firestore:"email" json:"email"`
	ClientID  string   `firestore:"client_id" json:"client_id"`
	Scopes    []string `firestore:"scopes" json:"scopes"`
	ExpiresAt string   `firestore:"exp" json:"exp"`
	// Family は access/refresh の発行ペアを束ねる ID（refresh ローテーション時、
	// 再利用検知で family ごと一括失効するため。token.go（Phase 1）が生成する）。
	Family string `firestore:"family" json:"family"`
	// Superseded は refresh token がローテーションで「使用済み」になったことを示す
	// （削除ではなくフラグ＝再提示されたら MarkOAuthTokenSuperseded 済みと判定
	// できる＝盗難 refresh の再利用検知の土台。GetOAuthToken はこれを無効として
	// 扱うが、GetOAuthTokenRaw は区別せず返す＝呼び手が「存在しない」と
	// 「使用済みを再提示された（reuse）」を区別するため）。
	Superseded bool `firestore:"superseded" json:"superseded,omitempty"`
}

func (c *Client) oauthTokensCol() *firestore.CollectionRef {
	return c.fs.Collection("oauth_tokens")
}

// CreateOAuthToken は access/refresh いずれかのトークンを保存する（tokenHash は
// トークン平文の sha256）。
func (c *Client) CreateOAuthToken(ctx context.Context, tokenHash string, t OAuthToken) error {
	_, err := c.oauthTokensCol().Doc(tokenHash).Set(ctx, map[string]any{
		"kind":      t.Kind,
		"email":     t.Email,
		"client_id": t.ClientID,
		"scopes":    t.Scopes,
		"exp":       t.ExpiresAt,
		"family":    t.Family,
	})
	return err
}

// GetOAuthTokenRaw はフィルタなしでトークンを読む（期限切れ・superseded でも
// 中身を返す）。refresh ローテーションの reuse 検知は「存在しない」と
// 「superseded 済みを再提示された」を区別する必要があるため、この生読みが要る
// （GetOAuthToken は後者も ok=false にしてしまい区別できない）。
func (c *Client) GetOAuthTokenRaw(ctx context.Context, tokenHash string) (OAuthToken, bool, error) {
	snap, err := c.oauthTokensCol().Doc(tokenHash).Get(ctx)
	if err != nil || !snap.Exists() {
		return OAuthToken{}, false, nil
	}
	var t OAuthToken
	if e := snap.DataTo(&t); e != nil {
		return OAuthToken{}, false, nil
	}
	return t, true, nil
}

// MarkOAuthTokenSuperseded は refresh token を「ローテーション済み」とマークする
// （削除しない＝再提示されたら reuse 検知できるようにするため。token.go が
// ローテーション時に古い refresh へ呼ぶ）。
func (c *Client) MarkOAuthTokenSuperseded(ctx context.Context, tokenHash string) error {
	_, err := c.oauthTokensCol().Doc(tokenHash).Set(ctx,
		map[string]any{"superseded": true}, firestore.MergeAll)
	return err
}

// GetOAuthToken はトークンを検証する（**削除しない**＝access token は
// 毎リクエスト repeated に検証されるため。期限切れ/superseded は ok=false
// （削除まではしない＝TTL ポリシーが掃除する。即時失効は RevokeOAuthToken）。
func (c *Client) GetOAuthToken(ctx context.Context, tokenHash string) (OAuthToken, bool, error) {
	t, ok, err := c.GetOAuthTokenRaw(ctx, tokenHash)
	if err != nil || !ok || t.Superseded {
		return OAuthToken{}, false, err
	}
	if exp, perr := time.Parse(time.RFC3339Nano, t.ExpiresAt); perr != nil || time.Now().After(exp) {
		return OAuthToken{}, false, nil
	}
	return t, true, nil
}

// RevokeOAuthToken は単一トークンを即時失効させる（Web UI の「MCP 連携を解除」用）。
func (c *Client) RevokeOAuthToken(ctx context.Context, tokenHash string) error {
	_, err := c.oauthTokensCol().Doc(tokenHash).Delete(ctx)
	return err
}

// RevokeOAuthTokenFamily は同じ family の全トークンを失効させる（refresh token
// 再利用検知時。盗難 refresh と正規 refresh のどちらが先に使われたか区別できない
// ため、検知した時点で family ごと道連れにする＝OAuth のローテーション失効の
// 標準的な防御）。
func (c *Client) RevokeOAuthTokenFamily(ctx context.Context, family string) error {
	docs, err := c.oauthTokensCol().Where("family", "==", family).Documents(ctx).GetAll()
	if err != nil {
		return err
	}
	for _, d := range docs {
		if _, derr := d.Ref.Delete(ctx); derr != nil {
			return derr
		}
	}
	return nil
}
