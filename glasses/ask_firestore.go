package glasses

import (
	"context"
	"fmt"
	"time"

	"cloud.google.com/go/firestore"
)

// RegisterAsk は glasses_asks/{pc}_{request_id} を作る（回答は空）。
func (s *Firestore) RegisterAsk(ctx context.Context, pc, requestID string) (time.Time, error) {
	if !ValidPC(pc) || !ValidRequestID(requestID) {
		return time.Time{}, fmt.Errorf("%w: pc / request_id が不正です", ErrInvalid)
	}
	now := s.now().UTC()
	exp := now.Add(s.ttl)
	if _, err := s.fs.Collection(AskCollection).Doc(DocID(pc, requestID)).Set(ctx, map[string]any{
		"pc":         pc,
		"request_id": requestID,
		"created_at": now,
		"answer":     "",
		// TTL ポリシーは Timestamp 型の expire_at を見る。
		"expire_at": exp,
	}); err != nil {
		return time.Time{}, err
	}
	return exp, nil
}

// askDoc は問い合わせ doc を読む（無い・期限切れは exists=false）。
func (s *Firestore) askDoc(ctx context.Context, pc, requestID string) (*firestore.DocumentSnapshot, map[string]any, bool, error) {
	snap, err := s.fs.Collection(AskCollection).Doc(DocID(pc, requestID)).Get(ctx)
	if snap != nil && !snap.Exists() {
		return nil, nil, false, nil // 存在しない（Get は NotFound を返すが snapshot は非 nil）
	}
	if err != nil {
		return nil, nil, false, err
	}
	m := snap.Data()
	exp, ok := m["expire_at"].(time.Time)
	if !ok || !exp.After(s.now().UTC()) {
		return nil, nil, false, nil
	}
	return snap, m, true, nil
}

// AnswerAsk は登録済みの問い合わせなら回答を格納する。
func (s *Firestore) AnswerAsk(ctx context.Context, pc, requestID, text string) (bool, time.Time, error) {
	if !ValidPC(pc) || !ValidRequestID(requestID) {
		return false, time.Time{}, nil
	}
	snap, _, ok, err := s.askDoc(ctx, pc, requestID)
	if err != nil || !ok {
		return false, time.Time{}, err
	}
	now := s.now().UTC()
	exp := now.Add(s.ttl)
	if _, err := snap.Ref.Update(ctx, []firestore.Update{
		{Path: "answer", Value: text},
		{Path: "answered_at", Value: now},
		{Path: "expire_at", Value: exp},
	}); err != nil {
		return false, time.Time{}, err
	}
	return true, exp, nil
}

// TakeAskReply は回答済みなら本文を返し、配達済みとして期限を短くする。
func (s *Firestore) TakeAskReply(ctx context.Context, pc, requestID string) (string, bool, error) {
	snap, m, ok, err := s.askDoc(ctx, pc, requestID)
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "", false, ErrAskNotFound
	}
	text, _ := m["answer"].(string)
	if text == "" {
		return "", false, nil
	}
	if _, delivered := m["delivered_at"]; !delivered {
		now := s.now().UTC()
		exp, _ := m["expire_at"].(time.Time)
		if grace := now.Add(DeliveredGrace); grace.Before(exp) {
			exp = grace
		}
		if _, err := snap.Ref.Update(ctx, []firestore.Update{
			{Path: "delivered_at", Value: now},
			{Path: "expire_at", Value: exp},
		}); err != nil {
			return "", false, err
		}
	}
	return text, true, nil
}
