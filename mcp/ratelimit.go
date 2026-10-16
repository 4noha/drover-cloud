package mcp

// インスタンス内トークンバケット（Firestore カウンタは使わない。Cloud Run は
// 通常1インスタンスなので十分＝drover-cloud-mcp-design.md §6-3）。
// レートクラスごとに email 単位でバケットを持つ。

import (
	"sync"
	"time"
)

// レート上限（1分あたり）。
var rateLimits = map[string]int{
	"read":    60,
	"input":   20,
	"control": 10,
	"admin":   3,
}

type bucket struct {
	tokens   float64
	lastFill time.Time
}

type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket // key = email + "|" + rateClass
}

func newLimiter() *limiter {
	return &limiter{buckets: map[string]*bucket{}}
}

// allow はトークンを1消費できれば true（できなければレート制限）。
// refill はクラスの per-分 上限を 60 秒で均等補充する素朴な実装
// （厳密なトークンバケットの最適化はしない＝近似で十分な用途）。
func (l *limiter) allow(email, rateClass string) bool {
	limit, ok := rateLimits[rateClass]
	if !ok {
		limit = rateLimits["read"]
	}
	key := email + "|" + rateClass
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(limit), lastFill: now}
		l.buckets[key] = b
	}
	elapsed := now.Sub(b.lastFill).Seconds()
	b.tokens += elapsed * (float64(limit) / 60.0)
	if b.tokens > float64(limit) {
		b.tokens = float64(limit)
	}
	b.lastFill = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
