package glasses

// 眼鏡の外出対応: HUD が LAN のブリッジを経由せず、公開 relay へ直接
// 問い合わせ（POST /glasses/ask）と返信の取得（GET /glasses/reply）を行う。
//
// 経路:
//
//	HUD → POST /glasses/ask（GLASSES_ASK_TOKEN）→ relay が問い合わせを
//	glasses_asks に登録し、ルーティンの webhook を発火（ブリッジと同じ
//	ペイロード {request_id, text, ts}）→ 回答は既存の投入口
//	（POST /glasses/answer / MCP glasses_answer）に届く → PutAnswer が
//	「relay 経由の問い合わせ」だと判定したら glasses_answers ではなく
//	glasses_asks 側に格納（＝LAN ブリッジの pull/ack には見えない）→
//	HUD が GET /glasses/reply で「[request_id] 本文」を受け取る。
//
// トークン・webhook の URL/キー・本文はログに出さない。

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// AskPath は HUD からの問い合わせ（POST・GLASSES_ASK_TOKEN）。
	AskPath = "/glasses/ask"
	// ReplyPath は HUD の返信取得（GET・GLASSES_ASK_TOKEN）。
	ReplyPath = "/glasses/reply"
	// AskCollection は relay 経由の問い合わせと、その回答を置くコレクション。
	AskCollection = "glasses_asks"
	// DeliveredGrace は返信を一度返したあと、同じ返信を再取得できる猶予
	// （HUD 側の受信取りこぼし対策）。過ぎたら TTL で消える。
	DeliveredGrace = time.Minute
	// webhookTimeout は webhook 呼び出しの上限。
	webhookTimeout = 15 * time.Second

	askAuditEmail = "glasses-ask-token"
	askAuditTool  = "glasses_ask_http"
)

// ErrAskNotFound は問い合わせが無い（未登録・期限切れ・他 pc）とき。
var ErrAskNotFound = errors.New("glasses ask not found")

// AskStore は relay 経由の問い合わせの保管（本番は Firestore）。
type AskStore interface {
	// RegisterAsk は問い合わせを登録する（回答の振り分けに使う）。
	RegisterAsk(ctx context.Context, pc, requestID string) (expiresAt time.Time, err error)
	// AnswerAsk は問い合わせが登録済みなら回答を格納して routed=true を返す。
	// 未登録なら routed=false（呼び手は従来の Put に回す）。
	AnswerAsk(ctx context.Context, pc, requestID, text string) (routed bool, expiresAt time.Time, err error)
	// TakeAskReply は回答済みなら本文を返す（answered=false は未回答）。
	// 問い合わせが無ければ ErrAskNotFound。
	TakeAskReply(ctx context.Context, pc, requestID string) (text string, answered bool, err error)
}

// WebhookSender はルーティンの webhook を発火する。
type WebhookSender interface {
	Send(ctx context.Context, requestID, text string) (status int, err error)
}

// httpWebhook はブリッジ（server.mjs の forwardToWebhook）と同じ形式で POST する。
type httpWebhook struct {
	url    string
	key    string
	client *http.Client
}

// NewWebhook は webhook 送信器を作る（https 必須・キー必須）。
func NewWebhook(rawURL, key string) (WebhookSender, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("glasses: GROK_WEBHOOK_URL は https の URL が必要です")
	}
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("glasses: GROK_WEBHOOK_KEY が空です")
	}
	return &httpWebhook{url: u.String(), key: strings.TrimSpace(key), client: &http.Client{Timeout: webhookTimeout}}, nil
}

