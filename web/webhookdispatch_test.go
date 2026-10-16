package web

// slave の /slave/push 経由での Webhook 代理送信（webhookdispatch.go）を
// 実 Firestore エミュレータ＋実 HTTP（受信側も httptest）で検証する。
// 鉄則2: 旧コード（dispatchWebhooksForTransitions 呼び出し自体が無い版）
// では以下のテストは「受信ゼロ」で FAIL することを実装前に確認済み。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/4noha/drover-cloud/relay"
	"github.com/4noha/drover-cloud/webauth"
	"github.com/4noha/drover-cloud/webhook"
)

// webhookReceiver は受信した Payload を蓄積するテスト用サーバー。
type webhookReceiver struct {
	mu  sync.Mutex
	got []webhook.Payload
	srv *httptest.Server
	hdr string // 最後に受けた検証ヘッダの値
}

// newWebhookReceiver は https のテストサーバーを立てる（webhook.Validate が
// https を必須にしているため http では検証できない）。webhook.Send は
// http.DefaultClient を使うため、このサーバーの自己署名証明書を一時的に
// 信頼させてから元に戻す（t.Cleanup。このプロセス内の全テストは逐次実行
// ＝並行の他テストが同じ瞬間に別の実 HTTPS 先へ出て行くことは無い前提）。
func newWebhookReceiver(t *testing.T) *webhookReceiver {
	t.Helper()
	wr := &webhookReceiver{}
	wr.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p webhook.Payload
		_ = json.NewDecoder(r.Body).Decode(&p)
		wr.mu.Lock()
		wr.got = append(wr.got, p)
		wr.hdr = r.Header.Get("X-Test-Secret")
		wr.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(wr.srv.Close)

	origTransport := http.DefaultClient.Transport
	http.DefaultClient.Transport = wr.srv.Client().Transport
	t.Cleanup(func() { http.DefaultClient.Transport = origTransport })
	return wr
}

func (wr *webhookReceiver) count() int {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	return len(wr.got)
}

func (wr *webhookReceiver) last() webhook.Payload {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	return wr.got[len(wr.got)-1]
}

func waitCount(t *testing.T, wr *webhookReceiver, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if wr.count() >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("webhook 受信件数が %d に届かない（現在 %d）", n, wr.count())
}

