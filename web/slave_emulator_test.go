//go:build !windows

package web

// slave（共用 PC）の relay 代行 + /session 認可を、実 relay + 実 Firestore
// エミュレータで検証（合成なし）。TestMain がエミュレータを起動する（不在
// なら Firestore 依存テストのみ skip・crypto/wiring テストは従来どおり実行）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/4noha/drover-cloud/relay"
	"github.com/4noha/drover-cloud/state"
	"github.com/4noha/drover-cloud/webauth"
	"github.com/coder/websocket"
)

const emuProject = "demo-cm"

var webEmuUp bool

func java21Bin() string {
	for _, d := range []string{
		"/opt/homebrew/opt/openjdk/bin",
		"/opt/homebrew/opt/openjdk@25/bin",
		"/opt/homebrew/opt/openjdk@21/bin",
	} {
		j := d + "/java"
		if fi, err := os.Stat(j); err == nil && !fi.IsDir() {
			out, _ := exec.Command(j, "-version").CombinedOutput()
			s := string(out)
			for _, v := range []string{"\"21", "\"22", "\"23", "\"24", "\"25", "\"26"} {
				if strings.Contains(s, v) {
					return d
				}
			}
		}
	}
	return ""
}

func freePort() int {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestMain(m *testing.M) {
	jbin := java21Bin()
	if _, err := exec.LookPath("gcloud"); err == nil && jbin != "" {
		port := freePort()
		host := fmt.Sprintf("127.0.0.1:%d", port)
		cmd := exec.Command("gcloud", "beta", "emulators", "firestore", "start",
			"--host-port="+host, "--quiet")
		cmd.Env = append(os.Environ(),
			"PATH="+jbin+":"+os.Getenv("PATH"),
			"CLOUDSDK_CORE_DISABLE_PROMPTS=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if cmd.Start() == nil {
			ready := false
			for i := 0; i < 80; i++ {
				if c, e := http.Get("http://" + host + "/"); e == nil {
					c.Body.Close()
					ready = true
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
			if ready {
				os.Setenv("FIRESTORE_EMULATOR_HOST", host)
				webEmuUp = true
				code := m.Run()
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				os.Exit(code)
			}
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	// エミュレータ不在: crypto/wiring テストのみ実行（Firestore 依存は skip）。
	os.Exit(m.Run())
}

func needEmu(t *testing.T) {
	t.Helper()
	if !webEmuUp {
		t.Skip("Firestore emulator 不在（gcloud / Java21+ 無し）")
	}
}

func newState(t *testing.T) *state.Client {
	t.Helper()
	st, err := state.New(context.Background(), emuProject, "relay")
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func sessionMap(sid string, cpu float64, active bool) map[string]any {
	return map[string]any{
		"key": sid, "session_id": sid, "pid": float64(4242),
		"short_dir": "proj", "cwd": "/x/proj", "cpu_percent": cpu,
		"is_active": active,
	}
}

// req は method/url/bearer/body で叩き、status と復号 JSON を返す。
func req(t *testing.T, method, u, bearer string, body any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	r, _ := http.NewRequest(method, u, rdr)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("%s %s: %v", method, u, err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

func TestSlaveEndpointsE2E(t *testing.T) {
	needEmu(t)
	sa, _ := testSAJSON(t)
	st := newState(t)
	rl := relay.NewServer()
	ws := New(rl, st, webauth.NewSigner("k"), "cid", allowEmail, fakeGV{}, emuProject, sa)
	ts := httptest.NewServer(ws.Handler())
	defer ts.Close()
	ctx := context.Background()
	const pc = "pce2e-herdr"
	const secret = "deadbeefcafef00d"
	if _, err := st.BindSlave(ctx, pc, sha256Hex(secret)); err != nil {
		t.Fatalf("BindSlave: %v", err)
	}

	// --- /slave/token ---
	if code, _ := req(t, "POST", ts.URL+"/slave/token", "",
		map[string]any{"pc": pc, "secret": "wrong"}); code != 401 {
		t.Fatalf("誤 secret が %d（401 のはず）", code)
	}
	if code, _ := req(t, "POST", ts.URL+"/slave/token", "",
		map[string]any{"pc": "no-such", "secret": secret}); code != 401 {
		t.Fatalf("未知 pc が %d（401 のはず）", code)
	}
	code, tokResp := req(t, "POST", ts.URL+"/slave/token", "",
		map[string]any{"pc": pc, "secret": secret})
	if code != 200 {
		t.Fatalf("/slave/token が %d", code)
	}
	tok, _ := tokResp["token"].(string)
	if tok == "" {
		t.Fatalf("token 空: %v", tokResp)
	}
	// 発行トークンが本物（verifySlaveToken が pc を返す）。
	if gotpc, err := verifySlaveToken([]byte(sa), tok, time.Now()); err != nil || gotpc != pc {
		t.Fatalf("発行 token 検証失敗: pc=%q err=%v", gotpc, err)
	}

	// bearer 無しの guard 経路は 401。
	if code, _ := req(t, "POST", ts.URL+"/slave/push", "",
		map[string]any{"sessions": []any{}}); code != 401 {
		t.Fatalf("bearer 無し push が %d（401 のはず）", code)
	}

	// --- /slave/register ---
	if code, _ := req(t, "POST", ts.URL+"/slave/register", tok,
		map[string]any{"agent_version": "v0.1.0"}); code != 200 {
		t.Fatalf("/slave/register が %d", code)
	}
	if role, _ := st.PCRole(ctx, pc); role != "slave" {
		t.Fatalf("register 後 role=%q（slave のはず）", role)
	}

	// --- /slave/push（content_hash ゲート） ---
	code, pushResp := req(t, "POST", ts.URL+"/slave/push", tok,
		map[string]any{"sessions": []any{sessionMap("sidA", 1.0, true)}})
	if code != 200 || pushResp["changed"].(float64) != 1 {
		t.Fatalf("初回 push: code=%d resp=%v", code, pushResp)
	}
	_, pushResp2 := req(t, "POST", ts.URL+"/slave/push", tok,
		map[string]any{"sessions": []any{sessionMap("sidA", 1.0, true)}})
	if pushResp2["changed"].(float64) != 0 {
		t.Fatalf("無差分 push で changed=%v（near-$0 違反）", pushResp2["changed"])
	}

	// --- /slave/sessions ---
	_, sess := req(t, "GET", ts.URL+"/slave/sessions", tok, nil)
	keys, _ := sess["keys"].([]any)
	if len(keys) != 1 || keys[0].(string) != "sidA" {
		t.Fatalf("/slave/sessions keys=%v", sess["keys"])
	}

	// --- /slave/grant（所有権） ---
	if code, _ := req(t, "POST", ts.URL+"/slave/grant", tok,
		map[string]any{"sid": "sidA", "ttl_seconds": 60}); code != 200 {
		t.Fatalf("自 sid の grant が %d", code)
	}
	if !st.SlaveGrantValid(ctx, pc, "sidA") {
		t.Fatal("slave grant が pc 名前空間に書かれていない")
	}
	// poisoning 根絶: master の relaygrants には一切書かれない。
	if st.CheckRelayGrant(ctx, "sidA", "source") {
		t.Fatal("slave grant が master relaygrants に漏れた")
	}
	if code, _ := req(t, "POST", ts.URL+"/slave/grant", tok,
		map[string]any{"sid": "not-mine", "ttl_seconds": 60}); code != 403 {
		t.Fatalf("未所有 sid の grant が %d（403 のはず）", code)
	}

	// --- /slave/wake（catch-up 200 と timeout 204） ---
	_ = st.Wake(ctx, pc, "sidA")
	code, wk := req(t, "GET", ts.URL+"/slave/wake?since=", tok, nil)
	if code != 200 || wk["sid"].(string) != "sidA" || wk["ts"].(string) == "" {
		t.Fatalf("/slave/wake catch-up: code=%d resp=%v", code, wk)
	}
	// since=<現 ts> なら新しい wake は無い → 204（hold 短縮）。
	prev := slaveWakeHold
	slaveWakeHold = 800 * time.Millisecond
	defer func() { slaveWakeHold = prev }()
	if code, _ := req(t, "GET", ts.URL+"/slave/wake?since="+url.QueryEscape(wk["ts"].(string)), tok, nil); code != 204 {
		t.Fatalf("新規 wake 無しで %d（204 のはず）", code)
	}

	// --- /slave/revoked（未失効は false） ---
	if code, rv := req(t, "GET", ts.URL+"/slave/revoked", tok, nil); code != 200 || rv["revoked"].(bool) {
		t.Fatalf("未失効の /slave/revoked: code=%d resp=%v", code, rv)
	}

	// --- /slave/delete ---
	if code, _ := req(t, "POST", ts.URL+"/slave/delete", tok,
		map[string]any{"key": "sidA"}); code != 200 {
		t.Fatalf("/slave/delete が %d", code)
	}
	if st.SessionOwnedBy(ctx, pc, "sidA") {
		t.Fatal("delete 後も session が残る")
	}

	// --- 失効 → 全て締め出し（guard/token/session） ---
	if err := st.SetRevoked(ctx, pc); err != nil {
		t.Fatalf("SetRevoked: %v", err)
	}
	if code, _ := req(t, "GET", ts.URL+"/slave/revoked", tok, nil); code != 403 {
		t.Fatalf("失効後 guard が %d（403 のはず）", code)
	}
	if code, _ := req(t, "POST", ts.URL+"/slave/token", "",
		map[string]any{"pc": pc, "secret": secret}); code != 403 {
		t.Fatalf("失効後 /slave/token が %d（403 のはず）", code)
	}
}

func TestSlaveEndpointsDisabledWhenNoSA(t *testing.T) {
	needEmu(t)
	st := newState(t)
	rl := relay.NewServer()
	// enrollSA="" ⇒ slave 機能 off。
	ws := New(rl, st, webauth.NewSigner("k"), "cid", allowEmail, fakeGV{}, emuProject, "")
	ts := httptest.NewServer(ws.Handler())
	defer ts.Close()
	if code, _ := req(t, "POST", ts.URL+"/slave/token", "",
		map[string]any{"pc": "x", "secret": "y"}); code != 404 {
		t.Fatalf("SA 無しで /slave/token が %d（404 のはず）", code)
	}
	if code, _ := req(t, "POST", ts.URL+"/slave/push", "tok",
		map[string]any{"sessions": []any{}}); code != 404 {
		t.Fatalf("SA 無しで guard が %d（404 のはず）", code)
	}
}

// wsDial は /session|/ws を net.Conn 化して dial（bearer/cookie 任意）。
func wsDial(ctx context.Context, u string, hdr http.Header) (net.Conn, error) {
	c, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		return nil, err
	}
	return websocket.NetConn(ctx, c, websocket.MessageBinary), nil
}

// TestSlaveSessionAdversarialE2E は §9 の決定的敵対テスト:
// owner→slave 操作が動く AND slave→owner 閲覧が 403、を **同時に**緑にする。
func TestSlaveSessionAdversarialE2E(t *testing.T) {
	needEmu(t)
	sa, _ := testSAJSON(t)
	st := newState(t)
	rl := relay.NewServer()
	rl.Grant = st.CheckRelayGrant
	rl.SlaveGate = NewSlaveGate(sa, st)
	ws := New(rl, st, webauth.NewSigner("k"), "cid", allowEmail, fakeGV{}, emuProject, sa)
	mux := http.NewServeMux()
	mux.Handle("/session", rl)    // 既存 CLI/agent 経路（main.go と同配線）
	mux.Handle("/", ws.Handler()) // /ws（owner viewer）等
	ts := httptest.NewServer(mux)
	defer ts.Close()
	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http")
	ctx := context.Background()

	const pc = "pcadv-herdr"
	const S = "w1:p2"
	if _, err := st.BindSlave(ctx, pc, sha256Hex("sek")); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterSlavePCVersion(ctx, pc, "v"); err != nil { // role=slave
		t.Fatal(err)
	}
	if _, err := st.PushStatusFor(ctx, pc, []map[string]any{sessionMap(S, 1.0, true)}); err != nil {
		t.Fatal(err)
	}
	// slave が /slave/grant 済み（relay が pc 刻印）を模す。
	if err := st.PutSlaveGrant(ctx, pc, S, time.Minute); err != nil {
		t.Fatal(err)
	}
	tok, err := mintSlaveToken([]byte(sa), pc, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	// (A) slave→owner 閲覧 = 常に 403（role=viewer は SlaveGate で遮断）。
	vh := http.Header{}
	vh.Set("Authorization", "Bearer "+tok)
	if _, err := wsDial(ctx, wsBase+"/session?sid="+url.QueryEscape("owner-sid")+"&role=viewer", vh); err == nil {
		t.Fatal("slave viewer が owner-sid を閲覧できた（覗き見停止破れ）")
	}
	// slave source でも未所有 sid は 403。
	if _, err := wsDial(ctx, wsBase+"/session?sid=unowned&role=source", vh); err == nil {
		t.Fatal("slave source が未所有 sid で通った")
	}

	// (B) owner→slave 操作 = 動く（byte レベル）。
	//   1) slave source を接続（自 sid・grant 済 ⇒ pc 名前空間キーで Accept）。
	sh := http.Header{}
	sh.Set("Authorization", "Bearer "+tok)
	src, err := wsDial(ctx, wsBase+"/session?sid="+url.QueryEscape(S)+"&role=source", sh)
	if err != nil {
		t.Fatalf("許可された slave source が 403: %v", err)
	}
	defer src.Close()
	//   2) owner viewer を /ws で接続（cookie）。wsViewer は PCRole=slave を見て
	//      同じ pc 名前空間キーで Accept ⇒ slave source と pairing する。
	oh := http.Header{}
	oh.Set("Cookie", authCookie(ws))
	vw, err := wsDial(ctx, wsBase+"/ws?pc="+url.QueryEscape(pc)+"&sid="+url.QueryEscape(S), oh)
	if err != nil {
		t.Fatalf("owner viewer /ws が失敗: %v", err)
	}
	defer vw.Close()
	//   3) source→viewer にバイトが届く（owner が slave 画面を見て操作できる線）。
	time.Sleep(300 * time.Millisecond) // pairing 確立待ち
	go src.Write([]byte("SLAVE_OK"))
	buf := make([]byte, 32)
	_ = vw.SetReadDeadline(time.Now().Add(4 * time.Second))
	n, rerr := vw.Read(buf)
	if rerr != nil || string(buf[:n]) != "SLAVE_OK" {
		t.Fatalf("owner→slave の line が繋がらない: n=%d err=%v got=%q", n, rerr, buf[:n])
	}

	// (C) 不変条件 §8.1: bearer 無し + 有効 master grant の source ⇒ 101。
	if err := st.PutRelayGrant(ctx, "master-sid", "source", time.Minute); err != nil {
		t.Fatal(err)
	}
	mc, err := wsDial(ctx, wsBase+"/session?sid=master-sid&role=source", http.Header{})
	if err != nil {
		t.Fatalf("bearer 無し master source が 403（invariant 破れ）: %v", err)
	}
	mc.Close()
}

// TestSlaveSourceHijackIsolation は §2.10: slave source が pc 名前空間キーへ
// 落ち、同じ sid 文字列の raw slot（master viewer）を hijack しないことを実証。
func TestSlaveSourceHijackIsolation(t *testing.T) {
	needEmu(t)
	sa, _ := testSAJSON(t)
	st := newState(t)
	rl := relay.NewServer()
	rl.Grant = st.CheckRelayGrant
	rl.SlaveGate = NewSlaveGate(sa, st)
	mux := http.NewServeMux()
	mux.Handle("/session", rl)
	ts := httptest.NewServer(mux)
	defer ts.Close()
	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http")
	ctx := context.Background()

	const pc = "pchijack-herdr"
	const S = "collide-sid"
	if _, err := st.BindSlave(ctx, pc, sha256Hex("k")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PushStatusFor(ctx, pc, []map[string]any{sessionMap(S, 1.0, true)}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSlaveGrant(ctx, pc, S, time.Minute); err != nil {
		t.Fatal(err)
	}
	tok, _ := mintSlaveToken([]byte(sa), pc, time.Now())

	// raw sid S に master viewer grant を用意し、無 bearer viewer を接続（raw slot）。
	if err := st.PutRelayGrant(ctx, S, "viewer", time.Minute); err != nil {
		t.Fatal(err)
	}
	rawViewer, err := wsDial(ctx, wsBase+"/session?sid="+url.QueryEscape(S)+"&role=viewer", http.Header{})
	if err != nil {
		t.Fatalf("master viewer 接続失敗: %v", err)
	}
	defer rawViewer.Close()

	// slave source（bearer）を同じ sid 文字列で接続 ⇒ pc\x00S へ隔離される。
	sh := http.Header{}
	sh.Set("Authorization", "Bearer "+tok)
	src, err := wsDial(ctx, wsBase+"/session?sid="+url.QueryEscape(S)+"&role=source", sh)
	if err != nil {
		t.Fatalf("slave source 接続失敗: %v", err)
	}
	defer src.Close()

	// slave source が書いても raw viewer（別 slot）には届かない＝hijack 不成立。
	time.Sleep(300 * time.Millisecond)
	go src.Write([]byte("HIJACK?"))
	buf := make([]byte, 16)
	_ = rawViewer.SetReadDeadline(time.Now().Add(1200 * time.Millisecond))
	n, rerr := rawViewer.Read(buf)
	if rerr == nil && n > 0 {
		t.Fatalf("slave source のバイトが master raw viewer に漏れた（hijack）: %q", buf[:n])
	}
}

func TestEnrollSlaveWithholdsSA(t *testing.T) {
	needEmu(t)
	sa, _ := testSAJSON(t)
	st := newState(t)
	rl := relay.NewServer()
	ws := New(rl, st, webauth.NewSigner("k"), "cid", allowEmail, fakeGV{}, emuProject, sa)
	ts := httptest.NewServer(ws.Handler())
	defer ts.Close()
	ctx := context.Background()
	cookie := authCookie(ws)

	apiEnrollCode := func(role string) map[string]any {
		u := ts.URL + "/api/enroll"
		if role != "" {
			u += "?role=" + role
		}
		r, _ := http.NewRequest("POST", u, nil)
		r.Header.Set("Cookie", cookie)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return m
	}
	enrollExchange := func(form url.Values) (int, map[string]any) {
		resp, err := http.PostForm(ts.URL+"/enroll", form)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}

	// --- master invariant（§8.5）: --slave 無し・sa_json 同梱 ---
	mCode := apiEnrollCode("")
	if strings.Contains(mCode["command"].(string), "--slave") {
		t.Fatalf("master コマンドに --slave が混入: %v", mCode["command"])
	}
	code, mResp := enrollExchange(url.Values{"code": {mCode["code"].(string)}})
	if code != 200 || mResp["sa_json"] == nil || mResp["role"] != nil {
		t.Fatalf("master enroll 応答が変化: code=%d resp=%v", code, mResp)
	}

	// --- slave: --slave 付き・sa_json 無し・slave_secret 有り ---
	sCode := apiEnrollCode("slave")
	if !strings.Contains(sCode["command"].(string), "--slave") {
		t.Fatalf("slave コマンドに --slave が無い: %v", sCode["command"])
	}
	const pc = "pcenroll-herdr"
	code, sResp := enrollExchange(url.Values{
		"code": {sCode["code"].(string)}, "pc": {pc}, "role": {"slave"}})
	if code != 200 {
		t.Fatalf("slave enroll が %d: %v", code, sResp)
	}
	if sResp["sa_json"] != nil {
		t.Fatal("slave enroll が sa_json を漏らした（設計違反）")
	}
	if sResp["role"] != "slave" || sResp["pc"] != pc {
		t.Fatalf("slave enroll 応答が不正: %v", sResp)
	}
	secret, _ := sResp["slave_secret"].(string)
	if secret == "" {
		t.Fatal("slave_secret が空")
	}
	// 発行 secret で /slave/token が通る（bind 済）。
	if c, _ := req(t, "POST", ts.URL+"/slave/token", "",
		map[string]any{"pc": pc, "secret": secret}); c != 200 {
		t.Fatalf("enroll 発行 secret で /slave/token が %d", c)
	}

	// --- 衝突: 既存 master pc を slave enroll で奪えない（409） ---
	const masterPC = "real-master"
	// PushStatusFor は pcs/{masterPC} を role 無し（=master）で作る。
	if _, err := st.PushStatusFor(ctx, masterPC,
		[]map[string]any{sessionMap("m1", 1.0, true)}); err != nil {
		t.Fatal(err)
	}
	cCode := apiEnrollCode("slave")
	code, _ = enrollExchange(url.Values{
		"code": {cCode["code"].(string)}, "pc": {masterPC}, "role": {"slave"}})
	if code != 409 {
		t.Fatalf("既存 master pc への slave enroll が %d（409 のはず）", code)
	}
}
