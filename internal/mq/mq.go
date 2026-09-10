package mq

import (
	"context"
	"encoding/json"
)

// QueueName 队列名常量
const (
	QueueAsyncChat = "async_chat_requests"
	QueueDead      = "async_chat_requests_dlq"
)

// RequestMessage 投递到队列的完整请求
type RequestMessage struct {
	RequestID string          `json:"request_id"`
	UserID    string          `json:"user_id"`
	Model     string          `json:"model"`
	Req       json.RawMessage `json:"req"`
}

// UnmarshalReq 把 RawMessage 解成实际请求
func (m *RequestMessage) UnmarshalReq(out interface{}) error {
	return json.Unmarshal(m.Req, out)
}

// Envelope 统一消息封装
type Envelope struct {
	RequestID string
	UserID    string
	Model     string
	Req       json.RawMessage

	ackFn  func() error // 确认已处理
	nackFn func() error // 重新入队
	deadFn func() error // 进死信队列
}

// Ack 确认
func (e *Envelope) Ack() error         { return e.ackFn() }
func (e *Envelope) NackRequeue() error { return e.nackFn() }
func (e *Envelope) NackDead() error    { return e.deadFn() }

// backend —— RabbitMQ 和 Redis 共用接口
type backend interface {
	Publish(ctx context.Context, msg *RequestMessage) error
	Consume(ctx context.Context, handler func(msg *RequestMessage, ack func() error, nack func() error, dead func() error) bool) error
	Close() error
	SetQoS(prefetch int) error
	Name() string
}

type ackFn func() error

// Client 统一门面
type Client struct {
	b backend
}

// New 按 URL 前缀自动选 backend
//
//	amqp://...  → RabbitMQ
//	redis://... 或裸地址 → Redis 队列
func New(url string) (*Client, error) {
	if len(url) >= 5 && url[:5] == "amqp:" {
		return newRabbitMQ(url)
	}
	return newRedisMQ(url)
}

// Publish 投递消息
func (c *Client) Publish(ctx context.Context, msg *RequestMessage) error {
	return c.b.Publish(ctx, msg)
}

// SetQoS 预取数
func (c *Client) SetQoS(prefetch int) error { return c.b.SetQoS(prefetch) }
func (c *Client) Name() string              { return c.b.Name() }
func (c *Client) Close() error              { return c.b.Close() }

// Consumer 简单回调
type Consumer func(ctx context.Context, msg *RequestMessage) error

// Consume 自动 ack/nack
func (c *Client) Consume(ctx context.Context, handler Consumer) error {
	return c.ConsumeEnvelope(ctx, func(env *Envelope) bool {
		if err := handler(ctx, &RequestMessage{
			RequestID: env.RequestID,
			UserID:    env.UserID,
			Model:     env.Model,
			Req:       env.Req,
		}); err != nil {
			_ = env.NackRequeue()
			return true
		}
		_ = env.Ack()
		return true
	})
}

// ConsumeEnvelope 底层消费循环，handler 返回 false 停止
func (c *Client) ConsumeEnvelope(ctx context.Context, handler func(*Envelope) bool) error {
	return c.b.Consume(ctx, func(msg *RequestMessage, ack, nack, dead func() error) bool {
		env := &Envelope{
			RequestID: msg.RequestID,
			UserID:    msg.UserID,
			Model:     msg.Model,
			Req:       msg.Req,
			ackFn:     ack,
			nackFn:    nack,
			deadFn:    dead,
		}
		return handler(env)
	})
}
