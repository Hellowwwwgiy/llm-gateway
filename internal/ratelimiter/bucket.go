package ratelimiter

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"smartproxy/internal/config"
)

// TokenBucket 令牌桶，支持 Redis 分布式模式 和 本地内存模式
type TokenBucket struct {
	rdb *redis.Client

	qps float64
	cap int

	// 内存 fallback
	mu      sync.Mutex
	buckets map[string]*localBucket
}

type localBucket struct {
	tokens float64
	last   time.Time
}

// New 创建，rdb 可 nil（自动内存模式）
func New(cfg *config.Config, rdb *redis.Client) *TokenBucket {
	return &TokenBucket{
		rdb:     rdb,
		qps:     cfg.RateLimitQPS,
		cap:     cfg.RateLimitCap,
		buckets: make(map[string]*localBucket),
	}
}

// Allow 检查 key 是否能通过；返回 true=放行
func (tb *TokenBucket) Allow(ctx context.Context, key string) (bool, error) {
	if tb.rdb == nil {
		return tb.allowLocal(key), nil
	}
	return tb.allowRedis(ctx, key)
}

// ========== 本地内存令牌桶 ==========

func (tb *TokenBucket) allowLocal(key string) bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	now := time.Now()
	b, ok := tb.buckets[key]
	if !ok {
		b = &localBucket{tokens: float64(tb.cap), last: now}
		tb.buckets[key] = b
	} else {
		elapsed := now.Sub(b.last).Seconds()
		b.tokens = min(float64(tb.cap), b.tokens+elapsed*tb.qps)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// ========== Redis 分布式令牌桶（Lua 原子） ==========

func (tb *TokenBucket) allowRedis(ctx context.Context, key string) (bool, error) {
	script := `
local key      = KEYS[1]
local qps      = tonumber(ARGV[1])
local cap      = tonumber(ARGV[2])
local now      = tonumber(ARGV[3])
local requested = tonumber(ARGV[4])

local bucket = redis.call('HMGET', key, 'tokens', 'last')
local tokens = bucket[1]
local last   = bucket[2]

if not tokens then
    tokens = cap
    last = now
else
    tokens = tonumber(tokens)
    last   = tonumber(last)
    local elapsed = (now - last) / 1000
    tokens = math.min(cap, tokens + elapsed * qps)
end

if tokens >= requested then
    tokens = tokens - requested
    redis.call('HMSET', key, 'tokens', tostring(tokens), 'last', tostring(now))
    redis.call('EXPIRE', key, cap * 10)
    return 1
else
    redis.call('HMSET', key, 'tokens', tostring(tokens), 'last', tostring(now))
    redis.call('EXPIRE', key, cap * 10)
    return 0
end
`
	res, err := tb.rdb.Eval(ctx, script,
		[]string{"ratelimit:" + key},
		tb.qps, tb.cap, time.Now().UnixMilli(), 1,
	).Int()
	if err != nil {
		log.Printf("[ratelimit] redis eval failed, fallback to local: %v", err)
		return tb.allowLocal(key), nil
	}
	return res == 1, nil
}
