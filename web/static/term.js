// Web ターミナル本体（/term?pc=&sid=&dir=）。端末一覧からリンクで開く。
// relay の frame protocol をそのまま話す（無改変）。
//
// 設計（暴走の原因除去）: Web は **固定論理サイズ 160×500 の素朴な
// ビューア**。自分の DOM 寸法を一切測らず（FitAddon 不使用）、RESIZE は
// 接続時に 1 回だけ送る。ブラウザの resize/ズーム/スクロール/モバイル
// URL バー出入りでは **再 RESIZE しない**＝「測る→RESIZE→proxy 全消去
// 再描画→レイアウト変化→また測る」のフィードバック暴走を構造的に断つ。
// proxy は 160×500 の viewport を絶対座標で再描画するだけ（モデル→
// viewport のまま＝claude --resume 再ストリームでも重複しない）。背の
// 高い固定グリッドを #term-host の overflow で **ブラウザ native スク
// ロール**して読む（セル書換は scrollTop を動かさないので崩れない）。
// 横の見切れは固定広幅＋ブラウザのピンチズーム/横パンで閲覧。
// コンソール切替は ‹/› ボタン（横スワイプは native スクロールと競合
// するため廃止）。
//
// ワイヤ v2（2026-10-29）: producer（herdr-drover）が対応していれば、上の
// 固定グリッドは使わず **元 pane の実サイズ**＋xterm.js の scrollback に
// 切り替わる（cmMakeMetaFilter / onMeta）。producer は observe を元 pane の
// サイズで起動し、ストリーム先頭に META と pane.read の履歴を 1 回流す。
// observe のフレームは `ESC[2J`＋絶対座標だけで一切スクロールしない（実測）
// ので、先に書いた履歴を壊さない。「RESIZE は接続時 1 回だけ・再送しない」
// の不変条件はそのまま（サイズ合わせは全てクライアント内で完結）。
"use strict";
const $ = (id) => document.getElementById(id);
const enc = new TextEncoder();
const qs = new URLSearchParams(location.search);
const pc = qs.get("pc"), sid = qs.get("sid"), dir = qs.get("dir") || "";
// agent はセッションのエージェント種別（producer が session doc に載せた canonical
// label）。一覧から引き継ぐ。**空でも動くこと**が要件 — 旧 PC の session doc には
// このキーが無く、旧リンク（agent 無し）で開かれることもある。空なら総称へ degrade。
const agName = qs.get("agent") || "";
const agDisp = agName || "エージェント";

// 固定論理サイズ。?cols=/?rows= で上書き可（1..2000）。既定 160×500。
// cols がモデル幅以上なら横は見切れず全文到達（余りは背景空白）。
// rows ぶんの最新行を native スクロールで読める（大きいほど深く読める
// が毎フレーム cols×rows 送信で重くなる＝500 が実用バランス）。
const clampNum = (v, def) => {
  const n = parseInt(v, 10);
  return n > 0 && n <= 2000 ? n : def;
};
const WEB_COLS = clampNum(qs.get("cols"), 160);
const WEB_ROWS = clampNum(qs.get("rows"), 500);

function resizeFrame(rows, cols) {
  const b = new Uint8Array(6);
  b[0] = 0xff; b[1] = 0xff;
  b[2] = (rows >> 8) & 0xff; b[3] = rows & 0xff;
  b[4] = (cols >> 8) & 0xff; b[5] = cols & 0xff;
  return b;
}

async function jget(u) {
  const r = await fetch(u, { headers: { Accept: "application/json" } });
  if (!r.ok) throw new Error(u + " -> " + r.status);
  return r.json();
}

// cmLastContentRow: カーソル行の下に続く実内容（エージェントが出す
// ヒント/ステータス行等）の最終行を求める純粋関数（node で決定論テスト）。
// この行数はエージェントごとにバラバラ（固定マージンにできない）なので、
// 「今届いているフレームで実際に内容がある最終行」を使う。lines は各行を
// 末尾空白除去した文字列にした配列（index=行番号）。戻り値は必ず cy 以上
// （カーソル行より上に戻らない＝入力位置自体は常に可視下端の対象のまま）。
function cmLastContentRow(lines, cy) {
  for (let r = lines.length - 1; r >= 0; r--) {
    if (lines[r] !== "") return Math.max(cy, r);
  }
  return cy;
}

// cmTrackScroll: ライブ行（カーソル）とその下の実内容を **ブラウザ native
// スクロール**で追従し続ける純粋計算。proxy は bottom-fill 撤去後 *上詰め
// の短い* フレーム（内容 < 500 行・パディング無＝Python parity）を毎フレーム
// 送り、claude のストリーム/全画面再描画で内容長＝カーソル行が変動
// する。クライアントが一度しかスクロールを合わせないと:
//  ・カーソル行が固定スクロールに対し上下する＝**入力位置がズレる**
//  ・短い内容の下＝固定 500 行グリッドの空き＝**下に黒いエリア**
// ScrollRenderer の follow/scrollback 不変条件を native スクロールへ
// 適用して解消する: following 中は沈静バースト毎に実内容の下端
// （s.pinRow。カーソル行の下に出るヒント/ステータス行まで含む）を可視
// 下端へ再ピン（位置が常に一定＝ズレない・空きを見ない）／ユーザが
// 履歴へ遡り（カーソルが可視域より下に隠れる）と following 解除＝
// スクロールを一切動かさず読書を妨げない／最下部へ戻すと再 following。
// pinRow まで含めるとカーソル行自体が可視域から上に押し出される（ヒント
// 込みの全体が高さに収まらない）場合は、カーソルを可視下端へ置く従来の
// ピンへフォールバックする（入力位置が見えることを優先）。
// 純粋＝node で決定論テスト（合成でなく実 static を抽出）。
// s.mode: "settle"=沈静後の再ピン / "userscroll"=ユーザ操作後の follow
//         再判定。返り値 {scrollTop, following} を呼び元が適用する。
function cmTrackScroll(s) {
  const cellH = s.cellH > 0 ? s.cellH : 1;
  const maxTop = Math.max(0, s.scrollHeight - s.clientHeight);
  const clamp = (t) => (t < 0 ? 0 : t > maxTop ? maxTop : t);
  const cursorPx = (s.cy + 1) * cellH;               // ライブ行の下端 px
  const pinRow = s.pinRow > s.cy ? s.pinRow : s.cy;  // 常に cy 以上（防御）
  const pinPx = (pinRow + 1) * cellH;                // 実内容（ヒント込み）下端 px
  let pinTop = clamp(pinPx - s.clientHeight);
  if (pinTop > s.cy * cellH) pinTop = clamp(cursorPx - s.clientHeight); // 収まらない→カーソル優先
  if (s.mode === "userscroll") {
    // ユーザ操作の結果位置から follow を再判定（スクロールは動かさ
    // ない）。可視下端がカーソル行に届いていれば=ライブ追跡中(緑)、
    // カーソルが可視域より 1 行以上下へ隠れる程遡ったら=履歴閲覧中
    // →follow 解除。プログラム的ピン直後の scroll でも geometry は
    // viewBottom≧cursorPx となり following=true で不変＝guard 不要。
    const viewBottom = s.scrollTop + s.clientHeight;
    return { scrollTop: s.scrollTop, following: viewBottom >= cursorPx - cellH };
  }
  // mode "settle": following 中だけ下端へ再ピン。遡り中
  // (following=false)はユーザのスクロール位置を一切動かさない。
  if (s.following) return { scrollTop: pinTop, following: true };
  return { scrollTop: s.scrollTop, following: false };
}

