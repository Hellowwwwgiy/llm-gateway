package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ===== 手写最小化 Prometheus 格式 metrics =====
// 避免引入 prometheus/client_golang 大依赖；面试也能讲清楚原理

var (
	registry = &Registry{
		counters:   make(map[string]*Counter),
		gauges:     make(map[string]*Gauge),
		histograms: make(map[string]*Histogram),
	}
)

type Registry struct {
	mu         sync.RWMutex
	counters   map[string]*Counter
	gauges     map[string]*Gauge
	histograms map[string]*Histogram
}

// Counter 单调累加
type Counter struct {
	mu    sync.Mutex
	name  string
	help  string
	value uint64
	labels map[string]string
}

// Gauge 可增可减
type Gauge struct {
	mu     sync.Mutex
	name   string
	help   string
	value  float64
	labels map[string]string
}

// Histogram 直方图（bucket 分位数）
type Histogram struct {
	mu      sync.Mutex
	name    string
	help    string
	buckets []float64 // 上界
	counts  []uint64  // 每个 bucket 的样本数
	count   uint64    // 总样本数
	sum     float64   // 总和
	labels  map[string]string
}

// ===== 注册 =====

func NewCounter(name, help string, labels ...string) *Counter {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	c := &Counter{name: name, help: help, labels: toLabelMap(labels)}
	registry.counters[name] = c
	return c
}

func NewGauge(name, help string, labels ...string) *Gauge {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	g := &Gauge{name: name, help: help, labels: toLabelMap(labels)}
	registry.gauges[name] = g
	return g
}

// DefaultBuckets 常用 latency bucket（ms）
var DefaultBuckets = []float64{5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000}

func NewHistogram(name, help string, buckets []float64, labels ...string) *Histogram {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if buckets == nil {
		buckets = DefaultBuckets
	}
	h := &Histogram{
		name:    name,
		help:    help,
		buckets: buckets,
		counts:  make([]uint64, len(buckets)),
		labels:  toLabelMap(labels),
	}
	registry.histograms[name] = h
	return h
}

func toLabelMap(labels []string) map[string]string {
	m := make(map[string]string)
	for i := 0; i+1 < len(labels); i += 2 {
		m[labels[i]] = labels[i+1]
	}
	return m
}

// ===== Counter =====

func (c *Counter) Inc() { atomic.AddUint64(&c.value, 1) }
func (c *Counter) Add(n uint64) { atomic.AddUint64(&c.value, n) }

// ===== Gauge =====

func (g *Gauge) Set(v float64) {
	g.mu.Lock()
	g.value = v
	g.mu.Unlock()
}
func (g *Gauge) Inc() { g.Add(1) }
func (g *Gauge) Dec() { g.Add(-1) }
func (g *Gauge) Add(d float64) {
	g.mu.Lock()
	g.value += d
	g.mu.Unlock()
}

// ===== Histogram =====

func (h *Histogram) Observe(v float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, b := range h.buckets {
		if v <= b {
			h.counts[i]++
		}
	}
	h.count++
	h.sum += v
}

// ===== HTTP Handler =====

func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, renderPrometheus())
	}
}

func renderPrometheus() string {
	registry.mu.RLock()
	defer registry.mu.RUnlock()

	var buf string

	// Counters
	names := make([]string, 0, len(registry.counters))
	for n := range registry.counters {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		c := registry.counters[n]
		buf += fmt.Sprintf("# HELP %s %s\n", c.name, c.help)
		buf += fmt.Sprintf("# TYPE %s counter\n", c.name)
		buf += fmt.Sprintf("%s%s %d\n", c.name, labelStr(c.labels), c.value)
	}

	// Gauges
	names = names[:0]
	for n := range registry.gauges {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		g := registry.gauges[n]
		buf += fmt.Sprintf("# HELP %s %s\n", g.name, g.help)
		buf += fmt.Sprintf("# TYPE %s gauge\n", g.name)
		buf += fmt.Sprintf("%s%s %g\n", g.name, labelStr(g.labels), g.value)
	}

	// Histograms
	names = names[:0]
	for n := range registry.histograms {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		h := registry.histograms[n]
		buf += fmt.Sprintf("# HELP %s %s\n", h.name, h.help)
		buf += fmt.Sprintf("# TYPE %s histogram\n", h.name)
		for i, b := range h.buckets {
			buf += fmt.Sprintf("%s_bucket%s{le=\"%g\"} %d\n", h.name, labelStr(h.labels), b, h.counts[i])
		}
		buf += fmt.Sprintf("%s_bucket%s{le=\"+Inf\"} %d\n", h.name, labelStr(h.labels), h.count)
		buf += fmt.Sprintf("%s_sum%s %g\n", h.name, labelStr(h.labels), h.sum)
		buf += fmt.Sprintf("%s_count%s %d\n", h.name, labelStr(h.labels), h.count)
	}

	return buf
}

func labelStr(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	s := "{"
	for i, k := range keys {
		if i > 0 {
			s += ","
		}
		s += fmt.Sprintf(`%s="%s"`, k, labels[k])
	}
	s += "}"
	return s
}

// ===== 业务级 metrics 单例（避免重复定义） =====

var (
	// HTTP 请求
	ReqTotal    = NewCounter("http_requests_total", "Total HTTP requests")
	ReqLatency  = NewHistogram("http_request_duration_ms", "HTTP request latency in ms", nil)
	ReqErrors   = NewCounter("http_request_errors_total", "Total HTTP 5xx errors")

	// Provider 调用
	ProviderCalls    = NewCounter("provider_calls_total", "Total LLM provider calls")
	ProviderLatency  = NewHistogram("provider_call_duration_ms", "Provider call latency in ms", nil)
	ProviderFailures = NewCounter("provider_failures_total", "Provider failures (network/5xx)")

	// 缓存
	CacheHits   = NewCounter("cache_hits_total", "Cache hits (llm result cache)")
	CacheMisses = NewCounter("cache_misses_total", "Cache misses")

	// 限流
	RateLimitRejects = NewCounter("ratelimit_rejects_total", "Requests rejected by rate limiter")

	// 队列
	MQPublishOk    = NewCounter("mq_publish_total", "MQ messages published")
	MQPublishFail  = NewCounter("mq_publish_failures_total", "MQ publish failures")
	MQConsumeOk    = NewCounter("mq_consume_total", "MQ messages consumed successfully")
	MQConsumeFail  = NewCounter("mq_consume_failures_total", "MQ consume failures")

	// 熔断器
	CircuitOpen = NewCounter("circuit_breaker_open_total", "Times circuit breaker opened")
)

// ObserveLatency 便捷方法：记录耗时（毫秒）
func ObserveLatency(start time.Time) float64 {
	return float64(time.Since(start).Milliseconds())
}
