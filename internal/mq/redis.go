package mq

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis 队列 key 常量
const (
	redisQueueKey     = "smartproxy:queue:async_chat"
	redisDeadQueueKey = "smartproxy:queue:dlq"
)

type redisBackend struct {
	rdb      *redis.Client
	name     string
	queueKey string
	deadKey  string
}

// newRedisMQ —— 用 Redis List 当消息队列（LPUSH + BRPOP）
// 传入 redis://host:port 或裸 host:port
func newRedisMQ(url string) (*Client, error) {
	addr := url
	if i := strings.Index(addr, "redis://"); i == 0 {
		addr = addr[8:]
	}
	if i := strings.Index(addr, ":"); i < 0 {
		addr = addr + ":6379"
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:         addr,
		DialTimeout:  3 * time.Second,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("redis mq ping: %w", err)
	}

	b := &redisBackend{
		rdb:      rdb,
		name:     "redis-queue",
		queueKey: redisQueueKey,
		deadKey:  redisDeadQueueKey,
	}

	log.Printf("[MQ] redis queue connected on %s (lpush/brpop, key=%s)", addr, b.queueKey)
	return &Client{b: b}, nil
}

func (b *redisBackend) Publish(ctx context.Context, msg *RequestMessage) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return b.rdb.LPush(ctx, b.queueKey, body).Err()
}

func (b *redisBackend) SetQoS(int) error { return nil }
func (b *redisBackend) Close() error      { return b.rdb.Close() }
func (b *redisBackend) Name() string       { return b.name }

func (b *redisBackend) Consume(ctx context.Context, handler func(*RequestMessage, func() error, func() error, func() error) bool) error {
	log.Printf("[MQ] redis consumer started on %s (dlq=%s)", b.queueKey, b.deadKey)
	for {
		// BRPOP 阻塞等待；timeout=0 无限等
		results, err := b.rdb.BRPop(ctx, 0, b.queueKey).Result()
		if err == context.Canceled || ctx.Err() != nil {
			return nil
		}
		if err != nil {
			log.Printf("[MQ] redis BRPOP error, retry in 1s: %v", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(1 * time.Second):
			}
			continue
		}
		// results[1] = value（results[0] = key）
		if len(results) < 2 {
			continue
		}

		var msg RequestMessage
		body := []byte(results[1])
		if err := json.Unmarshal(body, &msg); err != nil {
			// 坏消息 → 进死信
			log.Printf("[MQ] redis unmarshal error, push to dlq: %v", err)
			_ = b.rdb.LPush(context.Background(), b.deadKey, body).Err()
			continue
		}

		// Redis BRPOP 已经从队列移除了，ack 就是 no-op
		ack := func() error { return nil }
		// requeue: 推回首部
		nack := func() error {
			ctx2, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			return b.rdb.LPush(ctx2, b.queueKey, body).Err()
		}
		// dead: 推进死信队列
		dead := func() error {
			ctx2, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			return b.rdb.LPush(ctx2, b.deadKey, body).Err()
		}

		if !handler(&msg, ack, nack, dead) {
			return nil
		}
	}
}
