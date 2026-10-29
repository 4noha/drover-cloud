package state

// 遠隔命令チャネル（owner 限定＋実行前確認＋監査）。WatchWake と同系の
// 「常時・無料の制御線」。relay/byte tunnel は無改変（不変条件死守）＝
// 命令は Firestore commands/{pc}/q/{id} 経由のみ。near-$0: 人手起因の
// 稀イベントで数書込/命令。claim transaction で再配信時の二重実行を防ぐ。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"cloud.google.com/go/firestore"
)

// Command は遠隔命令の1件。status: pending→running→done|error。
// json タグは devices.js（c.cmd/c.requested_by 等 snake_case）と一致
// させること＝API の契約。firestore タグと同名（保存/表示で一貫）。
type Command struct {
	ID          string `firestore:"id" json:"id"`
	Cmd         string `firestore:"cmd" json:"cmd"`
	SID         string `firestore:"sid" json:"sid"`
	RequestedBy string `firestore:"requested_by" json:"requested_by"`
	TS          string `firestore:"ts" json:"ts"`
	Status      string `firestore:"status" json:"status"`
	Detail      string `firestore:"detail" json:"detail"`
	FinishedAt  string `firestore:"finished_at" json:"finished_at"`
	// Agent はコーディングエージェント種別（canonical label。"claude"/"codex" 等）。
	// **空 = その PC の全エージェント**（SID 空と同型）。旧 agent が投入した doc には
	// このキーが無いので必ず空文字に degrade する＝後方互換。
	//
	// ⚠struct タグだけでは Firestore に書かれない。PushCommand の **map リテラルにも
	// "agent" を足すこと**（Set は map をそのまま書くのでタグを見ない）。
	Agent string `firestore:"agent" json:"agent"`
	// Args は命令固有の引数（MCP 由来の新命令のみ使用。旧命令は空）。値は文字列に
	// 限定（Firestore map<string,string>・JSON 以外の型を持ち込ませない）。サイズ上限は
	// argsMaxBytes（PushCommandArgs で検証）。旧 daemon は未知フィールドを単に無視する
	// （DataTo の標準動作）ので後方互換。
	Args map[string]string `firestore:"args" json:"args,omitempty"`
	// Source は投入経路（"web" | "mcp" | "cli"）。監査用。空は旧 PushCommand 経由
	// （後方互換のため何かを推測して埋めない）。
	Source string `firestore:"source" json:"source,omitempty"`
}