// cmMakeMetaFilter: producer（herdr-drover ワイヤ v2）がストリーム先頭に流す
// 制御列 OSC 7707（ESC ] 7707 ; k=v;k=v ESC \）を xterm に流す前に剥がし、
// payload 文字列を onMeta に渡す純粋フィルタ（node で決定論テスト）。
//  ・`v=2;cols=C;rows=R;trunc=T` … 以降 `hist_end` までは **履歴**（producer
//    が pane.read で取った scrollback を CRLF 行＋R 行のパディングにした
//    もの）。履歴は **ページにつき 1 回だけ** emit し、再接続（quiescence
//    切断→再 wake）でまた届いても捨てる＝scrollback に重複しない。
//  ・META が来ない producer（旧 PC・↗窓）では何も起きない＝従来どおり。
// マーカーが chunk 境界で割れても carry で再結合（sync.js と同じ規律）。
// 2026 同期シムより **上流** に置く（OSC バイトを同期ブロックに混ぜない）。
// feed.reset() は接続ごとに呼ぶ（取りかけの carry/META を旧接続から継がない。
// 「履歴は 1 回だけ」の記憶は保つ）。
function cmMakeMetaFilter(onMeta, emit) {
  var OSC = [0x1b, 0x5d, 0x37, 0x37, 0x30, 0x37, 0x3b]; // ESC ] 7 7 0 7 ;
  var ST = [0x1b, 0x5c];                                 // ESC \
  var carry = new Uint8Array(0);
  var inMeta = false, acc = new Uint8Array(0);
  // inHist: v=2 META〜hist_end の間（履歴区間）。blockHasHist: その区間に
  // 本文があるか（META の hist=1）。histDone: 本文つき履歴を既に通したか＝
  // 以降の接続の履歴区間は捨てる。hist=0 の区間（producer が予算内に履歴を
  // 取れなかった接続）は histDone を立てない＝次の接続の履歴を受け入れる。
  var inHist = false, histDone = false, blockHasHist = false;
  function indexOf(d, pat, from) {
    outer:
    for (var i = from; i + pat.length <= d.length; i++) {
      for (var j = 0; j < pat.length; j++) {
        if (d[i + j] !== pat[j]) continue outer;
      }
      return i;
    }
    return -1;
  }
  function tailPrefix(d, pat) {
    var max = Math.min(pat.length - 1, d.length);
    for (var k = max; k >= 1; k--) {
      var ok = true;
      for (var j = 0; j < k; j++) {
        if (d[d.length - k + j] !== pat[j]) { ok = false; break; }
      }
      if (ok) return k;
    }
    return 0;
  }
  function concat(a, b) {
    var r = new Uint8Array(a.length + b.length);
    r.set(a, 0); r.set(b, a.length);
    return r;
  }
  function out(bytes) {
    if (bytes.length === 0) return;
    if (inHist && histDone) return; // 2 回目以降の履歴は捨てる
    emit(bytes);
  }
  function feed(chunk) {
    var data = carry.length ? concat(carry, chunk) : chunk;
    carry = new Uint8Array(0);
    var pos = 0;
    while (pos < data.length) {
      if (!inMeta) {
        var i = indexOf(data, OSC, pos);
        if (i < 0) {
          var rest = data.subarray(pos);
          var hold = tailPrefix(rest, OSC);
          var cut = rest.length - hold;
          if (cut > 0) out(rest.subarray(0, cut));
          carry = rest.subarray(cut).slice();
          return;
        }
        if (i > pos) out(data.subarray(pos, i));
        inMeta = true;
        acc = new Uint8Array(0);
        pos = i + OSC.length;
      } else {
        var j = indexOf(data, ST, pos);
        if (j < 0) {
          var rest2 = data.subarray(pos);
          var hold2 = tailPrefix(rest2, ST);
          var cut2 = rest2.length - hold2;
          if (cut2 > 0) acc = concat(acc, rest2.subarray(0, cut2));
          carry = rest2.subarray(cut2).slice();
          return;
        }
        acc = concat(acc, data.subarray(pos, j));
        var payload = String.fromCharCode.apply(null, acc);
        inMeta = false;
        pos = j + ST.length;
        if (payload.indexOf("v=") === 0) {
          inHist = true;
          blockHasHist = payload.indexOf(";hist=1") >= 0;
        } else if (payload === "hist_end") {
          inHist = false;
          if (blockHasHist) histDone = true;
        }
        onMeta(payload);
      }
    }
  }
  feed.reset = function () {
    carry = new Uint8Array(0);
    inMeta = false;
    acc = new Uint8Array(0);
    inHist = false;
  };
  return feed;
}

// cmParseMeta: `k=v;k=v` を {k: v} へ（値は数値なら number）。純粋。
function cmParseMeta(payload) {
  var o = {};
  var parts = payload.split(";");
  for (var i = 0; i < parts.length; i++) {
    var eq = parts[i].indexOf("=");
    if (eq < 0) { o[parts[i]] = true; continue; }
    var k = parts[i].slice(0, eq), v = parts[i].slice(eq + 1);
    o[k] = /^-?[0-9]+$/.test(v) ? parseInt(v, 10) : v;
  }
  return o;
}

// cmReconnectGate: 再接続して良いかの純粋判定（node で決定論テスト）。
// データ線は無通信 30s で quiescence 切断される設計（near-$0）なので、
// **閉じたら張りっぱなしに戻さない**。張り直すのは契機（Firestore push
// ＝セッション status 変化／ユーザ入力）があった時だけ。その上で:
//  ・CONNECTING(0)/OPEN(1) 中は張らない（多重接続は relay takeover の
//    無駄打ち）
//  ・失敗連打を backoff (1s×2^attempts・上限 30s) で抑える
// s: {wsState:-1|0..3, now, lastAttemptAt, attempts}
// 返り値: {connect, attempts}（connect 時は attempts+1。成功(onopen)で
// 呼び元が attempts=0 に戻す）
function cmReconnectGate(s) {
  if (s.wsState === 0 || s.wsState === 1) {
    return { connect: false, attempts: s.attempts };
  }
  const backoff = Math.min(30000, 1000 * Math.pow(2, s.attempts));
  if (s.now - s.lastAttemptAt < backoff) {
    return { connect: false, attempts: s.attempts };
  }
  return { connect: true, attempts: s.attempts + 1 };
}

