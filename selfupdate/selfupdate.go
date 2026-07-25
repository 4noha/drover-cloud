// Package selfupdate は GitHub Releases から最新の静的バイナリを取得し、
// sha256 検証して実行中バイナリを原子的に置換する（`herdr-drover update`
// ／遠隔命令 self-update）。依存は標準ライブラリのみ。
//
// cm（claude-master-go internal/selfupdate）のコピー適応（同一作者＝
// コピー自由・cm リポジトリは無改変）。drover 用の差分は
//   - Repo/asset 名/UA を herdr-drover へ（asset 規約:
//     herdr-drover_<goos>_<goarch>。install/配布側と一致させること）
//   - env 上書きは DROVER_REPO（cm は CM_REPO だが、drover のテスト環境
//     では CM_REPO が「cm リポジトリのローカルパス」を指す別用途で既用
//     〔test/webterm_e2e_test.go〕のため名前衝突を避ける）
//   - apiBase/dlBase/osExecutable のテスト seam（ローカル HTTP fixture で
//     Update 全経路を実 HTTP・実ファイル置換で検証するため。実 GitHub へ
//     出ないテストは「合成」ではなく fixture＝HTTP 契約と sha256 検証と
//     原子置換そのものを通す）
package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Repo は OWNER/REPO。リリースのホスト先。環境変数 DROVER_REPO で上書き可。
var Repo = "4noha/herdr-drover"

func repo() string {
	if r := os.Getenv("DROVER_REPO"); r != "" {
		return r
	}
	return Repo
}

// asset 名は配布側（install/goreleaser 相当）と一致させること:
//
//	herdr-drover_<goos>_<goarch>[.exe]
//
// Windows は DESIGN で out-of-scope だが、asset 規約は cm と同じ
// OS 拡張子規則を残す（将来の移植で checksums.txt の名前が揺れない）。
func assetName() string {
	name := fmt.Sprintf("herdr-drover_%s_%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

var httpc = &http.Client{Timeout: 60 * time.Second}

// UserAgent は GitHub API/Releases へ送る固有 UA。**UA 未指定は GitHub
// が即 403 で拒否**（規約・cm 実証済）／既定の `Go-http-client/1.1` は IP
// 共有枠のレート(匿名 60/h)を他ツールと食い合うため固有名で分離する。
var UserAgent = "herdr-drover"

// apiBase/dlBase はテスト seam（ローカル HTTP fixture へ向ける）。
// 本番は GitHub 固定＝実行時に変える経路は無い。
var (
	apiBase = "https://api.github.com"
	dlBase  = "https://github.com"
)

// osExecutable はテスト seam。Update の実置換検証で「実行中の go test
// バイナリ自身」を書き換えない（unix rename 自体は安全だが、テストが
// 自分の実行ファイルを差し替えるのは検証対象外の副作用）。
var osExecutable = os.Executable

// setGHHeaders は GitHub API/Releases へ送る共通ヘッダを付ける。
//
//   - Accept: 公式推奨の application/vnd.github+json（Releases JSON 強制）。
//   - User-Agent: 固有名（無 UA = 403／generic Go UA = 他ツールと共有枠）。
//   - Authorization: GITHUB_TOKEN env があれば Bearer 認証（匿名 60/h →
//     認証 5000/h で実質枯渇しない）。
func setGHHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", UserAgent)
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
}

// httpErr は non-200 のとき body 先頭 256B を error に含めて診断容易にする
// （cm 教訓: `403 Forbidden` だけでは rate limit / UA 拒否 / 権限を区別
// 不能だった）。
func httpErr(prefix string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	return fmt.Errorf("%s: %s (body=%q)", prefix, resp.Status, body)
}

// LatestTag は GitHub API で最新リリースの tag を返す。
func LatestTag() (string, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", apiBase, repo())
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	setGHHeaders(req)
	resp, err := httpc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", httpErr("github api", resp)
	}
	var r struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", err
	}
	if r.TagName == "" {
		return "", fmt.Errorf("リリースが見つかりません")
	}
	return r.TagName, nil
}

// dl はリリース asset を取得。GitHub Releases も同じく UA 必須・
// 認証で枠拡張。設定が悪い場合の body も error に含む。
func dl(url string) ([]byte, error) {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	setGHHeaders(req)
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, httpErr("download "+url, resp)
	}
	return io.ReadAll(resp.Body)
}

// expectedSHA は checksums.txt（`<sha256>  <name>` 行）から asset の
// 期待ハッシュを取り出す。
func expectedSHA(checksums []byte, name string) (string, bool) {
	for _, ln := range strings.Split(string(checksums), "\n") {
		f := strings.Fields(ln)
		if len(f) == 2 && f[1] == name {
			return strings.ToLower(f[0]), true
		}
	}
	return "", false
}

