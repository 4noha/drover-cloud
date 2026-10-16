package mcp

// Phase 2 入力系ツール（read_pane/send_input/interrupt/get_command_result）の
// 検証。herdr-drover 側の CommandRunner は実装していない（herdr-drover
// リポジトリ側で実herdrに対して検証済み）ため、ここでは Firestore の
// commands/{pc}/q を自分で WatchCommands して Ack する「偽 daemon」で代用する
// （実 Firestore エミュレータ・鉄則2）。offline/revoked/slave ゲートは
// state 側の実メソッド（TouchLastSeen/SetRevoked/RegisterSlavePCVersion）を
// 直接使って状態を作る。
//
// 注意: WatchCommands/AckCommand は呼び手の state.Client が**コンストラクタで
// 固定した pcID**を見る（pc 引数を取らない「自分宛」版）。mcp サーバー側の
// st（pcID="pc-mcp-test" 固定）とは別に、各テストの対象 pc にバインドした
// 専用クライアントを偽 daemon 用に作る必要がある（これを素通りしていた初版は
// 全ての偽 daemon が一切発火せず、全テストが 20 秒タイムアウトの pending に
// 落ちて気づかれないまま緑になっていた＝実装前に FAIL させて気づいた）。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/4noha/drover-cloud/state"
)

// startFakeDaemon は pc にバインドした専用クライアントで WatchCommands を
// 購読し handle で Ack する「偽 daemon」。返すクライアントは呼び手が
// PutCommandResult 等の直接操作にも使える。
func startFakeDaemon(t *testing.T, ctx context.Context, pc string, handle func(*state.Client, state.Command)) *state.Client {
	t.Helper()
	daemon, err := state.New(context.Background(), emuProject, pc)
	if err != nil {
		t.Fatalf("state.New(daemon pc=%s): %v", pc, err)
	}
	t.Cleanup(func() { daemon.Close() })
	go func() { _ = daemon.WatchCommands(ctx, func(cm state.Command) { handle(daemon, cm) }) }()
	time.Sleep(1500 * time.Millisecond) // listener attach（他テストと同じ余裕）
	return daemon
}

func TestToolReadPaneHappyPath(t *testing.T) {
	needEmu(t)
	st, _, ts := newTestEnv(t, "off")
	seedAccessToken(t, st, "tok-rp", []string{"drover.read"})
	const pc = "pc-readpane-ok"
	if err := st.TouchLastSeen(context.Background(), pc); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startFakeDaemon(t, ctx, pc, func(daemon *state.Client, cm state.Command) {
		if cm.Args["source"] != "visible" || cm.Args["lines"] != "10" {
			t.Errorf("args が渡っていない: %+v", cm.Args)
		}
		if err := daemon.PutCommandResult(context.Background(), cm.ID, "hello screen", false, 10*time.Minute); err != nil {
			t.Error(err)
		}
		_ = daemon.AckCommand(context.Background(), cm.ID, "done", "画面取得完了")
	})

	out, status := rpcCall(t, ts, "tok-rp", "tools/call", map[string]any{
		"name": "read_pane",
		"arguments": map[string]any{
			"pc": pc, "sid": "sid-1", "source": "visible", "lines": 10,
		},
	})
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	result, _ := out["result"].(map[string]any)
	if b, _ := result["isError"].(bool); b {
		t.Fatalf("read_pane が isError: %+v", result)
	}
	var parsed map[string]any
	_ = json.Unmarshal([]byte(firstContentText(t, result)), &parsed)
	if parsed["text"] != "hello screen" || parsed["truncated"] != false {
		t.Fatalf("read_pane の結果不正: %+v", parsed)
	}
}

