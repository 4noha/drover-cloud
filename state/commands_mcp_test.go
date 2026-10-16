//go:build !windows

package state

// drover-cloud-mcp-design.md Phase 0 の検証。実 Firestore エミュレータで
// PushCommandArgs/WaitCommand/validateArgs と、旧 doc（args/source 無し）の
// degrade を確認する（鉄則2: 合成では緑にしない）。

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 新規6命令が allowlist に乗っていること。
func TestMCPCommandsAreValid(t *testing.T) {
	for _, c := range []string{"read-pane", "send-input", "interrupt",
		"start-agent-session", "close-pane", "organize"} {
		if !ValidCommands[c] {
			t.Errorf("MCP 由来の新命令 %q が allowlist に無い", c)
		}
	}
}

func TestValidateArgsSizeAndKeys(t *testing.T) {
	if err := validateArgs(nil); err != nil {
		t.Fatalf("nil args でエラー: %v", err)
	}
	if err := validateArgs(map[string]string{"a": "b"}); err != nil {
		t.Fatalf("小さい args でエラー: %v", err)
	}
	if err := validateArgs(map[string]string{"": "x"}); err == nil {
		t.Fatal("空キーが通った")
	}
	big := strings.Repeat("x", argsMaxBytes+1)
	if err := validateArgs(map[string]string{"k": big}); err == nil {
		t.Fatal("上限超過の args が通った")
	}
}

// PushCommandArgs で投入した args/source が実際に daemon 側（WatchCommands）
// へ届き、Ack 後に WaitCommand が結果を即座に拾えること。
func TestPushCommandArgsRoundTripAndWaitCommand(t *testing.T) {
	ctx := context.Background()
	c := newClient(t, "pc-mcp-wait")
	const pc = "pc-mcp-wait"

	got := make(chan Command, 1)
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = c.WatchCommands(wctx, func(cm Command) { got <- cm }) }()
	time.Sleep(1500 * time.Millisecond) // listener attach 待ち（slave_test.go と同じ余裕）

	id, err := c.PushCommandArgs(ctx, pc, "read-pane", "sid-1", "claude", "owner@example.com",
		"mcp", map[string]string{"source": "visible", "lines": "50"})
	if err != nil {
		t.Fatalf("PushCommandArgs: %v", err)
	}

	var claimed Command
	select {
	case claimed = <-got:
	case <-time.After(8 * time.Second):
		t.Fatal("WatchCommands が命令を拾わない")
	}
	if claimed.ID != id {
		t.Fatalf("claim した id=%q, want %q", claimed.ID, id)
	}
	if claimed.Source != "mcp" {
		t.Fatalf("Source=%q, want mcp", claimed.Source)
	}
	if claimed.Args["source"] != "visible" || claimed.Args["lines"] != "50" {
		t.Fatalf("Args が届いていない: %+v", claimed.Args)
	}
	if err := c.AckCommand(ctx, id, "done", "画面テキスト取得OK"); err != nil {
		t.Fatalf("AckCommand: %v", err)
	}

	cm, timedOut, err := c.WaitCommand(ctx, pc, id, 10*time.Second)
	if err != nil {
		t.Fatalf("WaitCommand: %v", err)
	}
	if timedOut {
		t.Fatal("ack 済みなのに timedOut=true")
	}
	if cm.Status != "done" || cm.Detail != "画面テキスト取得OK" {
		t.Fatalf("WaitCommand 結果不正: %+v", cm)
	}
}

// GetCommand は get_command_result（WaitCommand タイムアウト後の後取得）用。
// 不在は ok=false でエラーにしない。
func TestGetCommandReadsSingleDoc(t *testing.T) {
	ctx := context.Background()
	c := newClient(t, "pc-mcp-get")
	const pc = "pc-mcp-get"

	id, err := c.PushCommandArgs(ctx, pc, "read-pane", "sid-1", "", "owner@example.com",
		"mcp", map[string]string{"source": "visible"})
	if err != nil {
		t.Fatalf("PushCommandArgs: %v", err)
	}
	if _, ok, err := c.GetCommand(ctx, pc, id); err != nil || !ok {
		t.Fatalf("pending 段階で GetCommand: ok=%v err=%v", ok, err)
	}
	if err := c.AckCommand(ctx, id, "done", "ok"); err != nil {
		t.Fatalf("AckCommand: %v", err)
	}
	cm, ok, err := c.GetCommand(ctx, pc, id)
	if err != nil || !ok {
		t.Fatalf("Ack後の GetCommand: ok=%v err=%v", ok, err)
	}
	if cm.Status != "done" || cm.Detail != "ok" || cm.Args["source"] != "visible" {
		t.Fatalf("GetCommand の内容が不正: %+v", cm)
	}

	if _, ok, err := c.GetCommand(ctx, pc, "no-such-id"); err != nil || ok {
		t.Fatalf("不在 id なのに ok=%v err=%v", ok, err)
	}
}

