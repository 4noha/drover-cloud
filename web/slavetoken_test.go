package web

// slave bearer の mint/verify を実 RSA 鍵で機械検証（合成・モック署名なし）。
// testSAJSON / verifyJWT は fbtoken_test.go の同パッケージヘルパを流用。

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestMintVerifySlaveTokenRoundTrip(t *testing.T) {
	sa, _ := testSAJSON(t)
	now := time.Unix(1765432100, 0)

	tok, err := mintSlaveToken([]byte(sa), "mac-studio-herdr", now)
	if err != nil {
		t.Fatal(err)
	}

	// 正常: 束縛 pc を返す（aud は Identity Toolkit 定数のままだが無視される）。
	pc, err := verifySlaveToken([]byte(sa), tok, now.Add(30*time.Minute))
	if err != nil || pc != "mac-studio-herdr" {
		t.Fatalf("round-trip 失敗: pc=%q err=%v", pc, err)
	}

	// 署名改竄 → 検証失敗。
	bad := tok[:len(tok)-2] + func() string {
		if tok[len(tok)-1] == 'A' {
			return "BB"
		}
		return "AA"
	}()
	if _, err := verifySlaveToken([]byte(sa), bad, now.Add(time.Minute)); err == nil {
		t.Fatal("改竄署名が検証を通った")
	}

	// 期限切れ → 検証失敗（exp = now+1h、now+2h で検証）。
	if _, err := verifySlaveToken([]byte(sa), tok, now.Add(2*time.Hour)); err == nil {
		t.Fatal("期限切れトークンが通った")
	}

	// uid が "slave:" でない（owner token）→ 検証失敗（覗き見防止の要）。
	owner, err := firebaseCustomToken([]byte(sa), fbOwnerUID, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifySlaveToken([]byte(sa), owner, now.Add(time.Minute)); err == nil {
		t.Fatal("uid=cm-owner のトークンが slave として通った")
	}

	// 別 SA 鍵（=別公開鍵）で署名した slave token は検証失敗。
	sa2, _ := testSAJSON(t)
	tok2, _ := mintSlaveToken([]byte(sa2), "mac-studio-herdr", now)
	if _, err := verifySlaveToken([]byte(sa), tok2, now.Add(time.Minute)); err == nil {
		t.Fatal("別鍵署名のトークンが通った")
	}

	// 空 uid（"slave:" のみ）は pc が空で拒否。
	empty, _ := mintSlaveToken([]byte(sa), "", now)
	if _, err := verifySlaveToken([]byte(sa), empty, now.Add(time.Minute)); err == nil {
		t.Fatal("空 pc のトークンが通った")
	}

	// ゴミ文字列 → 3 セグメント検査で拒否。
	if _, err := verifySlaveToken([]byte(sa), "not-a-jwt", now); err == nil {
		t.Fatal("非 JWT が通った")
	}
}

func TestBearerParsing(t *testing.T) {
	cases := []struct {
		hdr  string
		want string
	}{
		{"", ""},
		{"Bearer abc", "abc"},
		{"bearer abc", "abc"},   // 大小無視
		{"Bearer  abc ", "abc"}, // TrimSpace
		{"Basic abc", ""},       // 別スキーム
		{"Bearer", ""},          // 値なし
		{"Bearer ", ""},         // 空値
		{"BearerX abc", ""},     // 前方一致だけの偽装
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		if c.hdr != "" {
			r.Header.Set("Authorization", c.hdr)
		}
		if got := bearer(r); got != c.want {
			t.Fatalf("bearer(%q)=%q want %q", c.hdr, got, c.want)
		}
	}
}

func TestWakeNewer(t *testing.T) {
	base := "2026-07-18T00:00:00.5Z"
	older := "2026-07-18T00:00:00.45Z" // 文字列比較だと base < older に見える罠
	newer := "2026-07-18T00:00:01Z"
	if !wakeNewer(base, "") {
		t.Fatal("since 空 + ts 有りは新しい扱いのはず")
	}
	if wakeNewer("", "") {
		t.Fatal("ts 空は新しくない")
	}
	if !wakeNewer(newer, base) {
		t.Fatal("newer > base")
	}
	if wakeNewer(older, base) {
		t.Fatal("older < base（文字列比較の単調性の罠を time で回避できていない）")
	}
	if wakeNewer(base, base) {
		t.Fatal("同一 ts は新しくない（>）")
	}
}

func TestSlaveSessionKeyNamespacing(t *testing.T) {
	// NUL 区切りで pc と sid を連結＝二つの PC が同じ sid 文字列でも別キー。
	a := slaveSessionKey("pcA-herdr", "w1:p2")
	b := slaveSessionKey("pcB-herdr", "w1:p2")
	if a == b {
		t.Fatal("別 pc の同一 sid が同じキーになった（hijack 防止破れ）")
	}
	if a != "pcA-herdr\x00w1:p2" {
		t.Fatalf("キー形式が想定外: %q", a)
	}
}
