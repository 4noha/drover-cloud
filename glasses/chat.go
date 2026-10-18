package glasses

// ミラー（Herdr の会話の逐語コピー）の中継（外出対応）。
//
//	Mac ブリッジが Grok Bot のローカルキャッシュから拾った行を
//	POST /glasses/chat で push（GLASSES_MIRROR_TOKEN）→ Firestore
//	（glasses_chat・直近 50 件・短い TTL）→ 眼鏡の HUD が
//	GET /glasses/chat/recent で読む（GLASSES_ASK_TOKEN またはミラートークン）。
//
// 保持は「直近 MaxChatItems 件・短めの TTL」。読み出しは単一フィールドの
// orderBy（ts）だけを使う＝複合インデックスを要求しない。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// ChatCollection はミラー行を置く Firestore コレクション。
	ChatCollection = "glasses_chat"
	// ChatPath は push（POST・GLASSES_MIRROR_TOKEN）。
	ChatPath = "/glasses/chat"
	// ChatRecentPath は読み出し（GET・GLASSES_ASK_TOKEN または MIRROR）。
	ChatRecentPath = "/glasses/chat/recent"
	// MaxChatItems は読み出しで返す最大件数（＝保持の上限）。
	MaxChatItems = 50
	// MaxChatBatch は 1 回の push で受ける最大件数。
	MaxChatBatch = 50
	// MaxChatTextRunes は 1 行の本文上限（rune）。
	MaxChatTextRunes = 4000
	// DefaultChatTTL は短めの保持（HUD は直近しか見ない）。
	DefaultChatTTL = 10 * time.Minute
)

var chatIDRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// ChatItem はミラー 1 行（ブリッジの /chat/recent と同じ形）。
type ChatItem struct {
	ID   string
	Role string // "user" | "assistant"
	Kind string // 例 "widget"（無ければ空）
	Text string
	TS   int64 // epoch ms（0 なら保存時に now）
}

// ChatStore はミラーの保管（本番は Firestore、テストはインメモリ）。
type ChatStore interface {
	PutChat(ctx context.Context, items []ChatItem) error
	RecentChat(ctx context.Context, limit int) ([]ChatItem, error)
}

// ValidateChatItem は 1 行の検査。
func ValidateChatItem(it ChatItem) error {
	if !chatIDRe.MatchString(it.ID) {
		return fmt.Errorf("%w: id が不正です（^[A-Za-z0-9._:-]{1,64}$）", ErrInvalid)
	}
	if it.Role != "user" && it.Role != "assistant" {
		return fmt.Errorf("%w: role は user/assistant のみ", ErrInvalid)
	}
	if len(it.Kind) > 32 {
		return fmt.Errorf("%w: kind が長すぎます", ErrInvalid)
	}
	if n := utf8.RuneCountInString(it.Text); n < 1 || n > MaxChatTextRunes {
		return fmt.Errorf("%w: text は 1〜%d 文字（rune）です", ErrInvalid, MaxChatTextRunes)
	}
	return nil
}

// ClampChatLimit は読み出し件数を 1〜MaxChatItems に丸める。
func ClampChatLimit(n int) int {
	if n <= 0 {
		return MaxChatItems
	}
	if n > MaxChatItems {
		return MaxChatItems
	}
	return n
}

// EnableChat はミラー中継（POST /glasses/chat・GET /glasses/chat/recent）を
// 有効にする。token は 32 文字以上で、取得・投入・問い合わせトークンとは別で
// あること（役割分離）。読み出しは眼鏡のトークン（GLASSES_ASK_TOKEN）でも
// 通す（HUD は 1 つの認証情報しか持たないため）。
func (h *Handler) EnableChat(mirrorToken string) error {
	if len(mirrorToken) < MinTokenLen {
		return fmt.Errorf("glasses: GLASSES_MIRROR_TOKEN が短すぎます（%d 文字以上）", MinTokenLen)
	}
	if mirrorToken == h.pullToken || mirrorToken == h.answerToken || (h.askToken != "" && mirrorToken == h.askToken) {
		return errors.New("glasses: GLASSES_MIRROR_TOKEN は他のトークンと別にしてください")
	}
	cs, ok := h.store.(ChatStore)
	if !ok {
		return errors.New("glasses: ストアがミラーに対応していません")
	}
	h.chat, h.mirrorToken = cs, mirrorToken
	h.chatRL = newRateLimiter(rateLimitPerMin)
	return nil
}

// ChatEnabled はミラー中継が有効かを返す。
func (h *Handler) ChatEnabled() bool { return h.chat != nil && h.mirrorToken != "" }

// authorizedChatRead は読み出しの認証（ASK トークンまたはミラートークン）。
func (h *Handler) authorizedChatRead(r *http.Request) bool {
	for _, tok := range []string{h.askToken, h.mirrorToken} {
		if tok != "" && h.authorized(r, tok) {
			return true
		}
	}
	return false
}

// serveChat はミラー中継を捌く（無効なら 404）。
func (h *Handler) serveChat(w http.ResponseWriter, r *http.Request, method string, isPush bool, fn func(http.ResponseWriter, *http.Request)) {
	if !h.ChatEnabled() {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != method {
		methodNotAllowed(w, method)
		return
	}
	if isPush {
		if !h.authorized(r, h.mirrorToken) {
			unauthorized(w)
			return
		}
	} else if !h.authorizedChatRead(r) {
		unauthorized(w)
		return
	}
	if !h.chatRL.allow(clientKey(r), h.now()) {
		writeErr(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	fn(w, r)
}

type chatPushItem struct {
	ID   string `json:"id"`
	Role string `json:"role"`
	Kind string `json:"kind"`
	Text string `json:"text"`
	TS   int64  `json:"ts"`
}

// handleChatPush はブリッジからのミラー行を受ける。`{"items":[...]}` でも
// 単発の 1 行でも受ける。
func (h *Handler) handleChatPush(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "body が読めません（64KB 以内）")
		return
	}
	items := []chatPushItem{}
	var multi struct {
		Items []chatPushItem `json:"items"`
	}
	if err := json.Unmarshal(raw, &multi); err == nil && len(multi.Items) > 0 {
		items = multi.Items
	} else {
		var one chatPushItem
		if err := json.Unmarshal(raw, &one); err != nil {
			writeErr(w, http.StatusBadRequest, "body が不正です（JSON）")
			return
		}
		items = []chatPushItem{one}
	}
	if len(items) == 0 {
		writeErr(w, http.StatusBadRequest, "items が空です")
		return
	}
	if len(items) > MaxChatBatch {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("items が多すぎます（最大 %d）", MaxChatBatch))
		return
	}
	out := make([]ChatItem, 0, len(items))
	for _, it := range items {
		out = append(out, ChatItem{ID: it.ID, Role: it.Role, Kind: it.Kind, Text: it.Text, TS: it.TS})
	}
	if err := h.chat.PutChat(r.Context(), out); err != nil {
		if errors.Is(err, ErrInvalid) {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "保存に失敗しました")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(out)})
}

// handleChatRecent は直近のミラー行を古い→新しい順で返す。
func (h *Handler) handleChatRecent(w http.ResponseWriter, r *http.Request) {
	limit := MaxChatItems
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = ClampChatLimit(n)
		}
	}
	items, err := h.chat.RecentChat(r.Context(), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "取得に失敗しました")
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, map[string]any{
			"id": it.ID, "role": it.Role, "kind": it.Kind, "text": it.Text, "ts": it.TS,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}
