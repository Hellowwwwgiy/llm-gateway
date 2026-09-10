package ratelimiter

import (
	"context"
	"testing"
	"time"
)

// TestLocalBucket_Allow 本地令牌桶放行/拒绝
func TestLocalBucket_Allow(t *testing.T) {
	tb := &TokenBucket{
		qps:     10,
		cap:     5,
		buckets: make(map[string]*localBucket),
	}

	ctx := context.Background()
	key := "user-1"

	// cap=5，前 5 次全部放行
	for i := 0; i < 5; i++ {
		ok, _ := tb.Allow(ctx, key)
		if !ok {
			t.Fatalf("第 %d 次应该放行", i+1)
		}
	}

	// 第 6 次应该被拒绝
	ok, _ := tb.Allow(ctx, key)
	if ok {
		t.Fatal("第 6 次应该被限流拒绝")
	}
}

// TestLocalBucket_Refill 等待后令牌自动补充
func TestLocalBucket_Refill(t *testing.T) {
	tb := &TokenBucket{
		qps:     100, // 每秒补 100 个 = 每 10ms 补 1 个
		cap:     2,
		buckets: make(map[string]*localBucket),
	}
	ctx := context.Background()
	key := "user-2"

	// 用光
	tb.Allow(ctx, key)
	tb.Allow(ctx, key)
	tb.Allow(ctx, key) // 被拒

	// 等 20ms → 应该补了 2 个
	time.Sleep(25 * time.Millisecond)

	ok, _ := tb.Allow(ctx, key)
	if !ok {
		t.Fatal("等待后应该恢复放行")
	}
}

// TestLocalBucket_KeyIsolation 不同 key 独立
func TestLocalBucket_KeyIsolation(t *testing.T) {
	tb := &TokenBucket{
		qps:     10,
		cap:     2,
		buckets: make(map[string]*localBucket),
	}
	ctx := context.Background()

	// user-a 用光自己的配额
	tb.Allow(ctx, "user-a")
	tb.Allow(ctx, "user-a")
	tb.Allow(ctx, "user-a") // 被拒

	// user-b 不应该受影响
	for i := 0; i < 3; i++ {
		ok, _ := tb.Allow(ctx, "user-b")
		if i < 2 && !ok {
			t.Fatalf("user-b 第 %d 次应该放行", i+1)
		}
	}
}
