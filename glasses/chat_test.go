package glasses

// ミラー中継（POST /glasses/chat・GET /glasses/chat/recent）の検証。
// 保管はインメモリ（memStore の ChatStore 実装）で差し替える。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testMirrorToken = "aabbccddeeff00112233445566778899" // 32 文字

func testRegistry() Registry {
	return RegistryFunc(func(_ context.Context, pc string) (bool, error) { return pc == testPC, nil })
}

// newChatServer は pull/answer/ask/mirror を全部有効にしたハンドラを立てる
// （memAskStore は memStore を埋め込むので Store+AskStore+ChatStore を満たす）。
func newChatServer(t *testing.T, st *memAskStore) *httptest.Server {
	t.Helper()
	return newChatServerStore(t, st, true)
}

// newChatServerStore は chat を有効化するか選べる（無効時 404 の検証用）。
func newChatServerStore(t *testing.T, st *memAskStore, enableChat bool) *httptest.Server {
	t.Helper()
	h, err := NewHandler(st, testRegistry(), testToken, testAnswerToken)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	if err := h.EnableAsk(testAskToken, &fakeWebhook{}, testPC); err != nil {
		t.Fatalf("EnableAsk: %v", err)
	}
	if enableChat {
		if err := h.EnableChat(testMirrorToken); err != nil {
			t.Fatalf("EnableChat: %v", err)
		}
	}
	mux := http.NewServeMux()
	mux.Handle("/glasses/", h)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func postChat(t *testing.T, ts *httptest.Server, token, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+ChatPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", ChatPath, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func getChat(t *testing.T, ts *httptest.Server, token, query string) (int, []any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+ChatRecentPath+query, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", ChatRecentPath, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	items, _ := out["items"].([]any)
	return resp.StatusCode, items
}

func TestChatPushAndRecent(t *testing.T) {
	st := newMemAskStore()
	ts := newChatServer(t, st)

	body := `{"items":[` +
		`{"id":"t1s1","role":"user","text":"あなた行","ts":1000},` +
		`{"id":"t1s2","role":"assistant","kind":"widget","text":"質問カード","ts":2000}]}`
	code, out := postChat(t, ts, testMirrorToken, body)
	if code != http.StatusOK || out["ok"] != true || out["count"].(float64) != 2 {
		t.Fatalf("push 失敗: code=%d out=%v", code, out)
	}

	// 読み出しは ASK トークン（HUD の認証情報）で通る
	code, items := getChat(t, ts, testAskToken, "")
	if code != http.StatusOK || len(items) != 2 {
		t.Fatalf("recent 失敗: code=%d items=%v", code, items)
	}
	m0 := items[0].(map[string]any)
	m1 := items[1].(map[string]any)
	if m0["id"] != "t1s1" || m0["role"] != "user" || m0["text"] != "あなた行" {
		t.Fatalf("1 件目が不正: %v", m0)
	}
	if m1["kind"] != "widget" || m1["role"] != "assistant" {
		t.Fatalf("2 件目が不正: %v", m1)
	}
	if m0["ts"].(float64) != 1000 || m1["ts"].(float64) != 2000 {
		t.Fatalf("ts が不正: %v / %v", m0["ts"], m1["ts"])
	}

	// ミラートークンでも読める（ブリッジ/運用向け）
	if code, items = getChat(t, ts, testMirrorToken, ""); code != http.StatusOK || len(items) != 2 {
		t.Fatalf("ミラートークンでの読み出しが失敗: code=%d", code)
	}
}

func TestChatRecentSince(t *testing.T) {
	st := newMemAskStore()
	ts := newChatServer(t, st)
	body := `{"items":[` +
		`{"id":"s1","role":"user","text":"古い","ts":1000},` +
		`{"id":"s2","role":"assistant","text":"中","ts":2000},` +
		`{"id":"s3","role":"assistant","text":"新しい","ts":3000}]}`
	if code, out := postChat(t, ts, testMirrorToken, body); code != http.StatusOK {
		t.Fatalf("push 失敗: code=%d out=%v", code, out)
	}
	// since=2000 は 2000 以降（同一 ts を含む）＝2 件
	code, items := getChat(t, ts, testAskToken, "?since=2000")
	if code != http.StatusOK || len(items) != 2 {
		t.Fatalf("since=2000: code=%d len=%d items=%v", code, len(items), items)
	}
	if items[0].(map[string]any)["id"] != "s2" || items[1].(map[string]any)["id"] != "s3" {
		t.Fatalf("since=2000 の内容が不正: %v", items)
	}
	// since=2500 は 1 件
	if _, items = getChat(t, ts, testAskToken, "?since=2500"); len(items) != 1 {
		t.Fatalf("since=2500: len=%d", len(items))
	}
	// since=4000 は 0 件
	if _, items = getChat(t, ts, testAskToken, "?since=4000"); len(items) != 0 {
		t.Fatalf("since=4000: len=%d", len(items))
	}
	// 不正な since は無視（全件）
	if _, items = getChat(t, ts, testAskToken, "?since=abc"); len(items) != 3 {
		t.Fatalf("不正 since: len=%d", len(items))
	}
}

func TestChatPushSingleItem(t *testing.T) {
	st := newMemAskStore()
	ts := newChatServer(t, st)
	code, out := postChat(t, ts, testMirrorToken, `{"id":"one1","role":"assistant","text":"単発"}`)
	if code != http.StatusOK || out["count"].(float64) != 1 {
		t.Fatalf("単発 body が失敗: code=%d out=%v", code, out)
	}
	_, items := getChat(t, ts, testAskToken, "")
	if len(items) != 1 || items[0].(map[string]any)["id"] != "one1" {
		t.Fatalf("単発が保存されていない: %v", items)
	}
}

func TestChatAuthSeparation(t *testing.T) {
	st := newMemAskStore()
	ts := newChatServer(t, st)
	body := `{"id":"x1","role":"user","text":"x"}`

	// push はミラートークンのみ
	for _, tok := range []string{"", testToken, testAnswerToken, testAskToken, "wrong-token-wrong-token-wrong-token"} {
		if code, _ := postChat(t, ts, tok, body); code != http.StatusUnauthorized {
			t.Fatalf("push が %q で通った: %d", tok, code)
		}
	}
	// 読み出しは ASK かミラートークンのみ
	for _, tok := range []string{"", testToken, testAnswerToken, "wrong-token-wrong-token-wrong-token"} {
		if code, _ := getChat(t, ts, tok, ""); code != http.StatusUnauthorized {
			t.Fatalf("recent が %q で通った: %d", tok, code)
		}
	}
	// ミラートークンで取得系（pull）は通らない
	if resp, _ := doGet(t, ts, AnswersPath+"?pc="+testPC, testMirrorToken); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("ミラートークンで取得系が通った: %d", resp.StatusCode)
	}
	// ASK トークンで push は通らない（上のループで確認済みだが明示）
	if code, _ := postChat(t, ts, testAskToken, body); code != http.StatusUnauthorized {
		t.Fatalf("ASK トークンで push できた: %d", code)
	}
}