// cmWatchGate: 監視再接続して良いかの純粋判定（node で決定論テスト）。
// BUG-4 の対処。「出力が再開した」を伝える push が無いので、**見ている間だけ**
// 張り直す。維持ではなく張り直しなのが要点（near-$0 の設計を壊さない）。
//   ・visible でない → 張らない（裏に回れば即コスト 0）
//   ・is_active でない → 張らない（止まった相手を起こさない）
//   ・CONNECTING(0)/OPEN(1) → 張らない（多重接続は relay takeover の無駄打ち）
// backoff は呼び先の cmReconnectGate が持つので、ここでは見ない。
// s: {visible:bool, active:bool, wsState:-1|0..3}
function cmWatchGate(s) {
  if (!s.visible) return false;
  if (!s.active) return false;
  if (s.wsState === 0 || s.wsState === 1) return false;
  return true;
}

// アカウント内の全コンソールを端末一覧と同じ順で平坦化（pc→session）。
// ‹/› ボタンで前後のコンソールへ location 遷移して切り替える。
async function buildConsoleList() {
  const devs = await jget("/api/devices");
  const list = [];
  for (const d of devs) {
    const ss = await jget("/api/sessions?pc=" + encodeURIComponent(d.id));
    for (const x of ss || []) {
      list.push({
        pc: d.id, sid: x.key,
        dir: x.short_dir || x.key || "session",
        agent: x.agent || "",
      });
    }
  }
  return list;
}

function termURL(c) {
  return "/term?pc=" + encodeURIComponent(c.pc) +
    "&sid=" + encodeURIComponent(c.sid) +
    "&dir=" + encodeURIComponent(c.dir) +
    (c.agent ? "&agent=" + encodeURIComponent(c.agent) : "") +
    (qs.get("cols") ? "&cols=" + encodeURIComponent(qs.get("cols")) : "") +
    (qs.get("rows") ? "&rows=" + encodeURIComponent(qs.get("rows")) : "");
}

// コンソール切替（前後ボタン）。一覧取得失敗時は単独表示のまま無効化。
function setupSwitch() {
  let list = [], idx = -1;
  const prevB = $("prev"), nextB = $("next");
  const go = (delta) => {
    if (idx < 0 || list.length < 2) return;
    const n = (idx + delta + list.length) % list.length; // 巡回
    location.href = termURL(list[n]);
  };
  prevB.disabled = nextB.disabled = true;
  prevB.onclick = () => go(-1);
  nextB.onclick = () => go(1);

  buildConsoleList().then((l) => {
    list = l;
    idx = list.findIndex((c) => c.pc === pc && c.sid === sid);
    if (list.length > 1) {
      prevB.disabled = nextB.disabled = false;
      if (idx >= 0) $("pos").textContent = " (" + (idx + 1) + "/" + list.length + ")";
    }
  }).catch(() => { /* 切替不可でもターミナルは使える */ });
}

// cmSemiAutoView は session doc の semi_auto と「投入中」から表示状態を決める
// （純粋関数・node ゲートで真理値表を固定）。
//   on: バッジ表示＋ボタン点灯 / pending: ボタン無効＋「…」 / next: 次に投入する on 値
function cmSemiAutoView(on, pending) {
  return {
    badge: on === true,
    lit: on === true,
    disabled: pending === true,
    label: pending === true ? "…" : "⟳",
    next: on === true ? "0" : "1",
  };
}

