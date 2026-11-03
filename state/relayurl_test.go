package state

import (
	"context"
	"testing"
	"time"
)

// settings/relay の set → get → watch（実 Firestore エミュレータ）。
func TestRelayURLSetGetWatch(t *testing.T) {
	c := newClient(t, "relayurl-test-herdr")
	ctx := context.Background()
	if u, err := c.RelayURL(ctx); err != nil || u != "" {
		t.Fatalf("未設定は空のはず: %q %v", u, err)
	}
	got := make(chan string, 8)
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = c.WatchRelayURL(wctx, func(u string) { got <- u }) }()
	// 初回（doc 無し）は "" が来る。
	select {
	case u := <-got:
		if u != "" {
			t.Fatalf("初回は空のはず: %q", u)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("初回の通知が来ない")
	}
	if err := c.SetRelayURL(ctx, "wss://relay.example.org"); err != nil {
		t.Fatal(err)
	}
	select {
	case u := <-got:
		if u != "wss://relay.example.org" {
			t.Fatalf("変更の通知: %q", u)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("変更の通知が来ない")
	}
	if u, _ := c.RelayURL(ctx); u != "wss://relay.example.org" {
		t.Fatalf("get: %q", u)
	}
	if err := c.SetRelayURL(ctx, ""); err != nil {
		t.Fatal(err)
	}
	select {
	case u := <-got:
		if u != "" {
			t.Fatalf("解除の通知: %q", u)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("解除の通知が来ない")
	}
}
