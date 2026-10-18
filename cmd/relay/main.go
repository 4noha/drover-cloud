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

	"github.com/4noha/drover-cloud/glasses"
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

			// 眼鏡（Rokid）の回答受け口。GLASSES_ENABLED=1 のときだけ
			// ストアを作る（作れなければ nil＝MCP ツールも HTTP も無効の
			// まま＝既存挙動は不変）。
			gStore := glassesStoreFor(proj)

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
					if gStore != nil {
						ms.SetGlasses(gStore) // 眼鏡の回答を保存するツールを有効化
					}
					mux.Handle("/oauth/", as.Handler())
					mux.Handle("/.well-known/", as.Handler())
					mux.Handle("/mcp", ms.Handler())
					log.Printf("MCP 有効（self=%s, redirect_hosts=%q, slave_access=%q）", selfBase, redirectHosts, slaveAccess)
				}
			}
			// HTTP の受け口（ブリッジが外向きに取りに来る）。GLASSES_PULL_TOKEN
			// が無い/短いときはマウントしない＝fail closed。
			if gStore != nil {
				mountGlasses(mux, gStore, st)
			}
			return mux
		}
	} else {
		log.Printf("web 無効（GCP_PROJECT/WEB_SIGNING_KEY/GOOGLE_OAUTH_CLIENT_ID/ALLOWED_EMAILS 未設定）")
	}
	// web 無効でも GLASSES_ENABLED=1 なら受け口を立てる（未登録 pc の判定に
	// 使う state クライアントはここで別に作る）。
	if gStore := glassesStoreFor(proj); gStore != nil {
		if gst, err := state.New(context.Background(), proj, "relay"); err != nil {
			log.Printf("glasses 無効（state 接続失敗）: %v", err)
		} else {
			mountGlasses(mux, gStore, gst)
		}
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

// glasses の登録判定（glassesRegistry）は glasses.StateRegistry に移した
// （MCP の glasses_answer と HTTP の投入口で同じ判定を共有するため）。

// glassesStoreFor は GLASSES_ENABLED=1 かつ GCP_PROJECT があるときだけ
// Firestore ストアを作る（未設定・接続失敗は nil＝機能 off）。
func glassesStoreFor(proj string) glasses.Store {
	if os.Getenv("GLASSES_ENABLED") != "1" {
		return nil
	}
	if proj == "" {
		log.Printf("glasses 無効（GCP_PROJECT 未設定＝Firestore を作れない）")
		return nil
	}
	store, err := glasses.NewStore(context.Background(), proj)
	if err != nil {
		log.Printf("glasses 無効（Firestore 接続失敗）: %v", err)
		return nil
	}
	return store
}

// mountGlasses は /glasses/ をマウントする。GLASSES_PULL_TOKEN が無い/短い
// ときは何もしない（fail closed）。投入口（POST /glasses/answer）は
// GLASSES_ANSWER_TOKEN が 32 文字以上かつ取得トークンと別のときだけ有効。
func mountGlasses(mux *http.ServeMux, store glasses.Store, st *state.Client) bool {
	if store == nil || st == nil {
		return false
	}
	tok := os.Getenv("GLASSES_PULL_TOKEN")
	if len(tok) < glasses.MinTokenLen {
		log.Printf("glasses 無効（GLASSES_PULL_TOKEN が空/短すぎる）")
		return false
	}
	answerTok := os.Getenv("GLASSES_ANSWER_TOKEN")
	if answerTok != "" && (len(answerTok) < glasses.MinTokenLen || answerTok == tok) {
		log.Printf("glasses: GLASSES_ANSWER_TOKEN を無効化（32 文字未満 または 取得トークンと同一）")
	}
	h, err := glasses.NewHandler(store, glasses.StateRegistry{St: st}, tok, answerTok)
	if err != nil {
		log.Printf("glasses 無効: %v", err)
		return false
	}
	// 監査は mcp_audit へ（MCP の tools/call と同じコレクション。本文は書かない）。
	h.SetAuditor(glasses.AuditorFunc(func(ctx context.Context, e glasses.AuditEntry) error {
		return st.WriteMCPAudit(ctx, state.MCPAuditEntry{
			Email: e.Email, ClientID: e.ClientID, Tool: e.Tool,
			PC: e.PC, SID: e.SID, ArgsDigest: e.ArgsDigest, ResultCode: e.ResultCode,
		})
	}))
	mux.Handle("/glasses/", h)
	// トークンは値も長さもログに出さない。
	log.Printf("glasses 有効（取得=%s, 投入=%s）", glasses.AnswersPath, answerState(h))
	return true
}

// answerState は投入口の有効/無効をログ用に返す（トークンの値は出さない）。
func answerState(h *glasses.Handler) string {
	if h.AnswerEnabled() {
		return "有効"
	}
	return "無効"
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