// Update は current(=現在の埋め込みバージョン) と最新を比べ、必要なら
// 自分自身を置換する。戻り値 (更新後バージョン, 更新したか, error)。
func Update(current string) (string, bool, error) {
	tag, err := LatestTag()
	if err != nil {
		return "", false, err
	}
	if normalize(tag) == normalize(current) {
		return tag, false, nil // 既に最新
	}
	base := fmt.Sprintf("%s/%s/releases/latest/download", dlBase, repo())
	name := assetName()
	bin, err := dl(base + "/" + name)
	if err != nil {
		return "", false, err
	}
	sums, err := dl(base + "/checksums.txt")
	if err != nil {
		return "", false, fmt.Errorf("checksums 取得失敗: %w", err)
	}
	want, ok := expectedSHA(sums, name)
	if !ok {
		return "", false, fmt.Errorf("checksums に %s が無い", name)
	}
	got := sha256.Sum256(bin)
	if hex.EncodeToString(got[:]) != want {
		return "", false, fmt.Errorf("sha256 不一致（破損/改竄の疑い）")
	}
	if err := replaceSelf(bin); err != nil {
		return "", false, err
	}
	return tag, true, nil
}

// replaceSelf は実行中バイナリを新バイナリで原子的に置き換える。
// 同 FS なら rename、跨ぐ場合はコピー。実行中の旧 inode は保持される
// ので安全（次回起動から新版）。⚠バイナリはプロセス起動時のみ反映＝
// 常駐 agent は再起動（launchd kickstart / self-update の exit）で
// 初めて新版になる（cm 教訓）。
func replaceSelf(newBin []byte) error {
	exe, err := osExecutable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	dir := filepath.Dir(exe)
	// ⚠ Windows は拡張子が無いと os/exec が起動できない（lookExtensions）＝
	// 置換前の実行可否チェック（probeExec）のために .exe を付ける。最終的な
	// 名前は placeBinary の rename 先（= exe）なのでここでの名前は一時的。
	pattern := ".herdr-drover-new-*"
	if runtime.GOOS == "windows" {
		pattern = ".herdr-drover-new-*.exe"
	}
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		// 書込不可（/usr/local/bin 等）→ 明示エラーで再インストール案内
		return fmt.Errorf("%s に書込不可: %w（インストール手順の再実行 or sudo）", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(newBin); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// **置換前に新バイナリを 1 回起動して実行できることを確かめる**。
	// ここを通さないと「起動できないバイナリで稼働中のバイナリを上書きする」
	// ＝daemon が消えて誰も復帰させられない事故になる（2026-07-26 に実発生:
	// Windows の Smart App Control が配布バイナリの実行をブロックし、
	// self-update 後に daemon が起動不能のまま停止した。sha256 は一致しており
	// 検証では防げない＝「正しいバイナリだが**この環境では動かない**」）。
	// 失敗時は place しない＝稼働中バイナリは無傷のまま更新だけ諦める。
	if err := probeExec(tmpName); err != nil {
		return fmt.Errorf("新バイナリを実行できないため更新を中止した（稼働中のバイナリは無傷）: %w", err)
	}
	// 原子置換の最終段は place_{unix,windows}.go（cm の M8f OS-split 構成）。
	return placeBinary(tmpName, exe)
}

// probeArgs は probeExecutable が新バイナリへ渡す引数。副作用が無く即座に
// exit 0 する呼び出しであること（herdr-drover は `version`）。
var probeArgs = []string{"version"}

// probeExec は「置換前の実行可否チェック」の seam（テストで差し替える）。
var probeExec = probeExecutable

// probeExecutable は path を実際に起動して exit 0 を確認する。
// 検出できるのは sha256 では防げない級の失敗＝OS の実行ポリシーによる拒否
// （Windows の Smart App Control / AppLocker、unix の noexec マウント）・
// アーキテクチャ不一致・破損 PE/ELF など。
func probeExecutable(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, probeArgs...).CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("起動確認がタイムアウト（%v %v）", path, probeArgs)
		}
		s := strings.TrimSpace(string(out))
		if len(s) > 200 {
			s = s[:200] + "…"
		}
		return fmt.Errorf("起動確認に失敗（%v）: %w（出力: %q）%s", probeArgs, err, s, windowsSACHint())
	}
	return nil
}

// windowsSACHint は Windows でだけ足す原因候補の案内（silent に諦めない）。
func windowsSACHint() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	return "。Windows で未署名の配布バイナリが Smart App Control に" +
		"ブロックされている可能性がある（`Unblock-File` では解けない）＝" +
		"その場合はローカルビルドを手動配置すること"
}

func normalize(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}
