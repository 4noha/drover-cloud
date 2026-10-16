package state

// セッション状態 Webhook 通知の設定永続化（settings/webhook、単一 doc・
// account 全体で1つ）。送信ロジック自体は webhook パッケージ（Firestore に
// 依存しない）に置き、ここは Get/Set のみを担う（state パッケージの既存の
// 役割分担と同じ: 画面解釈・ビジネスロジックは持たず、永続化のみ）。

import (
	"context"

	"cloud.google.com/go/firestore"
	"github.com/4noha/drover-cloud/webhook"
)

func (c *Client) webhookDoc() *firestore.DocumentRef {
	return c.fs.Collection("settings").Doc("webhook")
}

// GetWebhookConfig は settings/webhook を読む。未設定（初回）はエラーにせず
// Config{}（Enabled=false）を返す＝既定 OFF（PCVersion 等と同じ「無ければ
// 安全側既定」規律）。
func (c *Client) GetWebhookConfig(ctx context.Context) (webhook.Config, error) {
	snap, err := c.webhookDoc().Get(ctx)
	if err != nil || !snap.Exists() {
		return webhook.Config{}, nil
	}
	var cfg webhook.Config
	if e := snap.DataTo(&cfg); e != nil {
		return webhook.Config{}, e
	}
	return cfg, nil
}

// SetWebhookConfig は settings/webhook を全置換する（owner 管理画面の保存）。
// 呼び手（web ハンドラ）が webhook.Validate を先に通す前提だが、ここでも
// 再検証する（defense-in-depth・鉄則5「silent にしない」の一環で、呼び手が
// 検証を漏らしても不正な設定を書かせない）。
func (c *Client) SetWebhookConfig(ctx context.Context, cfg webhook.Config) error {
	if err := webhook.Validate(cfg); err != nil {
		return err
	}
	_, err := c.webhookDoc().Set(ctx, map[string]any{
		"enabled": cfg.Enabled, "url": cfg.URL,
		"header_name": cfg.HeaderName, "header_secret": cfg.HeaderSecret,
	})
	return err
}