// ValidCommands は許可コマンド（web/agent 双方で検証）。
var ValidCommands = map[string]bool{
	"restart-agent": true, // launchd 2 デーモン kickstart -k
	"self-update":   true, // selfupdate.Update→自己/monitor 再起動
	"restart-proxy": true, // 当該 claude proxy を --resume で再起動（Phase3）
	// claude セッション本体を会話ごと作り直して**新しい claude バイナリ**を
	// 掴ませる（exec 済みプロセスは ~/.local/bin/claude の symlink 張替えを
	// 追わない）。sid 指定＝その 1 枚／sid 空＝その PC のローカル claude pane
	// 全部。drover 側の実体は restartclaude.go（agent 自身は再起動しない＝
	// restart-agent とは別物）。
	"restart-claude": true,
	// claude 本体を最新へ更新し、そのままセッションへ反映する 1 コマンド
	// （claude update → restart-claude）。**self-update は herdr-drover 自身**の
	// 更新なのに対し、こちらは **claude 本体**の更新＝別物。更新が無くても
	// 再起動する（ディスクは最新だがセッションが旧版、を直すのが目的）。
	"update-claude": true,
	// Web のワンボタン。claude 更新＋セッション反映 → herdr-drover 自己更新 →
	// 自身の再起動、を agent 側で**逐次**実行する。self-update / update-claude /
	// restart-agent の 3 ボタンを 1 つに集約したもの（個別命令も allowlist には
	// 残す＝CLI や旧 UI・トラブルシュートから引き続き投げられる）。
	"update-all": true,
	// セミオート続行（herdr-drover autocontinue）の pane 単位 ON/OFF。sid 必須・
	// Args["on"]="1"/"0"（無ければ反転）。Web コンソールの ⟳ ボタンと devices の表示が
	// 使う。agent 側の実体は herdr-drover internal/commands の DoSemiAuto。
	"semi-auto": true,

	// ── 一般化後の命令名（v0.1.11〜）────────────────────────────────
	// 一般化に伴い "agent" という語がコーディングエージェントを指すようになった
	// ため、旧名は語彙が衝突する（restart-agent は **drover デーモン**の再起動）。
	// **旧名は alias として残置**する（allowlist から外すと、まだ更新していない
	// PC の Web/CLI から投げられなくなる＝先行デプロイの意味が消える）。
	//
	// restart-claude → restart-agent-session（agent 指定で任意のエージェント）
	"restart-agent-session": true,
	// update-claude → update-agent-cli（agent 本体 CLI の更新＋セッション反映）
	"update-agent-cli": true,
	// restart-agent → restart-daemon（**drover デーモン**の kickstart。語彙衝突の解消）
	"restart-daemon": true,

	// ── MCP 由来の画面 I/O 命令（v0.2.0〜）─────────────────────────────
	// drover-cloud-mcp-design.md Phase 0。ここまでの9種は agent 自身の
	// restart/update 専用だったが、以下は pane の中身を直接やり取りする
	// **新しい種類の命令**＝実装は herdr-drover 側 internal/commands/commands.go
	// の CommandRunner に DoReadPane 等の新 seam を足すところから（Phase 2）。
	// Phase 0 時点では allowlist に載せるのみで、daemon 側は未配線＝
	// 投入すると CommandRunner.handle() の default 節で status=error Ack される
	// （黙殺せず監査に残る・安全側）。
	"read-pane":           true, // pane.read（画面テキスト取得）
	"send-input":          true, // pane.send_text/send_keys（テキスト/キー入力）
	"interrupt":           true, // pane.send_keys C-c
	"start-agent-session": true, // 指定 cwd で新規エージェントセッション起動
	"close-pane":          true, // pane.close（確認必須）
	"organize":            true, // organize サブコマンド相当
}

// ValidAgents はコーディングエージェント種別の canonical label（herdr 0.7.4 の
// src/detect/mod.rs agent_label() から実ソース抽出した 21 種）。
//
// ⚠herdr-drover 側の internal/agentid.CanonicalLabels と**同じ集合**でなければ
// ならない。ここは「Web から投入できる値」の関門で、agent 側は同じ集合で
// pane を同定する。片方だけ増えると「投入できるが誰も拾わない命令」または
// 「拾えるのに投入できない」になる。
var ValidAgents = map[string]bool{
	"agy": true, "amp": true, "claude": true, "cline": true, "codex": true,
	"copilot": true, "cursor": true, "devin": true, "droid": true, "gemini": true,
	"grok": true, "hermes": true, "kilo": true, "kimi": true, "kiro": true,
	"maki": true, "mastracode": true, "omp": true, "opencode": true, "pi": true,
	"qodercli": true,
}

// CommandAliases は旧命令名 → 新命令名の写像。agent 側は新名で分岐すれば足りる。
// 旧名で投入された doc は agent="claude" 固定として解釈する（旧名は claude 専用
// だったので、これが唯一の正しい degrade）。
var CommandAliases = map[string]string{
	"restart-claude": "restart-agent-session",
	"update-claude":  "update-agent-cli",
	"restart-agent":  "restart-daemon",
}