// Send は {request_id, text, ts} を Bearer 付きで送る。エラーに URL を含めない。
func (w *httpWebhook) Send(ctx context.Context, requestID, text string) (int, error) {
	body, _ := json.Marshal(map[string]string{
		"request_id": requestID,
		"text":       text,
		"ts":         time.Now().UTC().Format(time.RFC3339Nano),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return 0, errors.New("webhook リクエストを作れません")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+w.key)
	res, err := w.client.Do(req)
	if err != nil {
		return 0, errors.New("webhook に接続できません")
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return res.StatusCode, fmt.Errorf("webhook status=%d", res.StatusCode)
	}
	return res.StatusCode, nil
}

// NewRequestID は ^[rt]-[a-z0-9-]{1,48}$ に合う ID を作る（HUD の
// newRequestId と同じ形: r-<時刻36進>-<乱数16進4桁>）。
func NewRequestID() string {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return "r-" + strconv.FormatInt(time.Now().UnixMilli(), 36) + "-" + hex.EncodeToString(b[:])
}

// EnableAsk は問い合わせ口を有効にする。token は 32 文字以上で、取得・投入
// トークンのどちらとも別であること。store は AskStore を満たすこと。
// defaultPC は body/クエリに pc が無いときに使う（空なら pc 必須）。
func (h *Handler) EnableAsk(token string, ws WebhookSender, defaultPC string) error {
	if len(token) < MinTokenLen {
		return fmt.Errorf("glasses: GLASSES_ASK_TOKEN が短すぎます（%d 文字以上）", MinTokenLen)
	}
	if token == h.pullToken || (h.answerToken != "" && token == h.answerToken) {
		return errors.New("glasses: GLASSES_ASK_TOKEN は取得・投入トークンと別にしてください")
	}
	as, ok := h.store.(AskStore)
	if !ok {
		return errors.New("glasses: ストアが問い合わせに対応していません")
	}
	if ws == nil {
		return errors.New("glasses: webhook が未設定です")
	}
	if defaultPC != "" && !ValidPC(defaultPC) {
		return errors.New("glasses: GLASSES_ASK_PC が不正です")
	}
	h.askToken, h.askStore, h.webhook, h.askPC = token, as, ws, defaultPC
	h.askRL = newRateLimiter(rateLimitPerMin)
	return nil
}

// AskEnabled は問い合わせ口（/glasses/ask・/glasses/reply）が有効かを返す。
func (h *Handler) AskEnabled() bool { return h.askToken != "" }

// serveAsk は AskPath / ReplyPath を捌く（無効なら 404）。
func (h *Handler) serveAsk(w http.ResponseWriter, r *http.Request, method string, fn func(http.ResponseWriter, *http.Request)) {
	if !h.AskEnabled() {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != method {
		methodNotAllowed(w, method)
		return
	}
	if !h.authorized(r, h.askToken) {
		unauthorized(w)
		return
	}
	if !h.askRL.allow(clientKey(r), h.now()) {
		writeErr(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	fn(w, r)
}

type askRequest struct {
	PC        string          `json:"pc"`
	RequestID string          `json:"request_id"`
	Text      string          `json:"text"`
	TS        json.RawMessage `json:"ts"` // HUD が送る（使わない）
}

func (h *Handler) handleAsk(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req askRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "body が不正です（JSON・64KB 以内）")
		return
	}
	pc := req.PC
	if pc == "" {
		pc = h.askPC
	}
	if !ValidPC(pc) {
		writeErr(w, http.StatusBadRequest, "pc が不正です")
		return
	}
	rid := req.RequestID
	if rid == "" {
		rid = NewRequestID()
	}
	if !ValidRequestID(rid) {
		writeErr(w, http.StatusBadRequest, "request_id が不正です（^[rt]-[a-z0-9-]{1,48}$）")
		return
	}
	if !ValidText(req.Text) {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("text は 1〜%d 文字（rune）です", MaxTextRunes))
		return
	}
	registered, err := h.reg.Registered(r.Context(), pc)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "登録確認に失敗しました")
		return
	}
	if !registered {
		h.askAudit(r.Context(), "forbidden", pc, rid)
		writeErr(w, http.StatusForbidden, "pc が未登録です")
		return
	}
	// 回答が先に届いても振り分けられるよう、webhook より前に登録する。
	if _, err := h.askStore.RegisterAsk(r.Context(), pc, rid); err != nil {
		h.askAudit(r.Context(), "server_error", pc, rid)
		writeErr(w, http.StatusInternalServerError, "問い合わせの登録に失敗しました")
		return
	}
	status, err := h.webhook.Send(r.Context(), rid, req.Text)
	if err != nil {
		h.askAudit(r.Context(), "webhook_failed", pc, rid)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "webhook failed", "status": status, "request_id": rid})
		return
	}
	h.askAudit(r.Context(), "ok", pc, rid)
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": true, "request_id": rid, "upstreamStatus": status})
}

func (h *Handler) handleReply(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pc := q.Get("pc")
	if pc == "" {
		pc = h.askPC
	}
	if !ValidPC(pc) {
		writeErr(w, http.StatusBadRequest, "pc が不正です")
		return
	}
	rid := q.Get("request_id")
	if !ValidRequestID(rid) {
		writeErr(w, http.StatusBadRequest, "request_id が不正です（^[rt]-[a-z0-9-]{1,48}$）")
		return
	}
	text, answered, err := h.askStore.TakeAskReply(r.Context(), pc, rid)
	switch {
	case errors.Is(err, ErrAskNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"text": nil, "status": "not_found"})
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "取得に失敗しました")
	case !answered:
		// ブリッジの /reply と同じく「未着は text:null」。
		writeJSON(w, http.StatusOK, map[string]any{"text": nil, "status": "pending"})
	default:
		// ブリッジの /reply と同じ「[request_id] 本文」（HUD の parseReply が分解する）。
		writeJSON(w, http.StatusOK, map[string]any{
			"text":   "[" + rid + "] " + text,
			"ts":     h.now().UnixMilli(),
			"status": "delivered",
		})
	}
}

func (h *Handler) askAudit(ctx context.Context, resultCode, pc, requestID string) {
	if h.auditor == nil {
		return
	}
	_ = h.auditor.WriteAnswerAudit(ctx, AuditEntry{
		Email: askAuditEmail, Tool: askAuditTool, PC: pc,
		ArgsDigest: AnswerDigest(pc, requestID), ResultCode: resultCode,
	})
}
