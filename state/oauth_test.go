//go:build !windows

package state

import (
	"context"
	"testing"
	"time"
)

func TestOAuthClientCreateAndGet(t *testing.T) {
	ctx := context.Background()
	c := newClient(t, "pc-oauth-1")

	if _, ok, err := c.GetOAuthClient(ctx, "client-x"); err != nil || ok {
		t.Fatalf("未登録なのに取得できた: ok=%v err=%v", ok, err)
	}
	uris := []string{"https://grok.example.com/oauth/callback"}
	if err := c.CreateOAuthClient(ctx, "client-x", "Grok Bot", uris); err != nil {
		t.Fatalf("CreateOAuthClient: %v", err)
	}
	oc, ok, err := c.GetOAuthClient(ctx, "client-x")
	if err != nil || !ok {
		t.Fatalf("GetOAuthClient: ok=%v err=%v", ok, err)
	}
	if oc.Name != "Grok Bot" || len(oc.RedirectURIs) != 1 || oc.RedirectURIs[0] != uris[0] {
		t.Fatalf("内容不正: %+v", oc)
	}
	if oc.CreatedAt == "" {
		t.Fatal("created_at が空")
	}
}

// 認可コードは一回消費（ConsumePairing と同一パターン）。
func TestOAuthCodeConsumeOnce(t *testing.T) {
	ctx := context.Background()
	c := newClient(t, "pc-oauth-2")
	const hash = "codehash-abc"

	in := OAuthCode{
		ClientID: "client-x", Email: "owner@example.com", Scopes: []string{"drover.read", "drover.input"},
		CodeChallenge: "chal123", RedirectURI: "https://grok.example.com/cb", Resource: "https://host/mcp",
	}
	if err := c.CreateOAuthCode(ctx, hash, in, 60*time.Second); err != nil {
		t.Fatalf("CreateOAuthCode: %v", err)
	}

	got, ok, err := c.ConsumeOAuthCode(ctx, hash)
	if err != nil || !ok {
		t.Fatalf("1回目 ConsumeOAuthCode: ok=%v err=%v", ok, err)
	}
	if got.ClientID != in.ClientID || got.Email != in.Email || got.CodeChallenge != in.CodeChallenge ||
		got.RedirectURI != in.RedirectURI || got.Resource != in.Resource || len(got.Scopes) != 2 {
		t.Fatalf("内容不正: %+v", got)
	}

	// 2回目は既に削除済み＝ok=false（一回消費の証明）。
	if _, ok, err := c.ConsumeOAuthCode(ctx, hash); err != nil || ok {
		t.Fatalf("2回目の消費が成功した（一回消費が壊れている）: ok=%v err=%v", ok, err)
	}
}

// 期限切れコードは消費できず、かつ doc は掃除される。
func TestOAuthCodeExpired(t *testing.T) {
	ctx := context.Background()
	c := newClient(t, "pc-oauth-3")
	const hash = "codehash-expired"
	in := OAuthCode{ClientID: "client-x", Email: "owner@example.com"}
	if err := c.CreateOAuthCode(ctx, hash, in, -time.Second); err != nil { // 即時期限切れ
		t.Fatalf("CreateOAuthCode: %v", err)
	}
	if _, ok, err := c.ConsumeOAuthCode(ctx, hash); err != nil || ok {
		t.Fatalf("期限切れコードが消費できた: ok=%v err=%v", ok, err)
	}
	// 掃除されている＝再度 consume しても「不在」と同じ応答（エラーにならない）。
	if _, ok, err := c.ConsumeOAuthCode(ctx, hash); err != nil || ok {
		t.Fatalf("掃除されていない可能性: ok=%v err=%v", ok, err)
	}
}

