package resilience

import (
	"sync"
	"testing"
	"time"
)

// TestCircuitBreaker_StateTransition 验证三态熔断器的状态转换
func TestCircuitBreaker_StateTransition(t *testing.T) {
	cb := NewCircuitBreaker(3, 50*time.Millisecond)

	// 初始 Closed
	if cb.State() != StateClosed {
		t.Fatalf("initial state = %s, want closed", cb.State())
	}

	// Closed 状态下每次 Allow() 都应返回 true
	for i := 0; i < 10; i++ {
		if !cb.Allow() {
			t.Fatalf("Closed: Allow()=false at i=%d", i)
		}
	}

	// 记录 3 次失败，应该开闸
	cb.RecordFailure()
	cb.RecordFailure()
	cb.RecordFailure()
	if cb.State() != StateOpen {
		t.Fatalf("after 3 failures, state = %s, want open", cb.State())
	}

	// Open 状态下不允许
	if cb.Allow() {
		t.Fatal("Open: Allow() should be false")
	}

	// 等 resetTimeout，进入 HalfOpen
	time.Sleep(60 * time.Millisecond)
	if !cb.Allow() {
		t.Fatal("after timeout, should allow 1 probe (HalfOpen)")
	}
	if cb.State() != StateHalfOpen {
		t.Fatalf("after timeout, state = %s, want half_open", cb.State())
	}

	// HalfOpen 下再多一次失败 → 重新 Open
	cb.RecordFailure()
	if cb.State() != StateOpen {
		t.Fatalf("HalfOpen + failure = %s, want open", cb.State())
	}

	// 再等超时
	time.Sleep(60 * time.Millisecond)
	cb.Allow() // 进入 HalfOpen
	cb.RecordSuccess()
	if cb.State() != StateClosed {
		t.Fatalf("HalfOpen + success = %s, want closed", cb.State())
	}
}

// TestCircuitBreaker_Concurrent 并发安全
func TestCircuitBreaker_Concurrent(t *testing.T) {
	cb := NewCircuitBreaker(5, 1*time.Second)
	var wg sync.WaitGroup
	results := make([]bool, 100)

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = cb.Allow()
		}(i)
	}
	wg.Wait()

	for _, r := range results {
		if !r {
			t.Fatalf("Closed 状态并发下 Allow() 不应该返回 false")
		}
	}
}

// TestGroup_Isolation 不同 provider 独立熔断
func TestGroup_Isolation(t *testing.T) {
	g := NewGroup(2, 10*time.Millisecond)

	cbA := g.Get("provider-A")
	cbB := g.Get("provider-B")

	// A 记录 2 次失败开闸
	cbA.RecordFailure()
	cbA.RecordFailure()

	if cbA.State() != StateOpen {
		t.Fatal("A should be open")
	}
	if cbB.State() != StateClosed {
		t.Fatal("B should still be closed (独立隔离)")
	}
}
