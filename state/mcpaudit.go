package state

// mcp_audit/{auto-id}: 全 tools/call の監査ログ（drover-cloud-mcp-design.md
// §6-4）。text 本文は保存しない（args_digest のみ）＝画面内容を監査コレクション
// へ二次保存しない。TTL は 90 日（Firestore TTL ポリシー側が expire_at を見る。
// 実ポリシー設定は deploy 時＝Phase 1 のコードではここで値を書くところまで）。

import (
	"context"
	"time"
)

// MCPAuditEntry は1件の tools/call 監査レコード。
type MCPAuditEntry struct {
	Email      string `firestore:"email" json:"email"`
	ClientID   string `firestore:"client_id" json:"client_id"`
	Tool       string `firestore:"tool" json:"tool"`
	PC         string `firestore:"pc" json:"pc"`
	SID        string `firestore:"sid" json:"sid"`
	ArgsDigest string `firestore:"args_digest" json:"args_digest"`
	ResultCode string `firestore:"result_code" json:"result_code"`
}

const mcpAuditTTL = 90 * 24 * time.Hour

// WriteMCPAudit は mcp_audit へ1件追記する（auto-id・失敗しても呼び手の
// 本処理は止めない方針は mcp パッケージ側の責務＝ここは素直に書くだけ）。
func (c *Client) WriteMCPAudit(ctx context.Context, e MCPAuditEntry) error {
	now := time.Now().UTC()
	_, _, err := c.fs.Collection("mcp_audit").Add(ctx, map[string]any{
		"ts":          now.Format(time.RFC3339Nano),
		"email":       e.Email,
		"client_id":   e.ClientID,
		"tool":        e.Tool,
		"pc":          e.PC,
		"sid":         e.SID,
		"args_digest": e.ArgsDigest,
		"result_code": e.ResultCode,
		"expire_at":   now.Add(mcpAuditTTL).Format(time.RFC3339Nano),
	})
	return err
}
