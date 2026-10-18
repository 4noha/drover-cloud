//go:build !windows

package mcp

// GLASSES（眼鏡の回答受け口）の検証。無効時は tools/list が従来と完全一致
// し、呼ばれても not_found。有効時だけ glasses_answer が出て動く。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/4noha/drover-cloud/glasses"
	"github.com/4noha/drover-cloud/oauth"
	"github.com/4noha/drover-cloud/state"
	"github.com/4noha/drover-cloud/webauth"
)

// fakeGlasses は glasses.Store のうち mcp が使う部分（Put）を模す。
type fakeGlasses struct {
	mu            sync.Mutex
	pc, rid, text string
	n             int
}

func (f *fakeGlasses) Put(_ context.Context, pc, rid, text string) (time.Time, error) {
	if err := glasses.ValidatePut(pc, rid, text); err != nil {
		return time.Time{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pc, f.rid, f.text = pc, rid, text
	f.n++
	return time.Now().Add(glasses.DefaultTTL), nil
}

func (f *fakeGlasses) last() (string, string, string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pc, f.rid, f.text, f.n
}

func newGlassesEnv(t *testing.T, g glasses.AnswerStore) (*state.Client, *httptest.Server) {
	t.Helper()
	st, err := state.New(context.Background(), emuProject, "pc-mcp-glasses")
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	as := oauth.New(st, webauth.NewSigner("test-secret"), "test-client-id", testEmail, nil, "", "https://self.example.com")
	ms := New(st, as, "off")
	if g != nil {
		ms.SetGlasses(g)
	}
	ts := httptest.NewServer(ms.Handler())
	t.Cleanup(ts.Close)
	return st, ts
}

func isErrContains(t *testing.T, result map[string]any, want string) bool {
	t.Helper()
	if b, _ := result["isError"].(bool); !b {
		return false
	}
	return strings.Contains(firstContentText(t, result), want)
}

// TestToolsListDisabledIsUnchanged は GLASSES 無効時に tools/list が
// 従来（グローバルの toolDefs）と完全一致することを確認する。
func TestToolsListDisabledIsUnchanged(t *testing.T) {
	needEmu(t)
	st, ts := newGlassesEnv(t, nil)
	seedAccessToken(t, st, "tok-gl-off", []string{"drover.read"})

	out, status := rpcCall(t, ts, "tok-gl-off", "tools/list", nil)
	if status != http.StatusOK {
		t.Fatalf("tools/list status=%d", status)
	}
	res, _ := out["result"].(map[string]any)
	tools, _ := res["tools"].([]any)
	got, _ := json.Marshal(tools)
	want, _ := json.Marshal(toolDefs)
	if string(got) != string(want) {
		t.Fatalf("無効時の tools/list が従来と不一致:\n got=%s\nwant=%s", got, want)
	}
	for _, x := range tools {
		if m, _ := x.(map[string]any); m["name"] == glassesAnswerTool {
			t.Fatalf("無効なのに %s が tools/list に出た", glassesAnswerTool)
		}
	}
}

// TestToolsListEnabledAddsGlassesAnswer は有効時だけ 1 件増えることを確認する。
func TestToolsListEnabledAddsGlassesAnswer(t *testing.T) {
	needEmu(t)
	st, ts := newGlassesEnv(t, &fakeGlasses{})
	seedAccessToken(t, st, "tok-gl-on", []string{"drover.read"})

	out, status := rpcCall(t, ts, "tok-gl-on", "tools/list", nil)
	if status != http.StatusOK {
		t.Fatalf("tools/list status=%d", status)
	}
	res, _ := out["result"].(map[string]any)
	tools, _ := res["tools"].([]any)
	if len(tools) != len(toolDefs)+1 {
		t.Fatalf("tools 件数=%d, want %d", len(tools), len(toolDefs)+1)
	}
	last, _ := json.Marshal(tools[len(tools)-1])
	want, _ := json.Marshal(glassesAnswerDef)
	if string(last) != string(want) {
		t.Fatalf("glasses_answer の定義が違う:\n got=%s\nwant=%s", last, want)
	}
}

// TestGlassesAnswerNotFoundWhenDisabled は無効時に呼んでも not_found を確認する。
func TestGlassesAnswerNotFoundWhenDisabled(t *testing.T) {
	needEmu(t)
	st, ts := newGlassesEnv(t, nil)
	seedAccessToken(t, st, "tok-gl-off2", []string{"drover.input"})

	out, _ := rpcCall(t, ts, "tok-gl-off2", "tools/call", map[string]any{
		"name":      glassesAnswerTool,
		"arguments": map[string]any{"pc": "pc-a", "request_id": "r-a", "text": "x"},
	})
	result, _ := out["result"].(map[string]any)
	if !isErrContains(t, result, "not_found") {
		t.Fatalf("無効時の呼び出しが not_found でない: %+v", result)
	}
}

func TestGlassesAnswerToolCall(t *testing.T) {
	needEmu(t)
	fg := &fakeGlasses{}
	st, ts := newGlassesEnv(t, fg)
	ctx := context.Background()
	const pc = "pc-glasses-mcp"
	// pcs/{pc} を作る（登録済みの判定はコレクションの存在＝ListPCs と同じ）。
	if err := st.TouchLastSeen(ctx, pc); err != nil {
		t.Fatalf("TouchLastSeen: %v", err)
	}
	seedAccessToken(t, st, "tok-gl-in", []string{"drover.input"})
	seedAccessToken(t, st, "tok-gl-read", []string{"drover.read"})

	// 正常系
	out, status := rpcCall(t, ts, "tok-gl-in", "tools/call", map[string]any{
		"name": glassesAnswerTool,
		"arguments": map[string]any{
			"pc": pc, "request_id": "r-mvda0mhy-d5da", "text": "今は12時34分です。",
		},
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d", status)
	}
	result, _ := out["result"].(map[string]any)
	if b, _ := result["isError"].(bool); b {
		t.Fatalf("正常系が isError: %+v", result)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(firstContentText(t, result)), &parsed); err != nil {
		t.Fatalf("応答が JSON でない: %v", err)
	}
	if parsed["ok"] != true || parsed["pc"] != pc || parsed["request_id"] != "r-mvda0mhy-d5da" || parsed["expires_at"] == nil {
		t.Fatalf("応答が不正: %+v", parsed)
	}
	if gotPC, gotRID, gotText, n := fg.last(); gotPC != pc || gotRID != "r-mvda0mhy-d5da" || gotText != "今は12時34分です。" || n != 1 {
		t.Fatalf("ストアへ渡った値が不正: pc=%q rid=%q text=%q n=%d", gotPC, gotRID, gotText, n)
	}

	// 検査エラー（request_id 形式）
	out, _ = rpcCall(t, ts, "tok-gl-in", "tools/call", map[string]any{
		"name":      glassesAnswerTool,
		"arguments": map[string]any{"pc": pc, "request_id": "BAD_ID", "text": "x"},
	})
	result, _ = out["result"].(map[string]any)
	if !isErrContains(t, result, "invalid_request") {
		t.Fatalf("不正 request_id が invalid_request でない: %+v", result)
	}

	// 未登録 pc
	out, _ = rpcCall(t, ts, "tok-gl-in", "tools/call", map[string]any{
		"name":      glassesAnswerTool,
		"arguments": map[string]any{"pc": "pc-not-registered", "request_id": "r-a", "text": "x"},
	})
	result, _ = out["result"].(map[string]any)
	if !isErrContains(t, result, "forbidden") {
		t.Fatalf("未登録 pc が forbidden でない: %+v", result)
	}

	// スコープ不足（drover.read では input スコープが無い）
	out, _ = rpcCall(t, ts, "tok-gl-read", "tools/call", map[string]any{
		"name":      glassesAnswerTool,
		"arguments": map[string]any{"pc": pc, "request_id": "r-a", "text": "x"},
	})
	result, _ = out["result"].(map[string]any)
	if !isErrContains(t, result, "forbidden") {
		t.Fatalf("スコープ不足が forbidden でない: %+v", result)
	}
}
