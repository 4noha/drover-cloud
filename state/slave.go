package state

// slave（共用 PC）用の state メソッド群。relay の state client は
// pcID="relay" の固定 1 本なので、所有 pc を **都度明示**する `*For`
// 変種と、slave 認証情報（slaves/{pc}）を扱う。master 経路の
// PushStatus/DeleteSession/OwnSessionKeys/PutRelayGrant/CheckRelayGrant/
// Wake/WatchWake/RegisterPC* は無改変（本ファイルは strictly additive）。

import (
	"context"
	"regexp"
	"time"

	"cloud.google.com/go/firestore"
)

// pcNameRe は pc 名の許容文字集合（英数字と `._-` のみ）。`:`（slaveGrantDocID
// の区切り）・NUL（slaveSessionKey の区切り）・`/`（Firestore doc id 不可）を
// 構造的に排除し、`pc:sid` doc id の別名衝突を根絶する。実 pc は `<host>-herdr`。
var pcNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// ValidPCName は pc 名が安全か（enroll/BindSlave の入口で強制）。exact-match
// の allowlist＝ヒューリスティックでない。これを通った pc のみ token 化される
// ので、以降の slaveGrantDocID/PushStatusFor 等は衝突不能な pc しか受け取らない。
func ValidPCName(pc string) bool { return pcNameRe.MatchString(pc) }

// PushStatusFor は PushStatus（state.go:95）と同一ロジックで、collection を
// pcs/{pc}/sessions に固定する（pc は呼び出し側＝relay が slave トークンから
// 導出した値。slave が別 pc を詐称できないよう pc は relay 側で固定される）。
// content_hash 据置ゲート・親 doc の MergeAll tail も PushStatus と同一。
func (c *Client) PushStatusFor(ctx context.Context, pc string, sessions []map[string]any) (changed int, err error) {
	col := c.fs.Collection("pcs").Doc(pc).Collection("sessions")
	for _, s := range sessions {
		id := sessionKey(s)
		h := contentHash(s)
		ref := col.Doc(id)
		ver := int64(1)
		snap, gerr := ref.Get(ctx)
		if gerr == nil && snap.Exists() {
			d := snap.Data()
			pv, _ := d["version"].(int64)
			ph, _ := d["content_hash"].(string)
			if ph == h {
				// 差分なし＝書かない（near-$0 維持）。
				continue
			}
			ver = pv + 1
			changed++
		} else {
			changed++
		}
		doc := map[string]any{}
		for k, v := range s {
			doc[k] = v
		}
		doc["version"] = ver
		doc["content_hash"] = h
		doc["updated_at"] = time.Now().UTC().Format(time.RFC3339)
		if _, werr := ref.Set(ctx, doc); werr != nil {
			return changed, werr
		}
	}
	// 変化があった時だけ親 doc を明示書込（端末一覧に出すため。PushStatus と
	// 同じ MergeAll＝agent_kind 再表明・他フィールド非破壊）。
	if changed > 0 {
		_, _ = c.fs.Collection("pcs").Doc(pc).Set(ctx, map[string]any{
			"id":         pc,
			"updated_at": time.Now().UTC().Format(time.RFC3339),
			"agent_kind": "herdr-drover",
		}, firestore.MergeAll)
	}
	return changed, nil
}

// DeleteSessionFor は pcs/{pc}/sessions/{key} を削除（DeleteSession の pc 明示版）。
func (c *Client) DeleteSessionFor(ctx context.Context, pc, key string) error {
	if key == "" {
		return nil
	}
	_, err := c.fs.Collection("pcs").Doc(pc).
		Collection("sessions").Doc(key).Delete(ctx)
	return err
}

// SessionKeysFor は pcs/{pc}/sessions/* の doc id 一覧（OwnSessionKeys の pc 明示版）。
// slave agent 起動時に producer の prev 集合を seed するのに使う。
func (c *Client) SessionKeysFor(ctx context.Context, pc string) ([]string, error) {
	docs, err := c.fs.Collection("pcs").Doc(pc).
		Collection("sessions").Documents(ctx).GetAll()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.Ref.ID)
	}
	return out, nil
}

