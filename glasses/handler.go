package glasses

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// AnswersPath は未取得回答の取得（GET・GLASSES_PULL_TOKEN）。
	AnswersPath = "/glasses/answers"
	// AckPath は取得済み回答の削除（POST・GLASSES_PULL_TOKEN）。
	AckPath = "/glasses/answers/ack"
	// 投入口（POST AnswerPath・GLASSES_ANSWER_TOKEN）の定数は glasses.go 側。

	// maxBodyBytes は POST body の上限（64KB）。
	maxBodyBytes = 64 << 10
	// maxAckIDs は 1 回の ack で受ける id 数の上限。
	maxAckIDs = 200
	// rateLimitPerMin はインスタンス内の簡易レート制限（req/分・クライアント単位）。
	rateLimitPerMin = 120

	// auditEmail / auditTool は HTTP 投入口の監査名義（MCP の監査と同じ
	// mcp_audit コレクションへ書く）。
	auditEmail = "glasses-answer-token"
	auditTool  = "glasses_answer_http"
)

// Handler は /glasses/ 配下の HTTP ハンドラ。
type Handler struct {
	store       Store
	reg         Registry
	pullToken   string // 取得（GET /glasses/answers・ack）用
	answerToken string // 投入（POST /glasses/answer）用。空なら投入口は無効（404）
	auditor     Auditor
	rl          *rateLimiter
	now         func() time.Time

	// 問い合わせ口（/glasses/ask・/glasses/reply）。EnableAsk で有効化（ask.go）。
	askToken string
	askStore AskStore
	webhook  WebhookSender
	askPC    string
	askRL    *rateLimiter
}

// NewHandler はハンドラを作る。pullToken が空/短すぎる場合はエラーを返す
// （呼び手＝main はマウントしない＝fail closed）。answerToken は 32 文字以上
// **かつ** pullToken と別のときだけ投入口を有効にする（無ければ 404）。
func NewHandler(store Store, reg Registry, pullToken, answerToken string) (*Handler, error) {
	if store == nil {
		return nil, errors.New("glasses: store が nil です")
	}
	if reg == nil {
		return nil, errors.New("glasses: registry が nil です")
	}
	if len(pullToken) < MinTokenLen {
		return nil, fmt.Errorf("glasses: GLASSES_PULL_TOKEN が短すぎます（%d 文字以上）", MinTokenLen)
	}
	h := &Handler{store: store, reg: reg, pullToken: pullToken, rl: newRateLimiter(rateLimitPerMin), now: time.Now}
	if len(answerToken) >= MinTokenLen && answerToken != pullToken {
		h.answerToken = answerToken
	}
	return h, nil
}

// SetAuditor は監査の記録先を設定する（nil なら監査しない）。
func (h *Handler) SetAuditor(a Auditor) { h.auditor = a }

// AnswerEnabled は投入口（POST /glasses/answer）が有効かを返す。
func (h *Handler) AnswerEnabled() bool { return h.answerToken != "" }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case AnswersPath:
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		if !h.authorized(r, h.pullToken) {
			unauthorized(w)
			return
		}
		if !h.rl.allow(clientKey(r), h.now()) {
			writeErr(w, http.StatusTooManyRequests, "rate_limited")
			return
		}
		h.handleAnswers(w, r)
	case AckPath:
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		if !h.authorized(r, h.pullToken) {
			unauthorized(w)
			return
		}
		if !h.rl.allow(clientKey(r), h.now()) {
			writeErr(w, http.StatusTooManyRequests, "rate_limited")
			return
		}
		h.handleAck(w, r)
	case AnswerPath:
		if !h.AnswerEnabled() {
			// トークン未設定/短すぎる/取得トークンと同一＝投入口は無い。
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		if !h.authorized(r, h.answerToken) {
			unauthorized(w)
			return
		}
		if !h.rl.allow(clientKey(r), h.now()) {
			writeErr(w, http.StatusTooManyRequests, "rate_limited")
			return
		}
		h.handleAnswer(w, r)
	case AskPath:
		h.serveAsk(w, r, http.MethodPost, h.handleAsk)
	case ReplyPath:
		h.serveAsk(w, r, http.MethodGet, h.handleReply)
	default:
		writeErr(w, http.StatusNotFound, "not found")
	}
}

// authorized は `Authorization: Bearer <want>` を定数時間で比較する。
func (h *Handler) authorized(r *http.Request, want string) bool {
	got := strings.TrimSpace(r.Header.Get("Authorization"))
	scheme, rest, ok := strings.Cut(got, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(rest)), []byte(want)) == 1
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeErr(w, http.StatusUnauthorized, "unauthorized")
}

