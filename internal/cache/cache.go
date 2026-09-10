package cache

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"smartproxy/internal/config"
)

// Cache Redis 封装，支持 Redis 不可用时自动内存 fallback
type Cache struct {
	rdb *redis.Client

	// 内存 fallback
	mu    sync.RWMutex
	store map[string]memEntry
}

type memEntry struct {
	data    []byte
	expires time.Time
}

// New 初始化，cfg.Ping 失败时自动降级到内存模式
func New(cfg *config.Config) *Cache {
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
	})
	c := &Cache{
		rdb:   rdb,
		store: make(map[string]memEntry),
	}
	// 后台清理过期内存 key
	go c.gcLoop()
	return c
}

// UseMemory 强制使用内存模式（Redis 连不上时调用）
func (c *Cache) UseMemory() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rdb = nil
	log.Println("[cache] switched to in-memory fallback mode")
}

// IsMemory 只读内存模式？
func (c *Cache) IsMemory() bool { return c.rdb == nil }

// ErrMemoryMode 内存 fallback 模式的哨兵错误
var ErrMemoryMode = errors.New("in-memory fallback mode (redis not connected)")

// Ping 健康检查 — 内存模式返回 ErrMemoryMode 让调用方知道不是真 Redis
func (c *Cache) Ping(ctx context.Context) error {
	if c.rdb == nil {
		return ErrMemoryMode
	}
	return c.rdb.Ping(ctx).Err()
}

// Close 关闭连接
func (c *Cache) Close() error {
	if c.rdb != nil {
		return c.rdb.Close()
	}
	return nil
}

// gcLoop 内存过期清理（每 30s 扫一次）
func (c *Cache) gcLoop() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for range t.C {
		c.mu.Lock()
		now := time.Now()
		for k, v := range c.store {
			if !v.expires.IsZero() && v.expires.Before(now) {
				delete(c.store, k)
			}
		}
		c.mu.Unlock()
	}
}

// ============ 通用 KV ============

func (c *Cache) Get(ctx context.Context, key string, out interface{}) error {
	if c.rdb != nil {
		val, err := c.rdb.Get(ctx, key).Result()
		if err == nil {
			return json.Unmarshal([]byte(val), out)
		}
	}
	c.mu.RLock()
	e, ok := c.store[key]
	c.mu.RUnlock()
	if !ok {
		return fmt.Errorf("key not found")
	}
	if !e.expires.IsZero() && e.expires.Before(time.Now()) {
		c.mu.Lock()
		delete(c.store, key)
		c.mu.Unlock()
		return fmt.Errorf("key expired")
	}
	return json.Unmarshal(e.data, out)
}

func (c *Cache) Set(ctx context.Context, key string, val interface{}, ttl time.Duration) error {
	data, err := json.Marshal(val)
	if err != nil {
		return err
	}
	if c.rdb != nil {
		return c.rdb.Set(ctx, key, data, ttl).Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var exp time.Time
	if ttl > 0 {
		exp = time.Now().Add(ttl)
	}
	c.store[key] = memEntry{data: data, expires: exp}
	return nil
}

func (c *Cache) Delete(ctx context.Context, key string) error {
	if c.rdb != nil {
		return c.rdb.Del(ctx, key).Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.store, key)
	return nil
}

// ============ 请求缓存（prompt 去重） ============

// cacheKey 基于 model + 请求体的哈希
func cacheKey(model string, reqBody interface{}) string {
	raw, _ := json.Marshal(reqBody)
	sum := md5.Sum(raw)
	return fmt.Sprintf("llmcache:%s:%s", model, hex.EncodeToString(sum[:]))
}

// GetLLM 尝试命中相同 model+prompt 的结果
func (c *Cache) GetLLM(ctx context.Context, model string, reqBody interface{}, out interface{}) bool {
	key := cacheKey(model, reqBody)
	if err := c.Get(ctx, key, out); err != nil {
		return false
	}
	return true
}

// SetLLM 写入请求缓存
func (c *Cache) SetLLM(ctx context.Context, model string, reqBody interface{}, resp interface{}, ttl time.Duration) error {
	key := cacheKey(model, reqBody)
	return c.Set(ctx, key, resp, ttl)
}

// ============ 异步结果存取（轮询模式） ============

const resultKeyPrefix = "async_result:"

// SetAsyncResult 存异步任务结果（key=request_id）
func (c *Cache) SetAsyncResult(ctx context.Context, requestID string, val interface{}, ttl time.Duration) error {
	return c.Set(ctx, resultKeyPrefix+requestID, val, ttl)
}

// GetAsyncResult 取异步任务结果
func (c *Cache) GetAsyncResult(ctx context.Context, requestID string, out interface{}) error {
	return c.Get(ctx, resultKeyPrefix+requestID, out)
}

// ============ Redis client 直接暴露（限流需要） ============

func (c *Cache) RDB() *redis.Client { return c.rdb }
