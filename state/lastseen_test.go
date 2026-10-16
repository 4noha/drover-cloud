package state

// TouchLastSeen/PCLastSeen（drover-cloud-mcp-design.md §9 決定事項5）は
// Phase 1 で実装されたが専用テストが無かった。Phase 2（herdr-drover 側の
// 定期書き込み配線）で実際に使われ始めるため、ここで実 Firestore エミュレータ
// に対して検証する（鉄則2: 合成で緑にしない）。

import (
	"context"
	"testing"
	"time"
)

func TestTouchLastSeenAndPCLastSeen(t *testing.T) {
	c := newClient(t, "lastseen-pc1")
	ctx := context.Background()

	// 未書込の PC は online=false・lastSeen=""（偽の online=true を作らない）。
	ls, online, err := c.PCLastSeen(ctx, "lastseen-pc1")
	if err != nil {
		t.Fatalf("PCLastSeen(未書込): %v", err)
	}
	if online || ls != "" {
		t.Fatalf("未書込なのに online=%v ls=%q", online, ls)
	}

	if err := c.TouchLastSeen(ctx, "lastseen-pc1"); err != nil {
		t.Fatalf("TouchLastSeen: %v", err)
	}
	ls, online, err = c.PCLastSeen(ctx, "lastseen-pc1")
	if err != nil {
		t.Fatalf("PCLastSeen(書込後): %v", err)
	}
	if !online {
		t.Fatalf("Touch 直後なのに online=false（ls=%q）", ls)
	}
	if _, perr := time.Parse(time.RFC3339Nano, ls); perr != nil {
		t.Fatalf("last_seen が RFC3339Nano でない: %q (%v)", ls, perr)
	}
}

func TestPCLastSeenStaleIsOffline(t *testing.T) {
	c := newClient(t, "lastseen-pc2")
	ctx := context.Background()

	// pcOnlineWindow（10分）より古い last_seen は online=false。
	_, err := c.fs.Collection("pcs").Doc("lastseen-pc2").Set(ctx, map[string]any{
		"last_seen": time.Now().Add(-20 * time.Minute).UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	ls, online, err := c.PCLastSeen(ctx, "lastseen-pc2")
	if err != nil {
		t.Fatalf("PCLastSeen: %v", err)
	}
	if online {
		t.Fatalf("20分前の last_seen なのに online=true（ls=%q）", ls)
	}

	// 壊れた形式（パース不能）も online=false・エラーにはしない（owner の
	// 一覧表示が壊れた1 PC で巻き込まれて全滅しないように）。
	_, err = c.fs.Collection("pcs").Doc("lastseen-pc2").Set(ctx, map[string]any{
		"last_seen": "not-a-timestamp",
	})
	if err != nil {
		t.Fatalf("seed(壊れた形式): %v", err)
	}
	_, online, err = c.PCLastSeen(ctx, "lastseen-pc2")
	if err != nil {
		t.Fatalf("PCLastSeen(壊れた形式): %v", err)
	}
	if online {
		t.Fatal("壊れた last_seen 形式なのに online=true")
	}
}

// TouchLastSeen は pc を明示指定する（AckCommandFor 等と同じ additive
// パターン）。自分以外の pc へも書けることを確認する（将来 slave 仲介に
// 同じ関数を再利用する前提）。
func TestTouchLastSeenExplicitPCDiffersFromOwnPCID(t *testing.T) {
	c := newClient(t, "lastseen-self")
	ctx := context.Background()
	if err := c.TouchLastSeen(ctx, "lastseen-other"); err != nil {
		t.Fatalf("TouchLastSeen(他 pc): %v", err)
	}
	if _, online, _ := c.PCLastSeen(ctx, "lastseen-other"); !online {
		t.Fatal("明示指定した他 pc に書けていない")
	}
	if _, online, _ := c.PCLastSeen(ctx, "lastseen-self"); online {
		t.Fatal("自分（コンストラクタの pcID）には書いていないはずなのに online=true")
	}
}
