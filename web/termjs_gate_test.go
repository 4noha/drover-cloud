package web

// termjs_gate_test.go — term.js の**純粋判定関数**を実 node で評価して真理値表を
// 固定する（BUG-4 の監視再接続。cmReconnectGate と同じ「node で決定論テスト」の流儀）。
//
// なぜ Go テストから node を呼ぶか: この repo のゲートは `go test ./...` 一本で、
// JS 用のハーネスを別立てするとゲートから外れて腐るため。node が無い環境では
// skip する（CI 側の前提を増やさない）。
//
// ⚠ ここで担保するのは**判定表だけ**。実際に切れた線が張り直るかは実 relay の
// websocket 越しでしか確かめられない（BUG-3 の教訓＝合成では緑になっても本番
// 経路に当たっていなかった）。

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// extractFunc は term.js から `function <name>(...) { ... }` を素朴に切り出す。
// 対象は入れ子ブレースを持つが文字列/正規表現リテラルに `{}` を含まない純粋関数
// のみ＝この素朴な深さ数えで足りる（対象外の関数を渡したら見つからず fail）。
func extractFunc(t *testing.T, src, name string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^function ` + regexp.QuoteMeta(name) + `\(`)
	loc := re.FindStringIndex(src)
	if loc == nil {
		t.Fatalf("term.js に function %s が無い（改名したらこのテストも直す）", name)
	}
	depth, started := 0, false
	for i := loc[0]; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
			started = true
		case '}':
			depth--
			if started && depth == 0 {
				return src[loc[0] : i+1]
			}
		}
	}
	t.Fatalf("function %s の終端 } が見つからない", name)
	return ""
}

func TestTermJSGates(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node が無いので skip（判定表の担保のみ・本番経路は実 relay で確認）")
	}
	b, err := os.ReadFile("static/term.js")
	if err != nil {
		t.Fatalf("term.js: %v", err)
	}
	src := string(b)

	script := extractFunc(t, src, "cmReconnectGate") + "\n" +
		extractFunc(t, src, "cmWatchGate") + "\n" + `
const fail = [];
const eq = (got, want, msg) => { if (got !== want) fail.push(msg + " got=" + got + " want=" + want); };

// --- cmWatchGate: 見ている間だけ張り直す ---
eq(cmWatchGate({visible:true,  active:true,  wsState:3}), true,
   "visible かつ active かつ CLOSED は張る:");
eq(cmWatchGate({visible:true,  active:true,  wsState:-1}), true,
   "未接続(-1)でも張る:");
eq(cmWatchGate({visible:false, active:true,  wsState:3}), false,
   "裏タブでは張らない（コスト 0）:");
eq(cmWatchGate({visible:true,  active:false, wsState:3}), false,
   "止まったセッションは起こさない:");
eq(cmWatchGate({visible:true,  active:true,  wsState:0}), false,
   "CONNECTING 中は張らない:");
eq(cmWatchGate({visible:true,  active:true,  wsState:1}), false,
   "OPEN 中は張らない:");

// --- cmReconnectGate: 既存の backoff 規律が壊れていないこと ---
eq(cmReconnectGate({wsState:1, now:1e9, lastAttemptAt:0, attempts:0}).connect, false,
   "OPEN 中は再接続しない:");
eq(cmReconnectGate({wsState:3, now:1e9, lastAttemptAt:1e9-500, attempts:0}).connect, false,
   "backoff 1s 未満は張らない:");
eq(cmReconnectGate({wsState:3, now:1e9, lastAttemptAt:1e9-1500, attempts:0}).connect, true,
   "backoff 経過で張る:");
eq(cmReconnectGate({wsState:3, now:1e9, lastAttemptAt:1e9-20000, attempts:10}).connect, false,
   "attempts 大では上限 30s まで待つ:");
eq(cmReconnectGate({wsState:3, now:1e9, lastAttemptAt:1e9-31000, attempts:10}).connect, true,
   "上限 30s を超えたら張る:");

if (fail.length) { console.log(fail.join("\n")); process.exit(1); }
console.log("OK");
`
	cmd := exec.Command(node, "-e", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("判定表が一致しない:\n%s", out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("想定外の出力: %s", out)
	}
}
