package state

// mcp_results/{command_id}: MCP ツール呼び出しの大きな結果（画面テキスト等）の
// 一時保存。小さな結果は既存 commands/{pc}/q/{id}.detail を使う（ここは
// drover-cloud-mcp-design.md §3.2「結果の受け渡し」の大きい方のみ扱う）。
//
// ANSI 除去・64KB 上限等の整形は呼び手（herdr-drover daemon、Phase 2）の責務。
// この package は Firestore への保存/取得のみを扱う（鉄則: 画面解釈はしない）。
//
// TTL は Firestore の TTL ポリシー（`expire_at` フィールド、deploy 時に
// `gcloud firestore fields ttls update` で設定。Phase 1）が実際の削除を行う。
// ここでは正しい値のフィールドを書くところまでが責務。

import (
	"context"
	"time"

	"cloud.google.com/go/firestore"
)

// CommandResult は mcp_results/{command_id} の1件。
type CommandResult struct {
	PC        string `firestore:"pc" json:"pc"`
	Text      string `firestore:"text" json:"text"`
	Truncated bool   `firestore:"truncated" json:"truncated"`
	CreatedAt string `firestore:"created_at" json:"created_at"`
	ExpireAt  string `firestore:"expire_at" json:"expire_at"`
}

func (c *Client) mcpResultsCol() *firestore.CollectionRef {
	return c.fs.Collection("mcp_results")
}

// PutCommandResult は mcp_results/{id} に結果を保存する（自 PC 分。master 用。
// slave は drover-cloud の /slave/command-result 経由で別途仲介する＝Phase 2）。
// truncated は呼び手（daemon）が上限超過を検知した結果をそのまま渡す（ここでは
// 再判定しない＝整形は呼び手の責務）。ttl は expire_at の算出にのみ使う
// （実削除は Firestore TTL ポリシー側）。
func (c *Client) PutCommandResult(ctx context.Context, id, text string, truncated bool, ttl time.Duration) error {
	now := time.Now().UTC()
	_, err := c.mcpResultsCol().Doc(id).Set(ctx, map[string]any{
		"pc":         c.pcID,
		"text":       text,
		"truncated":  truncated,
		"created_at": now.Format(time.RFC3339Nano),
		"expire_at":  now.Add(ttl).Format(time.RFC3339Nano),
	})
	return err
}

// PutCommandResultFor は pc 明示版（relay が slave の結果を仲介する用。
// claimCommandFor/AckCommandFor と同じ additive パターン）。
func (c *Client) PutCommandResultFor(ctx context.Context, pc, id, text string, truncated bool, ttl time.Duration) error {
	now := time.Now().UTC()
	_, err := c.mcpResultsCol().Doc(id).Set(ctx, map[string]any{
		"pc":         pc,
		"text":       text,
		"truncated":  truncated,
		"created_at": now.Format(time.RFC3339Nano),
		"expire_at":  now.Add(ttl).Format(time.RFC3339Nano),
	})
	return err
}

// GetCommandResult は mcp_results/{id} を読む（MCP ハンドラの get_command_result
// 用）。pc は呼び手が別途 commands/{pc}/q/{id} の存在確認等で持っている前提
// （ここでは pc の一致検証はしない＝Firestore rules 側は catch-all deny で
// サーバ SDK 専用のため、呼び手側の認可チェックに委ねる）。
func (c *Client) GetCommandResult(ctx context.Context, id string) (CommandResult, bool, error) {
	snap, err := c.mcpResultsCol().Doc(id).Get(ctx)
	if err != nil || !snap.Exists() {
		// ConsumePairing と同じ規律: Get の NotFound エラーと不在を区別しない
		// （呼び手には「まだ無い」で十分。snap は err!=nil 時も .Exists() 呼び出し可能＝
		// 短絡評価で err!=nil のときは .Exists() を評価しない＝安全）。
		return CommandResult{}, false, nil
	}
	var r CommandResult
	if e := snap.DataTo(&r); e != nil {
		return CommandResult{}, false, e
	}
	return r, true, nil
}
