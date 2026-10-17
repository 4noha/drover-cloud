// Package webhook はセッション状態通知（agent_status が done/blocked に
// 変わった時に owner 設定の外部 URL へ JSON POST する）の送信ロジックと
// 設定の型。送信元は2箇所（herdr-drover daemon＝master が直接送信、
// drover-cloud web.slavePush＝slave 分を代理送信）で共有するため、
// Firestore 等への依存を持たない独立パッケージにする。
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Config は Firestore settings/webhook に保存される設定。owner の Web 管理
// 画面から設定する。HeaderSecret は API 応答へ絶対に載せない
// （json:"-"・保存後は値を表示しない仕様）。
type Config struct {
	Enabled      bool   `firestore:"enabled" json:"enabled"`
	URL          string `firestore:"url" json:"url"`
	HeaderName   string `firestore:"header_name" json:"header_name"`
	HeaderSecret string `firestore:"header_secret" json:"-"`
}

// Payload は送信する JSON 本体。
type Payload struct {
	PC         string `json:"pc"`
	SID        string `json:"sid"`
	WindowName string `json:"window_name"`
	Agent      string `json:"agent"`
	Cwd        string `json:"cwd"`
	OldStatus  string `json:"old_status"`
	NewStatus  string `json:"new_status"`
	At         string `json:"at"`
}

// NotifyStates は通知対象の遷移先状態。herdr は CLI エージェントの作業完了を
// "done" ではなく "idle" で表す（herdr CLI ヘルプの実文言: "done is a UI
// attention state; use idle for CLI agent completion waits"。内部の
// ManifestState にも done という値は無い＝実測で "done" は到達しないに等しい）。
// 呼び手（cmd/herdr-drover/webhooknotify.go・web/webhookdispatch.go）は
// 遷移元が "working" の時だけこの集合と照合すること（cmd/herdr-drover/notify.go
// の taskNotifyState と同じ規律。unknown からの遷移は一時的な検出揺れで誤通知
// しないために除外する）。
var NotifyStates = map[string]bool{"idle": true, "done": true, "blocked": true}

const sendTimeout = 10 * time.Second

// Validate は保存前の検証。https 必須・ヘッダ名/値に制御文字（CRLF 等）を
// 含めない（HTTP ヘッダインジェクション防止。net/http も書込み時に弾くが、
// 保存時点で明確なエラーを返すため独立に検証する）。Enabled=false なら
// URL 等が空でも構わない（オフ時は検証しない＝空欄のまま OFF で保存できる）。
func Validate(cfg Config) error {
	if !cfg.Enabled {
		return nil
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("webhook URL は https の絶対URLが必須")
	}
	if cfg.HeaderName == "" {
		return fmt.Errorf("ヘッダー名が必須")
	}
	if containsControlChar(cfg.HeaderName) || containsControlChar(cfg.HeaderSecret) {
		return fmt.Errorf("ヘッダー名/値に制御文字（改行等）を含められない")
	}
	return nil
}

func containsControlChar(s string) bool {
	for _, r := range s {
		if r == '\r' || r == '\n' || r == 0 {
			return true
		}
	}
	return false
}

// Send は Payload を JSON POST する。cfg.Enabled=false や URL 未設定は no-op
// （呼び手が送る前に Enabled を見るのが基本だが、ここでも defense-in-depth
// で確認する）。失敗は err を返すだけで、呼び手の本処理を止めない設計
// （呼び手は goroutine + ログで扱う。鉄則5「silent にしない」はログに残す
// ことで満たす）。
func Send(ctx context.Context, cfg Config, p Payload) error {
	if !cfg.Enabled || cfg.URL == "" {
		return nil
	}
	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("payload の JSON 化に失敗: %w", err)
	}
	cctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.HeaderName != "" {
		req.Header.Set(cfg.HeaderName, cfg.HeaderSecret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook 応答が異常: status=%d", resp.StatusCode)
	}
	return nil
}
