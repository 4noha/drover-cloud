//go:build !windows

package state

import (
	"context"
	"testing"
	"time"
)

func TestPutAndGetCommandResult(t *testing.T) {
	ctx := context.Background()
	c := newClient(t, "pc-result-1")
	const id = "cmd-abc123"

	if _, ok, err := c.GetCommandResult(ctx, id); err != nil || ok {
		t.Fatalf("書込み前なのに結果が取れた: ok=%v err=%v", ok, err)
	}

	if err := c.PutCommandResult(ctx, id, "画面テキストの中身です", false, 10*time.Minute); err != nil {
		t.Fatalf("PutCommandResult: %v", err)
	}
	r, ok, err := c.GetCommandResult(ctx, id)
	if err != nil || !ok {
		t.Fatalf("GetCommandResult: ok=%v err=%v", ok, err)
	}
	if r.PC != "pc-result-1" || r.Text != "画面テキストの中身です" || r.Truncated {
		t.Fatalf("結果内容不正: %+v", r)
	}
	createdAt, err := time.Parse(time.RFC3339Nano, r.CreatedAt)
	if err != nil {
		t.Fatalf("created_at パース失敗: %q: %v", r.CreatedAt, err)
	}
	expireAt, err := time.Parse(time.RFC3339Nano, r.ExpireAt)
	if err != nil {
		t.Fatalf("expire_at パース失敗: %q: %v", r.ExpireAt, err)
	}
	if diff := expireAt.Sub(createdAt); diff < 9*time.Minute || diff > 11*time.Minute {
		t.Fatalf("expire_at - created_at = %v, want ~10min（TTL ポリシーの土台）", diff)
	}
}

func TestPutCommandResultTruncatedFlag(t *testing.T) {
	ctx := context.Background()
	c := newClient(t, "pc-result-2")
	if err := c.PutCommandResult(ctx, "cmd-trunc", "...(省略)...", true, time.Minute); err != nil {
		t.Fatalf("PutCommandResult: %v", err)
	}
	r, ok, err := c.GetCommandResult(ctx, "cmd-trunc")
	if err != nil || !ok {
		t.Fatalf("GetCommandResult: ok=%v err=%v", ok, err)
	}
	if !r.Truncated {
		t.Fatal("truncated=true で書いたのに false で読めた")
	}
}

// pc 明示版（slave 仲介用）は自 pcID ではなく指定した pc で保存される。
func TestPutCommandResultForUsesExplicitPC(t *testing.T) {
	ctx := context.Background()
	c := newClient(t, "relay") // relay 自身の pcID は結果に使われないはず
	const otherPC = "slave-pc-1"
	if err := c.PutCommandResultFor(ctx, otherPC, "cmd-slave-1", "slave画面", false, time.Minute); err != nil {
		t.Fatalf("PutCommandResultFor: %v", err)
	}
	r, ok, err := c.GetCommandResult(ctx, "cmd-slave-1")
	if err != nil || !ok {
		t.Fatalf("GetCommandResult: ok=%v err=%v", ok, err)
	}
	if r.PC != otherPC {
		t.Fatalf("PC=%q, want %q（relay の pcID が混入している）", r.PC, otherPC)
	}
}