// NormalizeCommand は投入された (cmd, agent) を新名へ正規化する。
// 旧名なら agent を "claude" に固定する（旧名は claude 専用だった＝推測ではなく事実）。
// 戻り値の legacy は「旧名で投入された」ことを示す（Ack detail に残すため）。
func NormalizeCommand(cmd, agent string) (newCmd, newAgent string, legacy bool) {
	if n, ok := CommandAliases[cmd]; ok {
		// restart-agent（デーモン再起動）は元々 agent 概念を持たない＝空のまま。
		if cmd != "restart-agent" {
			agent = "claude"
		}
		return n, agent, true
	}
	return cmd, agent, false
}

var errAlreadyClaimed = errors.New("already claimed")

func (c *Client) cmdCol(pc string) *firestore.CollectionRef {
	return c.fs.Collection("commands").Doc(pc).Collection("q")
}

// argsMaxBytes は Command.Args の合計サイズ上限（key+value の UTF-8 bytes 和）。
// Firestore の 1 doc 1MiB 上限に対し、十分小さく固定してコマンド doc が
// 肥大化しないようにする（drover-cloud-mcp-design.md §3.2）。
const argsMaxBytes = 16 * 1024

// validateArgs は Args のサイズとキーの健全性を検証する（鍵は空文字禁止・
// 値を含めた合計 UTF-8 bytes が argsMaxBytes 以内）。nil/空は常に合格
// （旧命令は Args を使わない）。
func validateArgs(args map[string]string) error {
	if len(args) == 0 {
		return nil
	}
	total := 0
	for k, v := range args {
		if k == "" {
			return fmt.Errorf("args に空キーがある")
		}
		total += len(k) + len(v)
		if total > argsMaxBytes {
			return fmt.Errorf("args 合計サイズが上限（%d bytes）超過", argsMaxBytes)
		}
	}
	return nil
}

// PushCommand は owner 認証済 web が遠隔命令を投入（status=pending）。
// requestedBy（ログイン email）を監査に残す。戻り値は命令 id。
// Args/Source は使わない薄いラッパ（既存呼び出し無改変。source を推測して
// 埋めない＝空のまま）。MCP 等 Args/Source が要る新規呼び出しは
// PushCommandArgs を直接使うこと。
func (c *Client) PushCommand(ctx context.Context, pc, cmd, sid, agent, requestedBy string) (string, error) {
	return c.PushCommandArgs(ctx, pc, cmd, sid, agent, requestedBy, "", nil)
}

// PushCommandArgs は Args/Source 付きで遠隔命令を投入する（MCP 由来の新命令が
// 使う。drover-cloud-mcp-design.md §3.2）。検証は PushCommand と同じ
// allowlist に加え、Args のサイズ/キー検証（validateArgs）を行う。
func (c *Client) PushCommandArgs(ctx context.Context, pc, cmd, sid, agent, requestedBy, source string,
	args map[string]string) (string, error) {
	if !ValidCommands[cmd] {
		return "", fmt.Errorf("未知のコマンド: %s", cmd)
	}
	if agent != "" && !ValidAgents[agent] {
		// 未知の agent を通すと、受け手が「知らないので全 agent 扱い」に degrade して
		// **意図より広い破壊**をしかねない。推測せず投入時点で撥ねる。
		return "", fmt.Errorf("未知のエージェント種別: %s", agent)
	}
	if err := validateArgs(args); err != nil {
		return "", err
	}
	var b [12]byte
	_, _ = rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	doc := map[string]any{
		"id": id, "cmd": cmd, "sid": sid, "agent": agent, "requested_by": requestedBy,
		"ts": time.Now().UTC().Format(time.RFC3339Nano), "status": "pending",
		"source": source,
	}
	if len(args) > 0 {
		doc["args"] = args
	}
	_, err := c.cmdCol(pc).Doc(id).Set(ctx, doc)
	return id, err
}