func TestChatValidationAndLimits(t *testing.T) {
	st := newMemAskStore()
	ts := newChatServer(t, st)
	cases := []struct {
		name string
		body string
		want int
	}{
		{"role 不正", `{"id":"a","role":"system","text":"x"}`, http.StatusBadRequest},
		{"text 空", `{"id":"a","role":"user","text":""}`, http.StatusBadRequest},
		{"id 不正", `{"id":"あ","role":"user","text":"x"}`, http.StatusBadRequest},
		{"id 長すぎ", `{"id":"` + strings.Repeat("a", 65) + `","role":"user","text":"x"}`, http.StatusBadRequest},
		{"項目が違う", `{"nope":{"id":"a"}}`, http.StatusBadRequest},
		{"text 長すぎ", `{"id":"a","role":"user","text":"` + strings.Repeat("あ", MaxChatTextRunes+1) + `"}`, http.StatusBadRequest},
	}
	for _, c := range cases {
		if code, _ := postChat(t, ts, testMirrorToken, c.body); code != c.want {
			t.Fatalf("%s: code=%d want=%d", c.name, code, c.want)
		}
	}
	// MaxChatBatch+1 件は受け付けない
	if code, _ := postChat(t, ts, testMirrorToken, chatBatchBody(0, MaxChatBatch+1)); code != http.StatusBadRequest {
		t.Fatalf("バッチ超過が通った: %d", code)
	}
	// 64KB 超
	big := `{"id":"big","role":"user","text":"` + strings.Repeat("x", 100*1024) + `"}`
	if code, _ := postChat(t, ts, testMirrorToken, big); code != http.StatusBadRequest {
		t.Fatalf("64KB 超が通った: %d", code)
	}
}

