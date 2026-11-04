package state

import (
	"context"
	"testing"
	"time"
)

// TestPushStatusDoesNotReadUnchangedSessions — 無差分の push は Firestore を読まない。
//
// 旧実装は tick ごとに全 session doc を Get していた（5 台 65 docs × 5s tick ≈ 4.7 万
// 読み/時＝2026-11-04 実測の Firestore 読取の 8 割）。pushCache.verifyEvery を 0 にすると
// 旧挙動（毎回 Get）と同じになり、この検証が「無差分 push で Firestore を読んだ」で
// 落ちることを確認済み。実 Firestore API（エミュレータ）で検証する。
func TestPushStatusDoesNotReadUnchangedSessions(t *testing.T) {
	ctx := context.Background()
	c := newClient(t, "pc-cache")
	base := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	clock := base
	c.push.now = func() time.Time { return clock }

	ss := func(cpuB float64) []map[string]any {
		return []map[string]any{
			realSession("a", 1, true), realSession("b", cpuB, false), realSession("c", 3, true),
		}
	}
	doc := func(id string) (map[string]any, bool) {
		snap, err := c.fs.Collection("pcs").Doc("pc-cache").Collection("sessions").Doc(id).Get(ctx)
		if err != nil || !snap.Exists() {
			return nil, false
		}
		return snap.Data(), true
	}
	wantGets := func(step string, want int64) {
		t.Helper()
		if g := c.PushGets(); g != want {
			t.Fatalf("%s: Firestore Get 回数=%d（want %d）", step, g, want)
		}
	}

	// 初回: 記憶が無いので 3 件とも Get → 書込。
	if ch, err := c.PushStatus(ctx, ss(2)); err != nil || ch != 3 {
		t.Fatalf("初回 push: changed=%d err=%v", ch, err)
	}
	wantGets("初回", 3)

	// 無差分: 読みも書きもしない（旧コードはここで 3 件 Get する）。
	if ch, err := c.PushStatus(ctx, ss(2)); err != nil || ch != 0 {
		t.Fatalf("無差分 push: changed=%d err=%v", ch, err)
	}
	wantGets("無差分 push で Firestore を読んだ", 3)

	// 1 件だけ変化: その 1 件だけ Get（version を正しく +1 するため）→ version 2。
	if ch, err := c.PushStatus(ctx, ss(9)); err != nil || ch != 1 {
		t.Fatalf("1 件変化 push: changed=%d err=%v", ch, err)
	}
	wantGets("1 件変化", 4)
	if d, ok := doc("b"); !ok || d["version"].(int64) != 2 {
		t.Fatalf("b の version が 2 でない: %v", d)
	}

	// 外部からの削除（Web の端末削除など・この client の DeleteSession を経ない）は、
	// 再検証の窓内では気付かない＝読みも再生成もしない（near-$0 の代償。最長 PushVerifyEvery）。
	if _, err := c.fs.Collection("pcs").Doc("pc-cache").Collection("sessions").Doc("a").Delete(ctx); err != nil {
		t.Fatalf("外部削除: %v", err)
	}
	if ch, _ := c.PushStatus(ctx, ss(9)); ch != 0 {
		t.Fatalf("窓内の push で changed=%d", ch)
	}
	wantGets("窓内", 4)
	if _, ok := doc("a"); ok {
		t.Fatal("再検証の窓内なのに a が再生成された")
	}

	// 窓が過ぎると全件を実態で再検証（3 Get）し、消えていた a を version 1 で再生成する。
	clock = base.Add(PushVerifyEvery)
	if ch, _ := c.PushStatus(ctx, ss(9)); ch != 1 {
		t.Fatalf("再検証 push で changed=%d（want 1＝a の再生成）", ch)
	}
	wantGets("再検証", 7)
	if d, ok := doc("a"); !ok || d["version"].(int64) != 1 {
		t.Fatalf("a が再生成されていない／version 不正: %v", d)
	}
	// 再検証の直後の無差分はまた読まない。
	if ch, _ := c.PushStatus(ctx, ss(9)); ch != 0 {
		t.Fatalf("再検証直後の無差分 push で changed=%d", ch)
	}
	wantGets("再検証直後", 7)

	// この client の DeleteSession（producer の消滅 delete）は記憶も捨てる＝次の push で
	// その 1 件だけ Get して再生成する（旧挙動と同じ結果・読みは 1 件）。
	if err := c.DeleteSession(ctx, "c"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if ch, _ := c.PushStatus(ctx, ss(9)); ch != 1 {
		t.Fatalf("DeleteSession 後の push で changed=%d", ch)
	}
	wantGets("DeleteSession 後", 8)
	if _, ok := doc("c"); !ok {
		t.Fatal("DeleteSession 後に c が再生成されていない")
	}
}

// TestPushStatusForDoesNotReadUnchanged — relay が slave の代わりに書く PushStatusFor も
// 同じ記憶を使う（pc 名前空間付き）。DeleteSessionFor は記憶を捨てる。
func TestPushStatusForDoesNotReadUnchanged(t *testing.T) {
	ctx := context.Background()
	c := newClient(t, "relay")
	one := []map[string]any{realSession("sidZ", 1.0, true)}
	if ch, err := c.PushStatusFor(ctx, "pcz-herdr", one); err != nil || ch != 1 {
		t.Fatalf("初回: changed=%d err=%v", ch, err)
	}
	if ch, err := c.PushStatusFor(ctx, "pcz-herdr", one); err != nil || ch != 0 {
		t.Fatalf("無差分: changed=%d err=%v", ch, err)
	}
	if g := c.PushGets(); g != 1 {
		t.Fatalf("無差分 PushStatusFor で Firestore を読んだ: gets=%d（want 1）", g)
	}
	// 別 pc の同じ key は別の記憶（隔離）＝Get する。
	if ch, _ := c.PushStatusFor(ctx, "pcw-herdr", one); ch != 1 {
		t.Fatalf("別 pc: changed=%d", ch)
	}
	if g := c.PushGets(); g != 2 {
		t.Fatalf("別 pc の push が記憶を共有した: gets=%d（want 2）", g)
	}
	if err := c.DeleteSessionFor(ctx, "pcz-herdr", "sidZ"); err != nil {
		t.Fatalf("DeleteSessionFor: %v", err)
	}
	if ch, _ := c.PushStatusFor(ctx, "pcz-herdr", one); ch != 1 {
		t.Fatalf("DeleteSessionFor 後の push で changed=%d", ch)
	}
	if g := c.PushGets(); g != 3 {
		t.Fatalf("DeleteSessionFor 後の gets=%d（want 3）", g)
	}
}