// WatchCommands は自 PC の pending 命令を realtime 監視（常時・無料）。
// fn は claim 成功（pending→running を transaction で1度だけ）した命令
// のみ受ける＝Snapshot 再配信や複数 agent でも二重実行しない。
func (c *Client) WatchCommands(ctx context.Context, fn func(Command)) error {
	return keepSubscribed(ctx, watchMaxLifetime, func(subCtx context.Context) (func() error, func()) {
		it := c.cmdCol(c.pcID).Where("status", "==", "pending").Snapshots(subCtx)
		pump := func() error {
			for {
				qs, err := it.Next()
				if err != nil {
					return err // 終端 → keepSubscribed が再購読（resident 死なない）
				}
				if qs == nil {
					continue
				}
				for _, ch := range qs.Changes {
					if ch.Kind == firestore.DocumentRemoved {
						continue
					}
					var cm Command
					if e := ch.Doc.DataTo(&cm); e != nil || cm.Status != "pending" {
						continue
					}
					if !c.claimCommand(ctx, cm.ID) {
						continue
					}
					cm.Status = "running"
					fn(cm)
				}
			}
		}
		return pump, func() { it.Stop() }
	})
}

// claimCommand は pending→running を transaction で1度だけ成功させる。
func (c *Client) claimCommand(ctx context.Context, id string) bool {
	ref := c.cmdCol(c.pcID).Doc(id)
	err := c.fs.RunTransaction(ctx,
		func(ctx context.Context, tx *firestore.Transaction) error {
			snap, err := tx.Get(ref)
			if err != nil {
				return err
			}
			if st, _ := snap.Data()["status"].(string); st != "pending" {
				return errAlreadyClaimed
			}
			return tx.Set(ref, map[string]any{"status": "running"},
				firestore.MergeAll)
		})
	return err == nil
}

// AckCommand は実行結果を監査として書き戻す（status=done|error）。
// agent が claim した命令にのみ呼ぶ。
func (c *Client) AckCommand(ctx context.Context, id, status, detail string) error {
	_, err := c.cmdCol(c.pcID).Doc(id).Set(ctx, map[string]any{
		"status": status, "detail": detail,
		"finished_at": time.Now().UTC().Format(time.RFC3339Nano),
	}, firestore.MergeAll)
	return err
}

// GetCommand は commands/{pc}/q/{id} の1件を読む（MCP の get_command_result
// 用。WaitCommand がタイムアウトした後に AI が後から取りに来る経路）。
// 不在は ok=false（エラーにしない＝ConsumePairing 等と同じ規律）。
func (c *Client) GetCommand(ctx context.Context, pc, id string) (Command, bool, error) {
	snap, err := c.cmdCol(pc).Doc(id).Get(ctx)
	if err != nil || !snap.Exists() {
		return Command{}, false, nil
	}
	var cm Command
	if e := snap.DataTo(&cm); e != nil {
		return Command{}, false, e
	}
	return cm, true, nil
}

// RecentCommands は監査表示用に新しい順 n 件（web 用）。
func (c *Client) RecentCommands(ctx context.Context, pc string, n int) ([]Command, error) {
	docs, err := c.cmdCol(pc).OrderBy("ts", firestore.Desc).
		Limit(n).Documents(ctx).GetAll()
	if err != nil {
		return nil, err
	}
	out := make([]Command, 0, len(docs))
	for _, d := range docs {
		var cm Command
		if d.DataTo(&cm) == nil {
			out = append(out, cm)
		}
	}
	return out, nil
}

// WaitCommand は commands/{pc}/q/{id} の doc snapshot を listen し、
// status が done|error になるまで待つ（MCP ハンドラの同期待ち。
// drover-cloud-mcp-design.md §3.2）。timeout 以内に終われば
// (Command, timedOut=false, nil)。超過時は (ゼロ値 Command, timedOut=true, nil)
// ＝呼び手は command_id を保持して get_command_result で後から取りに来られる
// （エラーではなく正常系の分岐として扱う）。doc が存在しない場合も timedOut
// 扱い（投入直後の反映遅延を考慮し、エラーにしない）。
func (c *Client) WaitCommand(ctx context.Context, pc, id string, timeout time.Duration) (Command, bool, error) {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	it := c.cmdCol(pc).Doc(id).Snapshots(wctx)
	defer it.Stop()
	for {
		snap, err := it.Next()
		if err != nil {
			if wctx.Err() != nil {
				return Command{}, true, nil // timeout/親 ctx キャンセル＝正常系
			}
			return Command{}, false, err
		}
		if snap == nil || !snap.Exists() {
			continue
		}
		var cm Command
		if e := snap.DataTo(&cm); e != nil {
			continue
		}
		if cm.Status == "done" || cm.Status == "error" {
			return cm, false, nil
		}
	}
}

