// Package glasses は眼鏡（Rokid Glasses / RV101）の /ask への回答を
// Firestore 経由でローカルブリッジへ渡す受け口。
//
// 経路:
//
//	ルーティン（Grok Bot のエージェント）が MCP ツール glasses_answer で
//	回答を保存 → Mac のブリッジが GET /glasses/answers で外向きに取りに来て
//	（ポーリング）HUD に表示 → POST /glasses/answers/ack で消す。
//
// 設計上の制約:
//   - state / relay / web / webauth の 4 パッケージは claude-master-go（cm）と
//     バイト等価を保つ対象＝変更しない。本パッケージは Firestore クライアントを
//     自前で作る（state.Client は *firestore.Client を公開していない）。
//   - ブリッジは NAT 内で inbound を受けられないため、外向き HTTPS の
//     ポーリングで取りに来る。
//   - 認証は専用の GLASSES_PULL_TOKEN（Bearer）。トークンが無い/短いときは
//     ハンドラをマウントしない（fail closed）。
//   - Firestore の TTL ポリシーは expire_at が **Timestamp 型**でないと効かない
//     ため、必ず time.Time で書く（文字列は不可）。
package glasses

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"
)

const (
	// Collection は回答を置く Firestore コレクション。
	Collection = "glasses_answers"
	// DefaultTTL は回答の寿命（Firestore TTL ポリシーは expire_at を見る）。
	DefaultTTL = 10 * time.Minute
	// MaxItems は 1 回の Pull で返す最大件数。
	MaxItems = 20
	// MaxTextRunes は回答本文の上限（rune 数）。
	MaxTextRunes = 4000
	// MaxWait は Pull の待ち上限。
	MaxWait = 25 * time.Second
	// MinTokenLen は GLASSES_PULL_TOKEN の最小長。
	MinTokenLen = 32
	// pollInterval は待機中の再問合せ間隔。
	pollInterval = time.Second
	// fetchLimit は Firestore からの取得上限。expire_at 切れを捨てる分を
	// 見込んで MaxItems より多めに取る（doc id 順に取るので、期限切れが
	// 並んでいても新しい分を取りこぼさないようにする）。
	fetchLimit = MaxItems * 5
)

// ErrInvalid は入力検査エラー。呼び手（HTTP/MCP）は 400 / invalid_request に
// 対応付ける。
var ErrInvalid = errors.New("invalid glasses answer")

// ErrNotRegistered は pc が未登録（またはペアリング解除済み）のときのエラー。
// 呼び手は 403 / forbidden に対応付ける。
var ErrNotRegistered = errors.New("pc is not registered")

var (
	requestIDRe = regexp.MustCompile(`^[rt]-[a-z0-9-]{1,48}$`)
	pcRe        = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
)

// Answer は 1 件の回答。
type Answer struct {
	RequestID string
	Text      string
	CreatedAt time.Time
}

// Store は回答の保管（本番は Firestore、テストはインメモリで差し替える）。
type Store interface {
	// Put は回答を保存する（同一 doc id の再送は最新で上書き）。
	Put(ctx context.Context, pc, requestID, text string) (expiresAt time.Time, err error)
	// Pull は pc の未取得回答を返す。無ければ wait まで待つ（上限 MaxWait）。
	Pull(ctx context.Context, pc string, wait time.Duration) ([]Answer, error)
	// Ack は該当 doc を削除する（存在しなくても成功扱い）。
	Ack(ctx context.Context, pc string, ids []string) error
}

// Registry は pc が登録済みかを判定する（main が state.Client をアダプトする）。
type Registry interface {
	Registered(ctx context.Context, pc string) (bool, error)
}

// RegistryFunc は関数を Registry にする。
type RegistryFunc func(ctx context.Context, pc string) (bool, error)

// Registered は Registry を満たす。
func (f RegistryFunc) Registered(ctx context.Context, pc string) (bool, error) {
	return f(ctx, pc)
}

// ValidRequestID は request_id の形式を検査する。
func ValidRequestID(s string) bool { return requestIDRe.MatchString(s) }

// ValidPC は pc 名の形式を検査する。
func ValidPC(s string) bool { return pcRe.MatchString(s) }

// ValidText は本文の長さ（rune 数）を検査する。
func ValidText(s string) bool {
	n := utf8.RuneCountInString(s)
	return n >= 1 && n <= MaxTextRunes
}

// DocID は doc id 規約（{pc}_{request_id}）を返す。
func DocID(pc, requestID string) string { return pc + "_" + requestID }

