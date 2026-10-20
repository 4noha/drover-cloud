package glasses

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// memStore はテスト用のインメモリ Store（Firestore 実装と同じ pullLoop を
// 使う＝待機・期限切れ除外のロジックを共有して検証する）。
type memStore struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	entries map[string]memEntry
	chat    []memChatEntry // ミラー（ChatStore）
}

// memChatEntry はミラー 1 行（期限付き）。
type memChatEntry struct {
	item   ChatItem
	expire time.Time
}

type memEntry struct {
	pc, rid, text string
	created       time.Time
	expire        time.Time
}

func newMemStore() *memStore {
	return &memStore{ttl: DefaultTTL, now: time.Now, entries: map[string]memEntry{}}
}

func (m *memStore) Put(_ context.Context, pc, rid, text string) (time.Time, error) {
	if err := ValidatePut(pc, rid, text); err != nil {
		return time.Time{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().UTC()
	exp := now.Add(m.ttl)
	m.entries[DocID(pc, rid)] = memEntry{pc: pc, rid: rid, text: text, created: now, expire: exp}
	return exp, nil
}

func (m *memStore) Pull(ctx context.Context, pc string, wait time.Duration) ([]Answer, error) {
	if !ValidPC(pc) {
		return nil, fmt.Errorf("%w: pc が不正です", ErrInvalid)
	}
	return pullLoop(ctx, wait, m.now, func(context.Context) ([]Answer, error) { return m.snapshot(pc), nil })
}

func (m *memStore) snapshot(pc string) []Answer {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().UTC()
	out := []Answer{}
	for _, e := range m.entries {
		if e.pc != pc || !e.expire.After(now) {
			continue
		}
		out = append(out, Answer{RequestID: e.rid, Text: e.text, CreatedAt: e.created})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func (m *memStore) Ack(_ context.Context, pc string, ids []string) error {
	if !ValidPC(pc) {
		return fmt.Errorf("%w: pc が不正です", ErrInvalid)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		delete(m.entries, DocID(pc, id)) // 存在しなくても成功扱い
	}
	return nil
}

// expireNow は期限切れ（TTL 削除前の残骸）を作る。
func (m *memStore) expireNow(pc, rid string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[DocID(pc, rid)]
	e.expire = m.now().Add(-time.Minute)
	m.entries[DocID(pc, rid)] = e
}

// --- ミラー（ChatStore） -----------------------------------------------------

// PutChat は doc id（entry id）で上書き保存する（本番と同じ規則）。
func (m *memStore) PutChat(_ context.Context, items []ChatItem) error {
	if len(items) > MaxChatBatch {
		return fmt.Errorf("%w: items が多すぎます", ErrInvalid)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, it := range items {
		if err := ValidateChatItem(it); err != nil {
			return err
		}
		if it.TS <= 0 {
			it.TS = m.now().UnixMilli()
		}
		m.chat = append(m.chat, memChatEntry{item: it, expire: m.now().Add(DefaultChatTTL)})
	}
	return nil
}

// RecentChat は期限切れを除き、古い→新しい順で最大 limit 件返す。
func (m *memStore) RecentChat(_ context.Context, limit int, since int64) ([]ChatItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	limit = ClampChatLimit(limit)
	now := m.now().UTC()
	byID := map[string]ChatItem{}
	for _, e := range m.chat {
		if !e.expire.After(now) {
			continue
		}
		if since > 0 && e.item.TS < since {
			continue
		}
		byID[e.item.ID] = e.item // 同一 id は後勝ち（上書き）
	}
	out := make([]ChatItem, 0, len(byID))
	for _, it := range byID {
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS < out[j].TS })
	if len(out) > limit {
		out = out[len(out)-limit:] // 直近 limit 件（新しい側を残す）
	}
	return out, nil
}

// expireChat は 1 行を期限切れにする（TTL の検証用）。
func (m *memStore) expireChat(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.chat {
		if m.chat[i].item.ID == id {
			m.chat[i].expire = m.now().Add(-time.Minute)
		}
	}
}

const (
	testPC    = "pc-glasses-test"
	testToken = "0123456789abcdef0123456789abcdef" // 取得用（pull）トークン 32 文字
	// 投入用（answer）トークン。取得トークンとは別（32 文字）。
	testAnswerToken = "fedcba9876543210fedcba9876543210"
)

func newTestHandler(t *testing.T, st Store, registered bool) *httptest.Server {
	t.Helper()
	return newTestHandlerWith(t, st, registered, testAnswerToken)
}

// newTestHandlerWith は投入トークンを指定して作る（空/同一で無効化を検証する）。
func newTestHandlerWith(t *testing.T, st Store, registered bool, answerToken string) *httptest.Server {
	t.Helper()
	if st == nil {
		st = newMemStore()
	}
	reg := RegistryFunc(func(_ context.Context, pc string) (bool, error) {
		if pc == testPC {
			return registered, nil
		}
		return false, nil
	})
	h, err := NewHandler(st, reg, testToken, answerToken)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/glasses/", h)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// getJSONRaw はテスト外（goroutine 内）からも使えるエラー返し版
// （t.Fatalf はテスト goroutine 以外では使えないため）。
func getJSONRaw(ts *httptest.Server, path, token string) (int, map[string]any, error) {
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if err != nil {
		return 0, nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body, nil
}

func doGet(t *testing.T, ts *httptest.Server, path, token string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp, body
}

func TestValidation(t *testing.T) {
	for _, s := range []string{"r-abc", "t-1792301670", "r-a-b-c-1"} {
		if !ValidRequestID(s) {
			t.Fatalf("ValidRequestID(%q)=false, want true", s)
		}
	}
	for _, s := range []string{"", "abc", "x-abc", "R-abc", "r-", "r-ABC", "r-" + strings.Repeat("a", 49), "r-a b"} {
		if ValidRequestID(s) {
			t.Fatalf("ValidRequestID(%q)=true, want false", s)
		}
	}
	for _, s := range []string{"pc-a", "mac-studio-herdr", "a1"} {
		if !ValidPC(s) {
			t.Fatalf("ValidPC(%q)=false, want true", s)
		}
	}
	for _, s := range []string{"", "PC-A", "pc a", "pc_" + strings.Repeat("a", 64)} {
		if ValidPC(s) {
			t.Fatalf("ValidPC(%q)=true, want false", s)
		}
	}
	if !ValidText("あ") || !ValidText(strings.Repeat("a", MaxTextRunes)) {
		t.Fatal("ValidText: 正常値を弾いた")
	}
	if ValidText("") || ValidText(strings.Repeat("あ", MaxTextRunes+1)) {
		t.Fatal("ValidText: 異常値を通した")
	}
	if !strings.Contains(ValidatePut("PC-A", "r-a", "x").Error(), "pc") {
		t.Fatal("ValidatePut: pc の検査が効いていない")
	}
}

func TestClampAndParseWait(t *testing.T) {
	if ClampWait(-time.Second) != 0 || ClampWait(0) != 0 {
		t.Fatal("ClampWait: 負値/0 が 0 でない")
	}
	if ClampWait(100*time.Second) != MaxWait {
		t.Fatal("ClampWait: 上限が効いていない")
	}
	d, err := parseWaitSeconds("")
	if err != nil || d != 0 {
		t.Fatalf("未指定は 0 のはず: d=%v err=%v", d, err)
	}
	d, err = parseWaitSeconds("0.3")
	if err != nil || d != 300*time.Millisecond {
		t.Fatalf("0.3 → 300ms のはず: d=%v err=%v", d, err)
	}
	d, err = parseWaitSeconds("100")
	if err != nil || d != MaxWait {
		t.Fatalf("100 → MaxWait のはず: d=%v err=%v", d, err)
	}
	if _, err := parseWaitSeconds("abc"); err == nil {
		t.Fatal("不正な wait を通した")
	}
	if _, err := parseWaitSeconds("-1"); err == nil {
		t.Fatal("負の wait を通した")
	}
}

func TestNewHandlerFailsClosed(t *testing.T) {
	st := newMemStore()
	reg := RegistryFunc(func(context.Context, string) (bool, error) { return true, nil })
	if _, err := NewHandler(st, reg, "", ""); err == nil {
		t.Fatal("空トークンでハンドラが作れた")
	}
	if _, err := NewHandler(st, reg, strings.Repeat("a", MinTokenLen-1), ""); err == nil {
		t.Fatal("短いトークンでハンドラが作れた")
	}
	if _, err := NewHandler(st, reg, strings.Repeat("a", MinTokenLen), testAnswerToken); err != nil {
		t.Fatalf("正しい長さのトークンで失敗: %v", err)
	}
	if _, err := NewHandler(nil, reg, testToken, ""); err == nil {
		t.Fatal("store nil でハンドラが作れた")
	}
	if _, err := NewHandler(st, nil, testToken, ""); err == nil {
		t.Fatal("registry nil でハンドラが作れた")
	}
}

func TestAuthRequired(t *testing.T) {
	ts := newTestHandler(t, nil, true)
	resp, _ := doGet(t, ts, AnswersPath+"?pc="+testPC, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("認証なし: %d, want 401", resp.StatusCode)
	}
	resp, _ = doGet(t, ts, AnswersPath+"?pc="+testPC, "wrong-token-wrong-token-wrong-token")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("誤トークン: %d, want 401", resp.StatusCode)
	}
	resp, _ = doGet(t, ts, AnswersPath+"?pc="+testPC, testToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("正トークン: %d, want 200", resp.StatusCode)
	}
}

func TestAnswersAndAck(t *testing.T) {
	st := newMemStore()
	ctx := context.Background()
	if _, err := st.Put(ctx, testPC, "r-1", "一つ目"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, testPC, "r-2", "二つ目"); err != nil {
		t.Fatal(err)
	}
	// 上書き（同一 id の再送は最新）
	if _, err := st.Put(ctx, testPC, "r-1", "一つ目（更新）"); err != nil {
		t.Fatal(err)
	}
	ts := newTestHandler(t, st, true)

	resp, body := doGet(t, ts, AnswersPath+"?pc="+testPC, testToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%v", resp.StatusCode, body)
	}
	items, _ := body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items=%d, want 2: %v", len(items), body)
	}
	// created_at は上書きで更新される（最新で上書き）ため順序ではなく
	// request_id で引いて本文を確認する。
	textsByID := map[string]string{}
	for _, x := range items {
		m := x.(map[string]any)
		rid, _ := m["request_id"].(string)
		txt, _ := m["text"].(string)
		textsByID[rid] = txt
		if _, ok := m["created_at"].(string); !ok {
			t.Fatalf("created_at が文字列でない: %v", m)
		}
	}
	if textsByID["r-1"] != "一つ目（更新）" || textsByID["r-2"] != "二つ目" {
		t.Fatalf("上書きが反映されていない: %v", textsByID)
	}

	// ack（存在しない id も成功扱い）
	ackBody := strings.NewReader(`{"pc":"` + testPC + `","ids":["r-1","r-999"]}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+AckPath, ackBody)
	req.Header.Set("Authorization", "Bearer "+testToken)
	ar, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer ar.Body.Close()
	if ar.StatusCode != http.StatusOK {
		t.Fatalf("ack status=%d", ar.StatusCode)
	}
	var ackOut map[string]any
	_ = json.NewDecoder(ar.Body).Decode(&ackOut)
	if ackOut["ok"] != true {
		t.Fatalf("ack 応答不正: %v", ackOut)
	}

	_, body = doGet(t, ts, AnswersPath+"?pc="+testPC, testToken)
	items, _ = body["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["request_id"] != "r-2" {
		t.Fatalf("ack 後の items が不正: %v", body)
	}
}

func TestAnswersWaitDeliversWhenPutArrives(t *testing.T) {
	st := newMemStore()
	ts := newTestHandler(t, st, true)
	done := make(chan map[string]any, 1)
	start := time.Now()
	go func() {
		code, body, err := getJSONRaw(ts, AnswersPath+"?pc="+testPC+"&wait=2", testToken)
		if err != nil || code != http.StatusOK {
			done <- map[string]any{"__error": fmt.Sprintf("code=%d err=%v", code, err)}
			return
		}
		done <- body
	}()
	time.Sleep(200 * time.Millisecond)
	if _, err := st.Put(context.Background(), testPC, "r-wait", "待ってから届いた"); err != nil {
		t.Fatal(err)
	}
	select {
	case body := <-done:
		elapsed := time.Since(start)
		items, _ := body["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("items=%v", body)
		}
		if elapsed > 1500*time.Millisecond {
			t.Fatalf("待機中に届いたのに遅い: %v", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("待機中の Put を拾えなかった")
	}
}

func TestAnswersWaitTimesOutEmpty(t *testing.T) {
	ts := newTestHandler(t, nil, true)
	start := time.Now()
	resp, body := doGet(t, ts, AnswersPath+"?pc="+testPC+"&wait=0.3", testToken)
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	items, _ := body["items"].([]any)
	if len(items) != 0 {
		t.Fatalf("items=%v, want 空", body)
	}
	if elapsed < 250*time.Millisecond {
		t.Fatalf("wait を無視して即返した: %v", elapsed)
	}
}

func TestExpiredExcluded(t *testing.T) {
	st := newMemStore()
	ctx := context.Background()
	if _, err := st.Put(ctx, testPC, "r-old", "期限切れ"); err != nil {
		t.Fatal(err)
	}
	st.expireNow(testPC, "r-old")
	ts := newTestHandler(t, st, true)
	_, body := doGet(t, ts, AnswersPath+"?pc="+testPC, testToken)
	items, _ := body["items"].([]any)
	if len(items) != 0 {
		t.Fatalf("期限切れが返った: %v", body)
	}
}

func TestUnregisteredAndInvalidPC(t *testing.T) {
	ts := newTestHandler(t, nil, false) // 未登録を返す registry
	resp, _ := doGet(t, ts, AnswersPath+"?pc="+testPC, testToken)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("未登録 pc: %d, want 403", resp.StatusCode)
	}
	resp, _ = doGet(t, ts, AnswersPath+"?pc=BAD_PC", testToken)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("不正 pc: %d, want 400", resp.StatusCode)
	}
}

func TestMethodAndPath(t *testing.T) {
	ts := newTestHandler(t, nil, true)
	// POST /glasses/answers → 405
	req, _ := http.NewRequest(http.MethodPost, ts.URL+AnswersPath, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+testToken)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusMethodNotAllowed || r.Header.Get("Allow") != http.MethodGet {
		t.Fatalf("POST answers: %d allow=%q", r.StatusCode, r.Header.Get("Allow"))
	}
	// GET /glasses/answers/ack → 405
	resp, _ := doGet(t, ts, AckPath, testToken)
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != http.MethodPost {
		t.Fatalf("GET ack: %d allow=%q", resp.StatusCode, resp.Header.Get("Allow"))
	}
	// 未知パス → 404
	resp, _ = doGet(t, ts, "/glasses/nope", testToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("未知パス: %d, want 404", resp.StatusCode)
	}
}

func TestAckBodyTooLarge(t *testing.T) {
	ts := newTestHandler(t, nil, true)
	big := `{"pc":"` + testPC + `","ids":["` + strings.Repeat("x", 100*1024) + `"]}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+AckPath, strings.NewReader(big))
	req.Header.Set("Authorization", "Bearer "+testToken)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("64KB 超の body: %d, want 400", r.StatusCode)
	}
}

func TestRateLimited(t *testing.T) {
	ts := newTestHandler(t, nil, true)
	last := 0
	for i := 0; i < rateLimitPerMin+1; i++ {
		resp, _ := doGet(t, ts, AnswersPath+"?pc="+testPC, testToken)
		last = resp.StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("上限超過の status=%d, want 429", last)
	}
}

// --- 案β: HTTP の投入口（POST /glasses/answer） --------------------------------

// postAnswer は投入口へ 1 件投げる。
func postAnswer(t *testing.T, ts *httptest.Server, token string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+AnswerPath, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", AnswerPath, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestAnswerHTTPPutThenPull は正常系（投入 → 取得で見える）を確認する。
func TestAnswerHTTPPutThenPull(t *testing.T) {
	st := newMemStore()
	ts := newTestHandler(t, st, true)

	code, out := postAnswer(t, ts, testAnswerToken, map[string]any{
		"pc": testPC, "request_id": "r-http-1", "text": "HTTP から投入した回答",
	})
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%v", code, out)
	}
	if out["ok"] != true || out["pc"] != testPC || out["request_id"] != "r-http-1" || out["expires_at"] == nil {
		t.Fatalf("応答が不正: %v", out)
	}

	// 取得（pull トークン）で見える＝MCP 経路と同じ保存先
	_, body := doGet(t, ts, AnswersPath+"?pc="+testPC, testToken)
	items, _ := body["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["text"] != "HTTP から投入した回答" {
		t.Fatalf("取得できない: %v", body)
	}
}

// TestAnswerHTTPTokenSeparation は取得トークン/投入トークンが互いに使えない
// ことを確認する。
func TestAnswerHTTPTokenSeparation(t *testing.T) {
	ts := newTestHandler(t, nil, true)

	// 取得トークンで投入 → 401
	code, _ := postAnswer(t, ts, testToken, map[string]any{"pc": testPC, "request_id": "r-1", "text": "x"})
	if code != http.StatusUnauthorized {
		t.Fatalf("取得トークンで投入: %d, want 401", code)
	}
	// 投入トークン無しで投入 → 401
	code, _ = postAnswer(t, ts, "", map[string]any{"pc": testPC, "request_id": "r-1", "text": "x"})
	if code != http.StatusUnauthorized {
		t.Fatalf("トークン無しで投入: %d, want 401", code)
	}
	// 投入トークンで取得 → 401
	resp, _ := doGet(t, ts, AnswersPath+"?pc="+testPC, testAnswerToken)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("投入トークンで取得: %d, want 401", resp.StatusCode)
	}
	// 投入トークンで ack → 401
	b, _ := json.Marshal(map[string]any{"pc": testPC, "ids": []string{"r-1"}})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+AckPath, strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer "+testAnswerToken)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("投入トークンで ack: %d, want 401", r.StatusCode)
	}
}

// TestAnswerHTTPSameTokenDisabled は投入トークンが取得トークンと同一だと
// 投入口が無効（404）になることを確認する。
func TestAnswerHTTPSameTokenDisabled(t *testing.T) {
	ts := newTestHandlerWith(t, nil, true, testToken) // 同一トークン
	code, _ := postAnswer(t, ts, testToken, map[string]any{"pc": testPC, "request_id": "r-1", "text": "x"})
	if code != http.StatusNotFound {
		t.Fatalf("同一トークン時の投入口: %d, want 404", code)
	}
	// 取得系は無事
	resp, _ := doGet(t, ts, AnswersPath+"?pc="+testPC, testToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("取得系が壊れた: %d", resp.StatusCode)
	}
}

// TestAnswerHTTPDisabled は投入トークン未設定で 404 を確認する。
func TestAnswerHTTPDisabled(t *testing.T) {
	ts := newTestHandlerWith(t, nil, true, "")
	code, _ := postAnswer(t, ts, testToken, map[string]any{"pc": testPC, "request_id": "r-1", "text": "x"})
	if code != http.StatusNotFound {
		t.Fatalf("未設定時の投入口: %d, want 404", code)
	}
	// 短すぎるトークンも無効
	ts2 := newTestHandlerWith(t, nil, true, "short-token")
	code, _ = postAnswer(t, ts2, "short-token", map[string]any{"pc": testPC, "request_id": "r-1", "text": "x"})
	if code != http.StatusNotFound {
		t.Fatalf("短いトークン時の投入口: %d, want 404", code)
	}
	// 取得系は無事
	resp, _ := doGet(t, ts, AnswersPath+"?pc="+testPC, testToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("取得系が壊れた: %d", resp.StatusCode)
	}
}

// TestAnswerHTTPValidationAndRegistration は 400 / 403 を確認する。
func TestAnswerHTTPValidationAndRegistration(t *testing.T) {
	st := newMemStore()
	ts := newTestHandler(t, st, true)
	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"request_id 形式不正", map[string]any{"pc": testPC, "request_id": "BAD_ID", "text": "x"}, http.StatusBadRequest},
		{"text 空", map[string]any{"pc": testPC, "request_id": "r-1", "text": ""}, http.StatusBadRequest},
		{"text 長すぎ", map[string]any{"pc": testPC, "request_id": "r-1", "text": strings.Repeat("あ", MaxTextRunes+1)}, http.StatusBadRequest},
		{"pc 形式不正", map[string]any{"pc": "BAD_PC", "request_id": "r-1", "text": "x"}, http.StatusBadRequest},
		{"未登録 pc", map[string]any{"pc": "pc-not-registered", "request_id": "r-1", "text": "x"}, http.StatusForbidden},
	}
	for _, c := range cases {
		code, out := postAnswer(t, ts, testAnswerToken, c.body)
		if code != c.want {
			t.Fatalf("%s: code=%d want=%d body=%v", c.name, code, c.want, out)
		}
	}
	if len(st.snapshot(testPC)) != 0 {
		t.Fatal("検査エラー/未登録なのに保存された")
	}
}

