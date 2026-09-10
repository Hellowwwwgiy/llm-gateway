package stats

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"smartproxy/internal/llm"
)

// Recorder 统计收集器，支持 Redis 和 内存 fallback
type Recorder struct {
	rdb *redis.Client

	// 内存 fallback
	mu       sync.Mutex
	counters map[string]int
	records  []map[string]interface{}
}

// New 创建 recorder，rdb 可 nil
func New(rdb *redis.Client) *Recorder {
	return &Recorder{
		rdb:      rdb,
		counters: make(map[string]int),
		records:  make([]map[string]interface{}, 0, 1024),
	}
}

// RecordCall 记录一次 LLM 调用
func (r *Recorder) RecordCall(ctx context.Context, userID, model string, resp *llm.LLMResponse) error {
	if resp == nil {
		return nil
	}
	if r.rdb != nil {
		return r.recordRedis(ctx, userID, model, resp)
	}
	return r.recordLocal(userID, model, resp)
}

func (r *Recorder) recordRedis(ctx context.Context, userID, model string, resp *llm.LLMResponse) error {
	pipe := r.rdb.Pipeline()
	today := time.Now().Format("2006-01-02")

	pipe.Incr(ctx, fmt.Sprintf("stats:calls:total:%s", today))
	pipe.Incr(ctx, fmt.Sprintf("stats:calls:model:%s:%s", model, today))
	if userID != "" {
		pipe.Incr(ctx, fmt.Sprintf("stats:calls:user:%s:%s", userID, today))
	}
	if resp.Usage != nil {
		pipe.IncrBy(ctx, fmt.Sprintf("stats:tokens:total:%s", today), int64(resp.Usage.TotalTokens))
		pipe.IncrBy(ctx, fmt.Sprintf("stats:tokens:model:%s:%s", model, today), int64(resp.Usage.TotalTokens))
	}
	record := map[string]interface{}{
		"user_id": userID, "model": model, "latency": resp.Latency,
		"usage": resp.Usage, "response": resp.Content, "ts": time.Now().UnixMilli(),
	}
	data, _ := json.Marshal(record)
	key := fmt.Sprintf("stats:records:%s", today)
	pipe.LPush(ctx, key, data)
	pipe.LTrim(ctx, key, 0, 9999)

	_, err := pipe.Exec(ctx)
	if err != nil {
		log.Printf("[stats] redis pipeline failed: %v", err)
		return r.recordLocal(userID, model, resp)
	}
	return nil
}

func (r *Recorder) recordLocal(userID, model string, resp *llm.LLMResponse) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.counters["total_calls"]++
	if resp.Usage != nil {
		r.counters["total_tokens"] += resp.Usage.TotalTokens
	}
	if model != "" {
		r.counters["model:"+model]++
	}

	record := map[string]interface{}{
		"user_id": userID, "model": model, "latency_ms": resp.Latency,
		"content_length": len(resp.Content), "ts": time.Now().UnixMilli(),
	}
	r.records = append(r.records, record)
	if len(r.records) > 10000 {
		r.records = r.records[len(r.records)-10000:]
	}
	return nil
}

// DailySummary 拉取汇总
func (r *Recorder) DailySummary(ctx context.Context, date string) (map[string]interface{}, error) {
	if r.rdb != nil {
		if date == "" {
			date = time.Now().Format("2006-01-02")
		}
		result := map[string]interface{}{"date": date}
		calls, _ := r.rdb.Get(ctx, fmt.Sprintf("stats:calls:total:%s", date)).Int()
		tokens, _ := r.rdb.Get(ctx, fmt.Sprintf("stats:tokens:total:%s", date)).Int()
		result["total_calls"] = calls
		result["total_tokens"] = tokens
		return result, nil
	}
	// 内存模式
	r.mu.Lock()
	defer r.mu.Unlock()
	return map[string]interface{}{
		"date":         time.Now().Format("2006-01-02"),
		"total_calls":  r.counters["total_calls"],
		"total_tokens": r.counters["total_tokens"],
		"records_count": len(r.records),
	}, nil
}
