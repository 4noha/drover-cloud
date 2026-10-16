package mcp

// tools/call の監査記録（drover-cloud-mcp-design.md §6-4）。text 本文は
// 保存しない＝sha256 digest と長さのみ。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// argsDigest は引数の sha256 hex + バイト長（画面内容そのものを監査へ
// 保存しないため。read_pane の text 等、大きい結果はそもそも引数に乗らない
// ＝対象は pc/sid/options 程度の小さい JSON）。
func argsDigest(raw json.RawMessage) string {
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:8]) // 16 hex 文字で十分（衝突検出用途ではなく監査の手掛かり）
}

// pcSIDFromArgs は監査レコードの pc/sid 列用に、よくある引数形から拾う
// （厳密なパースはツール本体の責務。ここは「あれば出す」程度のベストエフォート）。
func pcSIDFromArgs(raw json.RawMessage) (pc, sid string) {
	var v struct {
		PC  string `json:"pc"`
		SID string `json:"sid"`
	}
	_ = json.Unmarshal(raw, &v)
	return v.PC, v.SID
}