// SessionOwnedBy は pcs/{pc}/sessions/{sid} が存在するか（slave の grant
// 所有権チェック用。slave は自分が push した sid にしか grant できない）。
func (c *Client) SessionOwnedBy(ctx context.Context, pc, sid string) bool {
	if pc == "" || sid == "" {
		return false
	}
	snap, err := c.fs.Collection("pcs").Doc(pc).
		Collection("sessions").Doc(sid).Get(ctx)
	return err == nil && snap != nil && snap.Exists()
}

// slaveGrantDocID は slavegrants/ の doc id。pc（＝`<host>-herdr`・コロン
// を含まない）と sid の組。master の relaygrants/{sid}:{role} とは**別
// コレクション**にすることで、slave は master の grant doc を一切上書き
// できない（grant-doc poisoning の構造的根絶）。pc にコロンが無いので
// `pc:sid` は (pc,sid) について曖昧なく一意。
func slaveGrantDocID(pc, sid string) string { return pc + ":" + sid }

// PutSlaveGrant は slave source が /session に接続してよい短命の許可を
// **slavegrants/{pc:sid}** に書く。pc 名前空間なので master の
// relaygrants（CheckRelayGrant が読む doc）には一切触れない＝slave が
// owner の sid を push→grant しても owner の grant を汚染できない。
// relay が token 由来の pc を強制刻印＝slave は自 pc の grant しか書けない。
func (c *Client) PutSlaveGrant(ctx context.Context, pc, sid string, ttl time.Duration) error {
	if pc == "" || sid == "" {
		return nil
	}
	_, err := c.fs.Collection("slavegrants").Doc(slaveGrantDocID(pc, sid)).
		Set(ctx, map[string]any{
			"pc": pc, "sid": sid,
			"exp": time.Now().Add(ttl).UTC().Format(time.RFC3339Nano),
		})
	return err
}

// SlaveGrantValid は slavegrants/{pc:sid} が期限内で存在するか（SlaveGate
// の source 認可 hot-path）。doc 無し/期限切れ/取得失敗は false（fail-closed）。
// master の relaygrants には一切アクセスしない＝相互汚染不能。失効判定は
// SlaveGate が別途 IsRevoked/SlaveRevoked で行う。
func (c *Client) SlaveGrantValid(ctx context.Context, pc, sid string) bool {
	if pc == "" || sid == "" {
		return false
	}
	snap, err := c.fs.Collection("slavegrants").
		Doc(slaveGrantDocID(pc, sid)).Get(ctx)
	if err != nil || snap == nil || !snap.Exists() {
		return false
	}
	es, _ := snap.Data()["exp"].(string)
	t, perr := time.Parse(time.RFC3339Nano, es)
	return perr == nil && time.Now().Before(t)
}

// WatchWakeFor は wake/{pc} を real-time 監視（WatchWake の pc 明示版）。
// slave 用の /slave/wake long-poll ハンドラが 1 回分の wake を捕まえるために使う。
func (c *Client) WatchWakeFor(ctx context.Context, pc string, cb func(sid string)) error {
	return keepSubscribed(ctx, func() (func() error, func()) {
		it := c.fs.Collection("wake").Doc(pc).Snapshots(ctx)
		pump := func() error {
			for {
				snap, err := it.Next()
				if err != nil {
					return err
				}
				if snap == nil || !snap.Exists() {
					continue
				}
				if sid, ok := snap.Data()["sid"].(string); ok && sid != "" {
					cb(sid)
				}
			}
		}
		return pump, func() { it.Stop() }
	})
}

// WakeDoc は wake/{pc} の現在値 (sid, ts) を返す（long-poll の catch-up:
// since より新しい wake が既にあれば即返す判定に使う）。doc 無し/sid 空は ok=false。
func (c *Client) WakeDoc(ctx context.Context, pc string) (sid, ts string, ok bool) {
	snap, err := c.fs.Collection("wake").Doc(pc).Get(ctx)
	if err != nil || snap == nil || !snap.Exists() {
		return "", "", false
	}
	d := snap.Data()
	sid, _ = d["sid"].(string)
	ts, _ = d["ts"].(string)
	return sid, ts, sid != ""
}

