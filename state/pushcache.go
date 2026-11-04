package state

// pushcache.go — PushStatus / PushStatusFor の差分判定の記憶。
//
// 旧実装は「差分が無ければ書かない」（content_hash ゲート）を実現するために、tick
// ごとに**全 session doc を Firestore から Get** して hash を比べていた。書込は
// near-$0 になるが読取は数えられておらず、5 台 65 docs × 720 tick/時 ≈ 4.7 万読み/時
// （2026-11-04 実測: Firestore 読取 6 万/時の 8 割・月 $13〜16 で最大の費目）になっていた。
//
// ここでは「前回 push した hash と version」をプロセス内に記憶し、無差分なら読みも
// 書きもしない。Firestore を Get するのは次の 3 つだけ:
//
//   - 記憶が無い（プロセス起動後の初回・書込失敗の後・DeleteSession の後）
//   - hash が変わった（version を正しく +1 するため現在値を読む＝旧挙動と同じ）
//   - 記憶が PushVerifyEvery より古い（外部からの削除・改変＝Web の端末削除
//     （DeletePCByID）等に追随する安全網。再生成は最長これだけ遅れる）
//
// 記憶はこの Client だけのもの。relay が slave の代わりに書く PushStatusFor も
// 同じ記憶を pc 名前空間付きで使う（pc ごとに独立）。

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cloud.google.com/go/firestore"
)

// PushVerifyEvery は記憶を Firestore の実態で再検証する間隔。全 doc を 1 回 Get する
// ので、読取は docs × (60/PushVerifyEvery 分) 回/時（65 docs なら 390/時）。
const PushVerifyEvery = 10 * time.Minute

type pushEntry struct {
	hash       string
	version    int64
	verifiedAt time.Time // Firestore の実態と突き合わせた時刻
}

type pushCache struct {
	mu          sync.Mutex
	entries     map[string]pushEntry
	now         func() time.Time // テストで時計を進めるための seam
	verifyEvery time.Duration
	gets        atomic.Int64 // Firestore を Get した累計回数（診断用）
}

func newPushCache() *pushCache {
	return &pushCache{entries: map[string]pushEntry{}, now: time.Now, verifyEvery: PushVerifyEvery}
}

func (p *pushCache) key(pc, id string) string { return pc + "/" + id }

func (p *pushCache) get(k string) (pushEntry, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[k]
	return e, ok
}

func (p *pushCache) set(k string, e pushEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries[k] = e
}

func (p *pushCache) forget(k string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.entries, k)
}

func (p *pushCache) forgetPC(pc string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	prefix := pc + "/"
	for k := range p.entries {
		if strings.HasPrefix(k, prefix) {
			delete(p.entries, k)
		}
	}
}

// PushGets は PushStatus / PushStatusFor が Firestore を Get した累計回数
// （診断・テスト用。無差分の tick では増えないことが near-$0 の根拠）。
func (c *Client) PushGets() int64 { return c.push.gets.Load() }

// pushSessions は PushStatus / PushStatusFor の共通本体。sessions を
// pcs/{pc}/sessions/{key} へ upsert する。content_hash が前回と同じなら version を
// 据え置く（＝差分なし。Functions/agent がこれで開閉判断）。戻り値 changed は今回
// version が上がった session 数。
func (c *Client) pushSessions(ctx context.Context, pc string, sessions []map[string]any) (changed int, err error) {
	col := c.fs.Collection("pcs").Doc(pc).Collection("sessions")
	now := c.push.now()
	for _, s := range sessions {
		id := sessionKey(s)
		h := contentHash(s)
		ck := c.push.key(pc, id)
		if e, ok := c.push.get(ck); ok && e.hash == h && now.Sub(e.verifiedAt) < c.push.verifyEvery {
			// 差分なし＝Firestore を読みも書きもしない（near-$0 維持。毎 tick Get すると
			// 読取が、毎 tick Set すると updated_at で常時書込＋無駄 listener wake が出る）。
			continue
		}
		ref := col.Doc(id)
		ver := int64(1)
		c.push.gets.Add(1)
		snap, gerr := ref.Get(ctx)
		if gerr == nil && snap.Exists() {
			d := snap.Data()
			pv, _ := d["version"].(int64)
			ph, _ := d["content_hash"].(string)
			if ph == h {
				c.push.set(ck, pushEntry{hash: h, version: pv, verifiedAt: now})
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
			c.push.forget(ck) // 書けたか不明＝次回は実態を読み直す
			return changed, werr
		}
		c.push.set(ck, pushEntry{hash: h, version: ver, verifiedAt: now})
	}
	// pcs/{pc} は subcollection 書込では暗黙の非存在 doc になり
	// Collection("pcs") 列挙に出ない。端末一覧（account scope）で
	// 拾えるよう、変化があった時だけ親 doc を明示書込（near-$0 維持）。
	if changed > 0 {
		// MergeAll＝id/updated_at/agent_kind のみ更新し RegisterPCVersion が入れた
		// cm_version 等の他フィールドを消さない（全置換 Set だと agent 版が毎
		// producer tick で消える実バグになる）。
		//
		// agent_kind を毎回 **再表明**するのが重要（敵対的レビューで確認）:
		// RegisterPCVersion は起動時 1 回しか呼ばれないため、owner の端末削除
		// （DeletePCByID で pcs/{pc} 全消し）後に、**同一プロセスが dormant→復帰**
		// して PushStatus すると、この tail が pcs/{pc} を {id,updated_at} だけで
		// 再生成し agent_kind を永久に欠く→DroverPCs が当該 PC を落とし、その全
		// セッションが他機のリモート注入から消える。ここで毎回書けば delete→
		// recreate されても常に agent_kind を持つ（この state モジュールの
		// PushStatus 書き手は herdr-drover agent のみ＝固定値で正しい）。
		_, _ = c.fs.Collection("pcs").Doc(pc).Set(ctx, map[string]any{
			"id":         pc,
			"updated_at": time.Now().UTC().Format(time.RFC3339),
			"agent_kind": "herdr-drover",
		}, firestore.MergeAll)
	}
	return changed, nil
}