function run() {
  if (!pc || !sid) { $("stat").textContent = "pc/sid がありません"; return; }
  const label = dir ? (dir + " — " + pc) : (pc + " : " + sid);
  $("title").textContent = label;
  document.title = (dir || sid) + " — drover-cloud";

  setupSwitch();

  // 固定論理グリッド 160×500（scrollback:0＝xterm 自前スクロール無し。
  // proxy が viewport を絶対再描画する。背の高い要素を #term-host の
  // overflow で native スクロールする）。FitAddon は使わない（自分の
  // 寸法を測って RESIZE 逆流させると暴走するため）。
  const term = new Terminal({ cursorBlink: true, scrollback: 0,
    cols: WEB_COLS, rows: WEB_ROWS,
    fontFamily: "Menlo,Consolas,monospace", fontSize: 13 });
  term.open($("term-host"));

  // Ctrl+V はブラウザの貼り付けにする（2026-10-29 ユーザー報告）。xterm.js の
  // 既定は Ctrl+V を C0 SYN(0x16) として pane へ送る＝Claude Code では「リモート
  // のクリップボードから画像貼付」が走り、ブラウザ側のテキストは貼れない。
  // custom handler で false を返すと xterm は preventDefault せずに手を引く
  // （実コード確認: _keyDown は cancel の前に return）ので、ブラウザの native
  // paste → xterm の paste ハンドラ（bracketed paste 込み）→ onData → pane の
  // 経路に乗る。Mac の Cmd+V は元々 native paste。画像は document の paste 捕捉
  // （下）と 📷 ボタンの別経路。0x16 を本当に送りたい場面は無い（画像貼付は
  // producer が自分で Ctrl+V を注入する）。
  term.attachCustomKeyEventHandler((ev) => {
    if (ev.type === "keydown" && ev.ctrlKey && !ev.shiftKey && !ev.altKey && !ev.metaKey &&
        (ev.code === "KeyV" || ev.key === "v" || ev.key === "V")) {
      return false;
    }
    return true;
  });

  // ---- ワイヤ v2（producer が対応していれば自動で切替・2026-10-29） ----
  // 接続のたび HELLO（SCROLL dy=0x7fff。旧 PC は SCROLL を parse-and-ignore
  // するので無害）を送ると、新 producer は **元 pane の実サイズ**で observe し、
  // ストリーム先頭に META（OSC 7707）＋履歴（pane.read の scrollback）を流す。
  // Web はそれを見て xterm を元 pane のサイズに合わせ scrollback を有効化する
  // ＝「履歴を遡れる」「接続で一発描画」「RESIZE 往復なし」。META が来なければ
  // 従来の固定 160×500 グリッドのまま（旧 PC との後方互換）。
  // ⚠ サイズを producer へ送る RESIZE は従来どおり接続時 1 回だけ（不変条件）。
  //   以下の fontSize 調整は **クライアント内だけ**で完結し、RESIZE を再送しない。
  const HELLO_V2 = new Uint8Array([0xff, 0xfe, 0x7f, 0xff]);
  // v2: v2 モードに入ったか／painted: 何か描いたか／histGot: 本文つき履歴を
  // 受け取ったか（producer は長寿命 pane の pane.read が遅い（4〜16s 実測）と
  // 初回接続を hist=0 で先に描き、次の接続で hist=1 を送ってくる）。
  let v2 = false, painted = false, histGot = false;
  const fontOverride = clampNum(qs.get("font"), 0);
  const fitFont = (rows) => {
    // 元 pane の行数が **必ず** 画面に収まる fontSize を実測で決める（sticky の
    // 端末要素が host に丸ごと見えていることが、下のプロキシスクロールの前提）。
    // 下限 4px＝読めなくてもピンチズームで読む。
    const host = $("term-host");
    if (fontOverride) {
      if (term.options.fontSize !== fontOverride) term.options.fontSize = fontOverride;
      return;
    }
    let px = Math.min(13, Math.max(4, Math.floor(host.clientHeight / rows / 1.2)));
    for (; px >= 4; px--) {
      term.options.fontSize = px;
      const scr = host.querySelector(".xterm-screen");
      const h = scr ? scr.getBoundingClientRect().height : 0;
      if (h === 0 || h <= host.clientHeight) break; // 測れない環境は推定値のまま
    }
  };
  const onMeta = (payload) => {
    const m = cmParseMeta(payload);
    if (m.v === 2) {
      if (!v2) {
        v2 = true;
        term.options.scrollback = 5000;
      }
      if (m.cols > 0 && m.rows > 0 && (term.cols !== m.cols || term.rows !== m.rows)) {
        term.resize(m.cols, m.rows);
      }
      if (m.rows > 0) fitFont(m.rows);
      // 履歴の遡りは host の native スクロールのプロキシ（下記）。サイズ・フォントが
      // 確定した **後** に行px を測り直す（古い行px で写像すると最下部がずれる）。
      enableProxyScroll();
      cellPx = rowPx() || cellPx;
      updateSpacer();
      termToHost(term.buffer.active.viewportY);
      // 履歴が遅れて届く（初回 hist=0 → 次の接続で hist=1）とき、既に描いた
      // 画面の上に履歴を書くと画面が scrollback へ押し上がって重複する＝
      // 先に端末をリセットし、履歴→full frame の順で描き直す。
      // hist=1 は「予算内に取得できた」（hlines=0 でも受領済み＝scrollback が空な
      // pane。Claude Code 系は可視画面未満しか保持されず常に 0 になり得る）。
      if (m.hist === 1 && !histGot) {
        histGot = true;
        if (painted && m.hlines > 0) term.reset();
      }
      let s = "接続済（" + m.cols + "×" + m.rows;
      if (!histGot) s += "・履歴は次の接続で";
      else if (m.hist === 1 && !(m.hlines > 0)) s += "・遡れる履歴なし";
      else if (m.trunc) s += "・履歴は上限で切れています";
      else s += "・履歴付き";
      if (m.scrolled > 0) s += "・ローカルで " + m.scrolled + " 行遡り中→⤓で最新へ";
      $("stat").textContent = s + "）";
    }
  };
  // META フィルタはページで 1 つ（「履歴は 1 回だけ」を接続をまたいで記憶）。
  // 下流の同期シムは接続ごとに差し替える。
  let syncFeed = null;
  const metaFeed = cmMakeMetaFilter(onMeta, (b) => { if (syncFeed) syncFeed(b); });

  // ---- データ線（/ws）。quiescence（無通信 30s）でサーバ側から閉じる
  // 設計なので、閉鎖は異常ではない。再接続は cmReconnectGate の規律で
  // 「契機があった時だけ」: ①Firestore push（status doc 変化＝
  // セッションが動いた） ②ユーザ入力（キー/画像）。アイドル中は接続
  // ゼロ＝Cloud Run も温まらない（near-$0 維持のままネイティブの
  // WatchSessions 同等の push 復帰）。
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  let ws = null;
  let attempts = 0, lastAttemptAt = 0;
  let inputQ = [], inputQBytes = 0; // 未接続中の入力（接続後に送出）
  const QMAX = 9 << 20; // 画像 8MB + キー入力余裕
  // BUG-4: セッションが動いているか（status doc の is_active）。監視再接続の
  // 条件に使う。push が使えない環境では false のままで従来動作に degrade。
  let sessionActive = false;
  // セミオート続行の表示状態（session doc の semi_auto。投入中は pending）。
  let semiAuto = false, semiAutoPending = false;
  const renderSemiAuto = () => {
    const v = cmSemiAutoView(semiAuto, semiAutoPending);
    const badge = $("auto"); if (badge) badge.style.display = v.badge ? "" : "none";
    const b = $("semiauto"); if (b) { b.classList.toggle("on", v.lit); b.disabled = v.disabled; b.textContent = v.label; }
  };
  // 相手 PC が画像貼付を受け付けるか（status doc の image_paste）。
  // null=未確定（doc 未着 or push 無効）／true=対応／false=無効。
  // 従来は送信キューに載せただけで「Ctrl+V 注入」と表示していたが、drover は
  // 既定で IMAGE frame を破棄する＝**成功表示が嘘になっていた**。これで事実を出す。
  let imagePasteOK = null;

  const wsState = () => (ws ? ws.readyState : -1);

  const flushQ = () => {
    while (inputQ.length && ws && ws.readyState === 1) {
      const b = inputQ.shift();
      inputQBytes -= b.length;
      ws.send(b);
    }
  };

  const connect = () => {
    const sock = new WebSocket(proto + "//" + location.host + "/ws?pc=" +
      encodeURIComponent(pc) + "&sid=" + encodeURIComponent(sid));
    ws = sock;
    sock.binaryType = "arraybuffer";
    // 同期シムは **接続ごとに新規**。旧接続の取りかけ frame (acc) を
    // 新接続のバイト列と継ぎ合わせない（frame 境界の保全）。さらに
    // 全ハンドラを「自分が現役 (ws === sock) の時だけ」動かす＝古い
    // 接続の遅延イベントが新しい接続へ干渉しない（relay 側 takeover
    // 修正と同じ規律のブラウザ版）。
    syncFeed = cmMakeSyncFilter((b) => { painted = true; term.write(b, scheduleLand); });
    metaFeed.reset();
    sock.onopen = () => {
      if (ws !== sock) return;
      attempts = 0; // 成功で backoff リセット
      // HELLO（v2 宣言）→ 固定論理サイズを **接続のたび 1 回だけ** 送る
      // （完全 frame での catch-up を兼ねる）。以後 resize/ズーム/スクロール/
      // URL バーで再送しない（暴走ループを断つ核心）。v2 producer は
      // サイズ要求を無視して元 pane 実サイズで応える。
      sock.send(HELLO_V2);
      sock.send(resizeFrame(WEB_ROWS, WEB_COLS));
      flushQ();
      $("stat").textContent = "接続済";
    };
    sock.onmessage = (ev) => {
      if (ws !== sock) return;
      metaFeed(new Uint8Array(ev.data));
    };
    sock.onclose = () => {
      if (ws !== sock) return;
      $("stat").textContent = "待機（更新/入力で自動再接続）";
      // 未送出の入力が残っていれば backoff 後に自走再試行（入力を
      // 失わない）。それ以外は push/次の入力まで張らない。
      if (inputQ.length) setTimeout(() => requestConnect("input"), 1100);
    };
    sock.onerror = () => {
      if (ws !== sock) return;
      $("stat").textContent = "エラー（自動再接続待機）";
    };
  };

  const requestConnect = (reason) => {
    const r = cmReconnectGate({
      wsState: wsState(), now: Date.now(),
      lastAttemptAt, attempts,
    });
    if (!r.connect) return;
    attempts = r.attempts;
    lastAttemptAt = Date.now();
    $("stat").textContent = "再接続中…（" + reason + "）";
    connect();
  };

  // 送信（キー入力/画像）。未接続なら queue して接続を要求＝タイプ
  // すれば線が再び開く。
  const sendBytes = (b) => {
    if (ws && ws.readyState === 1) {
      ws.send(b);
      return true;
    }
    if (inputQBytes + b.length > QMAX) {
      $("stat").textContent = "未接続のため送信破棄（再接続待ち）";
      return false;
    }
    inputQ.push(b);
    inputQBytes += b.length;
    requestConnect("input");
    return true;
  };
  // ライブ行（カーソル行）を native スクロールで **追従し続ける**。
  // 固定 500 行グリッドより idle セッションの内容は短く、proxy は
  // 上詰めの短いフレームを毎フレーム送る（内容長＝カーソル行が変動）。
  // 一度しか着地しないと固定スクロールに対しカーソルが流れ「入力位置
  // ズレ」、内容下の空きグリッドが「黒いエリア」になる。沈静バースト
  // 毎に cmTrackScroll で再ピンし（位置一定＝ズレない・空きを見ない）、
  // ユーザが履歴へ遡ったら following 解除して勝手に動かさない。
  // attach 直後は 80x24 catch-up→RESIZE 後 500x160 と複数フレームが
  // 来るため、最初のフレームでなく最後から 180ms 静止で沈静確定。
  const host = $("term-host");
  let following = true, landTimer = 0;
  // 行番号は **画面（viewport）相対**。cmTrackScroll は #term-host の高さ
  // （＝xterm の画面 rows ぶん）を行数で割った px を使うので、v2 で scrollback
  // が付いても buffer 絶対行（baseY+cursorY）ではなく cursorY を使う
  // （legacy は scrollback:0＝baseY=0 で同値）。
  const cursorRow = () => {
    try { return term.buffer.active.cursorY | 0; } catch (e) { return 0; }
  };
  // pinRow: カーソル行の下に出るヒント/ステータス行（本数はエージェント
  // ごとに違う）まで含めた実内容の最終行。cmLastContentRow（純粋）に渡す
  // ため、毎回画面 rows 行ぶんの文字列配列を作る（180ms スロットル沈静時
  // のみ呼ぶので頻度は問題にならない）。画面行は baseY から数える。
  const pinRow = () => {
    try {
      const a = term.buffer.active;
      const base = a.baseY | 0;
      const lines = [];
      for (let r = 0; r < term.rows; r++) {
        const line = a.getLine(base + r);
        lines.push(line ? line.translateToString(true) : "");
      }
      return cmLastContentRow(lines, cursorRow());
    } catch (e) { return cursorRow(); }
  };
  const stateNow = (mode) => ({
    mode, following, cy: cursorRow(), pinRow: pinRow(),
    cellH: host.scrollHeight / term.rows, // 1 行 px（全高/行数）
    clientHeight: host.clientHeight,
    scrollHeight: host.scrollHeight,
    scrollTop: host.scrollTop,
  });
  // ---- v2: 履歴スクロールは host の native スクロールのプロキシ ----
  // xterm 自身のタッチ/ホイール駆動スクロール（JS で viewport 要素を動かす）は
  // スマホで当てにならない（ソフトキーボード表示中やピンチズーム中は native に
  // 視覚ビューポートが動くだけで JS へ届かない。2026-10-29 実機報告）。legacy と
  // 同じ「ブラウザ任せの native スクロール」に戻し、host のスクロール位置を
  // xterm の scrollback 行へ写像する:
  //   端末要素（term.element）を sticky で画面に固定し、その下に履歴ぶんの高さ
  //   （(buffer 行数 − rows) × 行px）のスペーサーを置く。host.scrollTop / 行px
  //   = 表示先頭行 → term.scrollToLine。逆向き（xterm 側が動く: ホイール・新規行
  //   ・scrollOnUserInput で入力時に最下部へ）は term.onScroll → host.scrollTop。
  //   xterm 自前のタッチ処理は capture で止めて二重に動かさない。
  // RESIZE は相変わらず送らない（クライアント内で完結）。
  let spacer = null, cellPx = 0, syncing = false, progUntil = 0;
  const rowPx = () => {
    const scr = host.querySelector(".xterm-screen");
    const h = scr ? scr.getBoundingClientRect().height : 0;
    return h > 0 && term.rows > 0 ? h / term.rows : 0;
  };
  // 行px は沈静時（settle）と未計測時だけ測る（履歴書込中は onScroll が行ごとに
  // 来るので、毎回 layout を起こさない）。
  const updateSpacer = () => {
    if (!spacer) return;
    if (cellPx <= 0) cellPx = rowPx();
    if (cellPx <= 0) return;
    // 端末要素が host より低い（フィット時の余り）ぶんを足して、host の最大
    // scrollTop がちょうど (buffer 行数 − rows) × 行px になるようにする。これを
    // 欠くと最下部で line が余りぶん手前（実測 13 行）になる。
    const extra = Math.max(0, term.buffer.active.length - term.rows) * cellPx;
    const slack = Math.max(0, host.clientHeight - cellPx * term.rows);
    const px = Math.round(extra + slack) + "px";
    if (spacer.style.height !== px) spacer.style.height = px;
  };
  const hostToTerm = () => {
    if (!spacer || syncing || cellPx <= 0) return;
    // 自分で scrollTop を置いた直後に（非同期で）届く scroll イベントは無視する。
    // 履歴書込中はスペーサーが伸びる途中の古い位置で届くため、拾うと xterm を
    // 上へ引き戻して「最下部追従」が外れる（実測）。
    if (performance.now() < progUntil) return;
    const line = Math.round(host.scrollTop / cellPx);
    if (line !== term.buffer.active.viewportY) {
      syncing = true;
      try { term.scrollToLine(line); } finally { syncing = false; }
    }
  };
  const termToHost = (ydisp) => {
    if (!spacer || syncing) return;
    updateSpacer();
    if (cellPx <= 0) return;
    const top = Math.round(ydisp * cellPx);
    if (Math.abs(host.scrollTop - top) >= 1) {
      syncing = true;
      progUntil = performance.now() + 150;
      try { host.scrollTop = top; } finally { syncing = false; }
    }
  };
  function enableProxyScroll() {
    if (spacer) return;
    term.element.style.position = "sticky";
    term.element.style.top = "0";
    spacer = document.createElement("div");
    spacer.id = "sb-spacer";
    host.appendChild(spacer);
    // xterm 自前のタッチ/ホイール処理（JS で viewport 要素を動かす）を止め、
    // host の native スクロールに一本化する（capture で止めるだけ＝preventDefault
    // はしないので native スクロールは進む）。
    for (const t of ["touchstart", "touchmove", "touchend", "touchcancel", "wheel"]) {
      host.addEventListener(t, (e) => e.stopPropagation(), { capture: true, passive: true });
    }
    term.onScroll((ydisp) => termToHost(ydisp));
    updateSpacer();
  }

  // 沈静バースト後の再ピン（legacy: following 中のみスクロールを動かす。v2 は
  // ピンせずスペーサーの高さだけ追従）。
  const settle = () => {
    if (v2) {
      cellPx = rowPx() || cellPx;
      updateSpacer();
      termToHost(term.buffer.active.viewportY); // 行px が変わっていれば host を合わせ直す
      return;
    }
    const r = cmTrackScroll(stateNow("settle"));
    following = r.following;
    if (r.scrollTop !== host.scrollTop) host.scrollTop = r.scrollTop;
  };
  const scheduleLand = () => {
    clearTimeout(landTimer);
    landTimer = setTimeout(
      () => requestAnimationFrame(() => requestAnimationFrame(settle)),
      180); // 最後のフレームから静止したら沈静確定（idle: RESIZE 後すぐ）
  };
  setTimeout(settle, 4000); // 連続出力で沈静しなくても 4s で 1 回着地
  // ユーザの native スクロールから follow 状態だけ再判定（スクロール
  // 位置は動かさない）。内容成長は scrollTop を変えず scroll を発火
  // しない＝このハンドラはピン代入かユーザ操作のみ＝geometry で安全。
  host.addEventListener("scroll", () => {
    if (v2) { hostToTerm(); return; }
    const r = cmTrackScroll(stateNow("userscroll"));
    following = r.following;
  }, { passive: true });
  // 初回接続（同期シム cmMakeSyncFilter は connect() 内で接続ごとに
  // 新規生成: ws メッセージ境界でマーカーが割れても carry で再結合し、
  // 旧接続の取りかけ frame と新接続を継ぎ合わせない）。
  lastAttemptAt = Date.now();
  connect();

  // Firestore 更新 push: ネイティブの WatchSessions（snapshot listener）
  // と同型。relay が cookie 認証済みオーナーへ発行する custom token
  // （uid=cm-owner・全端末共通・rules で pcs/** read-only）で Firebase
  // に直結し、このセッションの status doc 変化＝「セッションが動いた」
  // を push で受けて切断中なら自動再接続する。未設定/失敗時は従来
  // どおり（push 無し・手動リロード）に degrade。
  (async () => {
    try {
      const r = await fetch("/api/fbtoken", { headers: { Accept: "application/json" } });
      if (!r.ok) return; // 未設定（404）等 → push なしで従来動作
      const { token, config } = await r.json();
      const base = "https://www.gstatic.com/firebasejs/11.6.1/";
      const [appM, authM, fsM] = await Promise.all([
        import(base + "firebase-app.js"),
        import(base + "firebase-auth.js"),
        import(base + "firebase-firestore.js"),
      ]);
      const app = appM.initializeApp(config);
      await authM.signInWithCustomToken(authM.getAuth(app), token);
      const db = fsM.getFirestore(app);
      fsM.onSnapshot(fsM.doc(db, "pcs", pc, "sessions", sid), (snap) => {
        // BUG-4: doc の is_active を拾って監視再接続の条件にする。
        // 「動いている」→ 見ている間は張り直す／「止まった」→ 張らない。
        try {
          const d = (snap && typeof snap.data === "function") ? snap.data() : null;
          if (d) {
            sessionActive = d.is_active === true;
            // producer は有効な時だけ image_paste を載せる（無効は「載せない」
            // で表現＝後方互換）。doc が来た時点で false は確定情報。
            imagePasteOK = d.image_paste === true;
            // semi_auto も同じ規律（ON の pane にだけ true が載る）。
            semiAuto = d.semi_auto === true;
            semiAutoPending = false;
            renderSemiAuto();
          }
        } catch (e) { /* 取れなくても push 再接続は従来どおり効く */ }
        requestConnect("更新push");
      });
    } catch (e) { /* push なしでも従来どおり動く */ }
  })();

  // ---- BUG-4: 「出力が再開した」を伝える経路が無い問題への対処。
  // status doc は content_hash が変わった時しか書かれず、その中身に出力は
  // 含まれない（producer が揮発フィールドを載せないのは near-$0 の設計）。
  // よってエージェントが working のまま 30s 以上黙る（＝思考中）と PC 側
  // bridge の DefaultIdle=30s で線が閉じ、**出力が再開しても push が飛ばない**
  // ＝ブラウザは張り直さない。ユーザから見ると「途中でフッと止まる」。
  //
  // 方針は「接続を維持する」ではなく「**見ている間だけ張り直す**」。
  //   ・タブが visible でない → 何もしない（裏に回れば即座にコスト 0）
  //   ・セッションが is_active でない → 何もしない（止まった相手は起こさない）
  // 上記 2 条件を満たす時だけ再接続を試みる＝コストが出るのは
  // 「人が実際に動いているセッションを見ている間」に限られる。
  // 実際の再接続頻度は PC 側 idle(30s) 律速で概ね 30s に 1 回。
  // 張り直し自体は既存経路のまま: /ws ハンドラが st.Wake を撃って PC を
  // 起こし、onopen の resizeFrame が完全 frame の catch-up を兼ねる。
  // 連打は cmReconnectGate の backoff が抑える。
  const WATCH_TICK_MS = 5000;
  setInterval(() => {
    if (cmWatchGate({
      visible: document.visibilityState === "visible",
      active: sessionActive,
      wsState: wsState(),
    })) requestConnect("watch");
  }, WATCH_TICK_MS);

  // タブを表に戻した瞬間は tick を待たずに張り直す（裏で切れたまま戻ると
  // 最大 WATCH_TICK_MS 待たされるため）。is_active 不明でも復帰させたいので
  // ここは visible だけを条件にする（1 回きり＝連打にならない）。
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible") requestConnect("visible");
  });

  term.onData((d) => { sendBytes(enc.encode(d)); });

  // 「Esc」中断ボタン: Esc のバイト(0x1b=ESC)を入力経路へ送る。claude の
  // 「esc to interrupt」＝生成中断に対応。モバイルには Esc キーが無く、
  // ブラウザでも打ちづらいので専用ボタンで補う（サーバ無改変＝term.onData
  // と同じ sendBytes 経路）。送信後はターミナルにフォーカスを戻す。
  const intrB = $("intr");
  if (intrB) {
    intrB.onclick = () => {
      sendBytes(new Uint8Array([0x1b]));
      try { term.focus(); } catch (e) { /* focus 無くても送信は成立 */ }
    };
  }

  // フローティング操作パッド（ESC＋十字キー）。モバイル等で物理キーが
  // 無くても矢印/ESC を送れる。各ボタンは sendBytes へ（term.onData と同
  // 入力経路・サーバ無改変）。矢印は CSI 標準シーケンス（ESC[A/B/C/D。
  // claude=Ink は CSI/SS3 両対応）。position:fixed なので native スクロール
  // に追従し、grip ドラッグで移動・位置は localStorage に記憶する。
  //
  // shiftLeft: codex 操作に Shift+← が要る（2026-09-28 ユーザー報告）。
  // 修飾キー付き矢印は CSI の拡張形 ESC[1;<mod>D（mod=2 が Shift）。
  // ブラウザで Shift+← を打つと native の範囲選択に食われて term まで届かない
  // ことがある＝ボタンで確実に送る。
  //
  // shiftTab: 「モード切替」ボタン（2026-09-28 ユーザー確認: Shift+Tab。
  // Claude Code の plan-mode 切替と同じ割当で、codex 側にも承認モード等の
  // 切替が Shift+Tab に割り当たっていることが多い）。Shift+Tab は
  // Back Tab の標準 CSI（ESC[Z）で送る＝拡張 modifier 形ではなく Tab 専用の
  // 確立した慣習（vim/readline 等が同じ形を認識）。ブラウザは Tab 系を
  // フォーカス移動に使うため、素の Shift+Tab はページ側に食われがち
  // ＝これもボタンで確実に送る。
  const PAD_KEYS = {
    esc: [0x1b],
    up: [0x1b, 0x5b, 0x41], // ESC [ A
    down: [0x1b, 0x5b, 0x42], // ESC [ B
    right: [0x1b, 0x5b, 0x43], // ESC [ C
    left: [0x1b, 0x5b, 0x44], // ESC [ D
    shiftLeft: [0x1b, 0x5b, 0x31, 0x3b, 0x32, 0x44], // ESC [ 1 ; 2 D
    shiftTab: [0x1b, 0x5b, 0x5a], // ESC [ Z (Back Tab)
    // enter: CR（0x0d）。LF(0x0a) ではない＝herdr 側 pane.send_text と同じ
    // 慣習（DESIGN 実測: \n だと改行にならず送信されない agent がある）。
    enter: [0x0d],
    // bottom（⤓ 最新へ）: SCROLL フレーム dy=0x7ffe（ワイヤ v2 の予約値）。
    // 元 pane がローカルで遡り表示のままだと observe はその位置を描くので Web
    // から最新行が見えない。これを送ると producer が pane.scroll 0 を実行する
    // （共有状態＝ローカル表示も最下部へ戻る。だから明示ボタンのみ）。旧 PC は
    // SCROLL を無視する。
    bottom: [0xff, 0xfe, 0x7f, 0xfe],
  };
  const pad = $("cmpad"), grip = $("cmpad-grip");
  if (pad) {
    pad.querySelectorAll("button[data-k]").forEach((b) => {
      b.addEventListener("click", () => {
        const seq = PAD_KEYS[b.getAttribute("data-k")];
        if (seq) {
          sendBytes(new Uint8Array(seq));
          try { term.focus(); } catch (e) { /* 送信は成立 */ }
        }
      });
    });
    // reload: 端末へは何も送らず、Webページ自体をリロードする
    // （F5相当。pane/セッションには無関係＝接続し直しが必要な時用）。
    pad.querySelectorAll('button[data-action="reload"]').forEach((b) => {
      b.addEventListener("click", () => { location.reload(); });
    });
    // semiauto（⟳）: セミオート続行の ON/OFF を遠隔命令 semi-auto で切り替える。
    // 表示は session doc の semi_auto（onSnapshot）に従う＝投入後は daemon が
    // 即 re-scan して doc を更新するまで「…」で待つ（楽観更新はしない）。
    const saB = $("semiauto");
    if (saB) {
      saB.addEventListener("click", async () => {
        if (semiAutoPending) return;
        const v = cmSemiAutoView(semiAuto, false);
        semiAutoPending = true;
        renderSemiAuto();
        try {
          const body = new URLSearchParams({ pc, cmd: "semi-auto", sid, on: v.next });
          const r = await fetch("/api/command", {
            method: "POST", headers: { Accept: "application/json",
              "Content-Type": "application/x-www-form-urlencoded" },
            body: body.toString(),
          });
          if (r.status === 401) { location.href = "/login"; return; }
          if (!r.ok) throw new Error("投入失敗 " + r.status);
          $("stat").textContent = "semi-auto " + (v.next === "1" ? "ON" : "OFF") + " 投入（反映まで数秒）";
          // doc 更新が来なければ 15s でボタンを戻す（古い PC は命令を知らない）。
          setTimeout(() => { if (semiAutoPending) { semiAutoPending = false; renderSemiAuto();
            $("stat").textContent = "semi-auto: PC 側が応答しません（herdr-drover が古い可能性）"; } }, 15000);
        } catch (e) {
          semiAutoPending = false; renderSemiAuto();
          if (e.message !== "unauth") alert("エラー: " + e.message);
        }
      });
    }
    // 位置の復元（保存済みなら left/top 絶対指定へ切替）。viewport 外に
    // ならないよう clamp（端末回転/リサイズで枠外固定を防ぐ）。
    const clampPad = (x, y) => [
      Math.max(0, Math.min(x, window.innerWidth - pad.offsetWidth)),
      Math.max(0, Math.min(y, window.innerHeight - pad.offsetHeight)),
    ];
    const placePad = (x, y) => {
      const [cx, cy] = clampPad(x, y);
      pad.style.left = cx + "px"; pad.style.top = cy + "px";
      pad.style.right = "auto"; pad.style.bottom = "auto";
    };
    try {
      const p = JSON.parse(localStorage.getItem("cm-pad-pos") || "null");
      if (p && typeof p.left === "number") placePad(p.left, p.top);
    } catch (e) { /* 既定の右下のまま */ }
    // grip ドラッグで移動（pointer events＝マウス/タッチ統一）。ボタンは
    // grip と別要素なのでタップ/ドラッグの取り違えは起きない。
    if (grip) {
      let dragging = false, sx = 0, sy = 0, ox = 0, oy = 0;
      grip.addEventListener("pointerdown", (e) => {
        dragging = true;
        try { grip.setPointerCapture(e.pointerId); } catch (er) { /* 無くても可 */ }
        const r = pad.getBoundingClientRect();
        ox = r.left; oy = r.top; sx = e.clientX; sy = e.clientY;
        placePad(ox, oy); // right/bottom 指定→left/top 指定へ確定
        e.preventDefault();
      });
      grip.addEventListener("pointermove", (e) => {
        if (!dragging) return;
        placePad(ox + (e.clientX - sx), oy + (e.clientY - sy));
      });
      const endDrag = () => {
        if (!dragging) return;
        dragging = false;
        try {
          localStorage.setItem("cm-pad-pos", JSON.stringify({
            left: parseInt(pad.style.left, 10),
            top: parseInt(pad.style.top, 10),
          }));
        } catch (e) { /* 記憶できなくても動作は継続 */ }
      };
      grip.addEventListener("pointerup", endDrag);
      grip.addEventListener("pointercancel", endDrag);
    }
  }

  // 画像送信: Blob を IMAGE フレーム(0xff 0xfd|u32 len|u8 ext|bytes)で
  // proxy へ。proxy がリモートホストのクリップボードへ載せ Ctrl+V 注入
  // で claude に添付（パス文字列では添付不可＝実機確定）。サーバ側
  // WebImagePaste 既定 off の時は無視される。
  const extOf = { "image/png": 1, "image/jpeg": 2, "image/gif": 3 };
  const sendImageBlob = async (blob) => {
    if (!blob) return false;
    const code = extOf[blob.type];
    if (!code) { $("stat").textContent = "未対応画像形式(" + blob.type + ")"; return false; }
    const buf = new Uint8Array(await blob.arrayBuffer());
    if (buf.length === 0 || buf.length > (8 << 20)) {
      $("stat").textContent = "画像サイズ超過/空"; return false;
    }
    const fr = new Uint8Array(7 + buf.length);
    fr[0] = 0xff; fr[1] = 0xfd;
    fr[2] = (buf.length >>> 24) & 0xff;
    fr[3] = (buf.length >>> 16) & 0xff;
    fr[4] = (buf.length >>> 8) & 0xff;
    fr[5] = buf.length & 0xff;
    fr[6] = code;
    fr.set(buf, 7);
    // 相手が受け付けないと分かっているなら送らない（8MB を無駄に流さない）。
    // ⚠ 分かっている時だけ止める。imagePasteOK===null（doc 未着／push 無効）は
    //   「対応していない」証拠ではないので送る＝機能を勝手に塞がない。
    if (imagePasteOK === false) {
      $("stat").textContent =
        "この PC は画像貼付が無効です（DROVER_WEB_IMAGE_PASTE=true で有効化。共用 PC は不可）";
      return false;
    }
    if (!sendBytes(fr)) return false;
    // 送信できたことしか分からない。相手が処理したかまでは名乗らない
    // （従来はここで常に「Ctrl+V 注入」と表示していた＝嘘だった）。
    $("stat").textContent = imagePasteOK === true
      ? "画像を送信（リモートで Ctrl+V 注入）"
      : "画像を送信（相手の対応状況は未確認）";
    return true;
  };

  // デスクトップ: paste(⌘V/Ctrl+V) で捕捉（モバイルは paste イベントが
  // 画像を渡さないので下のボタン経路を使う）。
  document.addEventListener("paste", async (e) => {
    const items = (e.clipboardData && e.clipboardData.items) || [];
    for (const it of items) {
      if (it.kind === "file" && it.type.indexOf("image/") === 0) {
        e.preventDefault();
        await sendImageBlob(it.getAsFile());
        return;
      }
    }
  }, true);

  // モバイル/汎用: 「📷」ボタン → まず Clipboard API(read)、不可なら
  // 写真ピッカー(<input type=file accept=image/*> capture 無し)。
  // iOS Safari/Android Chrome は paste では画像不可だがこの経路は可。
  const imgBtn = $("img"), imgFile = $("imgfile");
  if (imgBtn && imgFile) {
    imgBtn.onclick = async () => {
      try {
        if (navigator.clipboard && navigator.clipboard.read) {
          const list = await navigator.clipboard.read();
          for (const it of list) {
            const t = it.types.find((x) => x.indexOf("image/") === 0);
            if (t) { await sendImageBlob(await it.getType(t)); return; }
          }
        }
      } catch (e) { /* 権限拒否/未対応 → ピッカーへ */ }
      imgFile.click(); // 写真/カメラから選択（モバイル確実経路）
    };
    imgFile.onchange = async () => {
      const f = imgFile.files && imgFile.files[0];
      if (f) await sendImageBlob(f);
      imgFile.value = "";
    };
  }

  // 「再起動」: このセッションを restart-proxy（--resume で別プロセス
  // 復帰）。**全セッションに表示**。pid- は backend が claude の jsonl
  // から会話 UUID を自動解決して復帰（解決不可は履歴にエラー＝kill
  // せず保全）。既存 owner 限定 POST /api/command を再利用（無改変）。
  const rstB = $("restart");
  if (rstB) {
    rstB.style.display = "";
    rstB.onclick = async () => {
      if (!confirm((dir || sid) + "\n現在の claude を終了し --resume で別" +
        "プロセスとして復帰します。\nこの画面/元の端末には自動では戻り" +
        "ません（復帰後あらためて開いてください）。\nよろしいですか？")) return;
      rstB.disabled = true;
      try {
        const body = new URLSearchParams({ pc, cmd: "restart-proxy", sid });
        const r = await fetch("/api/command", {
          method: "POST", headers: { Accept: "application/json",
            "Content-Type": "application/x-www-form-urlencoded" },
          body: body.toString(),
        });
        if (r.status === 401) { location.href = "/login"; return; }
        if (!r.ok) throw new Error("投入失敗 " + r.status);
        $("stat").textContent = "restart-proxy 投入（復帰は別プロセス・履歴で監査）";
      } catch (e) {
        if (e.message !== "unauth") alert("エラー: " + e.message);
      } finally { rstB.disabled = false; }
    };
  }

  // 「⟳」: このセッションを**新しいエージェントバイナリ**で作り直す
  // （restart-agent-session）。exec 済みプロセスは ~/.local/bin/<agent> の symlink
  // 張替えを追わないので、本体を更新したら pane を作り直して初めて新版になる。
  // 会話は herdr が持つ会話 ref の resume 引数で継続（形はエージェントごとに違う）。
  // 上の「復帰」(restart-proxy)は bridge を張り直すだけでエージェントプロセスは
  // 触らない＝別物。
  // 文言は agName（クエリ由来・空なら総称）で動的化＝別エージェントに
  // 「claude」と言い切らない。
  const rcB = $("rstagent");
  if (rcB) {
    rcB.style.display = "";
    rcB.textContent = "⟳" + (agName || "");
    rcB.title = agDisp + " を新しいバイナリで再起動（会話は resume で継続）";
    rcB.onclick = async () => {
      if (!confirm((dir || sid) + "\nこの " + agDisp + " セッションを会話を引き継いだ" +
        "まま作り直し、新しい " + agDisp + " バイナリを読ませます。\n" +
        "作業中なら実行されません（履歴に skip として残ります）。\n" +
        "よろしいですか？")) return;
      rcB.disabled = true;
      try {
        const body = new URLSearchParams({ pc, cmd: "restart-agent-session", sid, agent: agName });
        const r = await fetch("/api/command", {
          method: "POST", headers: { Accept: "application/json",
            "Content-Type": "application/x-www-form-urlencoded" },
          body: body.toString(),
        });
        if (r.status === 401) { location.href = "/login"; return; }
        if (!r.ok) throw new Error("投入失敗 " + r.status);
        $("stat").textContent = "restart-agent-session 投入（結果は履歴で監査）";
      } catch (e) {
        if (e.message !== "unauth") alert("エラー: " + e.message);
      } finally { rcB.disabled = false; }
    };
  }

  // window resize / ズーム / スクロール / URL バーでは **何もしない**
  // （意図的にハンドラ無し＝RESIZE 逆流の暴走を構造的に防止）。
}
run();