// ValidatePut は Put の 3 引数をまとめて検査する。
func ValidatePut(pc, requestID, text string) error {
	if !ValidPC(pc) {
		return fmt.Errorf("%w: pc が不正です（^[a-z0-9-]{1,64}$）", ErrInvalid)
	}
	if !ValidRequestID(requestID) {
		return fmt.Errorf("%w: request_id が不正です（^[rt]-[a-z0-9-]{1,48}$）", ErrInvalid)
	}
	if !ValidText(text) {
		return fmt.Errorf("%w: text は 1〜%d 文字（rune）です", ErrInvalid, MaxTextRunes)
	}
	return nil
}

// ClampWait は待ち時間を 0〜MaxWait に丸める（負値は 0＝即返し）。
func ClampWait(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	if d > MaxWait {
		return MaxWait
	}
	return d
}

// pullLoop は「query して空なら間隔をあけて再問合せ」を wait まで繰り返す
// 共通ループ（Firestore 実装・テスト用インメモリ実装で共有する）。
func pullLoop(ctx context.Context, wait time.Duration, now func() time.Time,
	query func(context.Context) ([]Answer, error)) ([]Answer, error) {
	deadline := now().Add(ClampWait(wait))
	for {
		items, err := query(ctx)
		if err != nil {
			return nil, err
		}
		if len(items) > 0 || !now().Before(deadline) {
			return items, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// AnswerPath は HTTP から回答を投入する口（GLASSES_ANSWER_TOKEN が要る）。
const AnswerPath = "/glasses/answer"

// AnswerStore は投入に必要な最小のストア（Put だけ）。
type AnswerStore interface {
	Put(ctx context.Context, pc, requestID, text string) (expiresAt time.Time, err error)
}

// PutAnswer は「入力検査 → 登録済み pc の確認 → 保存」を一括で行う。
// MCP の glasses_answer ツールと HTTP の /glasses/answer が同じ処理を共有する
// （検査は ValidatePut、保存は Store.Put、TTL はストアの既定に従う）。
func PutAnswer(ctx context.Context, st AnswerStore, reg Registry, pc, requestID, text string) (time.Time, error) {
	if err := ValidatePut(pc, requestID, text); err != nil {
		return time.Time{}, err
	}
	registered, err := reg.Registered(ctx, pc)
	if err != nil {
		return time.Time{}, err
	}
	if !registered {
		return time.Time{}, fmt.Errorf("%w: %s", ErrNotRegistered, pc)
	}
	// relay 経由（POST /glasses/ask）の問い合わせへの回答は glasses_asks 側へ
	// 振り分ける（LAN ブリッジの pull/ack に拾われないようにする）。
	if as, ok := st.(AskStore); ok {
		routed, exp, err := as.AnswerAsk(ctx, pc, requestID, text)
		if err != nil {
			return time.Time{}, err
		}
		if routed {
			return exp, nil
		}
	}
	return st.Put(ctx, pc, requestID, text)
}

// stateClient は state.Client のうち登録判定に使うメソッドだけ（glasses から
// state パッケージへ依存しないための狭いインターフェース）。
type stateClient interface {
	ListPCs(ctx context.Context) ([]string, error)
	IsRevoked(ctx context.Context, pc string) bool
}

// StateRegistry は state.Client の既存公開メソッドだけで「登録済み pc か」を
// 判定する（master は role 未設定なので存在確認で見る。失効は拒否）。
type StateRegistry struct{ St stateClient }

// Registered は Registry を満たす。
func (r StateRegistry) Registered(ctx context.Context, pc string) (bool, error) {
	if r.St.IsRevoked(ctx, pc) {
		return false, nil
	}
	pcs, err := r.St.ListPCs(ctx)
	if err != nil {
		return false, err
	}
	for _, p := range pcs {
		if p == pc {
			return true, nil
		}
	}
	return false, nil
}

// AuditEntry は監査1件の内容（本文は入れない）。
type AuditEntry struct {
	Email      string
	ClientID   string
	Tool       string
	PC         string
	SID        string
	ArgsDigest string
	ResultCode string
}

// Auditor は監査の記録先（main が state.WriteMCPAudit をアダプトする）。
type Auditor interface {
	WriteAnswerAudit(ctx context.Context, e AuditEntry) error
}

// AuditorFunc は関数を Auditor にする。
type AuditorFunc func(ctx context.Context, e AuditEntry) error

// WriteAnswerAudit は Auditor を満たす。
func (f AuditorFunc) WriteAnswerAudit(ctx context.Context, e AuditEntry) error { return f(ctx, e) }

// AnswerDigest は監査用のダイジェスト（**本文を含めない**。pc と request_id だけ）。
func AnswerDigest(pc, requestID string) string {
	h := sha256.Sum256([]byte(`{"pc":"` + pc + `","request_id":"` + requestID + `"}`))
	return hex.EncodeToString(h[:8])
}