// ── pc 明示版（relay が slave pc の代理でコマンドを扱う用。master 経路の
// WatchCommands/AckCommand（自 pcID 直読）は無改変。slave は SA レスなので
// Firestore に触れず、relay がこれらで仲介する＝wake の WatchWakeFor/PutRelayGrant
// と同じ additive パターン）。

// claimCommandFor は pc 明示版 claim（pending→running を transaction で 1 度だけ）。
func (c *Client) claimCommandFor(ctx context.Context, pc, id string) bool {
	ref := c.cmdCol(pc).Doc(id)
	err := c.fs.RunTransaction(ctx,
		func(ctx context.Context, tx *firestore.Transaction) error {
			snap, err := tx.Get(ref)
			if err != nil {
				return err
			}
			if st, _ := snap.Data()["status"].(string); st != "pending" {
				return errAlreadyClaimed
			}
			return tx.Set(ref, map[string]any{"status": "running"},
				firestore.MergeAll)
		})
	return err == nil
}

// ClaimPendingCommands は commands/{pc}/q の pending を全て claim（pending→running）
// して返す（relay の /slave/commands が一発クエリで拾う用）。claim 済みのみ返す＝
// 二重配信しない（master の WatchCommands と同じ claim 規律）。
func (c *Client) ClaimPendingCommands(ctx context.Context, pc string) ([]Command, error) {
	docs, err := c.cmdCol(pc).Where("status", "==", "pending").Documents(ctx).GetAll()
	if err != nil {
		return nil, err
	}
	out := make([]Command, 0, len(docs))
	for _, d := range docs {
		var cm Command
		if d.DataTo(&cm) != nil || cm.Status != "pending" {
			continue
		}
		if !c.claimCommandFor(ctx, pc, cm.ID) {
			continue
		}
		cm.Status = "running"
		out = append(out, cm)
	}
	return out, nil
}

// WatchPendingCommands は commands/{pc}/q に pending が現れたら notify を呼ぶ
// （**claim はしない**＝relay の long-poll hold 用。claim は handler が
// ClaimPendingCommands で行い、claim と配信を原子的に一致させる）。
func (c *Client) WatchPendingCommands(ctx context.Context, pc string, notify func()) error {
	return keepSubscribed(ctx, watchMaxLifetime, func(subCtx context.Context) (func() error, func()) {
		it := c.cmdCol(pc).Where("status", "==", "pending").Snapshots(subCtx)
		pump := func() error {
			for {
				qs, err := it.Next()
				if err != nil {
					return err
				}
				if qs == nil {
					continue
				}
				for _, ch := range qs.Changes {
					if ch.Kind == firestore.DocumentRemoved {
						continue
					}
					if st, _ := ch.Doc.Data()["status"].(string); st == "pending" {
						notify()
					}
				}
			}
		}
		return pump, func() { it.Stop() }
	})
}

// AckCommandFor は pc 明示版 Ack（relay が slave の実行結果を書き戻す用）。
func (c *Client) AckCommandFor(ctx context.Context, pc, id, status, detail string) error {
	_, err := c.cmdCol(pc).Doc(id).Set(ctx, map[string]any{
		"status": status, "detail": detail,
		"finished_at": time.Now().UTC().Format(time.RFC3339Nano),
	}, firestore.MergeAll)
	return err
}
