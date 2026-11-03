// relayurl.go — relay の WS 接続先を Firestore で一括切替する（settings/relay）。
//
// Cloud Run の relay は WS 常時接続で 1 vCPU が 24 時間課金される（2026-11 実測: 月 ¥7〜8k）
// ため、WS の中継だけを東京の e2-micro へ移す。各 PC の agent / ↗窓 attach と、Cloud Run の
// Web（/ws の上流）は、この doc の url が空でなければそれへ繋ぐ。空／無ければ従来どおり
// 各自の設定（CLOUD_RELAY_URL / config.json）＝戻すのは url を空にするだけ。
//
// HTTP の API（/slave/* の token/grant/wake/commands・Web UI・MCP）は Cloud Run のまま
// （この url は WS の中継 `/session` にだけ使う）。
package state

import (
	"context"
	"time"
)

const relaySettingsDoc = "relay"

// SetRelayURL は WS 中継の接続先（wss://…）を書く。空で解除。
func (c *Client) SetRelayURL(ctx context.Context, url string) error {
	_, err := c.fs.Collection("settings").Doc(relaySettingsDoc).Set(ctx, map[string]any{
		"url":        url,
		"updated_at": time.Now().UTC().Format(time.RFC3339Nano),
		"updated_by": c.pcID,
	})
	return err
}

// RelayURL は設定された接続先を返す。未設定／doc 無しは ""（エラーではない）。
func (c *Client) RelayURL(ctx context.Context) (string, error) {
	snap, err := c.fs.Collection("settings").Doc(relaySettingsDoc).Get(ctx)
	if err != nil {
		if snap != nil && !snap.Exists() {
			return "", nil
		}
		return "", err
	}
	if !snap.Exists() {
		return "", nil
	}
	url, _ := snap.Data()["url"].(string)
	return url, nil
}

// WatchRelayURL は settings/relay を real-time 監視し、初回と変更ごとに cb(url) を呼ぶ
// （doc 無し／空は cb("")）。WatchWake と同じ keepSubscribed（再購読 backoff）。
func (c *Client) WatchRelayURL(ctx context.Context, cb func(url string)) error {
	return keepSubscribed(ctx, watchMaxLifetime, func(subCtx context.Context) (func() error, func()) {
		it := c.fs.Collection("settings").Doc(relaySettingsDoc).Snapshots(subCtx)
		pump := func() error {
			for {
				snap, err := it.Next()
				if err != nil {
					return err
				}
				if snap == nil || !snap.Exists() {
					cb("")
					continue
				}
				url, _ := snap.Data()["url"].(string)
				cb(url)
			}
		}
		return pump, func() { it.Stop() }
	})
}