// chatBatchBody は from..to-1 の n 件の push body を作る（ts は連番）。
func chatBatchBody(from, to int) string {
	base := time.Now().UnixMilli()
	var b strings.Builder
	b.WriteString(`{"items":[`)
	for i := from; i < to; i++ {
		if i > from {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":"c%03d","role":"user","text":"行%d","ts":%d}`, i, i, base+int64(i))
	}
	b.WriteString(`]}`)
	return b.String()
}

func TestChatRecentCapsAt50AndDropsExpired(t *testing.T) {
	st := newMemAskStore()
	ts := newChatServer(t, st)
	for _, part := range [][2]int{{0, 30}, {30, 60}} {
		if code, out := postChat(t, ts, testMirrorToken, chatBatchBody(part[0], part[1])); code != http.StatusOK {
			t.Fatalf("分割 push が失敗: code=%d out=%v", code, out)
		}
	}
	_, items := getChat(t, ts, testAskToken, "")
	if len(items) != MaxChatItems {
		t.Fatalf("件数=%d, want %d", len(items), MaxChatItems)
	}
	// 直近 50 件（新しい側）が残る＝先頭は 10 番目
	if items[0].(map[string]any)["id"] != "c010" {
		t.Fatalf("古い側が残っている: %v", items[0])
	}
	// limit 指定
	if _, few := getChat(t, ts, testAskToken, "?limit=3"); len(few) != 3 {
		t.Fatalf("limit=3 が効いていない: %d", len(few))
	}
	// 期限切れは返らない
	st.expireChat("c010")
	_, items = getChat(t, ts, testAskToken, "")
	for _, it := range items {
		if it.(map[string]any)["id"] == "c010" {
			t.Fatal("期限切れの行が返った")
		}
	}
}

func TestChatDisabledIs404(t *testing.T) {
	st := newMemAskStore()
	ts := newChatServerStore(t, st, false)
	if code, _ := postChat(t, ts, testToken, `{"id":"a","role":"user","text":"x"}`); code != http.StatusNotFound {
		t.Fatalf("無効時の push: %d, want 404", code)
	}
	if code, _ := getChat(t, ts, testToken, ""); code != http.StatusNotFound {
		t.Fatalf("無効時の recent: %d, want 404", code)
	}
}

func TestEnableChatRejects(t *testing.T) {
	st := newMemAskStore()
	h, err := NewHandler(st, testRegistry(), testToken, testAnswerToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.EnableChat("short"); err == nil {
		t.Fatal("短いトークンで有効化できた")
	}
	if err := h.EnableChat(testToken); err == nil {
		t.Fatal("取得トークンと同一で有効化できた")
	}
	if err := h.EnableChat(testAnswerToken); err == nil {
		t.Fatal("投入トークンと同一で有効化できた")
	}
	if err := h.EnableAsk(testAskToken, &fakeWebhook{}, testPC); err != nil {
		t.Fatal(err)
	}
	if err := h.EnableChat(testAskToken); err == nil {
		t.Fatal("ASK トークンと同一で有効化できた")
	}
	if err := h.EnableChat(testMirrorToken); err != nil {
		t.Fatalf("正しいトークンで失敗: %v", err)
	}
	if !h.ChatEnabled() {
		t.Fatal("ChatEnabled が false")
	}
}