// RegisterSlavePCVersion は pcs/{pc} を slave マーカー付きで作成する
// （RegisterPCVersion の pc 明示 + role="slave" 版）。role は wsViewer の
// pairing key 名前空間切替と owner 側 reconcile 絞り込みに使う。
func (c *Client) RegisterSlavePCVersion(ctx context.Context, pc, agentVersion string) error {
	doc := map[string]any{
		"id":         pc,
		"updated_at": time.Now().UTC().Format(time.RFC3339),
		"agent_kind": "herdr-drover",
		"role":       "slave",
	}
	if agentVersion != "" {
		doc["cm_version"] = agentVersion
	}
	_, err := c.fs.Collection("pcs").Doc(pc).Set(ctx, doc)
	return err
}

// PCRole は pcs/{pc}.role を返す（未登録/未設定は ""）。wsViewer が slave の
// セッションを pairing key で名前空間化するかの判定に使う。
func (c *Client) PCRole(ctx context.Context, pc string) (string, error) {
	snap, err := c.fs.Collection("pcs").Doc(pc).Get(ctx)
	if err != nil || !snap.Exists() {
		return "", nil
	}
	role, _ := snap.Data()["role"].(string)
	return role, nil
}

// BindSlave は slaves/{pc} を作成/更新する（enroll --slave 時）。
// TRANSACTION: pcs/{pc} が存在し role!="slave" なら ok=false で拒否
// （既存 master/未マーク pc の乗っ取り防止＝owner の実 PC は先に登録済みで
// 守られる）。それ以外（新規 pc / 既存 slave の再 enroll）は secret を
// 上書きし ok=true。
func (c *Client) BindSlave(ctx context.Context, pc, secretHash string) (ok bool, err error) {
	if !ValidPCName(pc) {
		return false, nil // 不正 pc 名（`:`/NUL 等）は束縛しない＝token 化不能
	}
	slaveRef := c.fs.Collection("slaves").Doc(pc)
	pcRef := c.fs.Collection("pcs").Doc(pc)
	terr := c.fs.RunTransaction(ctx,
		func(ctx context.Context, tx *firestore.Transaction) error {
			snap, gerr := tx.Get(pcRef)
			if gerr == nil && snap.Exists() {
				if role, _ := snap.Data()["role"].(string); role != "slave" {
					ok = false
					return nil // 衝突: 既存 master/未マーク pc は奪えない
				}
			}
			ok = true
			return tx.Set(slaveRef, map[string]any{
				"pc":          pc,
				"secret_hash": secretHash,
				"created_at":  time.Now().UTC().Format(time.RFC3339Nano),
				"revoked":     false,
			})
		})
	if terr != nil {
		return false, terr
	}
	return ok, nil
}

// SlaveSecretHash は slaves/{pc}.secret_hash を返す（/slave/token の検証用）。
func (c *Client) SlaveSecretHash(ctx context.Context, pc string) (hash string, ok bool) {
	snap, err := c.fs.Collection("slaves").Doc(pc).Get(ctx)
	if err != nil || !snap.Exists() {
		return "", false
	}
	h, _ := snap.Data()["secret_hash"].(string)
	return h, h != ""
}

// SlaveRevoked は slaves/{pc}.revoked==true か（doc 無しは false）。
func (c *Client) SlaveRevoked(ctx context.Context, pc string) bool {
	if pc == "" {
		return false
	}
	snap, err := c.fs.Collection("slaves").Doc(pc).Get(ctx)
	if err != nil || !snap.Exists() {
		return false
	}
	rv, _ := snap.Data()["revoked"].(bool)
	return rv
}

// SetSlaveRevoked は slaves/{pc}.revoked をトグルする。
func (c *Client) SetSlaveRevoked(ctx context.Context, pc string, revoked bool) error {
	if pc == "" {
		return nil
	}
	_, err := c.fs.Collection("slaves").Doc(pc).Set(ctx, map[string]any{
		"revoked": revoked,
	}, firestore.MergeAll)
	return err
}
