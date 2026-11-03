package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/4noha/drover-cloud/relayclient"
	"github.com/4noha/drover-cloud/state"
)

var relayEmuUp bool

// TestMain: Firestore エミュレータを立てる（web/slave_emulator_test.go と同じ流儀）。
// 無ければ relay-only の通しは skip（他のテストはそのまま走る）。
// java21Bin は Homebrew の OpenJDK 21+ の bin（エミュレータの前提）。無ければ ""。
func java21Bin() string {
	for _, d := range []string{"/opt/homebrew/opt/openjdk/bin", "/opt/homebrew/opt/openjdk@25/bin", "/opt/homebrew/opt/openjdk@21/bin"} {
		if fi, err := os.Stat(d + "/java"); err == nil && !fi.IsDir() {
			out, _ := exec.Command(d+"/java", "-version").CombinedOutput()
			for _, v := range []string{"\"21", "\"22", "\"23", "\"24", "\"25", "\"26"} {
				if strings.Contains(string(out), v) {
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
		host := fmt.Sprintf("127.0.0.1:%d", freePort())
		cmd := exec.Command("gcloud", "beta", "emulators", "firestore", "start", "--host-port="+host, "--quiet")
		cmd.Env = append(os.Environ(), "PATH="+jbin+":"+os.Getenv("PATH"), "CLOUDSDK_CORE_DISABLE_PROMPTS=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if cmd.Start() == nil {
			for i := 0; i < 80; i++ {
				if c, e := http.Get("http://" + host + "/"); e == nil {
					c.Body.Close()
					relayEmuUp = true
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
			if relayEmuUp {
				os.Setenv("FIRESTORE_EMULATOR_HOST", host)
				code := m.Run()
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				os.Exit(code)
			}
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	os.Exit(m.Run())
}

// RELAY_ONLY=1: /session だけを Cloud Run と同じ認可（Firestore グラント）で立て、Web は載せない。
func TestRelayOnlyMode(t *testing.T) {
	if !relayEmuUp {
		t.Skip("Firestore emulator 不在")
	}
	t.Setenv("RELAY_ONLY", "1")
	t.Setenv("GCP_PROJECT", "demo-relay-only")
	t.Setenv("WEB_SIGNING_KEY", "") // Web の条件が揃っていても RELAY_ONLY が優先
	ts := httptest.NewServer(handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "relay-only") {
		t.Fatalf("health: %q", b)
	}
	if r, _ := http.Get(ts.URL + "/api/sessions"); r == nil || r.StatusCode != 404 {
		t.Fatalf("Web は載らないはず: %v", r)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	base := "ws" + strings.TrimPrefix(ts.URL, "http")
	// グラント無し → 403。
	if _, err := relayclient.Dial(ctx, base, "sid-ro", "viewer"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("グラント無しは 403 のはず: %v", err)
	}
	// グラント有り → 対になって中継する。
	st, err := state.New(ctx, "demo-relay-only", "e2e-src")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, role := range []string{"source", "viewer"} {
		if err := st.PutRelayGrant(ctx, "sid-ro", role, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	src, err := relayclient.Dial(ctx, base, "sid-ro", "source")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	v, err := relayclient.Dial(ctx, base, "sid-ro", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	fmt.Fprint(src, "hello-relay-only")
	buf := make([]byte, 32)
	n, err := v.Read(buf)
	if err != nil || string(buf[:n]) != "hello-relay-only" {
		t.Fatalf("中継: %q %v", buf[:n], err)
	}
}
