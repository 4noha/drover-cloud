package web

// セッション状態 Webhook 通知の owner 向け設定 API（/api/webhook）。
// 設定の保存先・送信ロジックは state/webhook.go・webhook パッケージ
// （additive・既存 web.go 本体は Handler() への1行追加のみ）。
//
// accountScope（全PC）トークンのみ許可する（PC スコープの共有リンクからは
// 触れない＝アカウント全体の設定のため）。

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/4noha/drover-cloud/webauth"
	"github.com/4noha/drover-cloud/webhook"
)

// apiWebhook は GET（現在値取得）/POST（保存）を1ルートで扱う。
func (s *Server) apiWebhook(w http.ResponseWriter, r *http.Request, t webauth.Token) {
	if t.Scope != accountScope {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.apiWebhookGet(w, r)
	case http.MethodPost:
		s.apiWebhookSave(w, r)
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

// apiWebhookGet は現在値を返す。HeaderSecret は webhook.Config の
// json:"-" により自然に落ちる＝API 応答へ絶対に載らない（保存後は値を
// 表示しない仕様）。has_secret で「設定済みか」だけ画面に伝える。
func (s *Server) apiWebhookGet(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	cfg, err := s.st.GetWebhookConfig(ctx)
	if err != nil {
		http.Error(w, `{"error":"firestore"}`, http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"enabled": cfg.Enabled, "url": cfg.URL, "header_name": cfg.HeaderName,
		"has_secret": cfg.HeaderSecret != "",
	})
}

type webhookSaveRequest struct {
	Enabled      bool   `json:"enabled"`
	URL          string `json:"url"`
	HeaderName   string `json:"header_name"`
	HeaderSecret string `json:"header_secret"`
}

// apiWebhookSave は設定を保存する。HeaderSecret を空のまま送ると既存値を
// 維持する（画面は保存後に値を表示しないため、変更しない時は毎回打ち直さ
// せない UX。本当に空にしたい＝無効化したい場合は enabled:false で送る）。
func (s *Server) apiWebhookSave(w http.ResponseWriter, r *http.Request) {
	var req webhookSaveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	secret := req.HeaderSecret
	if secret == "" {
		cur, err := s.st.GetWebhookConfig(ctx)
		if err != nil {
			http.Error(w, `{"error":"firestore"}`, http.StatusInternalServerError)
			return
		}
		secret = cur.HeaderSecret
	}
	cfg := webhook.Config{Enabled: req.Enabled, URL: req.URL, HeaderName: req.HeaderName, HeaderSecret: secret}
	if err := webhook.Validate(cfg); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.st.SetWebhookConfig(ctx, cfg); err != nil {
		http.Error(w, `{"error":"firestore"}`, http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// writeJSONError は {"error": "..."} を安全にエンコードして書く（検証エラー
// 文言に `"` 等が混ざってもJSONを壊さない。既存コードの素の文字列埋め込みは
// 固定文言のみで安全だったため使い分ける）。
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
