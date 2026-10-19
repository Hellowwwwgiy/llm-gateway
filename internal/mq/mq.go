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
	RequestID  string          `json:"request_id"`
	TraceID    string          `json:"trace_id"` // 链路追踪 ID，Gateway 生成，Dispatcher 日志携带
	UserID     string          `json:"user_id"`
	Model      string          `json:"model"`
	Req        json.RawMessage `json:"req"`
	RetryCount int             `json:"retry_count"` // 已重试次数，超过阈值进死信
}

// UnmarshalReq 把 RawMessage 解成实际请求
func (m *RequestMessage) UnmarshalReq(out interface{}) error {
	return json.Unmarshal(m.Req, out)
}

// Envelope 统一消息封装
type Envelope struct {
	Msg *RequestMessage // 完整消息（dispatcher 可修改 RetryCount 后重新入队）

	ackFn  func() error            // 确认已处理
	nackFn func(body []byte) error // 重新入队（可传更新后的 body）
	deadFn func(body []byte) error // 进死信队列
}

// Ack 确认
func (e *Envelope) Ack() error { return e.ackFn() }

// NackRequeue 重新入队（用当前 msg 的最新内容 marshal，包含更新后的 RetryCount）
func (e *Envelope) NackRequeue() error {
	body, err := json.Marshal(e.Msg)
	if err != nil {
		return err
	}
	return e.nackFn(body)
}

// NackDead 进死信队列
func (e *Envelope) NackDead() error {
	body, err := json.Marshal(e.Msg)
	if err != nil {
		return err
	}
	return e.deadFn(body)
}

// backend —— RabbitMQ 和 Redis 共用接口
type backend interface {
	Publish(ctx context.Context, msg *RequestMessage) error
	Consume(ctx context.Context, handler func(msg *RequestMessage, ack func() error, nack func([]byte) error, dead func([]byte) error) bool) error
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
		if err := handler(ctx, env.Msg); err != nil {
			// 失败重试：RetryCount++，超过 3 次进死信
			env.Msg.RetryCount++
			if env.Msg.RetryCount >= 3 {
				_ = env.NackDead()
			} else {
				_ = env.NackRequeue()
			}
			return true
		}
		_ = env.Ack()
		return true
	})
}

// ConsumeEnvelope 底层消费循环，handler 返回 false 停止
func (c *Client) ConsumeEnvelope(ctx context.Context, handler func(*Envelope) bool) error {
	return c.b.Consume(ctx, func(msg *RequestMessage, ack func() error, nack func([]byte) error, dead func([]byte) error) bool {
		env := &Envelope{
			Msg:   msg,
			ackFn: ack,
			nackFn: func(body []byte) error {
				return nack(body)
			},
			deadFn: func(body []byte) error {
				return dead(body)
			},
		}
		return handler(env)
	})
}
