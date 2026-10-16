//go:build !windows

package mcp

// MCP サーバーを実 Firestore エミュレータ + 実 HTTP（httptest）で検証する
// （鉄則2）。トークンは oauth パッケージの HTTP フローを経由せず、
// state.CreateOAuthToken で直接シードする（mcp パッケージが検証するのは
// 「oauth.Server.Verify が通った後の挙動」であって OAuth フロー自体は
// oauth パッケージ側で別途検証済みのため、ここでは焦点を絞る）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/4noha/drover-cloud/oauth"
	"github.com/4noha/drover-cloud/state"
	"github.com/4noha/drover-cloud/webauth"
)

const emuProject = "demo-cm-mcp"

var emuUp bool

func java21Bin() string {
	for _, d := range []string{
		"/opt/homebrew/opt/openjdk/bin",
		"/opt/homebrew/opt/openjdk@25/bin",
		"/opt/homebrew/opt/openjdk@21/bin",
	} {
		j := d + "/java"
		if fi, err := os.Stat(j); err == nil && !fi.IsDir() {
			out, _ := exec.Command(j, "-version").CombinedOutput()
			s := string(out)
			for _, v := range []string{"\"21", "\"22", "\"23", "\"24", "\"25", "\"26"} {
				if strings.Contains(s, v) {
					return d
				}
			}
		}
	}
	return ""
}

