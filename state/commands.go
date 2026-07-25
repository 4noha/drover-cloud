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

// PushCommand は owner 認証済 web が遠隔命令を投入（status=pending）。
// requestedBy（ログイン email）を監査に残す。戻り値は命令 id。
func (c *Client) PushCommand(ctx context.Context, pc, cmd, sid, agent, requestedBy string) (string, error) {
	if !ValidCommands[cmd] {
		return "", fmt.Errorf("未知のコマンド: %s", cmd)
	}
	if agent != "" && !ValidAgents[agent] {
		// 未知の agent を通すと、受け手が「知らないので全 agent 扱い」に degrade して
		// **意図より広い破壊**をしかねない。推測せず投入時点で撥ねる。
		return "", fmt.Errorf("未知のエージェント種別: %s", agent)
	}
	var b [12]byte
	_, _ = rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	_, err := c.cmdCol(pc).Doc(id).Set(ctx, map[string]any{
		"id": id, "cmd": cmd, "sid": sid, "agent": agent, "requested_by": requestedBy,
		"ts": time.Now().UTC().Format(time.RFC3339Nano), "status": "pending",
	})
	return id, err
}

// WatchCommands は自 PC の pending 命令を realtime 監視（常時・無料）。
// fn は claim 成功（pending→running を transaction で1度だけ）した命令
// のみ受ける＝Snapshot 再配信や複数 agent でも二重実行しない。
func (c *Client) WatchCommands(ctx context.Context, fn func(Command)) error {
	return keepSubscribed(ctx, func() (func() error, func()) {
		it := c.cmdCol(c.pcID).Where("status", "==", "pending").Snapshots(ctx)
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
	return keepSubscribed(ctx, func() (func() error, func()) {
		it := c.cmdCol(pc).Where("status", "==", "pending").Snapshots(ctx)
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
