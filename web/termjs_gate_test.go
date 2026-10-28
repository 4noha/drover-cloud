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

// TestTermJSMetaFilter — ワイヤ v2 の META フィルタ（cmMakeMetaFilter/cmParseMeta）。
// producer（herdr-drover internal/bridge/history.go）が流す OSC 7707 を剥がして
// onMeta へ渡し、履歴区間はページで 1 回だけ通す。chunk 境界でマーカーが割れる
// ケース・2 回目の接続で履歴を捨てるケース・META が無い旧 producer の素通しを固定。
func TestTermJSMetaFilter(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node が無いので skip")
	}
	b, err := os.ReadFile("static/term.js")
	if err != nil {
		t.Fatalf("term.js: %v", err)
	}
	src := string(b)
	script := extractFunc(t, src, "cmMakeMetaFilter") + "\n" +
		extractFunc(t, src, "cmParseMeta") + "\n" + `
const fail = [];
const eq = (got, want, msg) => { if (got !== want) fail.push(msg + " got=" + JSON.stringify(got) + " want=" + JSON.stringify(want)); };
const enc = (s) => Uint8Array.from(Buffer.from(s, "latin1"));
const META = (p) => "\x1b]7707;" + p + "\x1b\\";
const FRAME = "\x1b[?2026h\x1b[2J\x1b[1;1Hlive\x1b[?2026l";

// --- 1 接続目: META → 履歴 → hist_end → frame（マーカーを chunk 境界で割る） ---
let metas = [], out = [];
const feed = cmMakeMetaFilter((p) => metas.push(p), (b) => out.push(Buffer.from(b).toString("latin1")));
const H1 = "v=2;cols=179;rows=70;trunc=1;hist=1;scrolled=0";
const stream = META(H1) + "HIST-1\r\nHIST-2\r\n\r\n" + META("hist_end") + FRAME;
const all = enc(stream);
// 3 バイトずつ刻む（OSC 先頭・ST・2026 マーカーが必ず割れる）
for (let i = 0; i < all.length; i += 3) feed(all.subarray(i, Math.min(all.length, i + 3)));
eq(metas.join("|"), H1 + "|hist_end", "META が順に onMeta へ渡る:");
eq(out.join(""), "HIST-1\r\nHIST-2\r\n\r\n" + FRAME, "1 回目は履歴もフレームも通り OSC は剥がれる:");

// --- 2 接続目（再接続）: 同じ先頭が来るが履歴は捨て、フレームは通す ---
metas = []; out = [];
feed.reset();
feed(all);
eq(metas.join("|"), H1 + "|hist_end", "再接続でも META は通知される:");
eq(out.join(""), FRAME, "再接続の履歴は捨て、フレームだけ通す:");

// --- 遅延履歴: 初回が hist=0（producer が予算内に取れず）なら次の接続の hist=1 を受け入れる ---
out = [];
const f3 = cmMakeMetaFilter(() => {}, (b) => out.push(Buffer.from(b).toString("latin1")));
f3(enc(META("v=2;cols=179;rows=70;trunc=0;hist=0;scrolled=3") + META("hist_end") + FRAME));
eq(out.join(""), FRAME, "hist=0 の接続はフレームだけ:");
out = []; f3.reset();
f3(enc(META(H1) + "LATE-1\r\n" + META("hist_end") + FRAME));
eq(out.join(""), "LATE-1\r\n" + FRAME, "hist=0 の後の hist=1 は履歴を通す:");
out = []; f3.reset();
f3(enc(META(H1) + "LATE-2\r\n" + META("hist_end") + FRAME));
eq(out.join(""), FRAME, "本文つき履歴を一度通した後は捨てる:");

// --- 旧 producer（META 無し）: 一切手を付けない（OSC 8 等の他の OSC も素通し） ---
out = [];
const legacy = "\x1b]8;;\x1b\\abc\x1b]7" + "\x1b[?2026h" + "xyz\x1b]7707";
const f2 = cmMakeMetaFilter(() => fail.push("旧 producer で onMeta が呼ばれた"), (b) => out.push(Buffer.from(b).toString("latin1")));
f2(enc(legacy));
// 末尾の "\x1b]7707" は OSC 先頭の可能性があるため carry（まだ出ない）
eq(out.join(""), "\x1b]8;;\x1b\\abc\x1b]7\x1b[?2026hxyz", "META 以外は無改変で通る（末尾の部分マーカーは保留）:");
f2(enc("x"));
eq(out.join(""), "\x1b]8;;\x1b\\abc\x1b]7\x1b[?2026hxyz\x1b]7707x", "部分マーカーが不成立なら後で吐き出す:");

// --- cmParseMeta ---
const m = cmParseMeta("v=2;cols=179;rows=70;trunc=0;hist=1;scrolled=12");
eq(m.v, 2, "v:"); eq(m.cols, 179, "cols:"); eq(m.rows, 70, "rows:"); eq(m.trunc, 0, "trunc:"); eq(m.hist, 1, "hist:"); eq(m.scrolled, 12, "scrolled:");
eq(cmParseMeta("hist_end").hist_end, true, "値なしキー:");

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

// TestTermJSTrackScroll — cmTrackScroll/cmLastContentRow の真理値表を固定する。
// 修正前（pinRow 非対応）はこの表のうち「ヒントが収まる時はヒント下端を
// 可視下端へピン」が落ちることを git stash で確認済み（カーソル行だけを
// 可視下端へピンしてしまい、下のヒント/ステータス行が見切れる＝今回の
// 報告「入力窓の下の行数がエージェントごとにバラバラなのに一番下で
// 切れる」の再現）。
func TestTermJSTrackScroll(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node が無いので skip（判定表の担保のみ・本番経路は実 relay で確認）")
	}
	b, err := os.ReadFile("static/term.js")
	if err != nil {
		t.Fatalf("term.js: %v", err)
	}
	src := string(b)

	script := extractFunc(t, src, "cmLastContentRow") + "\n" +
		extractFunc(t, src, "cmTrackScroll") + "\n" + `