func TestSlavePushDispatchesWebhookOnTransition(t *testing.T) {
	needEmu(t)
	sa, _ := testSAJSON(t)
	st := newState(t)
	rl := relay.NewServer()
	ws := New(rl, st, webauth.NewSigner("k"), "cid", allowEmail, fakeGV{}, emuProject, sa)
	ts := httptest.NewServer(ws.Handler())
	defer ts.Close()
	ctx := context.Background()
	const pc = "pwebhook-slave-herdr"
	const secret = "deadbeefcafef00d1234"
	if _, err := st.BindSlave(ctx, pc, sha256Hex(secret)); err != nil {
		t.Fatalf("BindSlave: %v", err)
	}
	_, tokResp := req(t, "POST", ts.URL+"/slave/token", "", map[string]any{"pc": pc, "secret": secret})
	tok, _ := tokResp["token"].(string)
	if tok == "" {
		t.Fatalf("token 空: %v", tokResp)
	}
	if code, _ := req(t, "POST", ts.URL+"/slave/register", tok, map[string]any{"agent_version": "v1"}); code != 200 {
		t.Fatal("register 失敗")
	}

	wr := newWebhookReceiver(t)
	if err := st.SetWebhookConfig(ctx, webhook.Config{
		Enabled: true, URL: wr.srv.URL, HeaderName: "X-Test-Secret", HeaderSecret: "whsecret",
	}); err != nil {
		t.Fatalf("SetWebhookConfig: %v", err)
	}

	sess := func(status string) map[string]any {
		return map[string]any{
			"key": "sidA", "session_id": "sidA", "agent_status": status,
			"window_name": "claude", "agent": "claude", "cwd": "/x/proj",
		}
	}

	// 1回目: working（初観測＝旧状態不明なので送らない。かつ working は
	// NotifyStates 対象外なので二重に送らない理由がある）。
	if code, _ := req(t, "POST", ts.URL+"/slave/push", tok, map[string]any{"sessions": []any{sess("working")}}); code != 200 {
		t.Fatal("push1 失敗")
	}
	time.Sleep(300 * time.Millisecond)
	if wr.count() != 0 {
		t.Fatalf("初回push（working）で送信された: %d件", wr.count())
	}

	// 2回目: working→done（既知の旧状態から新状態への遷移）＝送信される。
	if code, _ := req(t, "POST", ts.URL+"/slave/push", tok, map[string]any{"sessions": []any{sess("done")}}); code != 200 {
		t.Fatal("push2 失敗")
	}
	waitCount(t, wr, 1)
	p := wr.last()
	if p.PC != pc || p.SID != "sidA" || p.OldStatus != "working" || p.NewStatus != "done" ||
		p.WindowName != "claude" || p.Agent != "claude" || p.Cwd != "/x/proj" || p.At == "" {
		t.Fatalf("payload 不正: %+v", p)
	}
	if wr.hdr != "whsecret" {
		t.Fatalf("検証ヘッダが届いていない: %q", wr.hdr)
	}

	// 3回目: done→done（同一状態の重複）＝送信されない。
	if code, _ := req(t, "POST", ts.URL+"/slave/push", tok, map[string]any{"sessions": []any{sess("done")}}); code != 200 {
		t.Fatal("push3 失敗")
	}
	time.Sleep(300 * time.Millisecond)
	if wr.count() != 1 {
		t.Fatalf("同一状態（done→done）なのに再送信された: %d件", wr.count())
	}

	// 4回目: done→blocked（別の通知対象状態への遷移）＝再度送信される。
	if code, _ := req(t, "POST", ts.URL+"/slave/push", tok, map[string]any{"sessions": []any{sess("blocked")}}); code != 200 {
		t.Fatal("push4 失敗")
	}
	waitCount(t, wr, 2)
	if p2 := wr.last(); p2.OldStatus != "done" || p2.NewStatus != "blocked" {
		t.Fatalf("done→blocked の payload 不正: %+v", p2)
	}
}

// Webhook 無効（既定 OFF）なら slave push で何も送信されない。
func TestSlavePushNoDispatchWhenWebhookDisabled(t *testing.T) {
	needEmu(t)
	sa, _ := testSAJSON(t)
	st := newState(t)
	rl := relay.NewServer()
	ws := New(rl, st, webauth.NewSigner("k"), "cid", allowEmail, fakeGV{}, emuProject, sa)
	ts := httptest.NewServer(ws.Handler())
	defer ts.Close()
	ctx := context.Background()
	const pc = "pwebhook-slave-off-herdr"
	const secret = "deadbeefcafef00d5678"
	if _, err := st.BindSlave(ctx, pc, sha256Hex(secret)); err != nil {
		t.Fatalf("BindSlave: %v", err)
	}
	// 明示的に OFF へ（他テストの残留設定に依存しない）。
	if err := st.SetWebhookConfig(ctx, webhook.Config{}); err != nil {
		t.Fatal(err)
	}
	_, tokResp := req(t, "POST", ts.URL+"/slave/token", "", map[string]any{"pc": pc, "secret": secret})
	tok, _ := tokResp["token"].(string)
	if code, _ := req(t, "POST", ts.URL+"/slave/register", tok, map[string]any{"agent_version": "v1"}); code != 200 {
		t.Fatal("register 失敗")
	}

	wr := newWebhookReceiver(t)
	sess := map[string]any{"key": "sidB", "session_id": "sidB", "agent_status": "working"}
	req(t, "POST", ts.URL+"/slave/push", tok, map[string]any{"sessions": []any{sess}})
	sess["agent_status"] = "done"
	req(t, "POST", ts.URL+"/slave/push", tok, map[string]any{"sessions": []any{sess}})
	time.Sleep(300 * time.Millisecond)
	if wr.count() != 0 {
		t.Fatalf("Webhook OFF なのに送信された: %d件", wr.count())
	}
}
