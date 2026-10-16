package mcp

// Phase 1 の読み取り系ツール（list_machines/list_sessions/get_session/
// list_commands）＋ wait_for_status（スコープは drover.input だが Phase 1 で
// 実装し尽くす。drover-cloud-mcp-design.md §5.1/§5.2）。
// 既存の web.apiPCs/apiDevices/apiSessions と同じ state.Client メソッド
// （ListPCs/ListSessions/PCVersion/PCRole）をそのまま使う＝実績のある経路の再利用。

import (
	"context"
	"encoding/json"
	"time"

	"github.com/4noha/drover-cloud/state"
)

// toolErr は MCP ツールのビジネスエラー（JSON-RPC プロトコルエラーとは別・
// isError:true の content として返す。drover-cloud-mcp-design.md §5）。
type toolErr struct {
	Code    string
	Message string
}

type toolFn func(ctx context.Context, st *state.Client, args json.RawMessage) (any, *toolErr)

type toolTier struct {
	scope     string
	rateClass string
	fn        toolFn
}

func toolDef(name, desc string, schema map[string]any) map[string]any {
	return map[string]any{
		"name": name, "description": desc, "inputSchema": schema,
		"annotations": map[string]any{
			"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true,
		},
	}
}

var toolRegistry = map[string]toolTier{
	"list_machines":   {scope: "drover.read", rateClass: "read", fn: toolListMachines},
	"list_sessions":   {scope: "drover.read", rateClass: "read", fn: toolListSessions},
	"get_session":     {scope: "drover.read", rateClass: "read", fn: toolGetSession},
	"list_commands":   {scope: "drover.read", rateClass: "read", fn: toolListCommands},
	"wait_for_status": {scope: "drover.input", rateClass: "input", fn: toolWaitForStatus},
}

var toolDefs = []map[string]any{
	toolDef("list_machines", "登録済み全PCの一覧（role/version/online/セッション数/作業中数）を返す。引数なし。",
		map[string]any{"type": "object", "properties": map[string]any{}}),
	toolDef("list_sessions", "指定PC（省略時は全PC）のセッション一覧を返す。",
		map[string]any{"type": "object", "properties": map[string]any{
			"pc":     map[string]any{"type": "string"},
			"agent":  map[string]any{"type": "string"},
			"status": map[string]any{"type": "string", "enum": []string{"idle", "working", "blocked", "done", "unknown"}},
		}}),
	toolDef("get_session", "指定セッション1件の詳細と直近命令5件を返す。",
		map[string]any{"type": "object", "required": []string{"pc", "sid"},
			"properties": map[string]any{
				"pc": map[string]any{"type": "string"}, "sid": map[string]any{"type": "string"},
			}}),
	toolDef("list_commands", "指定PCの遠隔命令監査を新しい順で返す（最大20件）。",
		map[string]any{"type": "object", "required": []string{"pc"},
			"properties": map[string]any{
				"pc": map[string]any{"type": "string"}, "limit": map[string]any{"type": "integer", "maximum": 20},
			}}),
	toolDef("wait_for_status", "指定セッションの agent_status が until のいずれかになるまで待つ（最大50秒。PCへ命令は送らない）。",
		map[string]any{"type": "object", "required": []string{"pc", "sid", "until"},
			"properties": map[string]any{
				"pc":        map[string]any{"type": "string"},
				"sid":       map[string]any{"type": "string"},
				"until":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"timeout_s": map[string]any{"type": "integer", "maximum": 50},
			}}),
}

func decodeArgs[T any](raw json.RawMessage) (T, bool) {
	var v T
	if len(raw) == 0 {
		return v, true
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, false
	}
	return v, true
}

