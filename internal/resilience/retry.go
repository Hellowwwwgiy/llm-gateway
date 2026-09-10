package resilience

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// Retry backoff 参数
type RetryOption struct {
	MaxAttempts   int           // 总尝试次数，默认 3
	InitialDelay  time.Duration // 第一次重试等待，默认 100ms
	MaxDelay      time.Duration // 最大等待，默认 2s
	BackoffFactor float64       // 指数因子，默认 2.0
	Retryable     func(err error) bool
}

func DefaultRetryOption() RetryOption {
	return RetryOption{
		MaxAttempts:   3,
		InitialDelay:  100 * time.Millisecond,
		MaxDelay:      2 * time.Second,
		BackoffFactor: 2.0,
		Retryable:     defaultRetryable,
	}
}

func defaultRetryable(err error) bool {
	// 网络错误、5xx、超时才重试；4xx 不重试
	return err != nil
}

// DoWithRetry 带指数退避的重试
func DoWithRetry(ctx context.Context, op func() error, opt RetryOption) error {
	if opt.MaxAttempts <= 0 {
		opt.MaxAttempts = 3
	}
	if opt.BackoffFactor <= 0 {
		opt.BackoffFactor = 2.0
	}
	if opt.InitialDelay <= 0 {
		opt.InitialDelay = 100 * time.Millisecond
	}
	// MaxDelay 未设置时用 InitialDelay * 8 作为上限
	if opt.MaxDelay <= 0 {
		opt.MaxDelay = opt.InitialDelay * 8
	}

	var lastErr error
	for attempt := 0; attempt < opt.MaxAttempts; attempt++ {
		// 检查 ctx 取消
		if ctx.Err() != nil {
			return ctx.Err()
		}

		lastErr = op()
		if lastErr == nil {
			return nil
		}
		if opt.Retryable != nil && !opt.Retryable(lastErr) {
			return lastErr
		}

		// 最后一次就不等了
		if attempt == opt.MaxAttempts-1 {
			break
		}

		delay := float64(opt.InitialDelay) * math.Pow(opt.BackoffFactor, float64(attempt))
		if delay > float64(opt.MaxDelay) {
			delay = float64(opt.MaxDelay)
		}

		select {
		case <-time.After(time.Duration(delay)):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return errors.Join(lastErr, fmt.Errorf("after %d attempts", opt.MaxAttempts))
}
