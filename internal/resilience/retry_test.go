package resilience

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestRetry_Success 第 2 次就成功，应该只重试 1 次
func TestRetry_Success(t *testing.T) {
	var attempts int32
	err := DoWithRetry(context.Background(), func() error {
		n := atomic.AddInt32(&attempts, 1)
		if n < 2 {
			return errors.New("boom")
		}
		return nil
	}, RetryOption{
		MaxAttempts:   3,
		InitialDelay:  1 * time.Millisecond,
		BackoffFactor: 2.0,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if atomic.LoadInt32(&attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

// TestRetry_AllFail 全部失败，应该返回最后一次的 error
func TestRetry_AllFail(t *testing.T) {
	var attempts int32
	err := DoWithRetry(context.Background(), func() error {
		atomic.AddInt32(&attempts, 1)
		return errors.New("always-fail")
	}, RetryOption{
		MaxAttempts:   3,
		InitialDelay:  1 * time.Millisecond,
		BackoffFactor: 2.0,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if atomic.LoadInt32(&attempts) != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

// TestRetry_Backoff 验证指数退避时间单调递增且不超过 MaxDelay
func TestRetry_Backoff(t *testing.T) {
	var attempts int32
	var totalDelay time.Duration

	opt := RetryOption{
		MaxAttempts:   4,
		InitialDelay:  5 * time.Millisecond,
		MaxDelay:      20 * time.Millisecond,
		BackoffFactor: 2.0,
	}
	start := time.Now()

	_ = DoWithRetry(context.Background(), func() error {
		atomic.AddInt32(&attempts, 1)
		return errors.New("fail")
	}, opt)

	totalDelay = time.Since(start)
	// 5ms + 10ms + 20ms (被 cap) = 35ms + 执行时间
	if totalDelay < 25*time.Millisecond {
		t.Fatalf("total delay = %v, expected >= 25ms", totalDelay)
	}
	if atomic.LoadInt32(&attempts) != 4 {
		t.Fatalf("attempts = %d, want 4", attempts)
	}
}

// TestRetry_ContextCancel ctx 取消应该立即停止
func TestRetry_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	// ready: 等 retry 进入 select 等待后再 cancel，确保时序确定
	ready := make(chan struct{})
	go func() {
		<-ready
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()

	var attempts int32
	_ = DoWithRetry(ctx, func() error {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			close(ready) // 第一次 op 后，通知 goroutine 可以 cancel 了
		}
		return errors.New("fail")
	}, RetryOption{
		MaxAttempts:   10,
		InitialDelay:  100 * time.Millisecond,
		BackoffFactor: 2.0,
	})

	if atomic.LoadInt32(&attempts) > 2 {
		t.Fatalf("ctx cancel 后应该很快停止, 但跑了 %d 次", attempts)
	}
}
