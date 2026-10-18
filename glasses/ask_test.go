package glasses

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testAskToken = "aaaabbbbccccddddeeeeffff00001111" // 問い合わせ用（32 文字・他と別）

// memAskStore は memStore に問い合わせ（glasses_asks）を足したテスト用ストア。
type memAskStore struct {
	*memStore
	amu  sync.Mutex
	asks map[string]*memAsk
}

type memAsk struct {
	answer    string
	expire    time.Time
	delivered bool
}

func newMemAskStore() *memAskStore {
	return &memAskStore{memStore: newMemStore(), asks: map[string]*memAsk{}}
}

func (m *memAskStore) RegisterAsk(_ context.Context, pc, rid string) (time.Time, error) {
	m.amu.Lock()
	defer m.amu.Unlock()
	exp := m.now().Add(m.ttl)
	m.asks[DocID(pc, rid)] = &memAsk{expire: exp}
	return exp, nil
}

func (m *memAskStore) AnswerAsk(_ context.Context, pc, rid, text string) (bool, time.Time, error) {
	m.amu.Lock()
	defer m.amu.Unlock()
	a, ok := m.asks[DocID(pc, rid)]
	if !ok || !a.expire.After(m.now()) {
		return false, time.Time{}, nil
	}
	a.answer = text
	a.expire = m.now().Add(m.ttl)
	return true, a.expire, nil
}

func (m *memAskStore) TakeAskReply(_ context.Context, pc, rid string) (string, bool, error) {
	m.amu.Lock()
	defer m.amu.Unlock()
	a, ok := m.asks[DocID(pc, rid)]
	if !ok || !a.expire.After(m.now()) {
		return "", false, ErrAskNotFound
	}
	if a.answer == "" {
		return "", false, nil
	}
	a.delivered = true
	return a.answer, true, nil
}

type fakeWebhook struct {
	mu     sync.Mutex
	calls  []string // request_id|text
	status int
	err    error
}

func (f *fakeWebhook) Send(_ context.Context, rid, text string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, rid+"|"+text)
	if f.err != nil {
		return f.status, f.err
	}
	return 200, nil
}

func testReg() Registry {
	return RegistryFunc(func(_ context.Context, pc string) (bool, error) { return pc == testPC, nil })
}

func newAskServer(t *testing.T, st *memAskStore, wh WebhookSender, defaultPC string) *httptest.Server {
	t.Helper()
	h, err := NewHandler(st, testReg(), testToken, testAnswerToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.EnableAsk(testAskToken, wh, defaultPC); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/glasses/", h)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func doReq(t *testing.T, method, url, token, body string) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, url, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(r.Body).Decode(&out)
	return r.StatusCode, out
}

func TestAskReplyRoundTrip(t *testing.T) {
	st := newMemAskStore()
	wh := &fakeWebhook{}
	ts := newAskServer(t, st, wh, testPC)

	// HUD と同じ body（pc なし・ts は数値）
	code, out := doReq(t, http.MethodPost, ts.URL+AskPath, testAskToken, `{"request_id":"r-abc-1234","text":"今何時？","ts":1792309741128}`)
	if code != http.StatusAccepted || out["queued"] != true || out["request_id"] != "r-abc-1234" {
		t.Fatalf("ask: %d %v", code, out)
	}
	if len(wh.calls) != 1 || wh.calls[0] != "r-abc-1234|今何時？" {
		t.Fatalf("webhook 呼び出し不正: %v", wh.calls)
	}

	// 未回答は 200 text:null
	code, out = doReq(t, http.MethodGet, ts.URL+ReplyPath+"?request_id=r-abc-1234", testAskToken, "")
	if code != http.StatusOK || out["text"] != nil || out["status"] != "pending" {
		t.Fatalf("pending: %d %v", code, out)
	}

	// 既存の投入口で回答 → relay 経由の問い合わせなので glasses_asks に振り分け
	code, out = doReq(t, http.MethodPost, ts.URL+AnswerPath, testAnswerToken, `{"pc":"`+testPC+`","request_id":"r-abc-1234","text":"16時です"}`)
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("answer: %d %v", code, out)
	}
	// LAN ブリッジの pull には出ない
	if items, _ := st.Pull(context.Background(), testPC, 0); len(items) != 0 {
		t.Fatalf("relay 経由の回答が pull に漏れた: %v", items)
	}
	code, out = doReq(t, http.MethodGet, ts.URL+ReplyPath+"?request_id=r-abc-1234", testAskToken, "")
	if code != http.StatusOK || out["text"] != "[r-abc-1234] 16時です" || out["status"] != "delivered" {
		t.Fatalf("reply: %d %v", code, out)
	}
	// 猶予内の再取得も同じ本文（取りこぼし対策）
	code, out = doReq(t, http.MethodGet, ts.URL+ReplyPath+"?pc="+testPC+"&request_id=r-abc-1234", testAskToken, "")
	if code != http.StatusOK || out["text"] != "[r-abc-1234] 16時です" {
		t.Fatalf("reply 再取得: %d %v", code, out)
	}
}

