package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/4noha/drover-cloud/relay"
	"github.com/4noha/drover-cloud/webauth"
)

// /slave/revoked の応答に WS 中継の接続先（ws_url）が載る＝slave への切替経路。
func TestSlaveRevokedCarriesRelayWSURL(t *testing.T) {
	needEmu(t)
	st := newState(t)
	ws := New(relay.NewServer(), st, webauth.NewSigner("k"), "cid", allowEmail, fakeGV{}, emuProject, "")
	t.Setenv("RELAY_UPSTREAM_URL", "")
	rec := httptest.NewRecorder()
	ws.slaveRevoked(rec, httptest.NewRequest(http.MethodGet, "/slave/revoked", nil), "slave-x-herdr")
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if m["revoked"] != false || m["ws_url"] != "" {
		t.Fatalf("未設定なら ws_url は空: %v", m)
	}
	if err := st.SetRelayURL(t.Context(), "wss://relay.4noha.org"); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	ws.slaveRevoked(rec, httptest.NewRequest(http.MethodGet, "/slave/revoked", nil), "slave-x-herdr")
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if m["ws_url"] != "wss://relay.4noha.org" {
		t.Fatalf("settings/relay の url が載るはず: %v", m)
	}
	t.Cleanup(func() { _ = st.SetRelayURL(t.Context(), "") })
}
