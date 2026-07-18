package web

// slave（共用 PC）用の relay エンドポイント。共用 PC は SA 鍵を持たず、
// durable な refresh secret から短命 bearer を取り、全 Firestore アクセスを
// この /slave/* 経由で relay に代行させる。relay（SA 保持者）が唯一の
// trust boundary＝slave が viewer になったり他 pc のデータに触るのを
// **構造的に**禁じる。master/owner 経路は無改変（本ファイルは additive）。

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/4noha/drover-cloud/state"
)

// slaveWakeHold は /slave/wake の long-poll 保持時間（テストで短縮可能）。
var slaveWakeHold = 25 * time.Second

// injSuffix はリモート pane 注入の派生 sid 接尾辞（`<sid>#inj`）。slave は
// この接尾辞を剥がした base sid の所有権で #inj を配信できる（自 session を
// 注入経路にも出す＝相手 webterm の 403 respawn を根治）。
const injSuffix = "#inj"

// slaveGuard は RS256 bearer 必須ラッパ（apiGuard の slave 版）。enrollSA
// 未設定なら fail-closed 404。検証済 pc を handler へ渡す。失効（revoked/{pc}
// または slaves/{pc}.revoked）は 403。Content-Type は各 handler が設定する
// （SSE/plain path があり得るため guard では設定しない）。
func (s *Server) slaveGuard(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.enrollSA == "" {
			http.Error(w, `{"error":"slave disabled"}`, http.StatusNotFound)
			return
		}
		pc, err := verifySlaveToken([]byte(s.enrollSA), bearer(r), time.Now())
		if err != nil || pc == "" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		ctx := r.Context()
		if s.st.IsRevoked(ctx, pc) || s.st.SlaveRevoked(ctx, pc) {
			http.Error(w, `{"error":"revoked"}`, http.StatusForbidden)
			return
		}
		h(w, r, pc)
	}
}

// NewSlaveGate は relay.Server.SlaveGate に注入する closure を組み立てる
// （enrollSA + state を閉じ込める）。enrollSA=="" なら常に handled=false＝
// slave 機能 off で master path は完全に無改変。§2.9 の判定表そのまま:
//
//	bearer 無し           -> handled=false（既存 Grant 経路＝byte-identical）
//	invalid/expired       -> 403
//	viewer（任意）        -> 403（覗き見の構造的停止）
//	source × 未所有 sid   -> 403
//	source × 自 pc の sid -> 101/200（pc 名前空間キーで Accept）
func NewSlaveGate(enrollSA string, st *state.Client) func(r *http.Request, sid, role string) (handled bool, allow bool, effKey string) {
	return func(r *http.Request, sid, role string) (handled bool, allow bool, effKey string) {
		if enrollSA == "" {
			return false, false, "" // slave 機能 off ⇒ 常に master path
		}
		tok := bearer(r)
		if tok == "" {
			return false, false, "" // slave 試行でない ⇒ master path
		}
		pc, err := verifySlaveToken([]byte(enrollSA), tok, time.Now())
		if err != nil || pc == "" {
			return true, false, "" // 不正/期限切れ ⇒ 403
		}
		if role != "source" {
			return true, false, "" // viewer/その他 ⇒ 403（覗き見停止）
		}
		ctx := r.Context()
		if st.IsRevoked(ctx, pc) || st.SlaveRevoked(ctx, pc) {
			return true, false, ""
		}
		// sid 所有権: slave が /slave/grant で書いた **pc 名前空間の**
		// slavegrants/{pc:sid} を突合。master の relaygrants とは別
		// コレクション＝slave は自分の grant しか読み書きできず、owner の
		// grant を汚染も参照もできない。grant 無し/期限切れ ⇒ 403。
		if !st.SlaveGrantValid(ctx, pc, sid) {
			return true, false, ""
		}
		return true, true, slaveSessionKey(pc, sid)
	}
}

