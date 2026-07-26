package selfupdate

// cm selfupdate_test.go のパターン移植（ヘッダ契約・httpErr）＋
// ローカル HTTP fixture での Update 全経路検証（実 HTTP・実ファイル置換・
// sha256 検証。実 GitHub には一切出ない＝seam apiBase/dlBase/osExecutable）。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// setGHHeaders 必須挙動: UA 明示・Accept・GITHUB_TOKEN 有無で
// Authorization を自動制御。UA 未設定 = GitHub が即 403 で拒否する規約
// への境界（cm 実環境で `更新` が 403 を 3 連発した真因の固定回帰）。
func TestSetGHHeadersDefault(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	setGHHeaders(req)
	if got := req.Header.Get("User-Agent"); got != UserAgent {
		t.Fatalf("UA: got=%q want=%q", got, UserAgent)
	}
	if got := req.Header.Get("Accept"); got != "application/vnd.github+json" {
		t.Fatalf("Accept: got=%q want=%q", got, "application/vnd.github+json")
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("GITHUB_TOKEN 未設定で Authorization 付与: %q", got)
	}
}

func TestSetGHHeadersWithToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "abc123")
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	setGHHeaders(req)
	if got := req.Header.Get("Authorization"); got != "Bearer abc123" {
		t.Fatalf("Authorization: got=%q want=%q", got, "Bearer abc123")
	}
}

// httpErr は body 先頭 256B を error に含める＝`403 Forbidden` だけでは
// rate limit / UA 拒否 / 権限を判別不能だった cm 教訓の境界。
func TestHttpErrIncludesBody(t *testing.T) {
	resp := &http.Response{
		Status: "403 Forbidden",
		Body:   http.NoBody, // 空 body でも prefix/status は出る
	}
	err := httpErr("github api", resp)
	if err == nil {
		t.Fatal("error が nil")
	}
	s := err.Error()
	if !strings.Contains(s, "github api") || !strings.Contains(s, "403 Forbidden") || !strings.Contains(s, "body=") {
		t.Fatalf("error 形式が想定外: %q", s)
	}
}

// fixtureServer はローカル HTTP fixture（GitHub API/Releases の HTTP 契約
// 準拠のパス構成）を立て、seam を差し替える。checksums は引数で改竄可能に
// して sha256 検証の負経路も同じ器で通す。
func fixtureServer(t *testing.T, tag string, bin []byte, checksums string) (gotUA, gotAccept *string) {
	t.Helper()
	t.Setenv("DROVER_REPO", "") // 既定 Repo（4noha/herdr-drover）を強制
	name := assetName()
	var ua, accept string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/4noha/herdr-drover/releases/latest",
		func(w http.ResponseWriter, r *http.Request) {
			ua = r.Header.Get("User-Agent")
			accept = r.Header.Get("Accept")
			fmt.Fprintf(w, `{"tag_name":%q}`, tag)
		})
	mux.HandleFunc("/4noha/herdr-drover/releases/latest/download/"+name,
		func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(bin) })
	mux.HandleFunc("/4noha/herdr-drover/releases/latest/download/checksums.txt",
		func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, checksums) })
	ts := httptest.NewServer(mux)
	oldAPI, oldDL := apiBase, dlBase
	apiBase, dlBase = ts.URL, ts.URL
	t.Cleanup(func() {
		apiBase, dlBase = oldAPI, oldDL
		ts.Close()
	})
	return &ua, &accept
}

// seamProbeOK は「置換前の実行可否チェック」を成功側へ固定する。fixture の
// 新バイナリはただのテキスト（実行できない）なので、置換経路そのものを検証
// したいテストではここを差し替える。チェック本体の検証は
// TestProbeExecutableRealBinaries、チェックが働いて中止することの検証は
// TestUpdateAbortsWhenNewBinaryCannotRun が担う。
func seamProbeOK(t *testing.T) {
	t.Helper()
	old := probeExec
	probeExec = func(string) error { return nil }
	t.Cleanup(func() { probeExec = old })
}