func (h *Handler) handleAnswers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pc := q.Get("pc")
	if !ValidPC(pc) {
		writeErr(w, http.StatusBadRequest, "pc が不正です")
		return
	}
	wait, err := parseWaitSeconds(q.Get("wait"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "wait が不正です（秒・0〜25）")
		return
	}
	// 未登録 pc は受け付けない（既存の登録確認手段＝state の pc 一覧）。
	registered, err := h.reg.Registered(r.Context(), pc)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "登録確認に失敗しました")
		return
	}
	if !registered {
		writeErr(w, http.StatusForbidden, "pc が未登録です")
		return
	}
	items, err := h.store.Pull(r.Context(), pc, wait)
	if err != nil {
		if r.Context().Err() != nil {
			return // クライアント切断（応答不要）
		}
		if errors.Is(err, ErrInvalid) {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "取得に失敗しました")
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, map[string]any{
			"request_id": it.RequestID,
			"text":       it.Text,
			"created_at": it.CreatedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

type ackRequest struct {
	PC  string   `json:"pc"`
	IDs []string `json:"ids"`
}

func (h *Handler) handleAck(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req ackRequest
	dec := json.NewDecoder(r.Body)
	// 未知のフィールド（例: request_ids の綴り違い）を黙って成功扱いにしない。
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "body が不正です（JSON・64KB 以内・フィールドは pc と ids のみ）")
		return
	}
	if !ValidPC(req.PC) {
		writeErr(w, http.StatusBadRequest, "pc が不正です")
		return
	}
	if len(req.IDs) == 0 {
		writeErr(w, http.StatusBadRequest, "ids が空です")
		return
	}
	if len(req.IDs) > maxAckIDs {
		writeErr(w, http.StatusBadRequest, "ids が多すぎます")
		return
	}
	registered, err := h.reg.Registered(r.Context(), req.PC)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "登録確認に失敗しました")
		return
	}
	if !registered {
		writeErr(w, http.StatusForbidden, "pc が未登録です")
		return
	}
	if err := h.store.Ack(r.Context(), req.PC, req.IDs); err != nil {
		if errors.Is(err, ErrInvalid) {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "削除に失敗しました")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type answerRequest struct {
	PC        string `json:"pc"`
	RequestID string `json:"request_id"`
	Text      string `json:"text"`
}

// handleAnswer は HTTP からの回答投入（MCP の glasses_answer と同じ処理を
// PutAnswer で共有する）。
func (h *Handler) handleAnswer(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req answerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "body が不正です（JSON・64KB 以内）")
		return
	}
	exp, err := PutAnswer(r.Context(), h.store, h.reg, req.PC, req.RequestID, req.Text)
	switch {
	case err == nil:
		h.audit(r.Context(), "ok", req.PC, req.RequestID)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         true,
			"pc":         req.PC,
			"request_id": req.RequestID,
			"expires_at": exp.UTC().Format(time.RFC3339),
		})
	case errors.Is(err, ErrInvalid):
		h.audit(r.Context(), "invalid_request", req.PC, req.RequestID)
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNotRegistered):
		h.audit(r.Context(), "forbidden", req.PC, req.RequestID)
		writeErr(w, http.StatusForbidden, "pc が未登録です")
	default:
		h.audit(r.Context(), "server_error", req.PC, req.RequestID)
		writeErr(w, http.StatusInternalServerError, "保存に失敗しました")
	}
}

// audit は監査を1件書く（失敗しても本処理は止めない・本文は書かない）。
func (h *Handler) audit(ctx context.Context, resultCode, pc, requestID string) {
	if h.auditor == nil {
		return
	}
	_ = h.auditor.WriteAnswerAudit(ctx, AuditEntry{
		Email: auditEmail, Tool: auditTool, PC: pc,
		ArgsDigest: AnswerDigest(pc, requestID), ResultCode: resultCode,
	})
}

// parseWaitSeconds は wait クエリ（秒・小数可）を duration にする。
// 未指定は 0（即返し）。上限は ClampWait で丸める。
func parseWaitSeconds(v string) (time.Duration, error) {
	if strings.TrimSpace(v) == "" {
		return 0, nil
	}
	sec, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, err
	}
	if sec < 0 {
		return 0, fmt.Errorf("wait が負")
	}
	return ClampWait(time.Duration(sec * float64(time.Second))), nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr は {"error": "..."} を返す（内部詳細・トークン・本文は出さない）。
func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
}

// clientKey はレート制限のキー（クライアント IP。取れなければ全体で1つ）。
func clientKey(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil && host != "" {
		return host
	}
	return r.RemoteAddr
}

// rateLimiter はインスタンス内の固定ウィンドウ制限（Cloud Run は通常1
// インスタンス＝Firestore カウンタは使わない）。エントリは IP 数ぶんだけで
// 増え続けない（ウィンドウ経過で上書き）。
type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	seen   map[string]*rlEntry
}

type rlEntry struct {
	count   int
	resetAt time.Time
}

func newRateLimiter(perMin int) *rateLimiter {
	return &rateLimiter{limit: perMin, window: time.Minute, seen: map[string]*rlEntry{}}
}

// allow はトークンを1消費できれば true。
func (l *rateLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.seen[key]
	if !ok || now.After(e.resetAt) {
		l.seen[key] = &rlEntry{count: 1, resetAt: now.Add(l.window)}
		return true
	}
	e.count++
	return e.count <= l.limit
}
