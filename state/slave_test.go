//go:build !windows

package state

// slave（共用 PC）用 state メソッドを実 Firestore エミュレータで検証
// （合成なし。TestMain/newClient/realSession は state_test.go 共有）。
// relay client は pcID="relay" 固定なので全メソッドが pc 明示。

import (
	"context"
	"testing"
	"time"
)

func TestSlavePushStatusForIsolatedAndGated(t *testing.T) {
	ctx := context.Background()
	c := newClient(t, "relay") // 本番同様 pcID=relay
	const pcX, pcY = "pcx-herdr", "pcy-herdr"

	// pcX に push → pcX 配下のみに書かれる（pcY は空）。
	if ch, err := c.PushStatusFor(ctx, pcX, []map[string]any{
		realSession("sidX", 1.0, true)}); err != nil || ch != 1 {
		t.Fatalf("PushStatusFor pcX: ch=%d err=%v", ch, err)
	}
	if keys, _ := c.SessionKeysFor(ctx, pcX); len(keys) != 1 || keys[0] != "sidX" {
		t.Fatalf("pcX の session key 不正: %v", keys)
	}
	if keys, _ := c.SessionKeysFor(ctx, pcY); len(keys) != 0 {
		t.Fatalf("pcY に漏れて書かれた（隔離破れ）: %v", keys)
	}
	// content_hash ゲート: 無差分再 push は changed 0（near-$0）。
	if ch, _ := c.PushStatusFor(ctx, pcX, []map[string]any{
		realSession("sidX", 1.0, true)}); ch != 0 {
		t.Fatalf("無差分 push で changed=%d（near-$0 違反）", ch)
	}
	// 内容変化 → changed 1。
	if ch, _ := c.PushStatusFor(ctx, pcX, []map[string]any{
		realSession("sidX", 9.9, false)}); ch != 1 {
		t.Fatalf("差分 push で changed=%d", ch)
	}
	// 親 doc に agent_kind が入り DroverPCs に出る。
	pcs, _ := c.DroverPCs(ctx)
	has := false
	for _, p := range pcs {
		if p == pcX {
			has = true
		}
	}
	if !has {
		t.Fatalf("PushStatusFor 後 pcX が DroverPCs に居ない: %v", pcs)
	}
}

func TestSlaveSessionOwnershipAndDelete(t *testing.T) {
	ctx := context.Background()
	c := newClient(t, "relay")
	const pc = "pcown-herdr"
	if _, err := c.PushStatusFor(ctx, pc, []map[string]any{
		realSession("s1", 1.0, true), realSession("s2", 2.0, true)}); err != nil {
		t.Fatalf("push: %v", err)
	}
	if !c.SessionOwnedBy(ctx, pc, "s1") {
		t.Fatal("自 pc の push 済 sid が未所有判定")
	}
	if c.SessionOwnedBy(ctx, pc, "nope") {
		t.Fatal("push していない sid が所有判定された")
	}
	if c.SessionOwnedBy(ctx, "other-herdr", "s1") {
		t.Fatal("別 pc の名で所有判定が通った（grant 越権の芽）")
	}
	if err := c.DeleteSessionFor(ctx, pc, "s2"); err != nil {
		t.Fatalf("DeleteSessionFor: %v", err)
	}
	if c.SessionOwnedBy(ctx, pc, "s2") {
		t.Fatal("削除した sid がまだ所有判定")
	}
	// 空キー削除は安全 no-op。
	if err := c.DeleteSessionFor(ctx, pc, ""); err != nil {
		t.Fatalf("空キー削除でエラー: %v", err)
	}
}