// seamExecutable は置換先を一時ファイルに向ける（実行中テストバイナリを
// 書き換えない）。旧内容 0755 で作る＝実バイナリ配置と同型。
func seamExecutable(t *testing.T, oldContent []byte) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "herdr-drover")
	if err := os.WriteFile(exe, oldContent, 0o755); err != nil {
		t.Fatalf("exe fixture: %v", err)
	}
	oldExec := osExecutable
	osExecutable = func() (string, error) { return exe, nil }
	t.Cleanup(func() { osExecutable = oldExec })
	return exe
}

func TestUpdateViaLocalFixture(t *testing.T) {
	newBin := []byte("NEW-BINARY-v9.9.9\n")
	sum := sha256.Sum256(newBin)
	sums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), assetName())
	ua, accept := fixtureServer(t, "v9.9.9", newBin, sums)
	exe := seamExecutable(t, []byte("OLD-BINARY-v0.1.0\n"))
	seamProbeOK(t)

	tag, updated, err := Update("v0.1.0")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if tag != "v9.9.9" || !updated {
		t.Fatalf("tag=%q updated=%v（v9.9.9/true のはず）", tag, updated)
	}
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("read exe: %v", err)
	}
	if string(got) != string(newBin) {
		t.Fatalf("バイナリが置換されていない: %q", got)
	}
	// 実行権限は POSIX のみの概念（Windows は拡張子で実行可否が決まり、Go の
	// FileMode は 0666/0444 しか返さない）＝unix でだけ厳密に検査する。
	// silent に落とさないよう Windows では検査しない理由をログに残す。
	fi, _ := os.Stat(exe)
	if runtime.GOOS == "windows" {
		t.Logf("Windows: POSIX 実行権限は無い（mode=%v）＝置換内容のみで担保", fi.Mode().Perm())
	} else if fi.Mode().Perm() != 0o755 {
		t.Fatalf("実行権限が %v（0755 のはず）", fi.Mode().Perm())
	}
	// HTTP 契約: 固有 UA と Accept が fixture に実到達している。
	if *ua != UserAgent || *accept != "application/vnd.github+json" {
		t.Fatalf("GitHub ヘッダ契約が守られていない: UA=%q Accept=%q", *ua, *accept)
	}

	// 既に最新（tag 一致・v prefix 正規化）→ 置換しない。
	tag2, updated2, err := Update("9.9.9")
	if err != nil || tag2 != "v9.9.9" || updated2 {
		t.Fatalf("既に最新の判定が壊れている: tag=%q updated=%v err=%v", tag2, updated2, err)
	}
}

func TestUpdateChecksumMismatchRejects(t *testing.T) {
	newBin := []byte("NEW-BINARY-evil\n")
	// 改竄 fixture: checksums は別内容の sha256 を返す。
	bogus := sha256.Sum256([]byte("something else"))
	sums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(bogus[:]), assetName())
	fixtureServer(t, "v9.9.9", newBin, sums)
	oldContent := []byte("OLD-BINARY-v0.1.0\n")
	exe := seamExecutable(t, oldContent)

	_, _, err := Update("v0.1.0")
	if err == nil || !strings.Contains(err.Error(), "sha256 不一致") {
		t.Fatalf("sha256 検証が拒否しない: err=%v", err)
	}
	got, _ := os.ReadFile(exe)
	if string(got) != string(oldContent) {
		t.Fatalf("検証失敗なのにバイナリが書き換わっている: %q", got)
	}
}

func TestExpectedSHAExactMatch(t *testing.T) {
	sums := []byte("aaaa  other_asset\nBBBB  " + assetName() + "\n")
	sha, ok := expectedSHA(sums, assetName())
	if !ok || sha != "bbbb" {
		t.Fatalf("expectedSHA: sha=%q ok=%v（小文字化・名前 exact-match のはず）", sha, ok)
	}
	if _, ok := expectedSHA(sums, "no_such"); ok {
		t.Fatalf("不在 asset で ok=true")
	}
}

// ============ 置換前の実行可否チェック（2026-07-26 の実障害の再発防止） ============

