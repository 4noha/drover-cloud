// Package mcp は drover-cloud-mcp-design.md の MCP サーバー本体。
// Streamable HTTP（2025-06-18 仕様）を stateless モードで実装する:
// POST /mcp に JSON-RPC 2.0、応答は application/json の単発レスポンス
// （SSE ストリームは使わない）。Mcp-Session-Id は発行しない＝Cloud Run が
// どのインスタンスに振っても成立し、常時接続ゼロのまま near-$0 を保つ。
//
// 依存を増やさないため、公式 SDK は使わず initialize/ping/tools/list/
// tools/call だけの手書き JSON-RPC（§3.1 の代替案）。
package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/4noha/drover-cloud/glasses"
	"github.com/4noha/drover-cloud/oauth"
	"github.com/4noha/drover-cloud/state"
)

const protocolVersion = "2025-06-18"
const maxBodyBytes = 1 << 20 // 1MiB（tools/call の引数は小さい想定。read_pane 等の大きい結果は mcp_results 経由）

// Server は MCP サーバー本体。
type Server struct {
	st *state.Client
	as *oauth.Server
	rl *limiter
	// slaveAccess は slave（共用 PC）への read_pane/send_input/interrupt の
	// 許可範囲（env MCP_SLAVE_ACCESS。既定 "off"）。"read"=read_pane のみ、
	// "full"=両方。drover-cloud-mcp-design.md §4.3「共用PCには他人の作業や
	// 秘密が映りうる」。
	slaveAccess string
	// glasses は眼鏡の回答ストア（GLASSES_ENABLED=1 のときだけ SetGlasses で
	// 差し込まれる）。nil なら glasses_answer は tools/list に出ず、呼ばれても
	// not_found＝無効時の挙動は完全に従来どおり。
	glasses glasses.AnswerStore
}

func New(st *state.Client, as *oauth.Server, slaveAccess string) *Server {
	if slaveAccess == "" {
		slaveAccess = "off"
	}
	return &Server{st: st, as: as, rl: newLimiter(), slaveAccess: slaveAccess}
}

// slaveAllowed は pc の role が "slave" のとき need（"read"|"full"）が
// 許可されているかを判定する。slave でなければ無条件許可。
func (s *Server) slaveAllowed(ctx context.Context, pc, need string) (bool, error) {
	role, err := s.st.PCRole(ctx, pc)
	if err != nil {
		return false, err
	}
	if role != "slave" {
		return true, nil
	}
	switch s.slaveAccess {
	case "full":
		return true, nil
	case "read":
		return need == "read", nil
	default: // "off" または未知値は安全側で拒否
		return false, nil
	}
}

// Handler は Bearer 検証込みの /mcp ハンドラ（main.go が mux.Handle("/mcp", ...) する）。
func (s *Server) Handler() http.Handler {
	return s.as.RequireBearer(http.HandlerFunc(s.serveMCP))
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, rerr *rpcError) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: id, Result: result, Error: rerr})
}

func (s *Server) serveMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// GET /mcp は 405（Streamable HTTP の stateless モードは SSE を
		// 張らないため・drover-cloud-mcp-design.md §3.1）。
		http.Error(w, "POST のみ（Streamable HTTP stateless モード）", http.StatusMethodNotAllowed)
		return
	}
	vt, _ := oauth.TokenFromContext(r.Context())

	var req rpcRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeRPC(w, nil, nil, &rpcError{Code: -32700, Message: "parse error"})
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeRPC(w, req.ID, nil, &rpcError{Code: -32600, Message: "invalid request"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 55*time.Second) // wait_for_status の最大50秒+余裕
	defer cancel()

	switch req.Method {
	case "initialize":
		writeRPC(w, req.ID, map[string]any{
			"protocolVersion": protocolVersion,
			"serverInfo":      map[string]string{"name": "drover-cloud-mcp", "version": "0.1.0"},
			"capabilities":    map[string]any{"tools": map[string]any{}},
		}, nil)
	case "ping":
		writeRPC(w, req.ID, map[string]any{}, nil)
	case "tools/list":
		writeRPC(w, req.ID, map[string]any{"tools": s.toolList()}, nil)
	case "tools/call":
		s.handleToolsCall(ctx, w, req, vt)
	default:
		writeRPC(w, req.ID, nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method})
	}
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// toolList は tools/list の応答に載せる定義を返す。GLASSES 有効時だけ
// glasses_answer を足す（グローバルの toolDefs は書き換えない＝無効時は
// 従来と完全に同一の応答）。
func (s *Server) toolList() []map[string]any {
	if s.glasses == nil {
		return toolDefs
	}
	out := make([]map[string]any, 0, len(toolDefs)+1)
	out = append(out, toolDefs...)
	return append(out, glassesAnswerDef)
}

// writeToolResult/writeToolErrorResult は MCP ツール結果の形
// （content:[{type:"text",...}]、エラーは isError:true。JSON-RPC の
// error フィールドとは別物＝プロトコルエラーとツールのビジネスエラーを
// 混同しない。drover-cloud-mcp-design.md §5）。
func writeToolResult(w http.ResponseWriter, id json.RawMessage, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		writeRPC(w, id, nil, &rpcError{Code: -32603, Message: "internal error"})
		return
	}
	writeRPC(w, id, map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(b)}},
	}, nil)
}

func writeToolErrorResult(w http.ResponseWriter, id json.RawMessage, code, message string) {
	b, _ := json.Marshal(map[string]string{"code": code, "message": message})
	writeRPC(w, id, map[string]any{
		"isError": true,
		"content": []map[string]any{{"type": "text", "text": string(b)}},
	}, nil)
}

func (s *Server) handleToolsCall(ctx context.Context, w http.ResponseWriter, req rpcRequest, vt oauth.VerifiedToken) {
	var p toolCallParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeRPC(w, req.ID, nil, &rpcError{Code: -32602, Message: "invalid params"})
		return
	}
	tier, ok := toolRegistry[p.Name]
	if !ok {
		// GLASSES 有効時のみ存在するツール（グローバルの toolRegistry は
		// 書き換えない）。無効時は nil 判定で not_found のまま。
		tier, ok = s.glassesTier(p.Name)
	}
	if !ok {
		writeToolErrorResult(w, req.ID, "not_found", "unknown tool: "+p.Name)
		return
	}
	if !vt.HasScope(tier.scope) {
		writeToolErrorResult(w, req.ID, "forbidden", "スコープ "+tier.scope+" がありません")
		return
	}
	if !s.rl.allow(vt.Email, tier.rateClass) {
		writeToolErrorResult(w, req.ID, "rate_limited", "レート制限を超過しました（"+tier.rateClass+"）")
		return
	}

	result, terr := tier.fn(ctx, s, p.Arguments)

	// 監査は失敗しても本処理を止めない（鉄則⑤の精神＝silent にはしないが、
	// 監査書込みの一時障害でユーザー操作まで巻き込まない）。
	pc, sid := pcSIDFromArgs(p.Arguments)
	resultCode := "ok"
	if terr != nil {
		resultCode = terr.Code
	}
	_ = s.st.WriteMCPAudit(ctx, state.MCPAuditEntry{
		Email: vt.Email, Tool: p.Name, PC: pc, SID: sid,
		ArgsDigest: argsDigest(p.Arguments), ResultCode: resultCode,
	})

	if terr != nil {
		writeToolErrorResult(w, req.ID, terr.Code, terr.Message)
		return
	}
	writeToolResult(w, req.ID, result)
}
