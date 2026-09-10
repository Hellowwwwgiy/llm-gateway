package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"smartproxy/internal/cache"
	"smartproxy/internal/config"
	"smartproxy/internal/llm"
	"smartproxy/internal/metrics"
	"smartproxy/internal/mq"
	"smartproxy/internal/resilience"
	"smartproxy/internal/stats"
)

// 配置：dispatcher 独立读取，支持 worker pool 大小
type dispatcherConfig struct {
	WorkerPoolSize int           // 并发 worker 数
	PrefetchCount  int           // QoS prefetch，防止单 worker 一次拿太多
	RetryDelay     time.Duration // 失败后重试间隔
	MetricsPort    int           // metrics + healthz HTTP 端口
}

func main() {
	cfg := config.Load()
	dc := dispatcherConfig{
		WorkerPoolSize: envInt("DISPATCHER_WORKERS", 8),
		PrefetchCount:  envInt("DISPATCHER_PREFETCH", 1), // 每个 worker 一次只拿 1 条
		RetryDelay:     5 * time.Second,
		MetricsPort:    cfg.DispatcherPort,
	}

	log.Printf("[dispatcher] starting workers=%d prefetch=%d", dc.WorkerPoolSize, dc.PrefetchCount)

	// === 依赖 ===
	redisCache := cache.New(cfg)
	if redisCache.Ping(context.Background()) != nil {
		log.Println("[dispatcher] redis NOT reachable — using in-memory fallback")
		redisCache.UseMemory()
	} else {
		log.Println("[dispatcher] redis connected")
	}

	recorder := stats.New(redisCache.RDB())
	router := llm.NewRouter()
	cbGroup := resilience.NewGroup(5, 30*time.Second)

	if cfg.OpenAIAPIKey != "" {
		router.Register(
			llm.NewOpenAIProvider("openai", cfg.OpenAIAPIKey, cfg.OpenAIBaseURL, cfg.OpenAIModel),
			cfg.OpenAIModel,
		)
	}

	// MQ 连接策略：RabbitMQ > Redis 队列
	var mqClient *mq.Client
	if cfg.RabbitMQURL != "" && len(cfg.RabbitMQURL) >= 5 && cfg.RabbitMQURL[:5] == "amqp:" {
		if m, err := mq.New(cfg.RabbitMQURL); err != nil {
			log.Printf("[dispatcher] rabbitmq connect failed: %v (fallback to redis)", err)
		} else {
			mqClient = m
		}
	}
	if mqClient == nil {
		if m, err := mq.New(cfg.RedisAddr); err != nil {
			log.Fatalf("[dispatcher] mq connect failed (rabbitmq + redis both unavailable): %v", err)
		} else {
			mqClient = m
			log.Printf("[dispatcher] mq connected: %s", m.Name())
		}
	}

	// 设置 QoS：prefetch，确保多个 worker 均匀消费
	if err := mqClient.SetQoS(dc.PrefetchCount); err != nil {
		log.Printf("[dispatcher] set QoS failed: %v", err)
	}

	// === HTTP 监控端口：/metrics + /healthz ===
	httpMux := http.NewServeMux()
	httpMux.Handle("/metrics", metrics.Handler())
	httpMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	httpSrv := &http.Server{Addr: ":" + strconv.Itoa(dc.MetricsPort), Handler: httpMux}
	go func() {
		log.Printf("[dispatcher] metrics http listening on :%d", dc.MetricsPort)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[dispatcher] metrics http server error: %v", err)
		}
	}()

	// === 消费主循环 ===
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
		<-quit
		log.Println("[dispatcher] shutting down...")
		cancel()
	}()

	// 带 worker pool 的消费：channel 分发任务
	taskCh := make(chan *mq.Envelope, 256)
	var wg sync.WaitGroup

	// 启动 worker pool
	for i := 0; i < dc.WorkerPoolSize; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for env := range taskCh {
				processTask(ctx, workerID, env, router, cbGroup, redisCache, recorder, dc.RetryDelay)
			}
		}(i)
	}

	// 主 goroutine：从 MQ 拉到 taskCh
	go func() {
		err := mqClient.ConsumeEnvelope(ctx, func(env *mq.Envelope) bool {
			select {
			case taskCh <- env:
				return true
			case <-ctx.Done():
				return false
			}
		})
		if err != nil {
			log.Printf("[dispatcher] consume loop ended: %v", err)
		}
		close(taskCh)
	}()

	// 等信号退出
	<-ctx.Done()
	log.Println("[dispatcher] shutting down...")

	// 优雅关闭 HTTP 监控端口（最多等 5s）
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = httpSrv.Shutdown(shutdownCtx)

	log.Println("[dispatcher] waiting for workers to drain...")

	// 给正在处理的 worker 最多 10s 收尾
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer drainCancel()

	// 简单做法：等 drainCtx 超时再关掉 MQ
	<-drainCtx.Done()

	wg.Wait()
	_ = redisCache.Close()
	_ = mqClient.Close()
	log.Println("[dispatcher] exited")
}