// タイムアウト（偽 daemon を起こさない）→ {status:pending, command_id}。
// その後 get_command_result で後から取得できる。
func TestToolReadPanePendingThenGetCommandResult(t *testing.T) {
	needEmu(t)
	st, _, ts := newTestEnv(t, "off")
	seedAccessToken(t, st, "tok-pending", []string{"drover.read"})
	const pc = "pc-readpane-pending"
	if err := st.TouchLastSeen(context.Background(), pc); err != nil {
		t.Fatal(err)
	}
	// 誰も claim/Ack しない＝WaitCommand が実際にタイムアウトする
	// （inputCommandTTL=20s をこのテストだけ許容する）。
	out, status := rpcCall(t, ts, "tok-pending", "tools/call", map[string]any{
		"name":      "read_pane",
		"arguments": map[string]any{"pc": pc, "sid": "sid-1"},
	})
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	result, _ := out["result"].(map[string]any)
	var parsed map[string]any
	_ = json.Unmarshal([]byte(firstContentText(t, result)), &parsed)
	cmdID, _ := parsed["command_id"].(string)
	if parsed["status"] != "pending" || cmdID == "" {
		t.Fatalf("pending 応答が不正: %+v", parsed)
	}

	// 後から daemon が Ack したとして、get_command_result で取れることを確認
	// （pc にバインドした専用クライアントで書く＝実 daemon と同じ経路）。
	daemon, err := state.New(context.Background(), emuProject, pc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { daemon.Close() })
	if err := daemon.PutCommandResult(context.Background(), cmdID, "late result", false, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := daemon.AckCommand(context.Background(), cmdID, "done", "画面取得完了（遅延）"); err != nil {
		t.Fatal(err)
	}
	out, status = rpcCall(t, ts, "tok-pending", "tools/call", map[string]any{
		"name":      "get_command_result",
		"arguments": map[string]any{"pc": pc, "command_id": cmdID},
	})
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	result, _ = out["result"].(map[string]any)
	_ = json.Unmarshal([]byte(firstContentText(t, result)), &parsed)
	if parsed["status"] != "done" || parsed["result_text"] != "late result" {
		t.Fatalf("get_command_result の結果不正: %+v", parsed)
	}
}

func TestToolSendInputTextAndKeysExclusive(t *testing.T) {
	needEmu(t)
	st, _, ts := newTestEnv(t, "off")
	seedAccessToken(t, st, "tok-excl", []string{"drover.input"})

	for _, args := range []map[string]any{
		{"pc": "pc-x", "sid": "sid-1"},                                       // 両方無し
		{"pc": "pc-x", "sid": "sid-1", "text": "hi", "keys": []string{"Up"}}, // 両方
	} {
		out, status := rpcCall(t, ts, "tok-excl", "tools/call",
			map[string]any{"name": "send_input", "arguments": args})
		if status != 200 {
			t.Fatalf("status=%d", status)
		}
		result, _ := out["result"].(map[string]any)
		if b, _ := result["isError"].(bool); !b {
			t.Fatalf("text/keys 排他違反なのに isError=false: args=%+v result=%+v", args, result)
		}
	}
}

func TestToolSendInputHappyPathText(t *testing.T) {
	needEmu(t)
	st, _, ts := newTestEnv(t, "off")
	seedAccessToken(t, st, "tok-si", []string{"drover.input"})
	const pc = "pc-sendinput-ok"
	if err := st.TouchLastSeen(context.Background(), pc); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startFakeDaemon(t, ctx, pc, func(daemon *state.Client, cm state.Command) {
		if cm.Args["text"] != "echo hi" || cm.Args["submit"] != "true" {
			t.Errorf("args が想定外: %+v", cm.Args)
		}
		if cm.Args["expires_at"] == "" {
			t.Error("expires_at がツール側で自動設定されていない")
		}
		if _, err := time.Parse(time.RFC3339Nano, cm.Args["expires_at"]); err != nil {
			t.Errorf("expires_at が RFC3339Nano でない: %v", err)
		}
		_ = daemon.AckCommand(context.Background(), cm.ID, "done", "入力完了")
	})

	out, status := rpcCall(t, ts, "tok-si", "tools/call", map[string]any{
		"name":      "send_input",
		"arguments": map[string]any{"pc": pc, "sid": "sid-1", "text": "echo hi"},
	})
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	result, _ := out["result"].(map[string]any)
	if b, _ := result["isError"].(bool); b {
		t.Fatalf("send_input が isError: %+v", result)
	}
	var parsed map[string]any
	_ = json.Unmarshal([]byte(firstContentText(t, result)), &parsed)
	if parsed["status"] != "done" {
		t.Fatalf("send_input の結果不正: %+v", parsed)
	}
}

func TestToolSendInputHappyPathKeys(t *testing.T) {
	needEmu(t)
	st, _, ts := newTestEnv(t, "off")
	seedAccessToken(t, st, "tok-keys", []string{"drover.input"})
	const pc = "pc-sendinput-keys"
	if err := st.TouchLastSeen(context.Background(), pc); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startFakeDaemon(t, ctx, pc, func(daemon *state.Client, cm state.Command) {
		if cm.Args["keys"] != "Up,Up,Enter" {
			t.Errorf("keys がカンマ結合で渡っていない: %+v", cm.Args)
		}
		_ = daemon.AckCommand(context.Background(), cm.ID, "done", "キー入力完了")
	})

	out, status := rpcCall(t, ts, "tok-keys", "tools/call", map[string]any{
		"name": "send_input",
		"arguments": map[string]any{
			"pc": pc, "sid": "sid-1", "keys": []string{"Up", "Up", "Enter"},
		},
	})
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	result, _ := out["result"].(map[string]any)
	if b, _ := result["isError"].(bool); b {
		t.Fatalf("send_input(keys) が isError: %+v", result)
	}
	var parsed map[string]any
	_ = json.Unmarshal([]byte(firstContentText(t, result)), &parsed)
	if parsed["status"] != "done" {
		t.Fatalf("send_input(keys) の結果不正（pending のままなら偽daemonが発火していない）: %+v", parsed)
	}
}

func TestToolInterruptHappyPath(t *testing.T) {
	needEmu(t)
	st, _, ts := newTestEnv(t, "off")
	seedAccessToken(t, st, "tok-int", []string{"drover.input"})
	const pc = "pc-interrupt-ok"
	if err := st.TouchLastSeen(context.Background(), pc); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startFakeDaemon(t, ctx, pc, func(daemon *state.Client, cm state.Command) {
		if cm.Args["expires_at"] == "" {
			t.Error("interrupt にも expires_at が自動設定されるはず")
		}
		_ = daemon.AckCommand(context.Background(), cm.ID, "done", "中断完了")
	})

	out, status := rpcCall(t, ts, "tok-int", "tools/call", map[string]any{
		"name":      "interrupt",
		"arguments": map[string]any{"pc": pc, "sid": "sid-1"},
	})
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	result, _ := out["result"].(map[string]any)
	if b, _ := result["isError"].(bool); b {
		t.Fatalf("interrupt が isError: %+v", result)
	}
	var parsed map[string]any
	_ = json.Unmarshal([]byte(firstContentText(t, result)), &parsed)
	if parsed["status"] != "done" {
		t.Fatalf("interrupt の結果不正（pending のままなら偽daemonが発火していない）: %+v", parsed)
	}
}

// オフライン PC（last_seen 未書込）への入力系は既定 pc_offline で拒否。
// queue_if_offline:true で投入可（偽 daemon が Ack すれば done で返る）。
func TestToolPrecheckOfflineRejectedUnlessQueued(t *testing.T) {
	needEmu(t)
	st, _, ts := newTestEnv(t, "off")
	seedAccessToken(t, st, "tok-off", []string{"drover.read", "drover.input"})
	const pc = "pc-offline-test" // TouchLastSeen を一度も呼ばない＝常に offline

	out, status := rpcCall(t, ts, "tok-off", "tools/call", map[string]any{
		"name":      "read_pane",
		"arguments": map[string]any{"pc": pc, "sid": "sid-1"},
	})
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	result, _ := out["result"].(map[string]any)
	text := firstContentText(t, result)
	if b, _ := result["isError"].(bool); !b || !strings.Contains(text, "pc_offline") {
		t.Fatalf("offline なのに pc_offline で拒否されない: %+v", result)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startFakeDaemon(t, ctx, pc, func(daemon *state.Client, cm state.Command) {
		_ = daemon.PutCommandResult(context.Background(), cm.ID, "queued screen", false, 10*time.Minute)
		_ = daemon.AckCommand(context.Background(), cm.ID, "done", "画面取得完了")
	})

	out, status = rpcCall(t, ts, "tok-off", "tools/call", map[string]any{
		"name": "read_pane",
		"arguments": map[string]any{
			"pc": pc, "sid": "sid-1", "queue_if_offline": true,
		},
	})
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	result, _ = out["result"].(map[string]any)
	if b, _ := result["isError"].(bool); b {
		t.Fatalf("queue_if_offline:true なのに拒否された: %+v", result)
	}
	var parsed map[string]any
	_ = json.Unmarshal([]byte(firstContentText(t, result)), &parsed)
	if parsed["text"] != "queued screen" {
		t.Fatalf("queue_if_offline:true 経路で結果が届いていない（偽daemon未発火の疑い）: %+v", parsed)
	}
}

// ペアリング解除済み PC は forbidden（オンラインかどうかに関わらず）。
func TestToolPrecheckRevokedRejected(t *testing.T) {
	needEmu(t)
	st, _, ts := newTestEnv(t, "off")
	seedAccessToken(t, st, "tok-rvk", []string{"drover.read"})
	const pc = "pc-revoked-test"
	ctx := context.Background()
	if err := st.TouchLastSeen(ctx, pc); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRevoked(ctx, pc); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.ClearRevoked(ctx, pc) })

	out, status := rpcCall(t, ts, "tok-rvk", "tools/call", map[string]any{
		"name":      "read_pane",
		"arguments": map[string]any{"pc": pc, "sid": "sid-1"},
	})
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	result, _ := out["result"].(map[string]any)
	text := firstContentText(t, result)
	if b, _ := result["isError"].(bool); !b || !strings.Contains(text, "forbidden") {
		t.Fatalf("revoked PC なのに forbidden で拒否されない: %+v", result)
	}
}

