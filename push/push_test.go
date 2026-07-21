package push

// Send の決定論テスト（httptest サーバで FCM v1 API を模す）。実 OAuth2/SA 鍵は
// 使わず、Send は認証済み *http.Client を受け取るだけなので素の http.Client で足りる
// （認証は NewAuthenticatedClient 側の責務・ここでは Send の HTTP/JSON 契約のみ検証）。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSendSuccess(t *testing.T) {
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/projects/demo-proj/messages:send" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"name": "projects/demo-proj/messages/1"})
	}))
	defer ts.Close()

	invalid, err := Send(context.Background(), ts.Client(), ts.URL, "demo-proj", "tok-abc", "タスク完了", "herdr-drover")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if invalid {
		t.Fatal("invalidToken=true のはずがない（成功応答）")
	}
	msg, _ := gotBody["message"].(map[string]any)
	if msg["token"] != "tok-abc" {
		t.Fatalf("送信 payload の token 不一致: %+v", gotBody)
	}
	notif, _ := msg["notification"].(map[string]any)
	if notif["title"] != "タスク完了" || notif["body"] != "herdr-drover" {
		t.Fatalf("notification payload 不一致: %+v", notif)
	}
}

func TestSendUnregisteredToken(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code": 404, "message": "Requested entity was not found.", "status": "NOT_FOUND",
				"details": []map[string]any{
					{"@type": "type.googleapis.com/google.firebase.fcm.v1.FcmError", "errorCode": "UNREGISTERED"},
				},
			},
		})
	}))
	defer ts.Close()

	invalid, err := Send(context.Background(), ts.Client(), ts.URL, "demo-proj", "stale-tok", "t", "b")
	if err == nil {
		t.Fatal("エラーになるはず")
	}
	if !invalid {
		t.Fatal("invalidToken=true になるはず（UNREGISTERED）＝呼び手が token を掃除できるように")
	}
}

func TestSendOtherServerError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":500,"message":"internal","status":"INTERNAL"}}`))
	}))
	defer ts.Close()

	invalid, err := Send(context.Background(), ts.Client(), ts.URL, "demo-proj", "tok", "t", "b")
	if err == nil {
		t.Fatal("エラーになるはず")
	}
	if invalid {
		t.Fatal("UNREGISTERED でないので invalidToken=false のはず（一時的なサーバエラーで token を消してはいけない）")
	}
}

func TestSendRequiresProjectAndToken(t *testing.T) {
	if _, err := Send(context.Background(), http.DefaultClient, "", "", "tok", "t", "b"); err == nil {
		t.Fatal("projectID 空でエラーになるはず")
	}
	if _, err := Send(context.Background(), http.DefaultClient, "", "proj", "", "t", "b"); err == nil {
		t.Fatal("token 空でエラーになるはず")
	}
}