// sessionAgentStatus/sessionAgent は session map[string]any から文字列フィールドを
// 安全に取り出す（herdr-drover daemon が書く実データは map[string]any の
// ゆるい形なので型アサーションで吸収する。state package 自身も画面解釈は
// しない＝ここも「文字列として渡す」以上のことはしない）。
func strField(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func toolListMachines(ctx context.Context, st *state.Client, _ json.RawMessage) (any, *toolErr) {
	pcs, err := st.ListPCs(ctx)
	if err != nil {
		return nil, &toolErr{Code: "server_error", Message: err.Error()}
	}
	out := make([]map[string]any, 0, len(pcs))
	for _, pc := range pcs {
		ss, _ := st.ListSessions(ctx, pc)
		working := 0
		for _, x := range ss {
			if strField(x, "agent_status") == "working" {
				working++
			}
		}
		ver, _ := st.PCVersion(ctx, pc)
		role, _ := st.PCRole(ctx, pc)
		if role == "" {
			role = "master"
		}
		lastSeen, online, _ := st.PCLastSeen(ctx, pc)
		out = append(out, map[string]any{
			"pc": pc, "role": role, "version": ver,
			"sessions": len(ss), "working": working,
			"online": online, "last_seen": lastSeen,
		})
	}
	return map[string]any{"machines": out}, nil
}

type listSessionsArgs struct {
	PC     string `json:"pc"`
	Agent  string `json:"agent"`
	Status string `json:"status"`
}

func toolListSessions(ctx context.Context, st *state.Client, raw json.RawMessage) (any, *toolErr) {
	a, ok := decodeArgs[listSessionsArgs](raw)
	if !ok {
		return nil, &toolErr{Code: "invalid_request", Message: "引数の解析に失敗"}
	}
	var pcs []string
	if a.PC != "" {
		pcs = []string{a.PC}
	} else {
		var err error
		pcs, err = st.ListPCs(ctx)
		if err != nil {
			return nil, &toolErr{Code: "server_error", Message: err.Error()}
		}
	}
	out := []map[string]any{}
	for _, pc := range pcs {
		ss, err := st.ListSessions(ctx, pc)
		if err != nil {
			continue
		}
		for _, x := range ss {
			if a.Agent != "" && strField(x, "agent") != a.Agent {
				continue
			}
			if a.Status != "" && strField(x, "agent_status") != a.Status {
				continue
			}
			out = append(out, map[string]any{
				"pc": pc, "sid": strField(x, "key"), "agent": strField(x, "agent"),
				"agent_status": strField(x, "agent_status"), "cwd": strField(x, "cwd"),
				"short_dir": strField(x, "short_dir"), "window_name": strField(x, "window_name"),
				"updated_at": strField(x, "updated_at"),
			})
		}
	}
	return map[string]any{"sessions": out}, nil
}

type pcSIDArgs struct {
	PC  string `json:"pc"`
	SID string `json:"sid"`
}

func toolGetSession(ctx context.Context, st *state.Client, raw json.RawMessage) (any, *toolErr) {
	a, ok := decodeArgs[pcSIDArgs](raw)
	if !ok || a.PC == "" || a.SID == "" {
		return nil, &toolErr{Code: "invalid_request", Message: "pc/sid が必要"}
	}
	ss, err := st.ListSessions(ctx, a.PC)
	if err != nil {
		return nil, &toolErr{Code: "server_error", Message: err.Error()}
	}
	var found map[string]any
	for _, x := range ss {
		if strField(x, "key") == a.SID {
			found = x
			break
		}
	}
	if found == nil {
		return nil, &toolErr{Code: "not_found", Message: "セッションが見つかりません"}
	}
	cmds, _ := st.RecentCommands(ctx, a.PC, 5)
	recent := make([]map[string]any, 0, len(cmds))
	for _, c := range cmds {
		if c.SID != "" && c.SID != a.SID {
			continue
		}
		recent = append(recent, map[string]any{
			"id": c.ID, "cmd": c.Cmd, "status": c.Status, "detail": c.Detail, "ts": c.TS,
		})
	}
	return map[string]any{
		"pc": a.PC, "sid": a.SID, "agent": strField(found, "agent"),
		"agent_status": strField(found, "agent_status"), "cwd": strField(found, "cwd"),
		"short_dir": strField(found, "short_dir"), "window_name": strField(found, "window_name"),
		"updated_at": strField(found, "updated_at"), "recent_commands": recent,
	}, nil
}

type listCommandsArgs struct {
	PC    string `json:"pc"`
	Limit int    `json:"limit"`
}

func toolListCommands(ctx context.Context, st *state.Client, raw json.RawMessage) (any, *toolErr) {
	a, ok := decodeArgs[listCommandsArgs](raw)
	if !ok || a.PC == "" {
		return nil, &toolErr{Code: "invalid_request", Message: "pc が必要"}
	}
	limit := a.Limit
	if limit <= 0 || limit > 20 {
		limit = 20
	}
	cmds, err := st.RecentCommands(ctx, a.PC, limit)
	if err != nil {
		return nil, &toolErr{Code: "server_error", Message: err.Error()}
	}
	out := make([]map[string]any, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, map[string]any{
			"id": c.ID, "cmd": c.Cmd, "sid": c.SID, "agent": c.Agent,
			"status": c.Status, "detail": c.Detail, "requested_by": c.RequestedBy,
			"source": c.Source, "ts": c.TS, "finished_at": c.FinishedAt,
		})
	}
	return map[string]any{"commands": out}, nil
}

type waitForStatusArgs struct {
	PC       string   `json:"pc"`
	SID      string   `json:"sid"`
	Until    []string `json:"until"`
	TimeoutS int      `json:"timeout_s"`
}

// toolWaitForStatus は PC へ命令を送らず、Cloud Run 側で session doc の
// snapshot listener を張って agent_status の変化を待つ（drover-cloud-mcp-
// design.md §5.2。「producer が変化時のみ書くので安価」＝WaitCommand と同じ
// listen-until-match パターンだが対象は sessions/{sid} ）。
func toolWaitForStatus(ctx context.Context, st *state.Client, raw json.RawMessage) (any, *toolErr) {
	a, ok := decodeArgs[waitForStatusArgs](raw)
	if !ok || a.PC == "" || a.SID == "" || len(a.Until) == 0 {
		return nil, &toolErr{Code: "invalid_request", Message: "pc/sid/until が必要"}
	}
	timeout := 45 * time.Second
	if a.TimeoutS > 0 {
		timeout = time.Duration(a.TimeoutS) * time.Second
		if timeout > 50*time.Second {
			timeout = 50 * time.Second
		}
	}
	want := map[string]bool{}
	for _, u := range a.Until {
		want[u] = true
	}
	status, waited, timedOut, err := st.WaitSessionStatus(ctx, a.PC, a.SID, want, timeout)
	if err != nil {
		return nil, &toolErr{Code: "server_error", Message: err.Error()}
	}
	return map[string]any{
		"agent_status": status, "waited_s": int(waited.Seconds()), "timed_out": timedOut,
	}, nil
}