// 未 claim/未 Ack のまま放置すると、WaitCommand はエラーにせず
// timedOut=true を返す（§3.2「超過時は {status:"pending", command_id} を
// 返す」の土台。呼び手はこれを使って get_command_result へ切り替える）。
func TestWaitCommandTimesOutWithoutError(t *testing.T) {
	ctx := context.Background()
	c := newClient(t, "pc-mcp-timeout")
	const pc = "pc-mcp-timeout"

	id, err := c.PushCommandArgs(ctx, pc, "read-pane", "sid-1", "", "owner@example.com", "mcp", nil)
	if err != nil {
		t.Fatalf("PushCommandArgs: %v", err)
	}
	start := time.Now()
	cm, timedOut, err := c.WaitCommand(ctx, pc, id, 2*time.Second)
	if err != nil {
		t.Fatalf("WaitCommand でエラー（timeout はエラーにしない設計）: %v", err)
	}
	if !timedOut {
		t.Fatalf("pending のままなのに timedOut=false: %+v", cm)
	}
	if elapsed := time.Since(start); elapsed < 1800*time.Millisecond {
		t.Fatalf("timeout より早く返った: %v", elapsed)
	}
}

// 旧 PushCommand（Args/Source 未指定の薄いラッパ）経由の doc には
// args フィールド自体が書かれず、source も空のまま＝既存呼び出しの
// 挙動が変わっていないことの確認。
func TestLegacyPushCommandOmitsArgsAndSource(t *testing.T) {
	ctx := context.Background()
	c := newClient(t, "pc-mcp-legacy")
	const pc = "pc-mcp-legacy"

	id, err := c.PushCommand(ctx, pc, "restart-agent-session", "", "claude", "owner@example.com")
	if err != nil {
		t.Fatalf("PushCommand: %v", err)
	}
	snap, err := c.cmdCol(pc).Doc(id).Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	data := snap.Data()
	if _, has := data["args"]; has {
		t.Fatalf("旧 PushCommand 経由なのに args フィールドが書かれた: %v", data)
	}
	if src, _ := data["source"].(string); src != "" {
		t.Fatalf("旧 PushCommand 経由の source が空でない: %q", src)
	}
	var cm Command
	if err := snap.DataTo(&cm); err != nil {
		t.Fatalf("旧形式 doc の DataTo でエラー: %v", err)
	}
	if len(cm.Args) != 0 {
		t.Fatalf("旧 doc から Args が湧いた: %+v", cm.Args)
	}
}

// args/source キー自体が存在しない「本当に旧い」doc（過去に書かれた本物の
// 旧形式を想定）を直接 Firestore へ書いて、新コードの DataTo が落ちない
// ことを確認する（鉄則2: 旧 doc の degrade を実データで示す）。
func TestTrulyLegacyDocWithoutArgsFieldDegrades(t *testing.T) {
	ctx := context.Background()
	c := newClient(t, "pc-mcp-truelegacy")
	const pc = "pc-mcp-truelegacy"
	id := "legacy-doc-1"
	if _, err := c.cmdCol(pc).Doc(id).Set(ctx, map[string]any{
		"id": id, "cmd": "restart-agent-session", "sid": "", "agent": "claude",
		"requested_by": "owner@example.com",
		"ts":           time.Now().UTC().Format(time.RFC3339Nano),
		"status":       "pending",
		// args/source キー自体を書かない（本当に旧い doc の形）。
	}); err != nil {
		t.Fatalf("legacy doc 書き込み: %v", err)
	}
	snap, err := c.cmdCol(pc).Doc(id).Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var cm Command
	if err := snap.DataTo(&cm); err != nil {
		t.Fatalf("args/source キー無し doc の DataTo でエラー（後方互換が壊れている）: %v", err)
	}
	if cm.Args != nil {
		t.Fatalf("args キー無し doc から Args が非 nil: %+v", cm.Args)
	}
	if cm.Source != "" {
		t.Fatalf("source キー無し doc から Source が非空: %q", cm.Source)
	}
}