// slave アクセスゲート: MCP_SLAVE_ACCESS="read" なら read_pane は許可・
// send_input は拒否。"off"（既定）ならどちらも拒否。"full" は両方許可。
func TestToolSlaveAccessGating(t *testing.T) {
	needEmu(t)
	const pc = "pc-slave-test"

	for _, tc := range []struct {
		slaveAccess  string
		readAllowed  bool
		inputAllowed bool
	}{
		{"off", false, false},
		{"read", true, false},
		{"full", true, true},
	} {
		t.Run(tc.slaveAccess, func(t *testing.T) {
			st, _, ts := newTestEnv(t, tc.slaveAccess)
			tok := "tok-slave-" + tc.slaveAccess
			seedAccessToken(t, st, tok, []string{"drover.read", "drover.input"})
			ctx := context.Background()
			if err := st.RegisterSlavePCVersion(ctx, pc, "v1"); err != nil {
				t.Fatal(err)
			}
			if err := st.TouchLastSeen(ctx, pc); err != nil {
				t.Fatal(err)
			}

			wctx, cancel := context.WithCancel(ctx)
			t.Cleanup(cancel)
			startFakeDaemon(t, wctx, pc, func(daemon *state.Client, cm state.Command) {
				if cm.Cmd == "read-pane" {
					_ = daemon.PutCommandResult(context.Background(), cm.ID, "slave screen", false, 10*time.Minute)
				}
				_ = daemon.AckCommand(context.Background(), cm.ID, "done", "ok")
			})

			out, status := rpcCall(t, ts, tok, "tools/call", map[string]any{
				"name":      "read_pane",
				"arguments": map[string]any{"pc": pc, "sid": "sid-1"},
			})
			if status != 200 {
				t.Fatalf("status=%d", status)
			}
			result, _ := out["result"].(map[string]any)
			isErr, _ := result["isError"].(bool)
			if tc.readAllowed && isErr {
				t.Fatalf("read_pane が許可されるはずなのに拒否: %+v", result)
			}
			if !tc.readAllowed && !isErr {
				t.Fatalf("read_pane が拒否されるはずなのに許可: %+v", result)
			}

			out, status = rpcCall(t, ts, tok, "tools/call", map[string]any{
				"name":      "send_input",
				"arguments": map[string]any{"pc": pc, "sid": "sid-1", "text": "hi"},
			})
			if status != 200 {
				t.Fatalf("status=%d", status)
			}
			result, _ = out["result"].(map[string]any)
			isErr, _ = result["isError"].(bool)
			if tc.inputAllowed && isErr {
				t.Fatalf("send_input が許可されるはずなのに拒否: %+v", result)
			}
			if !tc.inputAllowed && !isErr {
				t.Fatalf("send_input が拒否されるはずなのに許可: %+v", result)
			}
		})
	}
}
