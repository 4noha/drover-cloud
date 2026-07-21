package state

// Package state の push token 管理: Web Push（FCM）の登録トークンを
// pushtokens/{token} へ保存する。ブラウザの購読は「オーナー全体」に属し
// 特定 PC に紐付かない（オーナーがどの PC でタスクを終えても、登録済み
// 全ブラウザへ通知したい）ので pcs/** 配下ではなくトップレベル collection。
// doc id をトークン自体にする＝同一トークンの再登録が自然に upsert になる
// （ブラウザが定期的に同じ token を再登録しても重複が増えない）。

import (
	"context"
	"time"

	"cloud.google.com/go/firestore"
)

func (c *Client) pushTokenCol() *firestore.CollectionRef {
	return c.fs.Collection("pushtokens")
}

// SavePushToken は FCM 登録トークンを upsert する（last_seen を毎回更新）。
// token が空なら no-op（呼び手のバグを踏んでも書かない）。
func (c *Client) SavePushToken(ctx context.Context, token, ua string) error {
	if token == "" {
		return nil
	}
	_, err := c.pushTokenCol().Doc(token).Set(ctx, map[string]any{
		"token": token, "ua": ua,
		"last_seen": time.Now().UTC().Format(time.RFC3339Nano),
	})
	return err
}

// ListPushTokens は登録済み全トークンを返す（送信対象の列挙。件数は
// オーナー個人の購読ブラウザ数＝実運用で数件程度を想定）。
func (c *Client) ListPushTokens(ctx context.Context) ([]string, error) {
	docs, err := c.pushTokenCol().Documents(ctx).GetAll()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		if tok, _ := d.Data()["token"].(string); tok != "" {
			out = append(out, tok)
		}
	}
	return out, nil
}

// DeletePushToken は無効化されたトークン（FCM send が
// UNREGISTERED/invalid を返した場合）を除去する。
func (c *Client) DeletePushToken(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	_, err := c.pushTokenCol().Doc(token).Delete(ctx)
	return err
}