func TestSlaveGrantIsolatedFromMaster(t *testing.T) {
	ctx := context.Background()
	c := newClient(t, "relay")
	const pc = "pcgrant-herdr"
	const other = "pcother-herdr"
	// grant 無し → SlaveGrantValid は false。
	if c.SlaveGrantValid(ctx, pc, "sg") {
		t.Fatal("grant 無しで valid=true")
	}
	// pc 名前空間で書く → 自 pc は valid。
	if err := c.PutSlaveGrant(ctx, pc, "sg", time.Minute); err != nil {
		t.Fatalf("PutSlaveGrant: %v", err)
	}
	if !c.SlaveGrantValid(ctx, pc, "sg") {
		t.Fatal("自 pc の grant が valid=false")
	}
	// 別 pc からは同 sid でも見えない（pc 分離＝租借不能）。
	if c.SlaveGrantValid(ctx, other, "sg") {
		t.Fatal("別 pc が他 pc の slave grant を valid 判定")
	}
	// 期限切れ（負 TTL）→ false。
	if err := c.PutSlaveGrant(ctx, pc, "sgexp", -time.Second); err != nil {
		t.Fatal(err)
	}
	if c.SlaveGrantValid(ctx, pc, "sgexp") {
		t.Fatal("期限切れ grant が valid=true")
	}
	// ★poisoning 根絶の証明: slave grant は master の relaygrants を一切
	// 作らない＝master 経路 CheckRelayGrant は slave grant を認可根拠に
	// しない（旧実装＝slave が relaygrants/{sid}:source を書いていた頃は
	// ここが true になり本アサーションが FAIL する）。
	if c.CheckRelayGrant(ctx, "sg", "source") {
		t.Fatal("slave grant が master CheckRelayGrant に漏れている（poisoning 未修正）")
	}
}

func TestValidPCNameRejectsCollisionChars(t *testing.T) {
	for _, ok := range []string{"mac-studio-herdr", "n9htqcr6g0-herdr", "a.b_c-1"} {
		if !ValidPCName(ok) {
			t.Fatalf("valid pc %q rejected", ok)
		}
	}
	// `:`（slaveGrantDocID 区切り）/NUL/`/`/空/非ASCII は拒否＝別名衝突根絶。
	for _, bad := range []string{"", "victim-herdr:w1", "a\x00b", "a/b", "a b", "中"} {
		if ValidPCName(bad) {
			t.Fatalf("invalid pc %q accepted", bad)
		}
	}
	// BindSlave も不正 pc を束縛しない（旧コードは `:` を通し衝突可能だった）。
	ctx := context.Background()
	c := newClient(t, "relay")
	if ok, err := c.BindSlave(ctx, "evil-herdr:w1", "hash"); err != nil || ok {
		t.Fatalf("BindSlave が不正 pc を束縛: ok=%v err=%v", ok, err)
	}
}

func TestSlaveBindAndSecret(t *testing.T) {
	ctx := context.Background()
	c := newClient(t, "relay")
	const pc = "pcbind-herdr"

	// 新規 pc → bind 成功。
	ok, err := c.BindSlave(ctx, pc, "hash-1")
	if err != nil || !ok {
		t.Fatalf("新規 BindSlave: ok=%v err=%v", ok, err)
	}
	if h, ok := c.SlaveSecretHash(ctx, pc); !ok || h != "hash-1" {
		t.Fatalf("SlaveSecretHash=%q ok=%v", h, ok)
	}
	if c.SlaveRevoked(ctx, pc) {
		t.Fatal("bind 直後に revoked")
	}
	// 既存 slave の再 enroll → secret 上書き成功。
	ok, err = c.BindSlave(ctx, pc, "hash-2")
	if err != nil || !ok {
		t.Fatalf("再 BindSlave: ok=%v err=%v", ok, err)
	}
	if h, _ := c.SlaveSecretHash(ctx, pc); h != "hash-2" {
		t.Fatalf("secret 上書きされない: %q", h)
	}

	// 既存 master pc（role 無し）を slave で奪おうとする → 拒否。
	const master = "pcmaster-real"
	if err := c.RegisterPCVersion(ctx, "v1"); err != nil { // pcID=relay に書く…不可
		t.Fatal(err)
	}
	// RegisterPCVersion は c.pcID(relay) に書くので master 用に別途 pcs doc を作る。
	if _, err := c.fs.Collection("pcs").Doc(master).Set(ctx, map[string]any{
		"id": master, "agent_kind": "herdr-drover", // role 無し＝master 扱い
	}); err != nil {
		t.Fatal(err)
	}
	ok, err = c.BindSlave(ctx, master, "hash-x")
	if err != nil {
		t.Fatalf("BindSlave(master) err=%v", err)
	}
	if ok {
		t.Fatal("既存 master pc を slave が奪えた（衝突検査破れ）")
	}
	if _, ok := c.SlaveSecretHash(ctx, master); ok {
		t.Fatal("拒否されたのに slaves/{master} が書かれた")
	}

	// role=slave の既存 pc は BindSlave 可（RegisterSlavePCVersion 後の再 enroll）。
	const sl = "pcslave2-herdr"
	if err := c.RegisterSlavePCVersion(ctx, sl, "v2"); err != nil {
		t.Fatal(err)
	}
	if role, _ := c.PCRole(ctx, sl); role != "slave" {
		t.Fatalf("RegisterSlavePCVersion で role!=slave: %q", role)
	}
	if ok, err := c.BindSlave(ctx, sl, "h"); err != nil || !ok {
		t.Fatalf("既存 slave pc の再 bind 拒否: ok=%v err=%v", ok, err)
	}
}

