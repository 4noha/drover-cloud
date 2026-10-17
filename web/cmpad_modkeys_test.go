package web

// cmpad_modkeys_test.go — 操作パッドに追加した補助ボタン（Shift+←・
// Shift+Tab・Enter・ページ再読み込み）の担保。
// 出典:
//   - shiftLeft: 2026-09-28 ユーザー報告「codex のコマンドで shift+← 入力が必要」
//   - shiftTab:  2026-09-28 ユーザー確認「モード切替は Shift+Tab」
//   - enter:     2026-09-30 ユーザー報告「フローティングボタンに Enter を追加」
//   - reload:    2026-10-06 ユーザー報告「パッドの左下に再読み込みボタンが欲しい」
//                （端末へのキー送信ではなくWebページ自体のF5相当。PAD_KEYSとは
//                 別経路＝data-k ではなく data-action="reload" で区別する）
// HTML(ui.go)側のボタンと term.js 側の PAD_KEYS が食い違って壊れる
//（ボタンはあるが押しても何も送られない／その逆）事故を防ぐ。

import (
	"os"
	"strings"
	"testing"
)

func TestCmpadModKeyButtonsPresent(t *testing.T) {
	for _, dataK := range []string{"shiftLeft", "shiftTab", "enter"} {
		want := `data-k="` + dataK + `"`
		if !strings.Contains(termHTML, want) {
			t.Fatalf("cmpad に %s ボタンが無い", want)
		}
	}
}

// TestCmpadModKeySequences は term.js の PAD_KEYS が期待するバイト列と
// 一致することを固定する。関数ではなくオブジェクトリテラルなので
// termjs_gate_test.go の extractFunc は使えず、ここではソース中の
// 該当行をそのまま突き合わせる。
func TestCmpadModKeySequences(t *testing.T) {
	b, err := os.ReadFile("static/term.js")
	if err != nil {
		t.Fatalf("term.js: %v", err)
	}
	src := string(b)
	cases := map[string]string{
		"shiftLeft": "shiftLeft: [0x1b, 0x5b, 0x31, 0x3b, 0x32, 0x44]", // ESC[1;2D
		"shiftTab":  "shiftTab: [0x1b, 0x5b, 0x5a]",                    // ESC[Z (Back Tab)
		"enter":     "enter: [0x0d]",                                   // CR
	}
	for name, marker := range cases {
		if !strings.Contains(src, marker) {
			t.Fatalf("PAD_KEYS.%s が期待するバイト列と一致しない。"+
				"term.js を書き換えたらこのテストも直すこと（現物: %s 周辺を確認）", name, marker)
		}
	}
}

// TestCmpadReloadButton は reload ボタンが HTML 側にあり、term.js 側に
// それを location.reload() へ繋ぐハンドラがあることを固定する。
// data-k/PAD_KEYS とは別経路（端末へは何も送らない）なので
// TestCmpadModKeySequences の仕組みでは担保できず、個別に見る。
func TestCmpadReloadButton(t *testing.T) {
	if !strings.Contains(termHTML, `data-action="reload"`) {
		t.Fatalf("cmpad に data-action=\"reload\" ボタンが無い")
	}
	b, err := os.ReadFile("static/term.js")
	if err != nil {
		t.Fatalf("term.js: %v", err)
	}
	src := string(b)
	if !strings.Contains(src, `data-action="reload"`) || !strings.Contains(src, "location.reload()") {
		t.Fatalf("term.js に reload ボタンの click ハンドラ（location.reload()）が無い")
	}
}
