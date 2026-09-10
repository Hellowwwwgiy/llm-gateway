package resilience

import (
	"sync"
	"time"
)

// CircuitBreaker 手动实现的简单熔断器（三态：Closed/Open/HalfOpen）
// 避免引入 sony/gobreaker 增加依赖；面试时也能自己讲清楚原理
type CircuitBreaker struct {
	mu              sync.Mutex
	failures        int           // 连续失败数
	threshold       int           // 失败阈值，超过后开闸
	resetTimeout    time.Duration // 开闸持续时间
	lastFailure     time.Time
	state           State // 当前状态
	halfOpenAllowed bool  // HalfOpen 状态下只允许一个请求试探
}

type State string

const (
	StateClosed   State = "closed"   // 正常通行
	StateOpen     State = "open"     // 熔断中，直接拒绝
	StateHalfOpen State = "half_open"// 试探恢复
)

// NewCircuitBreaker threshold: 连续失败 N 次后开闸；resetTimeout: 开闸多久后进入 HalfOpen
func NewCircuitBreaker(threshold int, resetTimeout time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		threshold:    threshold,
		resetTimeout: resetTimeout,
		state:        StateClosed,
	}
}

// Allow 判断当前请求是否放行
func (cb *CircuitBreaker) Allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case StateClosed:
		return true
	case StateOpen:
		if time.Since(cb.lastFailure) >= cb.resetTimeout {
			cb.state = StateHalfOpen
			cb.halfOpenAllowed = false
			return true // 放一个试探请求
		}
		return false
	case StateHalfOpen:
		if cb.halfOpenAllowed {
			return false
		}
		cb.halfOpenAllowed = true
		return true
	}
	return true
}

// RecordSuccess 成功调用
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.failures = 0
	cb.state = StateClosed
	cb.halfOpenAllowed = false
}

// RecordFailure 失败调用
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.failures++
	cb.lastFailure = time.Now()
	if cb.state == StateHalfOpen || cb.failures >= cb.threshold {
		cb.state = StateOpen
	}
}

// State 当前状态（只读）
func (cb *CircuitBreaker) State() State {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state
}

// Group 按 provider 名隔离的熔断器集合
type Group struct {
	mu    sync.RWMutex
	cbs   map[string]*CircuitBreaker
	thr   int
	reset time.Duration
}

// NewGroup 每个 provider 独立一个熔断器
func NewGroup(threshold int, resetTimeout time.Duration) *Group {
	return &Group{
		cbs:   make(map[string]*CircuitBreaker),
		thr:   threshold,
		reset: resetTimeout,
	}
}

func (g *Group) Get(name string) *CircuitBreaker {
	g.mu.RLock()
	cb, ok := g.cbs[name]
	g.mu.RUnlock()
	if ok {
		return cb
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	cb, ok = g.cbs[name]
	if !ok {
		cb = NewCircuitBreaker(g.thr, g.reset)
		g.cbs[name] = cb
	}
	return cb
}
