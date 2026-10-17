package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// herdr は CLI エージェントの作業完了を "done" ではなく "idle" で報告する
// （herdr CLI ヘルプの実文言: "done is a UI attention state; use idle for CLI
// agent completion waits"）。NotifyStates にこれが欠けていると、通常の完了
// 遷移（working→idle）を一切通知できない（2026-10-17 の調査で発見した実バグ）。
func TestNotifyStatesIncludesIdle(t *testing.T) {
	for _, want := range []string{"idle", "done", "blocked"} {
		if !NotifyStates[want] {
			t.Errorf("NotifyStates[%q] = false, want true", want)
		}
	}
	for _, notWant := range []string{"working", "unknown", ""} {
		if NotifyStates[notWant] {
			t.Errorf("NotifyStates[%q] = true, want false", notWant)
		}
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"disabled_empty_ok", Config{Enabled: false}, false},
		{"disabled_garbage_url_still_ok", Config{Enabled: false, URL: "not a url"}, false},
		{"enabled_valid", Config{Enabled: true, URL: "https://example.com/hook", HeaderName: "X-Secret", HeaderSecret: "s3cr3t"}, false},
		{"enabled_http_rejected", Config{Enabled: true, URL: "http://example.com/hook", HeaderName: "X-Secret", HeaderSecret: "s"}, true},
		{"enabled_no_scheme_rejected", Config{Enabled: true, URL: "example.com/hook", HeaderName: "X-Secret", HeaderSecret: "s"}, true},
		{"enabled_empty_header_name_rejected", Config{Enabled: true, URL: "https://example.com/hook", HeaderSecret: "s"}, true},
		{"enabled_crlf_in_header_name_rejected", Config{Enabled: true, URL: "https://example.com/hook", HeaderName: "X-Secret\r\nX-Evil: 1", HeaderSecret: "s"}, true},
		{"enabled_crlf_in_secret_rejected", Config{Enabled: true, URL: "https://example.com/hook", HeaderName: "X-Secret", HeaderSecret: "s\r\nX-Evil: 1"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(c.cfg)
			if (err != nil) != c.wantErr {
				t.Fatalf("Validate(%+v) err=%v, wantErr=%v", c.cfg, err, c.wantErr)
			}
		})
	}
}

func TestSendPostsJSONWithHeader(t *testing.T) {
	var gotBody Payload
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Secret")
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type=%q", r.Header.Get("Content-Type"))
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := Config{Enabled: true, URL: srv.URL, HeaderName: "X-Secret", HeaderSecret: "s3cr3t"}
	p := Payload{PC: "pc1", SID: "w1:p1", WindowName: "claude", Agent: "claude",
		Cwd: "/Users/x/proj", OldStatus: "working", NewStatus: "done", At: "2026-10-16T00:00:00Z"}
	if err := Send(context.Background(), cfg, p); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotHeader != "s3cr3t" {
		t.Fatalf("header=%q", gotHeader)
	}
	if gotBody != p {
		t.Fatalf("body=%+v, want %+v", gotBody, p)
	}
}

func TestSendDisabledIsNoop(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()
	if err := Send(context.Background(), Config{Enabled: false, URL: srv.URL}, Payload{}); err != nil {
		t.Fatalf("Send(disabled): %v", err)
	}
	if called {
		t.Fatal("disabled config なのに実際に送信された")
	}
}

func TestSendNonOKStatusIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	err := Send(context.Background(), Config{Enabled: true, URL: srv.URL, HeaderName: "X", HeaderSecret: "y"}, Payload{})
	if err == nil {
		t.Fatal("5xx 応答なのに err=nil")
	}
}

func TestSendTimesOutOnSlowEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	cfg := Config{Enabled: true, URL: srv.URL, HeaderName: "X", HeaderSecret: "y"}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := Send(ctx, cfg, Payload{}); err == nil {
		t.Fatal("親 ctx の短いタイムアウトなのに err=nil")
	}
}