func processTask(
	ctx context.Context,
	workerID int,
	env *mq.Envelope,
	router *llm.Router,
	cbGroup *resilience.Group,
	redisCache *cache.Cache,
	recorder *stats.Recorder,
	retryDelay time.Duration,
) {
	msg := &mq.RequestMessage{
		RequestID: env.RequestID,
		UserID:    env.UserID,
		Model:     env.Model,
		Req:       env.Req,
	}

	log.Printf("[worker %d] processing request_id=%s model=%s", workerID, msg.RequestID, msg.Model)

	// 1. 反序列化 LLMRequest
	var llmReq llm.LLMRequest
	if err := msg.UnmarshalReq(&llmReq); err != nil {
		log.Printf("[worker %d] unmarshal req failed: %v", workerID, err)
		_ = env.NackDead() // 坏消息 → 死信
		return
	}

	// 2. 选 provider
	provider, err := router.Pick(msg.Model)
	if err != nil {
		log.Printf("[worker %d] pick provider failed: %v", workerID, err)
		_ = redisCache.SetAsyncResult(ctx, msg.RequestID, map[string]interface{}{
			"error": err.Error(), "done": true,
		}, 24*time.Hour)
		_ = env.Ack()
		return
	}

	cb := cbGroup.Get(provider.Name())

	// 3. 熔断器检查
	if !cb.Allow() {
		metrics.CircuitOpen.Inc()
		log.Printf("[worker %d] circuit open for %s — retry later", workerID, provider.Name())
		time.Sleep(retryDelay)
		_ = env.NackRequeue()
		return
	}

	metrics.ProviderCalls.Inc()
	start := time.Now()

	// 4. 带重试的调用（最多 3 次，指数退避）
	opt := resilience.DefaultRetryOption()
	opt.MaxAttempts = 3
	opt.InitialDelay = 500 * time.Millisecond

	var resp *llm.LLMResponse
	err = resilience.DoWithRetry(ctx, func() error {
		var e error
		resp, e = provider.Chat(&llmReq)
		return e
	}, opt)

	lat := metrics.ObserveLatency(start)
	metrics.ProviderLatency.Observe(lat)

	if err != nil {
		metrics.ProviderFailures.Inc()
		cb.RecordFailure()
		log.Printf("[worker %d] provider failed after retries: %v", workerID, err)

		// 达到重试上限 → 写错误结果 + ack（不再重试）
		_ = redisCache.SetAsyncResult(ctx, msg.RequestID, map[string]interface{}{
			"error": err.Error(), "done": true,
		}, 24*time.Hour)
		metrics.MQConsumeFail.Inc()
		_ = env.Ack() // 已写错误状态，ack 避免无限重入
		return
	}

	cb.RecordSuccess()
	metrics.MQConsumeOk.Inc()

	// 写回结果
	if err := redisCache.SetAsyncResult(ctx, msg.RequestID, resp, 24*time.Hour); err != nil {
		log.Printf("[worker %d] write async result failed: %v", workerID, err)
	}

	// 异步统计
	_ = recorder.RecordCall(ctx, msg.UserID, msg.Model, resp)

	_ = env.Ack()
	log.Printf("[worker %d] done request_id=%s latency_ms=%.0f", workerID, msg.RequestID, lat)
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		for _, c := range []byte(v) {
			if c >= '0' && c <= '9' {
				n = n*10 + int(c-'0')
			}
		}
		if n > 0 {
			return n
		}
	}
	return fallback
}