// access/refresh トークンは repeated に検証できる（削除されない）。
func TestOAuthTokenGetRepeatable(t *testing.T) {
	ctx := context.Background()
	c := newClient(t, "pc-oauth-4")
	const hash = "tokenhash-1"
	tok := OAuthToken{
		Kind: "access", Email: "owner@example.com", ClientID: "client-x",
		Scopes: []string{"drover.read"}, Family: "fam-1",
		ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
	}
	if err := c.CreateOAuthToken(ctx, hash, tok); err != nil {
		t.Fatalf("CreateOAuthToken: %v", err)
	}
	for i := 0; i < 3; i++ {
		got, ok, err := c.GetOAuthToken(ctx, hash)
		if err != nil || !ok {
			t.Fatalf("GetOAuthToken 試行%d: ok=%v err=%v", i, ok, err)
		}
		if got.Email != tok.Email || got.ClientID != tok.ClientID || got.Family != tok.Family {
			t.Fatalf("内容不正（試行%d）: %+v", i, got)
		}
	}
}

func TestOAuthTokenExpiredRejected(t *testing.T) {
	ctx := context.Background()
	c := newClient(t, "pc-oauth-5")
	const hash = "tokenhash-expired"
	tok := OAuthToken{
		Kind: "access", Email: "owner@example.com", ClientID: "client-x",
		ExpiresAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), // 既に期限切れ
	}
	if err := c.CreateOAuthToken(ctx, hash, tok); err != nil {
		t.Fatalf("CreateOAuthToken: %v", err)
	}
	if _, ok, err := c.GetOAuthToken(ctx, hash); err != nil || ok {
		t.Fatalf("期限切れトークンが有効判定された: ok=%v err=%v", ok, err)
	}
}

// Web UI の「MCP 連携を解除」= RevokeOAuthToken で即時失効。
func TestRevokeOAuthToken(t *testing.T) {
	ctx := context.Background()
	c := newClient(t, "pc-oauth-6")
	const hash = "tokenhash-revoke"
	tok := OAuthToken{Kind: "access", Email: "owner@example.com", ClientID: "client-x",
		ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)}
	if err := c.CreateOAuthToken(ctx, hash, tok); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := c.GetOAuthToken(ctx, hash); !ok {
		t.Fatal("revoke 前なのに無効")
	}
	if err := c.RevokeOAuthToken(ctx, hash); err != nil {
		t.Fatalf("RevokeOAuthToken: %v", err)
	}
	if _, ok, err := c.GetOAuthToken(ctx, hash); err != nil || ok {
		t.Fatalf("revoke 後も有効: ok=%v err=%v", ok, err)
	}
}

// refresh token 再利用検知時の family 一括失効: 同じ family の access/refresh が
// まとめて失効し、別 family のトークンは影響を受けない。
func TestRevokeOAuthTokenFamily(t *testing.T) {
	ctx := context.Background()
	c := newClient(t, "pc-oauth-7")
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	mk := func(kind, family string) OAuthToken {
		return OAuthToken{Kind: kind, Email: "owner@example.com", ClientID: "client-x",
			Family: family, ExpiresAt: exp}
	}
	if err := c.CreateOAuthToken(ctx, "tok-access-fam1", mk("access", "fam-1")); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateOAuthToken(ctx, "tok-refresh-fam1", mk("refresh", "fam-1")); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateOAuthToken(ctx, "tok-access-fam2", mk("access", "fam-2")); err != nil {
		t.Fatal(err)
	}

	if err := c.RevokeOAuthTokenFamily(ctx, "fam-1"); err != nil {
		t.Fatalf("RevokeOAuthTokenFamily: %v", err)
	}
	if _, ok, _ := c.GetOAuthToken(ctx, "tok-access-fam1"); ok {
		t.Fatal("fam-1 の access が失効していない")
	}
	if _, ok, _ := c.GetOAuthToken(ctx, "tok-refresh-fam1"); ok {
		t.Fatal("fam-1 の refresh が失効していない（reuse 検知の家族失効が壊れている）")
	}
	if _, ok, _ := c.GetOAuthToken(ctx, "tok-access-fam2"); !ok {
		t.Fatal("別 family (fam-2) まで巻き添えで失効した")
	}
}