func TestAskGeneratesRequestID(t *testing.T) {
	ts := newAskServer(t, newMemAskStore(), &fakeWebhook{}, testPC)
	code, out := doReq(t, http.MethodPost, ts.URL+AskPath, testAskToken, `{"text":"hi"}`)
	rid, _ := out["request_id"].(string)
	if code != http.StatusAccepted || !ValidRequestID(rid) {
		t.Fatalf("自動 request_id: %d %v", code, out)
	}
	for i := 0; i < 50; i++ {
		if id := NewRequestID(); !ValidRequestID(id) {
			t.Fatalf("NewRequestID が形式外: %q", id)
		}
	}
}

func TestAnswerWithoutAskStillGoesToPull(t *testing.T) {
	st := newMemAskStore()
	ts := newAskServer(t, st, &fakeWebhook{}, testPC)
	code, _ := doReq(t, http.MethodPost, ts.URL+AnswerPath, testAnswerToken, `{"pc":"`+testPC+`","request_id":"r-lan-1","text":"LAN 経路"}`)
	if code != http.StatusOK {
		t.Fatalf("answer: %d", code)
	}
	items, _ := st.Pull(context.Background(), testPC, 0)
	if len(items) != 1 || items[0].RequestID != "r-lan-1" {
		t.Fatalf("LAN 経路の回答が pull に無い: %v", items)
	}
}

func TestAskAuth(t *testing.T) {
	ts := newAskServer(t, newMemAskStore(), &fakeWebhook{}, testPC)
	body := `{"request_id":"r-x-1","text":"a"}`
	for _, tok := range []string{"", testToken, testAnswerToken, "wrong-token-wrong-token-wrong-tok"} {
		if code, _ := doReq(t, http.MethodPost, ts.URL+AskPath, tok, body); code != http.StatusUnauthorized {
			t.Fatalf("ask tok=%q: %d, want 401", tok[:min(4, len(tok))], code)
		}
		if code, _ := doReq(t, http.MethodGet, ts.URL+ReplyPath+"?request_id=r-x-1", tok, ""); code != http.StatusUnauthorized {
			t.Fatalf("reply: %d, want 401", code)
		}
	}
	// 問い合わせトークンで既存の取得・投入は使えない
	if code, _ := doReq(t, http.MethodGet, ts.URL+AnswersPath+"?pc="+testPC, testAskToken, ""); code != http.StatusUnauthorized {
		t.Fatalf("ask トークンで answers: %d", code)
	}
	if code, _ := doReq(t, http.MethodPost, ts.URL+AnswerPath, testAskToken, `{"pc":"`+testPC+`","request_id":"r-x-1","text":"a"}`); code != http.StatusUnauthorized {
		t.Fatalf("ask トークンで answer: %d", code)
	}
	// メソッド違い
	if code, _ := doReq(t, http.MethodGet, ts.URL+AskPath, testAskToken, ""); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET ask: %d", code)
	}
}

func TestAskValidation(t *testing.T) {
	wh := &fakeWebhook{}
	ts := newAskServer(t, newMemAskStore(), wh, testPC)
	cases := []struct {
		body string
		want int
	}{
		{`{"request_id":"r-v-1","text":""}`, 400},
		{`{"request_id":"r-v-1","text":"` + strings.Repeat("あ", MaxTextRunes+1) + `"}`, 400},
		{`{"request_id":"BAD","text":"a"}`, 400},
		{`{"pc":"Bad PC","text":"a"}`, 400},
		{`{"pc":"no-such-pc","text":"a"}`, 403},
		{`not json`, 400},
	}
	for _, c := range cases {
		if code, _ := doReq(t, http.MethodPost, ts.URL+AskPath, testAskToken, c.body); code != c.want {
			t.Fatalf("body=%.40s: %d, want %d", c.body, code, c.want)
		}
	}
	if len(wh.calls) != 0 {
		t.Fatalf("不正入力で webhook が呼ばれた: %v", wh.calls)
	}
	if code, out := doReq(t, http.MethodGet, ts.URL+ReplyPath+"?request_id=r-unknown-1", testAskToken, ""); code != http.StatusNotFound || out["status"] != "not_found" {
		t.Fatalf("未登録 reply: %d %v", code, out)
	}
	if code, _ := doReq(t, http.MethodGet, ts.URL+ReplyPath+"?request_id=bad", testAskToken, ""); code != http.StatusBadRequest {
		t.Fatalf("不正 request_id: %d", code)
	}
}

