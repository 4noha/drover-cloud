package web

// slave（共用PC）分のセッション状態 Webhook 送信代理。master は herdr-drover
// daemon 自身が直接送る（Firestore に直接書けるため）。slave は SA レスで
// drover-cloud の /slave/push 経由でしか状態を書けないため、ここ（slavePush
// ハンドラ側）で push 前後を比較して代理送信する。

import (
	"context"
	"log"
	"time"

	"github.com/4noha/drover-cloud/webhook"
)

// sessionStatusByKey は pc の現在の session を key→agent_status で返す
// （webhook の旧状態比較用。読めなければ空 map＝旧状態不明として扱う＝
// 初回 push 等では誤って通知しない安全側）。
func (s *Server) sessionStatusByKey(ctx context.Context, pc string) map[string]string {
	ss, err := s.st.ListSessions(ctx, pc)
	out := map[string]string{}
	if err != nil {
		return out
	}
	for _, x := range ss {
		key, _ := x["key"].(string)
		if key == "" {
			continue
		}
		status, _ := x["agent_status"].(string)
		out[key] = status
	}
	return out
}

// dispatchWebhooksForTransitions は push 前（old）と push しようとしている
// 生セッション（sessions）を比較し、done/blocked へ**新しく**遷移したものだけ
// 非同期で送る（同じ状態への重複は送らない）。Webhook が無効なら何もしない。
// 送信失敗は本処理（push 応答）をブロックしない＝goroutine 起動＋ログのみ
// （鉄則5「silent にしない」はログに残すことで満たす）。
func (s *Server) dispatchWebhooksForTransitions(pc string, old map[string]string, sessions []map[string]any) {
	cfg, err := s.st.GetWebhookConfig(context.Background())
	if err != nil || !cfg.Enabled {
		return
	}
	for _, x := range sessions {
		key, _ := x["key"].(string)
		if key == "" {
			continue
		}
		newStatus, _ := x["agent_status"].(string)
		if !webhook.NotifyStates[newStatus] {
			continue
		}
		oldStatus, known := old[key]
		// 未観測（＝初めて見る sid）は「以前の状態が不明」として送らない
		// （cmd/herdr-drover/notify.go の taskNotifier と同じ安全側の考え方。
		// slave はこの old が Firestore 由来で daemon 再起動でも消えないため
		// 実際には影響しないが、master の実装と対称にして挙動を揃える）。
		if !known || oldStatus == newStatus {
			continue
		}
		p := webhook.Payload{
			PC: pc, SID: key,
			WindowName: strField(x, "window_name"), Agent: strField(x, "agent"),
			Cwd: strField(x, "cwd"), OldStatus: oldStatus, NewStatus: newStatus,
			At: time.Now().UTC().Format(time.RFC3339Nano),
		}
		go func(p webhook.Payload) {
			// r.Context() ではなく独立 ctx＝HTTP 応答が先に返って request ctx が
			// キャンセルされても送信を続ける。
			if err := webhook.Send(context.Background(), cfg, p); err != nil {
				log.Printf("[webhook] 送信失敗 pc=%s sid=%s: %v", p.PC, p.SID, err)
			}
		}(p)
	}
}

// strField は session map[string]any から文字列フィールドを安全に取り出す
// （herdr-drover daemon が書く実データはゆるい map 形なので型アサーションで
// 吸収する。mcp パッケージの同名ヘルパと同じ考え方）。
func strField(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}
