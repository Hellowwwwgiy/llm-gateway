package mq

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	amqp "github.com/rabbitmq/amqp091-go"
)

type rabbitBackend struct {
	conn    *amqp.Connection
	channel *amqp.Channel
	name    string
}

func newRabbitMQ(url string) (*Client, error) {
	conn, err := amqp.Dial(url)
	if err != nil {
		return nil, fmt.Errorf("rabbitmq dial: %w", err)
	}
	ch, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("rabbitmq channel: %w", err)
	}

	b := &rabbitBackend{conn: conn, channel: ch, name: "rabbitmq"}

	// 声明死信队列
	if _, err := ch.QueueDeclare(QueueDead, true, false, false, false, nil); err != nil {
		_ = ch.Close()
		_ = conn.Close()
		return nil, fmt.Errorf("declare dlq: %w", err)
	}

	args := amqp.Table{
		"x-dead-letter-exchange":    "",
		"x-dead-letter-routing-key": QueueDead,
	}
	if _, err := ch.QueueDeclare(QueueAsyncChat, true, false, false, false, args); err != nil {
		_ = ch.Close()
		_ = conn.Close()
		return nil, fmt.Errorf("declare queue: %w", err)
	}

	log.Printf("[MQ] rabbitmq connected, queues ready: %s (dlq: %s)", QueueAsyncChat, QueueDead)
	return &Client{b: b}, nil
}

func (b *rabbitBackend) Publish(ctx context.Context, msg *RequestMessage) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return b.channel.PublishWithContext(ctx,
		"", QueueAsyncChat,
		false, false,
		amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			MessageId:    msg.RequestID,
			Body:         body,
		},
	)
}

func (b *rabbitBackend) SetQoS(prefetch int) error {
	if prefetch <= 0 {
		prefetch = 1
	}
	return b.channel.Qos(prefetch, 0, false)
}

func (b *rabbitBackend) Close() error {
	if b.channel != nil {
		_ = b.channel.Close()
	}
	if b.conn != nil {
		return b.conn.Close()
	}
	return nil
}

func (b *rabbitBackend) Name() string { return b.name }

func (b *rabbitBackend) Consume(ctx context.Context, handler func(*RequestMessage, func() error, func() error, func() error) bool) error {
	deliveries, err := b.channel.ConsumeWithContext(ctx,
		QueueAsyncChat,
		"smartproxy-dispatcher",
		false, false, false, false, nil,
	)
	if err != nil {
		return fmt.Errorf("rabbitmq consume: %w", err)
	}

	log.Printf("[MQ] rabbitmq consumer started on %s", QueueAsyncChat)
	for {
		select {
		case <-ctx.Done():
			return nil
		case d, ok := <-deliveries:
			if !ok {
				log.Println("[MQ] rabbitmq delivery channel closed")
				return nil
			}
			var msg RequestMessage
			if err := json.Unmarshal(d.Body, &msg); err != nil {
				log.Printf("[MQ] rabbitmq unmarshal error, nack to dlq: %v", err)
				_ = d.Nack(false, false) // 不 requeue → 死信
				continue
			}

			ack := func() error { return d.Ack(false) }
			nack := func() error { return d.Nack(false, true) }  // requeue
			dead := func() error { return d.Nack(false, false) } // → dlq

			if !handler(&msg, ack, nack, dead) {
				return nil
			}
		}
	}
}