// NewViewerKey は relay.KeyFor へ注入する seam。master path（bearer 無し）の
// **viewer** に、リモート pane 注入の source PC(`spc`)が slave の時だけ
// slaveSessionKey(spc,sid) を返し、slave source（同じ pc 名前空間キー）と
// ペアさせる（wsViewer と同一ロジックを注入経路へ）。spc 未指定・master
// source PC・source role は raw sid＝master path byte-identical。認可は relay の
// Grant(raw sid,viewer) が既に済（key 変更は pairing スロット選択のみ）。slave
// トークンが viewer になれない不変条件は SlaveGate（bearer 経路）が別途保証。
func NewViewerKey(st *state.Client) func(r *http.Request, sid, role string) string {
	return func(r *http.Request, sid, role string) string {
		if role != "viewer" {
			return sid
		}
		spc := r.URL.Query().Get("spc")
		if spc == "" {
			return sid
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if pr, _ := st.PCRole(ctx, spc); pr == "slave" {
			return slaveSessionKey(spc, sid)
		}
		return sid
	}
}

// slaveToken は POST /slave/token（refresh secret ゲート・slaveGuard 非経由）。
// secret を照合し短命 bearer を発行する。
func (s *Server) slaveToken(w http.ResponseWriter, r *http.Request) {
	if s.enrollSA == "" {
		http.Error(w, `{"error":"slave disabled"}`, http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"post only"}`, http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		PC     string `json:"pc"`
		Secret string `json:"secret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
		body.PC == "" || body.Secret == "" {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	hash, ok := s.st.SlaveSecretHash(ctx, body.PC)
	if !ok || subtle.ConstantTimeCompare(
		[]byte(hash), []byte(sha256Hex(body.Secret))) != 1 {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	if s.st.IsRevoked(ctx, body.PC) || s.st.SlaveRevoked(ctx, body.PC) {
		http.Error(w, `{"error":"revoked"}`, http.StatusForbidden)
		return
	}
	now := time.Now()
	tok, err := mintSlaveToken([]byte(s.enrollSA), body.PC, now)
	if err != nil {
		http.Error(w, `{"error":"mint"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token": tok, "exp": now.Add(time.Hour).Unix(),
	})
}

// slaveRegister は POST /slave/register（pc は token 由来）。
func (s *Server) slaveRegister(w http.ResponseWriter, r *http.Request, pc string) {
	var body struct {
		AgentVersion string `json:"agent_version"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if err := s.st.RegisterSlavePCVersion(r.Context(), pc, body.AgentVersion); err != nil {
		http.Error(w, `{"error":"firestore"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// slavePush は POST /slave/push（session upsert + sid→pc 学習）。pc は token
// 由来なので slave は自 pc 配下にしか書けない。
func (s *Server) slavePush(w http.ResponseWriter, r *http.Request, pc string) {
	var body struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	changed, err := s.st.PushStatusFor(r.Context(), pc, body.Sessions)
	if err != nil {
		http.Error(w, `{"error":"firestore"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"changed": changed})
}

// slaveDelete は POST /slave/delete（session tombstone）。
func (s *Server) slaveDelete(w http.ResponseWriter, r *http.Request, pc string) {
	var body struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	if err := s.st.DeleteSessionFor(r.Context(), pc, body.Key); err != nil {
		http.Error(w, `{"error":"firestore"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// slaveSessions は GET /slave/sessions（自 pc の session key 一覧＝起動 seed）。
func (s *Server) slaveSessions(w http.ResponseWriter, r *http.Request, pc string) {
	keys, err := s.st.SessionKeysFor(r.Context(), pc)
	if err != nil {
		http.Error(w, `{"error":"firestore"}`, http.StatusInternalServerError)
		return
	}
	if keys == nil {
		keys = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
}

// slaveWake は GET /slave/wake（long-poll・?since=<RFC3339Nano>）。
// since より新しい wake が既にあれば即返す（lossless catch-up）、無ければ
// ~25s 待ち・変化で {sid,ts}・timeout で 204・mid-hold 失効で 403。
func (s *Server) slaveWake(w http.ResponseWriter, r *http.Request, pc string) {
	since := r.URL.Query().Get("since")
	// 1. catch-up。
	if sid, ts, ok := s.st.WakeDoc(r.Context(), pc); ok && wakeNewer(ts, since) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"sid": sid, "ts": ts})
		return
	}
	// 2. ~25s watch。
	ctx, cancel := context.WithTimeout(r.Context(), slaveWakeHold)
	defer cancel()
	fired := make(chan struct{}, 1)
	go func() {
		_ = s.st.WatchWakeFor(ctx, pc, func(string) {
			select {
			case fired <- struct{}{}:
			default:
			}
		})
	}()
	for {
		select {
		case <-ctx.Done():
			w.WriteHeader(http.StatusNoContent)
			return
		case <-fired:
			// mid-hold 失効 ⇒ 403。
			if s.st.IsRevoked(r.Context(), pc) || s.st.SlaveRevoked(r.Context(), pc) {
				http.Error(w, `{"error":"revoked"}`, http.StatusForbidden)
				return
			}
			if sid, ts, ok := s.st.WakeDoc(r.Context(), pc); ok && wakeNewer(ts, since) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"sid": sid, "ts": ts})
				return
			}
			// 初回スナップショット等で since より新しくない ⇒ 継続待ち。
		}
	}
}

// wakeNewer は wake の ts が since より新しいか（RFC3339Nano は幅可変で
// 文字列比較が単調でないため time で比較）。since 空＝初回は「有れば新しい」。
func wakeNewer(ts, since string) bool {
	if ts == "" {
		return false
	}
	if since == "" {
		return true
	}
	tt, e1 := time.Parse(time.RFC3339Nano, ts)
	if e1 != nil {
		return false
	}
	ss, e2 := time.Parse(time.RFC3339Nano, since)
	if e2 != nil {
		return true // since 解釈不能 ⇒ lossless 優先で新しいとみなす
	}
	return tt.After(ss)
}

// slaveGrant は POST /slave/grant（role 強制 source）。自 pc が push した
// sid にしか grant できない（未所有は 403）。ttl は [1,300] にクランプ。
func (s *Server) slaveGrant(w http.ResponseWriter, r *http.Request, pc string) {
	var body struct {
		SID        string `json:"sid"`
		TTLSeconds int    `json:"ttl_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.SID == "" {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	// #inj（注入の派生 sid）は base sid の所有権で判定＝slave が自 session を
	// 注入経路にも配信できる。grant は full sid（…#inj）で書く（SlaveGate が
	// full sid で SlaveGrantValid を引き slaveSessionKey(pc,full sid) でペア）。
	if !s.st.SessionOwnedBy(ctx, pc, strings.TrimSuffix(body.SID, injSuffix)) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		return
	}
	ttl := body.TTLSeconds
	if ttl < 1 {
		ttl = 1
	}
	if ttl > 300 {
		ttl = 300
	}
	// slave source 専用 grant を pc 名前空間の slavegrants/{pc:sid} へ。
	// viewer は決して書かない（覗き見停止は SlaveGate が role で拒否）。
	if err := s.st.PutSlaveGrant(ctx, pc, body.SID,
		time.Duration(ttl)*time.Second); err != nil {
		http.Error(w, `{"error":"firestore"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// slaveRevoked は GET /slave/revoked（自己失効チェック）。slaveGuard を通過
// している＝失効なら 403 で弾かれるため、ここに到達した時点で常に false。
// agent は 403 も「失効」と解釈して自停止する（herdr-drover 側 IsSelfRevoked）。
func (s *Server) slaveRevoked(w http.ResponseWriter, r *http.Request, pc string) {
	ctx := r.Context()
	rv := s.st.IsRevoked(ctx, pc) || s.st.SlaveRevoked(ctx, pc)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"revoked": rv})
}