// 実障害: Windows の Smart App Control が**配布バイナリの実行**をブロックする
// 環境で self-update が走り、sha256 一致の正しいバイナリで稼働中バイナリを
// 上書きした結果、**新バイナリが起動できず daemon が消えた**（タスク
// スケジューラは logon トリガのみ＝誰も復帰させない）。sha256 検証では
// 「正しいが**この環境では動かない**」を防げないのが要点。
//
// 本テストは probeExec を差し替えず **実プロセス起動**で検証する。
func TestProbeExecutableRealBinaries(t *testing.T) {
	// (a) 実行できるファイル: 自テストバイナリを「1 件も走らせない」引数で起動。
	//     実際に CreateProcess/execve が通ることを確かめる（合成しない）。
	oldArgs := probeArgs
	probeArgs = []string{"-test.run=^$"}
	t.Cleanup(func() { probeArgs = oldArgs })
	if err := probeExecutable(os.Args[0]); err != nil {
		t.Fatalf("実行できるバイナリを弾いた（偽陽性＝更新が永久に止まる）: %v", err)
	}

	// (b) 実行できないファイル: 実行ビットを立てたテキスト（unix=exec format
	//     error / windows=有効な PE でない）。旧コードはこれを place していた。
	bad := filepath.Join(t.TempDir(), "not-a-binary.exe")
	if err := os.WriteFile(bad, []byte("#not an executable\x00\x01\x02"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := probeExecutable(bad)
	if err == nil {
		t.Fatalf("実行できないファイルを通した（この穴が daemon 消失事故の本体）")
	}
	if !strings.Contains(err.Error(), "起動確認に失敗") {
		t.Fatalf("原因が伝わらないエラー文: %v", err)
	}
}

// 起動確認に失敗したら **place しない**＝稼働中バイナリは無傷のまま更新だけ
// 諦めること（旧コードは上書きしてしまい復帰不能だった）。
func TestUpdateAbortsWhenNewBinaryCannotRun(t *testing.T) {
	newBin := []byte("NEW-BINARY-v9.9.9\n")
	sum := sha256.Sum256(newBin)
	sums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), assetName())
	fixtureServer(t, "v9.9.9", newBin, sums)
	oldContent := []byte("OLD-BINARY-v0.1.0\n")
	exe := seamExecutable(t, oldContent)

	// 実環境の「SAC が実行を拒否する」に相当（sha256 は一致している＝
	// 検証段では通ってしまう経路）。
	old := probeExec
	probeExec = func(string) error { return fmt.Errorf("Application Control policy") }
	t.Cleanup(func() { probeExec = old })

	tag, updated, err := Update("v0.1.0")
	if err == nil {
		t.Fatalf("起動できない新バイナリで更新が成功扱いになった: tag=%q updated=%v", tag, updated)
	}
	if updated {
		t.Fatalf("updated=true になっている（置換していないのに更新済みと報告）")
	}
	if !strings.Contains(err.Error(), "稼働中のバイナリは無傷") {
		t.Fatalf("何が起きたか伝わらないエラー文: %v", err)
	}
	got, _ := os.ReadFile(exe)
	if string(got) != string(oldContent) {
		t.Fatalf("起動確認に失敗したのに稼働中バイナリが書き換わった（事故の再発）: %q", got)
	}
	// 一時ファイルを置き去りにしない（defer os.Remove の経路が生きていること）。
	entries, _ := os.ReadDir(filepath.Dir(exe))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".herdr-drover-new-") {
			t.Fatalf("一時ファイルが残っている: %s", e.Name())
		}
	}
}

// ── 最新タグ解決の API 非依存化（2026-07-26）────────────────────────────
//
// api.github.com は匿名 60 回/時・**IP 単位**なので、同じ回線に複数台の PC が
// ぶら下がっていると連続リリース時に枯れる（実障害: 1 日 6 リリース × fleet 5 台で
// `403 API rate limit exceeded`）。共用 PC を含む fleet に GITHUB_TOKEN を配るのは
// 採らないので、**API を使わない経路を主**にした。

