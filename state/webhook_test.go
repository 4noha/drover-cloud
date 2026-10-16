package state

// settings/webhook の Get/Set を実 Firestore エミュレータで検証する（鉄則2）。
// 送信ロジック自体（webhook.Send/Validate）は webhook パッケージの自前テストが
// 担保済み＝ここは永続化（未設定時の既定・Validate 経由の拒否）だけを見る。
//
// ⚠ settings/webhook は account 全体で1つの singleton doc（pc でスコープしない）
// なので、他テストと同じ emulator を共有する以上 1 つの関数内で順序を
// 制御する（分けると書込み順序に依存して誤って緑になる＝実際に最初の
// 実装で踏んだ）。t.Cleanup で必ず削除し、他テストへ漏らさない。

import (
	"context"
	"testing"

	"github.com/4noha/drover-cloud/webhook"
)

func TestWebhookConfig(t *testing.T) {
	c := newClient(t, "webhook-pc1")
	ctx := context.Background()
	t.Cleanup(func() { _, _ = c.webhookDoc().Delete(ctx) })

	// 未設定（初回）は Enabled=false のゼロ値をエラーなしで返す。
	cfg, err := c.GetWebhookConfig(ctx)
	if err != nil {
		t.Fatalf("GetWebhookConfig(未設定): %v", err)
	}
	if cfg.Enabled {
		t.Fatalf("未設定なのに Enabled=true: %+v", cfg)
	}

	// SetWebhookConfig は webhook.Validate を再検証する（呼び手の web ハンドラ
	// が検証を漏らしても不正な設定を書かせない・defense-in-depth）。拒否された
	// 設定が書き込まれていないことをここで先に確認する（まだ何も書いていない
	// クリーンな状態で検証するため、有効な設定を書く前に行う）。
	bad := webhook.Config{Enabled: true, URL: "http://example.com/hook", HeaderName: "X", HeaderSecret: "y"}
	if err := c.SetWebhookConfig(ctx, bad); err == nil {
		t.Fatal("http スキームの URL が保存できた（Validate が効いていない）")
	}
	cfg, err = c.GetWebhookConfig(ctx)
	if err != nil {
		t.Fatalf("GetWebhookConfig(拒否後): %v", err)
	}
	if cfg.Enabled {
		t.Fatalf("拒否されたはずの設定が書き込まれている: %+v", cfg)
	}

	// 有効な設定は round trip する。
	want := webhook.Config{
		Enabled: true, URL: "https://example.com/hook",
		HeaderName: "X-Secret", HeaderSecret: "s3cr3t",
	}
	if err := c.SetWebhookConfig(ctx, want); err != nil {
		t.Fatalf("SetWebhookConfig: %v", err)
	}
	got, err := c.GetWebhookConfig(ctx)
	if err != nil {
		t.Fatalf("GetWebhookConfig(設定後): %v", err)
	}
	if got != want {
		t.Fatalf("round trip 不一致: got=%+v want=%+v", got, want)
	}
}
