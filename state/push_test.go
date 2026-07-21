package state

import (
	"context"
	"testing"
)

func TestPushTokenSaveListDelete(t *testing.T) {
	ctx := context.Background()
	c := newClient(t, "pc-push")

	if err := c.SavePushToken(ctx, "", "ua-should-be-noop"); err != nil {
		t.Fatalf("空 token は no-op のはず: %v", err)
	}
	if err := c.SavePushToken(ctx, "tok-1", "Chrome/1"); err != nil {
		t.Fatalf("SavePushToken tok-1: %v", err)
	}
	if err := c.SavePushToken(ctx, "tok-2", "Safari/1"); err != nil {
		t.Fatalf("SavePushToken tok-2: %v", err)
	}
	// 同一 token の再登録は upsert（重複を増やさない）。
	if err := c.SavePushToken(ctx, "tok-1", "Chrome/2"); err != nil {
		t.Fatalf("SavePushToken tok-1 再登録: %v", err)
	}

	toks, err := c.ListPushTokens(ctx)
	if err != nil {
		t.Fatalf("ListPushTokens: %v", err)
	}
	if len(toks) != 2 {
		t.Fatalf("token 数 = %d, want 2（再登録で重複していないか）: %v", len(toks), toks)
	}
	seen := map[string]bool{}
	for _, tk := range toks {
		seen[tk] = true
	}
	if !seen["tok-1"] || !seen["tok-2"] {
		t.Fatalf("期待した token が無い: %v", toks)
	}

	if err := c.DeletePushToken(ctx, "tok-1"); err != nil {
		t.Fatalf("DeletePushToken: %v", err)
	}
	toks, err = c.ListPushTokens(ctx)
	if err != nil {
		t.Fatalf("ListPushTokens after delete: %v", err)
	}
	if len(toks) != 1 || toks[0] != "tok-2" {
		t.Fatalf("削除後の token = %v, want [tok-2]", toks)
	}
}