func freePort() int {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestMain(m *testing.M) {
	jbin := java21Bin()
	if _, err := exec.LookPath("gcloud"); err == nil && jbin != "" {
		port := freePort()
		host := fmt.Sprintf("127.0.0.1:%d", port)
		cmd := exec.Command("gcloud", "beta", "emulators", "firestore", "start",
			"--host-port="+host, "--quiet")
		cmd.Env = append(os.Environ(),
			"PATH="+jbin+":"+os.Getenv("PATH"), "CLOUDSDK_CORE_DISABLE_PROMPTS=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if cmd.Start() == nil {
			ready := false
			for i := 0; i < 80; i++ {
				if c, e := http.Get("http://" + host + "/"); e == nil {
					c.Body.Close()
					ready = true
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
			if ready {
				os.Setenv("FIRESTORE_EMULATOR_HOST", host)
				emuUp = true
				code := m.Run()
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				os.Exit(code)
			}
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	os.Exit(m.Run())
}

func needEmu(t *testing.T) {
	t.Helper()
	if !emuUp {
		t.Skip("Firestore emulator 不在（gcloud / Java21+ 無し）")
	}
}

const testEmail = "owner@example.com"

// sha256Hex は oauth.Server.Verify と同じハッシュ規則（plain sha256 hex）を
// テスト側でも使う（シードしたトークンが実装と同じ鍵で見つかるように）。
func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// seedAccessToken は state に直接 access token を書き込む（oauth の HTTP
// フローを経由しない近道。oauth パッケージ自体のテストは oauth_test.go が担保）。
func seedAccessToken(t *testing.T, st *state.Client, raw string, scopes []string) {
	t.Helper()
	if err := st.CreateOAuthToken(context.Background(), sha256Hex(raw), state.OAuthToken{
		Kind: "access", Email: testEmail, ClientID: "test-client", Scopes: scopes, Family: "fam-test",
		ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("seedAccessToken: %v", err)
	}
}

func newTestEnv(t *testing.T) (st *state.Client, as *oauth.Server, ts *httptest.Server) {
	t.Helper()
	var err error
	st, err = state.New(context.Background(), emuProject, "pc-mcp-test")
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	as = oauth.New(st, webauth.NewSigner("test-secret"), "test-client-id", testEmail, nil, "", "https://self.example.com")
	ms := New(st, as)
	ts = httptest.NewServer(ms.Handler())
	t.Cleanup(ts.Close)
	return st, as, ts
}

func rpcCall(t *testing.T, ts *httptest.Server, bearer string, method string, params any) (map[string]any, int) {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("rpcCall: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return out, resp.StatusCode
}

func TestUnauthenticatedRejected(t *testing.T) {
	needEmu(t)
	_, _, ts := newTestEnv(t)
	_, status := rpcCall(t, ts, "", "tools/list", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", status)
	}
}

func TestInitializePingToolsList(t *testing.T) {
	needEmu(t)
	st, _, ts := newTestEnv(t)
	seedAccessToken(t, st, "tok-basic", []string{"drover.read"})

	out, status := rpcCall(t, ts, "tok-basic", "initialize", map[string]any{})
	if status != 200 {
		t.Fatalf("initialize status=%d", status)
	}
	res, _ := out["result"].(map[string]any)
	if res["protocolVersion"] != protocolVersion {
		t.Fatalf("protocolVersion不一致: %+v", res)
	}

	out, status = rpcCall(t, ts, "tok-basic", "ping", nil)
	if status != 200 || out["result"] == nil {
		t.Fatalf("ping失敗: status=%d out=%+v", status, out)
	}

	out, status = rpcCall(t, ts, "tok-basic", "tools/list", nil)
	if status != 200 {
		t.Fatalf("tools/list status=%d", status)
	}
	res, _ = out["result"].(map[string]any)
	tools, _ := res["tools"].([]any)
	if len(tools) != 5 {
		t.Fatalf("tools 件数=%d, want 5: %+v", len(tools), tools)
	}
}

func TestToolsCallListMachines(t *testing.T) {
	needEmu(t)
	st, _, ts := newTestEnv(t)
	seedAccessToken(t, st, "tok-read", []string{"drover.read"})

	if _, err := st.PushStatus(context.Background(), []map[string]any{
		{"key": "sid1", "session_id": "sid1", "agent": "claude", "agent_status": "working", "cwd": "/x", "short_dir": "x"},
	}); err != nil {
		t.Fatalf("PushStatus: %v", err)
	}

	out, status := rpcCall(t, ts, "tok-read", "tools/call",
		map[string]any{"name": "list_machines", "arguments": map[string]any{}})
	if status != 200 {
		t.Fatalf("status=%d out=%+v", status, out)
	}
	result, _ := out["result"].(map[string]any)
	if b, _ := result["isError"].(bool); b {
		t.Fatalf("list_machines が isError: %+v", result)
	}
	text := firstContentText(t, result)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatalf("content text が JSON でない: %v / %s", err, text)
	}
	machines, _ := parsed["machines"].([]any)
	if len(machines) == 0 {
		t.Fatalf("machines が空: %s", text)
	}
	m0 := machines[0].(map[string]any)
	if m0["working"].(float64) != 1 {
		t.Fatalf("working カウント不正: %+v", m0)
	}
}

func TestToolsCallScopeForbidden(t *testing.T) {
	needEmu(t)
	st, _, ts := newTestEnv(t)
	seedAccessToken(t, st, "tok-noscope", []string{}) // スコープ無し

	out, status := rpcCall(t, ts, "tok-noscope", "tools/call",
		map[string]any{"name": "list_machines", "arguments": map[string]any{}})
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	result, _ := out["result"].(map[string]any)
	if b, _ := result["isError"].(bool); !b {
		t.Fatalf("スコープ無しなのに isError=false: %+v", result)
	}
}

func TestToolsCallUnknownTool(t *testing.T) {
	needEmu(t)
	st, _, ts := newTestEnv(t)
	seedAccessToken(t, st, "tok-x", []string{"drover.read"})

	out, status := rpcCall(t, ts, "tok-x", "tools/call",
		map[string]any{"name": "delete_everything", "arguments": map[string]any{}})
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	result, _ := out["result"].(map[string]any)
	if b, _ := result["isError"].(bool); !b {
		t.Fatalf("未知ツールなのに isError=false: %+v", result)
	}
}

func TestRateLimitTrips(t *testing.T) {
	needEmu(t)
	st, _, ts := newTestEnv(t)
	seedAccessToken(t, st, "tok-rl", []string{"drover.read"})

	hit429 := false
	for i := 0; i < rateLimits["read"]+5; i++ {
		out, status := rpcCall(t, ts, "tok-rl", "tools/call",
			map[string]any{"name": "list_machines", "arguments": map[string]any{}})
		if status != 200 {
			t.Fatalf("status=%d", status)
		}
		result, _ := out["result"].(map[string]any)
		if b, _ := result["isError"].(bool); b {
			text := firstContentText(t, result)
			if strings.Contains(text, "rate_limited") {
				hit429 = true
				break
			}
		}
	}
	if !hit429 {
		t.Fatal("read 上限を超えてもレート制限にならなかった")
	}
}

func TestWaitForStatusImmediateMatch(t *testing.T) {
	needEmu(t)
	st, _, ts := newTestEnv(t)
	seedAccessToken(t, st, "tok-wait", []string{"drover.input"})
	if _, err := st.PushStatus(context.Background(), []map[string]any{
		{"key": "sid-w1", "session_id": "sid-w1", "agent": "claude", "agent_status": "idle"},
	}); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	out, status := rpcCall(t, ts, "tok-wait", "tools/call", map[string]any{
		"name": "wait_for_status",
		"arguments": map[string]any{
			"pc": "pc-mcp-test", "sid": "sid-w1", "until": []string{"idle"}, "timeout_s": 10,
		},
	})
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("既に idle なのに即返らなかった: %v", elapsed)
	}
	result, _ := out["result"].(map[string]any)
	text := firstContentText(t, result)
	var parsed map[string]any
	_ = json.Unmarshal([]byte(text), &parsed)
	if parsed["timed_out"] != false || parsed["agent_status"] != "idle" {
		t.Fatalf("結果不正: %s", text)
	}
}

func firstContentText(t *testing.T, result map[string]any) string {
	t.Helper()
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("content が空: %+v", result)
	}
	c0, _ := content[0].(map[string]any)
	text, _ := c0["text"].(string)
	return text
}
