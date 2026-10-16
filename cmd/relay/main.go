// Command cloud-relay は Cloud Run 上で動く WSS バイト透過リレー＋
// Web 管理 UI（M7）。relay は画面解釈をしない（不変条件）。Cloud Run
// は min-instances=0 でスケール・トゥ・ゼロ、WS 接続中のみ温存。
// 1 リクエスト最大 3600s（要 --timeout）→ 超過時は client 再接続。
package main

import (
	"context"
	"encoding/base64"
	"io"
	"log"
	"net/http"
	"os"

	"github.com/4noha/drover-cloud/mcp"
	"github.com/4noha/drover-cloud/oauth"
	"github.com/4noha/drover-cloud/relay"
	"github.com/4noha/drover-cloud/state"
	"github.com/4noha/drover-cloud/web"
	"github.com/4noha/drover-cloud/webauth"
)

// handler は Cloud Run サービスの http.Handler を組み立てる
// （main から分離＝ローカルで実検証可能にするため）。
// GCP_PROJECT と WEB_SIGNING_KEY が両方あれば Web 管理 UI を有効化し、
// /session 以外を web へ。無ければ relay のみ（/＝health, /session）。
func handler() http.Handler {
	rl := relay.NewServer()
	mux := http.NewServeMux()
	mux.Handle("/session", rl) // 既存 CLI/agent 経路（無改変・常時）

	proj := os.Getenv("GCP_PROJECT")
	key := os.Getenv("WEB_SIGNING_KEY")
	clientID := os.Getenv("GOOGLE_OAUTH_CLIENT_ID")
	allowed := os.Getenv("ALLOWED_EMAILS")
	if proj != "" && key != "" && clientID != "" && allowed != "" {
		st, err := state.New(context.Background(), proj, "relay")
		if err != nil {
			log.Printf("web 無効（Firestore 接続失敗）: %v", err)
		} else {
			// 公開 /session を Firestore グラントで認可（即 enforce）。
			rl.Grant = st.CheckRelayGrant
			enrollSA := os.Getenv("ENROLL_SA_JSON")
			if enrollSA == "" {
				// JSON は --set-env-vars を壊すため b64 で渡せる
				if b, e := base64.StdEncoding.DecodeString(
					os.Getenv("ENROLL_SA_JSON_B64")); e == nil {
					enrollSA = string(b)
				}
			}
			// slave（共用 PC）認可 seam。enrollSA=="" なら常に handled=false
			// ＝slave 機能 off で master path 無改変（NewSlaveGate 内で guard）。
			rl.SlaveGate = web.NewSlaveGate(enrollSA, st)
			// 注入 viewer を slave source と同じ pc 名前空間でペアさせる
			// （spc==slave PC の時のみ・master path は byte-identical）。
			rl.KeyFor = web.NewViewerKey(st)
			ws := web.New(rl, st, webauth.NewSigner(key),
				clientID, allowed, nil, proj, enrollSA)
			// Firestore 更新 push（任意）: Firebase Web config（公開
			// JSON）。JSON は --set-env-vars を壊すため b64 で渡す。
			if b, e := base64.StdEncoding.DecodeString(
				os.Getenv("FIREBASE_WEB_CONFIG_B64")); e == nil && len(b) > 0 {
				ws.SetFirebaseWebConfig(string(b))
			}
			// Web Push（FCM）公開鍵（任意）。未設定ならクライアントが
			// 購読ボタンを出さない＝既存構成は無影響。
			if v := os.Getenv("FIREBASE_VAPID_KEY"); v != "" {
				ws.SetVapidKey(v)
			}
			mux.Handle("/", ws.Handler()) // /,/login,/auth/google,/api,/ws

			// MCP（drover-cloud-mcp-design.md）。MCP_ENABLED=1 のときだけ
			// /mcp・/oauth/*・/.well-known/* をマウントする＝未設定なら
			// 既存構成（relay/web/webauth/state の4パッケージ＝「cm とバイト
			// 等価」対象）には一切コードパスが増えない。main.go 自体は
			// この4パッケージに含まれないため、1 行マウントは対象外
			// （§9 決定事項1・4noha 確認済み）。
			if os.Getenv("MCP_ENABLED") == "1" {
				selfBase := os.Getenv("MCP_SELF_BASE_URL")
				if selfBase == "" {
					log.Printf("MCP 無効（MCP_SELF_BASE_URL 未設定＝issuer/resource を組み立てられない）")
				} else {
					// 未決事項7（Grok Bot の OAuth コールバック URL）はまだ
					// 確定していないため、MCP_ALLOWED_REDIRECT_HOSTS は空/
					// プレースホルダのままでよい。空なら DCR は全拒否
					// （oauth.hostAllowed のデフォルト deny）＝安全側。
					redirectHosts := os.Getenv("MCP_ALLOWED_REDIRECT_HOSTS")
					// slave（共用PC）への read_pane/send_input の許可範囲。既定off
					// （§4.3「共用PCには他人の作業や秘密が映りうる」。未設定/未知値は
					// mcp.New 側で "off" に正規化する＝安全側デフォルト）。
					slaveAccess := os.Getenv("MCP_SLAVE_ACCESS")
					as := oauth.New(st, webauth.NewSigner(key), clientID, allowed, nil,
						redirectHosts, selfBase)
					ms := mcp.New(st, as, slaveAccess)
					mux.Handle("/oauth/", as.Handler())
					mux.Handle("/.well-known/", as.Handler())
					mux.Handle("/mcp", ms.Handler())
					log.Printf("MCP 有効（self=%s, redirect_hosts=%q, slave_access=%q）", selfBase, redirectHosts, slaveAccess)
				}
			}
			return mux
		}
	} else {
		log.Printf("web 無効（GCP_PROJECT/WEB_SIGNING_KEY/GOOGLE_OAUTH_CLIENT_ID/ALLOWED_EMAILS 未設定）")
	}
	// relay のみ: ヘルスは "/"（/healthz は Google Front End が予約・遮断）
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	})
	return mux
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080" // Cloud Run 既定
	}
	srv := &http.Server{Addr: ":" + port, Handler: handler()}
	log.Printf("cloud-relay listening on :%s", port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("relay 終了: %v", err)
	}
}