const fail = [];
const eq = (got, want, msg) => { if (got !== want) fail.push(msg + " got=" + JSON.stringify(got) + " want=" + JSON.stringify(want)); };

// --- cmLastContentRow: ヒント行の本数はエージェントごとに違う前提 ---
eq(cmLastContentRow(["abc", "", "def", "", "", ""], 0), 2,
   "末尾の空行を無視し実内容の最終行を返す:");
eq(cmLastContentRow(["x", "", "", ""], 3), 3,
   "最終行が cy より上でも cy を下回らない:");
eq(cmLastContentRow([], 5), 5,
   "空配列は cy を返す:");

// cellH=10px・clientHeight=5行分(50px)・scrollHeight=500行分(5000px) の共通前提。
// cy=40（カーソル行）。

// --- ヒントが可視域に収まる場合: ヒント下端(pinRow)を可視下端へピン ---
eq(cmTrackScroll({
  mode: "settle", following: true, cy: 40, pinRow: 43,
  cellH: 10, clientHeight: 50, scrollHeight: 5000, scrollTop: 0,
}).scrollTop, 390, "ヒントが収まる時はヒント下端を可視下端へピン:");

// --- ヒントが大きすぎて収まらない場合: カーソル優先へフォールバック ---
eq(cmTrackScroll({
  mode: "settle", following: true, cy: 40, pinRow: 48,
  cellH: 10, clientHeight: 50, scrollHeight: 5000, scrollTop: 0,
}).scrollTop, 360, "収まらない時は従来どおりカーソル行を可視下端へピン:");

// --- pinRow===cy（ヒント無し）: 従来のカーソルピンと同じ結果 ---
eq(cmTrackScroll({
  mode: "settle", following: true, cy: 40, pinRow: 40,
  cellH: 10, clientHeight: 50, scrollHeight: 5000, scrollTop: 0,
}).scrollTop, 360, "pinRow=cy の時は従来のカーソルピンと同じ結果:");

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
