// Package push は Firebase Cloud Messaging (FCM) HTTP v1 API で Web Push
// 通知を送る（Firebase Admin SDK は使わない＝依存追加なし・静的バイナリ
// 維持。web/fbtoken.go の custom token 自前実装と同じ方針）。
//
// 認証は SA 鍵 JSON を `https://www.googleapis.com/auth/firebase.messaging`
// スコープで OAuth2 access token に変換し、Bearer で叩く（golang.org/
// x/oauth2/google は cloud.google.com/go/firestore 経由で既に間接依存に
// 解決済み＝新規依存なし）。
package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// oauthHTTPClient は creds.TokenSource を使う *http.Client を作る
// （リクエストごとに自動で Bearer ヘッダを付与・token 期限切れも自動更新）。
func oauthHTTPClient(ctx context.Context, creds *google.Credentials) *http.Client {
	return oauth2.NewClient(ctx, creds.TokenSource)
}

// MessagingScope は FCM v1 送信に必要な OAuth2 スコープ。
const MessagingScope = "https://www.googleapis.com/auth/firebase.messaging"

// DefaultBaseURL は FCM v1 API の本番エンドポイント。テストは
// httptest.Server の URL に差し替える。
const DefaultBaseURL = "https://fcm.googleapis.com"

// NewAuthenticatedClient は SA 鍵 JSON から FCM 送信スコープの OAuth2
// 認証済み *http.Client を作る（以後の呼び出しは自動で Bearer が付く。
// token 更新も内部で自動）。呼び手はこれを 1 回作って使い回す想定
// （毎送信で認証し直さない＝near-$0）。
func NewAuthenticatedClient(ctx context.Context, saJSON []byte) (*http.Client, error) {
	creds, err := google.CredentialsFromJSON(ctx, saJSON, MessagingScope)
	if err != nil {
		return nil, fmt.Errorf("push: SA 鍵から credentials 作成失敗: %w", err)
	}
	return oauthHTTPClient(ctx, creds), nil
}

// fcmErrorDetail は FCM v1 のエラー応答内の詳細（errorCode を見て
// UNREGISTERED＝無効トークンを判定する）。
type fcmErrorDetail struct {
	Type      string `json:"@type"`
	ErrorCode string `json:"errorCode"`
}

type fcmErrorBody struct {
	Error struct {
		Code    int              `json:"code"`
		Message string           `json:"message"`
		Status  string           `json:"status"`
		Details []fcmErrorDetail `json:"details"`
	} `json:"error"`
}

// Send は 1 トークンへ通知を送る。baseURL は "" なら DefaultBaseURL
// （テストは httptest.Server の URL を渡す）。tag は "" でなければ
// message.data.tag として乗せる（SW 側が Notification の tag に使い、
// 同一セッションの通知は最新1件に集約・別セッションは別々に積む＝
// 呼び手が「どのタスクが完了したか」を区別する用途。空なら data は省く）。
// 戻り値 invalidToken は FCM が UNREGISTERED（トークン失効・アプリ削除済み
// 等）を返した時 true＝呼び手はこの token を ListPushTokens 管理から削除
// すべき合図。
func Send(ctx context.Context, hc *http.Client, baseURL, projectID, token, title, body, tag string) (invalidToken bool, err error) {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if projectID == "" || token == "" {
		return false, fmt.Errorf("push: projectID/token は必須")
	}
	message := map[string]any{
		"token": token,
		"notification": map[string]string{
			"title": title,
			"body":  body,
		},
		// webpush.fcm_options.link は通知クリックで開く URL。
		// Web コンソール自体を指す（相対パス不可＝FCM 仕様で絶対 URL 必須）。
	}
	if tag != "" {
		message["data"] = map[string]string{"tag": tag}
	}
	payload := map[string]any{"message": message}
	buf, err := json.Marshal(payload)
	if err != nil {
		return false, fmt.Errorf("push: payload marshal: %w", err)
	}
	url := baseURL + "/v1/projects/" + projectID + "/messages:send"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return false, fmt.Errorf("push: request 作成: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return false, fmt.Errorf("push: FCM 呼び出し失敗: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		return false, nil
	}
	var eb fcmErrorBody
	_ = json.Unmarshal(respBody, &eb)
	for _, d := range eb.Error.Details {
		if d.ErrorCode == "UNREGISTERED" {
			return true, fmt.Errorf("push: token 失効(UNREGISTERED): %s", eb.Error.Message)
		}
	}
	return false, fmt.Errorf("push: FCM send 失敗 status=%d body=%.500s", resp.StatusCode, respBody)
}