// TestAnswerHTTPBodyTooLarge は 64KB 超の body を弾くことを確認する。
func TestAnswerHTTPBodyTooLarge(t *testing.T) {
	ts := newTestHandler(t, nil, true)
	big := map[string]any{"pc": testPC, "request_id": "r-1", "text": strings.Repeat("x", 100*1024)}
	code, _ := postAnswer(t, ts, testAnswerToken, big)
	if code != http.StatusBadRequest {
		t.Fatalf("64KB 超: %d, want 400", code)
	}
}

// TestAnswerHTTPAudit は監査に本文が入らないことを確認する。
func TestAnswerHTTPAudit(t *testing.T) {
	ts := newTestHandler(t, nil, true)
	// auditor を差し込む（ハンドラはテストから直接作れないので再構築）
	reg := RegistryFunc(func(_ context.Context, pc string) (bool, error) { return pc == testPC, nil })
	h, err := NewHandler(newMemStore(), reg, testToken, testAnswerToken)
	if err != nil {
		t.Fatal(err)
	}
	var got []AuditEntry
	h.SetAuditor(AuditorFunc(func(_ context.Context, e AuditEntry) error { got = append(got, e); return nil }))
	mux := http.NewServeMux()
	mux.Handle("/glasses/", h)
	ts2 := httptest.NewServer(mux)
	defer ts2.Close()

	code, _ := postAnswer(t, ts2, testAnswerToken, map[string]any{"pc": testPC, "request_id": "r-audit", "text": "本文シークレット"})
	if code != http.StatusOK {
		t.Fatalf("status=%d", code)
	}
	code, _ = postAnswer(t, ts2, testAnswerToken, map[string]any{"pc": testPC, "request_id": "BAD", "text": "本文シークレット"})
	if code != http.StatusBadRequest {
		t.Fatalf("status=%d", code)
	}
	if len(got) != 2 {
		t.Fatalf("監査件数=%d, want 2: %+v", len(got), got)
	}
	if got[0].Email != auditEmail || got[0].Tool != auditTool || got[0].ResultCode != "ok" || got[0].PC != testPC {
		t.Fatalf("監査内容が不正: %+v", got[0])
	}
	if got[1].ResultCode != "invalid_request" {
		t.Fatalf("失敗時の監査が不正: %+v", got[1])
	}
	want := AnswerDigest(testPC, "r-audit")
	if got[0].ArgsDigest != want {
		t.Fatalf("ArgsDigest=%q want=%q", got[0].ArgsDigest, want)
	}
	for _, e := range got {
		if strings.Contains(e.ArgsDigest, "本文シークレット") || strings.Contains(e.PC, "本文") {
			t.Fatal("監査に本文が入った")
		}
	}
	_ = ts
}

// ack の綴り違い（request_ids）や空の ids は 400 で弾く（黙って 200 にしない）。
func TestAckRejectsUnknownFieldsAndEmpty(t *testing.T) {
	ctx := context.Background()
	st := newMemStore()
	if _, err := st.Put(ctx, testPC, "r-keep", "残る"); err != nil {
		t.Fatal(err)
	}
	ts := newTestHandler(t, st, true)
	for _, body := range []string{
		`{"pc":"` + testPC + `","request_ids":["r-keep"]}`,
		`{"pc":"` + testPC + `","ids":[]}`,
		`{"pc":"` + testPC + `"}`,
	} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+AckPath, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testToken)
		req.Header.Set("Content-Type", "application/json")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("body=%s status=%d, want 400", body, r.StatusCode)
		}
	}
	items, err := st.Pull(ctx, testPC, 0)
	if err != nil || len(items) != 1 {
		t.Fatalf("r-keep が消えた: items=%v err=%v", items, err)
	}
}