func TestAskPCRequiredWithoutDefault(t *testing.T) {
	ts := newAskServer(t, newMemAskStore(), &fakeWebhook{}, "")
	if code, _ := doReq(t, http.MethodPost, ts.URL+AskPath, testAskToken, `{"text":"a"}`); code != http.StatusBadRequest {
		t.Fatalf("pc 無し・既定無し: %d, want 400", code)
	}
	if code, _ := doReq(t, http.MethodPost, ts.URL+AskPath, testAskToken, `{"pc":"`+testPC+`","text":"a"}`); code != http.StatusAccepted {
		t.Fatalf("pc 指定: %d", code)
	}
}

func TestAskWebhookFailure(t *testing.T) {
	ts := newAskServer(t, newMemAskStore(), &fakeWebhook{status: 500, err: errors.New("x")}, testPC)
	code, out := doReq(t, http.MethodPost, ts.URL+AskPath, testAskToken, `{"request_id":"r-w-1","text":"a"}`)
	if code != http.StatusBadGateway || out["error"] != "webhook failed" {
		t.Fatalf("webhook 失敗: %d %v", code, out)
	}
}

func TestAskDisabledIs404(t *testing.T) {
	ts := newTestHandler(t, newMemAskStore(), true) // EnableAsk しない
	if code, _ := doReq(t, http.MethodPost, ts.URL+AskPath, testAskToken, `{"text":"a"}`); code != http.StatusNotFound {
		t.Fatalf("無効時 ask: %d", code)
	}
	if code, _ := doReq(t, http.MethodGet, ts.URL+ReplyPath+"?request_id=r-a-1", testAskToken, ""); code != http.StatusNotFound {
		t.Fatalf("無効時 reply: %d", code)
	}
}

func TestEnableAskRejects(t *testing.T) {
	mk := func(st Store) *Handler {
		h, err := NewHandler(st, testReg(), testToken, testAnswerToken)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	wh := &fakeWebhook{}
	if err := mk(newMemAskStore()).EnableAsk("short", wh, testPC); err == nil {
		t.Fatal("短いトークンを受け付けた")
	}
	if err := mk(newMemAskStore()).EnableAsk(testToken, wh, testPC); err == nil {
		t.Fatal("取得トークンと同一を受け付けた")
	}
	if err := mk(newMemAskStore()).EnableAsk(testAnswerToken, wh, testPC); err == nil {
		t.Fatal("投入トークンと同一を受け付けた")
	}
	if err := mk(newMemStore()).EnableAsk(testAskToken, wh, testPC); err == nil {
		t.Fatal("AskStore でないストアを受け付けた")
	}
	if err := mk(newMemAskStore()).EnableAsk(testAskToken, nil, testPC); err == nil {
		t.Fatal("webhook 無しを受け付けた")
	}
}

func TestHTTPWebhook(t *testing.T) {
	if _, err := NewWebhook("http://example.com/x", "k"); err == nil {
		t.Fatal("http を受け付けた")
	}
	if _, err := NewWebhook("https://example.com/x", ""); err == nil {
		t.Fatal("空キーを受け付けた")
	}
	var got map[string]string
	var auth string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	ws, err := NewWebhook(srv.URL+"/hook", "secret-key")
	if err != nil {
		t.Fatal(err)
	}
	ws.(*httpWebhook).client = srv.Client()
	status, err := ws.Send(context.Background(), "r-h-1", "本文")
	if err != nil || status != 200 {
		t.Fatalf("send: %d %v", status, err)
	}
	if auth != "Bearer secret-key" || got["request_id"] != "r-h-1" || got["text"] != "本文" || got["ts"] == "" {
		t.Fatalf("ペイロード不正: auth=%q got=%v", auth, got)
	}
	if _, err := time.Parse(time.RFC3339Nano, got["ts"]); err != nil {
		t.Fatalf("ts が ISO でない: %v", got["ts"])
	}
}