func TestSlaveRevokeAndDeleteRemovesCredential(t *testing.T) {
	ctx := context.Background()
	c := newClient(t, "relay")
	const pc = "pcrev-herdr"
	if _, err := c.BindSlave(ctx, pc, "h"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetSlaveRevoked(ctx, pc, true); err != nil {
		t.Fatalf("SetSlaveRevoked: %v", err)
	}
	if !c.SlaveRevoked(ctx, pc) {
		t.Fatal("SetSlaveRevoked(true) 後に revoked でない")
	}
	// secret_hash は保持（MergeAll でトグルのみ）。
	if _, ok := c.SlaveSecretHash(ctx, pc); !ok {
		t.Fatal("revoke で secret_hash まで消えた")
	}
	if err := c.SetSlaveRevoked(ctx, pc, false); err != nil {
		t.Fatal(err)
	}
	if c.SlaveRevoked(ctx, pc) {
		t.Fatal("SetSlaveRevoked(false) 後も revoked")
	}
	// 端末解除（deletePC）で slaves/{pc} も撤去される。
	if err := c.DeletePCByID(ctx, pc); err != nil {
		t.Fatalf("DeletePCByID: %v", err)
	}
	if _, ok := c.SlaveSecretHash(ctx, pc); ok {
		t.Fatal("端末解除後も slaves/{pc} が残る（refresh secret 撤去漏れ）")
	}
}

func TestSlaveWatchWakeForAndWakeDoc(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := newClient(t, "relay")
	const pc = "pcwake-herdr"

	// 初期 wake 無し → WakeDoc ok=false。
	if _, _, ok := c.WakeDoc(ctx, pc); ok {
		t.Fatal("wake 無しで WakeDoc ok=true")
	}
	got := make(chan string, 8)
	go func() { _ = c.WatchWakeFor(ctx, pc, func(sid string) { got <- sid }) }()
	time.Sleep(1500 * time.Millisecond) // listener attach 待ち

	// 別クライアント（viewer 相当）が wake を書く。
	cf := newClient(t, "other")
	if err := cf.Wake(ctx, pc, "sess-Z"); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	select {
	case s := <-got:
		if s != "sess-Z" {
			t.Fatalf("WatchWakeFor 受信 sid=%q", s)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("WatchWakeFor が wake を受信できない")
	}
	// WakeDoc が sid/ts を返す（long-poll catch-up の土台）。
	sid, ts, ok := c.WakeDoc(ctx, pc)
	if !ok || sid != "sess-Z" || ts == "" {
		t.Fatalf("WakeDoc=%q,%q,%v", sid, ts, ok)
	}
}
