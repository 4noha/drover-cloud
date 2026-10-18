package mcp

// 眼鏡（Rokid）の /ask への回答を保存するツール（drover-cloud の
// glasses パッケージ＝Firestore の glasses_answers コレクション）。
//
// GLASSES_ENABLED=1 のときに main が Server.SetGlasses でストアを差し込む。
// 無効時はグローバルの toolDefs / toolRegistry に一切触らない＝tools/list の
// 応答は従来と完全一致し、呼ばれても not_found のまま。
//
// 入力検査・登録済み pc の確認・保存は glasses.PutAnswer を HTTP の投入口
// （/glasses/answer）と共有する＝両経路で同じ規則になる。

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/4noha/drover-cloud/glasses"
)

// glassesAnswerTool はツール名（tools/list・tools/call の両方で使う）。
const glassesAnswerTool = "glasses_answer"

// SetGlasses は眼鏡の回答ストアを後付けする（nil なら無効のまま）。
// New のシグネチャは変えない＝既存呼び出しは無改変。
func (s *Server) SetGlasses(st glasses.AnswerStore) { s.glasses = st }

// glassesAnswerDef は GLASSES 有効時だけ tools/list に足す定義。グローバルの
// toolDefs を書き換えない（無効時の応答を完全に従来どおり保つため）。
var glassesAnswerDef = toolDefAnn(glassesAnswerTool,
	"眼鏡（Rokid）の /ask への回答を保存する。pc のローカルブリッジが取りに来て HUD に表示する。"+
		"request_id は webhook payload の値をそのまま、text は回答本文（[request_id] 前置は不要、4000字以内）。",
	map[string]any{"type": "object", "required": []string{"pc", "request_id", "text"},
		"properties": map[string]any{
			"pc":         map[string]any{"type": "string"},
			"request_id": map[string]any{"type": "string"},
			"text":       map[string]any{"type": "string", "maxLength": 4000},
		}}, false, false, true)

// glassesTier は glasses_answer の tier（有効時のみ）。
func (s *Server) glassesTier(name string) (toolTier, bool) {
	if s.glasses == nil || name != glassesAnswerTool {
		return toolTier{}, false
	}
	return toolTier{scope: "drover.input", rateClass: "input", fn: toolGlassesAnswer}, true
}

type glassesAnswerArgs struct {
	PC        string `json:"pc"`
	RequestID string `json:"request_id"`
	Text      string `json:"text"`
}

func toolGlassesAnswer(ctx context.Context, srv *Server, raw json.RawMessage) (any, *toolErr) {
	a, ok := decodeArgs[glassesAnswerArgs](raw)
	if !ok || a.PC == "" || a.RequestID == "" || a.Text == "" {
		return nil, &toolErr{Code: "invalid_request", Message: "pc/request_id/text が必要です"}
	}
	// 入力検査・登録確認・保存は HTTP 投入口と同じ処理（glasses.PutAnswer）。
	exp, err := glasses.PutAnswer(ctx, srv.glasses, glasses.StateRegistry{St: srv.st}, a.PC, a.RequestID, a.Text)
	if err != nil {
		switch {
		case errors.Is(err, glasses.ErrInvalid):
			// 本文はメッセージに載せない（検査内容のみ）。
			return nil, &toolErr{Code: "invalid_request", Message: err.Error()}
		case errors.Is(err, glasses.ErrNotRegistered):
			return nil, &toolErr{Code: "forbidden", Message: "未登録の PC です"}
		default:
			return nil, &toolErr{Code: "server_error", Message: "保存に失敗しました"}
		}
	}
	return map[string]any{
		"ok":         true,
		"pc":         a.PC,
		"request_id": a.RequestID,
		"expires_at": exp.UTC().Format(time.RFC3339),
	}, nil
}
