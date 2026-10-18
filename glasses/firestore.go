package glasses

import (
	"context"
	"fmt"
	"sort"
	"time"

	"cloud.google.com/go/firestore"
)

// Firestore は Store（回答）と ChatStore（ミラー）の本番実装。
type Firestore struct {
	fs  *firestore.Client
	ttl time.Duration
	now func() time.Time
}

// NewStore は projectID で Firestore クライアントを作る
// （FIRESTORE_EMULATOR_HOST が立っていればエミュレータへ）。
func NewStore(ctx context.Context, projectID string) (*Firestore, error) {
	fs, err := firestore.NewClient(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return NewFirestoreStore(fs), nil
}

// NewFirestoreStore は既存の *firestore.Client を包む。
func NewFirestoreStore(fs *firestore.Client) *Firestore {
	return &Firestore{fs: fs, ttl: DefaultTTL, now: time.Now}
}

// Put は doc id = {pc}_{request_id} で上書き保存する（再送は最新で上書き）。
func (s *Firestore) Put(ctx context.Context, pc, requestID, text string) (time.Time, error) {
	if err := ValidatePut(pc, requestID, text); err != nil {
		return time.Time{}, err
	}
	now := s.now().UTC()
	exp := now.Add(s.ttl)
	if _, err := s.fs.Collection(Collection).Doc(DocID(pc, requestID)).Set(ctx, map[string]any{
		"pc":         pc,
		"request_id": requestID,
		"text":       text,
		"created_at": now,
		// expire_at は **time.Time（Firestore Timestamp）**。文字列だと
		// Firestore の TTL ポリシーが効かない。
		"expire_at": exp,
	}); err != nil {
		return time.Time{}, err
	}
	return exp, nil
}

// Pull は pc の未取得回答を返す（無ければ wait まで待つ。上限 MaxWait）。
func (s *Firestore) Pull(ctx context.Context, pc string, wait time.Duration) ([]Answer, error) {
	if !ValidPC(pc) {
		return nil, fmt.Errorf("%w: pc が不正です（^[a-z0-9-]{1,64}$）", ErrInvalid)
	}
	return pullLoop(ctx, wait, s.now, func(ctx context.Context) ([]Answer, error) {
		return s.query(ctx, pc)
	})
}

// query は pc == X で引き、expire_at > now のものだけを新しい順で返す。
//
// expire_at の比較はクライアント側で行う（pc == と expire_at > の複合条件は
// Firestore の複合インデックス定義が要るため、単一フィールドの自動索引だけで
// 済ませる）。並びも orderBy を使わず Go 側で整える（同じ理由）。
func (s *Firestore) query(ctx context.Context, pc string) ([]Answer, error) {
	docs, err := s.fs.Collection(Collection).
		Where("pc", "==", pc).
		Limit(fetchLimit).
		Documents(ctx).GetAll()
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	out := make([]Answer, 0, len(docs))
	for _, d := range docs {
		m := d.Data()
		exp, ok := m["expire_at"].(time.Time)
		if !ok || !exp.After(now) {
			continue // 期限切れ（TTL 削除前の残骸）は返さない
		}
		rid, _ := m["request_id"].(string)
		if rid == "" {
			continue
		}
		text, _ := m["text"].(string)
		created, _ := m["created_at"].(time.Time)
		out = append(out, Answer{RequestID: rid, Text: text, CreatedAt: created})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	if len(out) > MaxItems {
		out = out[:MaxItems]
	}
	return out, nil
}

// Ack は該当 doc を削除する（存在しなくても成功扱い）。
func (s *Firestore) Ack(ctx context.Context, pc string, ids []string) error {
	if !ValidPC(pc) {
		return fmt.Errorf("%w: pc が不正です（^[a-z0-9-]{1,64}$）", ErrInvalid)
	}
	for _, id := range ids {
		if !ValidRequestID(id) {
			continue // 不正 id は無視（他 pc の doc を作らない・触らない）
		}
		// Firestore の Delete は存在しない doc でもエラーにしない。
		if _, err := s.fs.Collection(Collection).Doc(DocID(pc, id)).Delete(ctx); err != nil {
			return err
		}
	}
	return nil
}

// PutChat はミラー行を上書き保存する（doc id = ブリッジ側の entry id）。
// TTL は短め（DefaultChatTTL）。
func (s *Firestore) PutChat(ctx context.Context, items []ChatItem) error {
	if len(items) == 0 {
		return nil
	}
	if len(items) > MaxChatBatch {
		return fmt.Errorf("%w: items が多すぎます（最大 %d）", ErrInvalid, MaxChatBatch)
	}
	now := s.now().UTC()
	batch := s.fs.Batch()
	for _, it := range items {
		if err := ValidateChatItem(it); err != nil {
			return err
		}
		ts := it.TS
		if ts <= 0 {
			ts = now.UnixMilli()
		}
		batch.Set(s.fs.Collection(ChatCollection).Doc(it.ID), map[string]any{
			"id":         it.ID,
			"role":       it.Role,
			"kind":       it.Kind,
			"text":       it.Text,
			"ts":         ts,
			"created_at": now,
			"expire_at":  now.Add(DefaultChatTTL),
		})
	}
	_, err := batch.Commit(ctx)
	return err
}

// RecentChat は直近のミラー行を古い→新しい順で返す（HUD がその順で追記する）。
// ts の降順で取ってから Go 側で昇順に戻す＝単一フィールド索引だけで済む。
func (s *Firestore) RecentChat(ctx context.Context, limit int) ([]ChatItem, error) {
	limit = ClampChatLimit(limit)
	docs, err := s.fs.Collection(ChatCollection).
		OrderBy("ts", firestore.Desc).
		Limit(limit).
		Documents(ctx).GetAll()
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	out := make([]ChatItem, 0, len(docs))
	for _, d := range docs {
		m := d.Data()
		if exp, ok := m["expire_at"].(time.Time); !ok || !exp.After(now) {
			continue
		}
		text, _ := m["text"].(string)
		if text == "" {
			continue
		}
		role, _ := m["role"].(string)
		kind, _ := m["kind"].(string)
		ts, _ := m["ts"].(int64)
		out = append(out, ChatItem{ID: d.Ref.ID, Role: role, Kind: kind, Text: text, TS: ts})
	}
	// ts 降順で取ったものを昇順（古い→新しい）に戻す。
	sort.Slice(out, func(i, j int) bool { return out[i].TS < out[j].TS })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