// TestTagFromReleaseURL は Location からタグを取り出す純関数のテーブル。
// ⚠ 想定外の URL で**黙って何かを返さない**ことが要（誤ったタグで更新判定をしない）。
func TestTagFromReleaseURL(t *testing.T) {
	for _, c := range []struct {
		name, in, want string
		ok             bool
	}{
		{"通常", "https://github.com/o/r/releases/tag/v0.5.33", "v0.5.33", true},
		{"クエリ付き", "https://github.com/o/r/releases/tag/v1.2.3?x=1", "v1.2.3", true},
		{"フラグメント付き", "https://github.com/o/r/releases/tag/v1.2.3#a", "v1.2.3", true},
		{"末尾スラッシュ", "https://github.com/o/r/releases/tag/v1.2.3/", "v1.2.3", true},
		{"タグ部が空", "https://github.com/o/r/releases/tag/", "", false},
		{"marker 無し（想定外の飛び先）", "https://github.com/login", "", false},
		{"空", "", "", false},
	} {
		got, ok := tagFromReleaseURL(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: tagFromReleaseURL(%q) = (%q,%v), want (%q,%v)",
				c.name, c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestLatestTagUsesRedirectNotAPI は **API を叩かずに**タグを解決することを確認する。
// API ハンドラが呼ばれたら失敗＝レート制限の当たる経路へ戻っていないことの担保。
func TestLatestTagUsesRedirectNotAPI(t *testing.T) {
	var apiHits int
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/4noha/herdr-drover/releases/latest",
		func(w http.ResponseWriter, r *http.Request) {
			apiHits++
			fmt.Fprint(w, `{"tag_name":"v9.9.9"}`)
		})
	mux.HandleFunc("/4noha/herdr-drover/releases/latest",
		func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://github.com/4noha/herdr-drover/releases/tag/v0.5.33", http.StatusFound)
		})
	ts := httptest.NewServer(mux)
	oldAPI, oldDL := apiBase, dlBase
	apiBase, dlBase = ts.URL, ts.URL
	t.Cleanup(func() { apiBase, dlBase = oldAPI, oldDL; ts.Close() })

	tag, err := LatestTag()
	if err != nil {
		t.Fatalf("LatestTag: %v", err)
	}
	if tag != "v0.5.33" {
		t.Fatalf("tag = %q, want v0.5.33", tag)
	}
	if apiHits != 0 {
		t.Fatalf("api.github.com を %d 回叩いた（レート制限に当たる経路に戻っている）", apiHits)
	}
}

// TestLatestTagFallsBackToAPI は**リダイレクト経路が壊れても更新手段が全滅しない**
// ことを確認する。self-update は不具合修正の唯一の配布経路なので、GitHub 側の挙動が
// 変わったときに API へ落ちられることが要。
func TestLatestTagFallsBackToAPI(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/4noha/herdr-drover/releases/latest",
		func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"tag_name":"v1.0.0"}`) })
	// リダイレクトを返さない＝解析できない（GitHub 側の仕様変更を模す）。
	mux.HandleFunc("/4noha/herdr-drover/releases/latest",
		func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "<html>not a redirect</html>") })
	ts := httptest.NewServer(mux)
	oldAPI, oldDL := apiBase, dlBase
	apiBase, dlBase = ts.URL, ts.URL
	t.Cleanup(func() { apiBase, dlBase = oldAPI, oldDL; ts.Close() })

	tag, err := LatestTag()
	if err != nil {
		t.Fatalf("fallback が働かなかった: %v", err)
	}
	if tag != "v1.0.0" {
		t.Fatalf("tag = %q, want v1.0.0", tag)
	}
}

// TestLatestTagReportsBothFailures は両経路が落ちたとき**両方の理由**を出すことを
// 確認する（片方だけだと原因を取り違える）。
func TestLatestTagReportsBothFailures(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/4noha/herdr-drover/releases/latest",
		func(w http.ResponseWriter, r *http.Request) { http.Error(w, "rate limited", http.StatusForbidden) })
	mux.HandleFunc("/4noha/herdr-drover/releases/latest",
		func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", http.StatusInternalServerError) })
	ts := httptest.NewServer(mux)
	oldAPI, oldDL := apiBase, dlBase
	apiBase, dlBase = ts.URL, ts.URL
	t.Cleanup(func() { apiBase, dlBase = oldAPI, oldDL; ts.Close() })

	_, err := LatestTag()
	if err == nil {
		t.Fatal("両経路が落ちたのに成功した")
	}
	for _, want := range []string{"リダイレクト経路", "API 経路"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error に %q が含まれない: %v", want, err)
		}
	}
}
