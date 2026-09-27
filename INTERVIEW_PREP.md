# SmartProxy 面试深挖题库

> **简历原文在左边**，**讲解 + 追问题链在右边**。数字全 grep 代码可证，别吹生产 QPS，诚实说没做压力测试。
> **仓库**：https://github.com/Hellowwwwgiy/llm-gateway

---

## 简历条目 ①：项目简介

> Go + Gin 工程级 LLM API 网关，Gateway + Dispatcher 双进程架构，Redis 同时承担 Lua 限流、Prompt MD5 缓存、LPUSH/BRPOP 异步队列三职责，三态熔断按 provider 隔离。

### 逐字讲解

| 短语 | 背后是什么 |
|------|-----------|
| **Go + Gin** | Go 1.21 + Gin web 框架（`cmd/gateway/main.go:111`）。选 Go 的核心理由：单二进制静态链接 exe 直接跑、高并发 goroutine 原生、大厂后端 Go 份额越来越高。选 Gin 因为轻量 + 性能强 + 生态成熟。 |
| **工程级 LLM API 网关** | 不是 demo。真实调用 DeepSeek API、真实 Redis 做三件事、真实 Prometheus 14 指标、真实熔断重试、真实异步队列、真实 Docker 一键部署。 |
| **双进程 Gateway + Dispatcher** | Gateway (:8080) 同步处理所有 HTTP 请求 + 异步入队入口；Dispatcher (:8081) 只做 BRPOP 消费 + worker pool 并发执行 + 写回 Redis。独立扩缩、独立 metrics、职责清晰。 |
| **Redis 三职责** | ① Lua 令牌桶限流（原子脚本一次 RTT 搞定）② Prompt MD5 缓存（同 prompt 不重复调 LLM）③ Redis List 当异步队列（LPUSH/BRPOP + 死信 List）。Sidekiq/Bull/Celery 都是这套。 |
| **三态熔断按 provider 隔离** | `resilience.NewGroup(5, 30*time.Second)`（`gateway/main.go:67`）。每个 Provider（deepseek/qwen/豆包）独立一个熔断器——一家挂了不影响别家。三态：Closed（正常）→ Open（5 次失败后开闸 30s）→ HalfOpen（放一个试探，成功回 Closed，失败再 Open）。 |

### 面试官可能追问

**Q: 为什么双进程不单进程？**
> 三个理由：① **独立扩缩** — Dispatcher 要消费更多任务时单独多启几个就行，不影响 Gateway 接流量；② **独立 metrics** — Prometheus 按 job label 区分 Gateway 和 Dispatcher 的指标，Dashboard 好做；③ **职责清晰** — Gateway 扛 HTTP，Dispatcher 扛消费，出问题好定位。

**Q: 为什么 Redis 当 MQ 不选 RabbitMQ？**
> 三点：① **零额外组件** — 已经依赖 Redis 做限流和缓存，再加 RabbitMQ 运维成本翻倍；② **够用** — LPUSH/BRPOP 阻塞消费天然轮询，死信 List 手动 ack，Sidekiq/Bull/Celery 同款方案；③ **可选切换** — `mq.New(url)` 自动判断协议（`amqp://` 走 RabbitMQ），代码无感知。生产真要 MQ 就一行配置。

**Q: 熔断为什么按 provider 隔离？**
> 全局熔断会导致 DeepSeek 挂了→所有 LLM 请求都被拒→业务完全不可用。隔离后 DeepSeek 挂了通义和豆包还能正常用。实现上 `resilience.Group` 就是个 map，`group.Get("deepseek")` / `group.Get("qwen")` 各拿各的。

**Q: 熔断逻辑你自己写的为什么不用库？**
> 面试自己写能讲清楚原理。Sony/gobreaker 的三态状态机我理解了才自己写的。关键数据结构：`failures int`（连续失败计数）、`lastFailure time.Time`（开闸时间）、`halfOpenAllowed bool`（HalfOpen 只放一个试探）。

---

## 简历条目 ②：冷启动 700-1000ms，缓存 ~500µs，加速 1400x

### 逐字讲解

**数据源：Gin 服务端 handler 内部耗时日志**（不是 PowerShell 客户端计时）

```
冷启动（走真实 DeepSeek LLM）:     缓存命中（直接 Redis GET）:
  714ms   ← 最快                    503µs
  1.01s                              507µs
  2.65s                              520µs
  2.07s                              546µs
```

**加速比 = 最快冷启动 714ms / 稳定缓存命中 500µs ≈ 1428 → 简历写 1400x**

### 面试官可能追问

**Q: 为什么用最快冷启动算加速比？**
> 诚实。面试官自己 curl 测到的冷启动可能是 1.5s，那加速比就是 3000x——只会比简历写的更高。如果用最慢的 2.65s 算就是 5000x，假。

**Q: 缓存 key 怎么算的？**
> `md5(json.Marshal(*llm.LLMRequest))` — model 名 + messages 数组 + system prompt，一起序列化后 md5。Bug 历史：一开始用 handler 层的 `chatRequest` struct 序列化，后来改成 `internal/llm.LLMRequest`——两个 struct 字段一样但 JSON tag 顺序不同 → md5 不同 → 永远 miss。

**Q: 缓存失效策略？**
> 当前 Redis key TTL 是硬编码 1 小时（`cache.go` 里 `Set(key, value, 1*time.Hour)`）。没做主动失效，因为 LLM 输出是确定性的（同 prompt + 同 model + 同参数 → 同结果），不存实时数据。生产可能加版本号做主动失效。

**Q: 缓存穿透/击穿/雪崩防了吗？**
> ① 穿透（查不存在的 key）：当前没防。生产要加布隆过滤器或存空值标记。② 击穿（热点 key 过期瞬间大量并发）：当前没防。可以加 singleflight（Go 标准库）合并并发请求。③ 雪崩（大量 key 同时过期）：TTL 加随机偏移，分散过期时间。诚实说这三点是后续优化方向。

---

## 简历条目 ③：双端口暴露 14 个 Prometheus 指标

### 逐字讲解

`internal/metrics/metrics.go:226-249` 精确 14 个：

| 分类 | 指标 | 类型 | 谁暴露 |
|------|------|------|--------|
| HTTP | http_requests_total | Counter | Gateway |
|      | http_request_errors_total | Counter | Gateway |
|      | http_request_duration_ms | Histogram (11 bucket + +Inf) | Gateway |
| Provider | provider_calls_total | Counter | Gateway + Dispatcher |
|          | provider_failures_total | Counter | Gateway + Dispatcher |
|          | provider_call_duration_ms | Histogram | Gateway + Dispatcher |
| 缓存 | cache_hits_total / cache_misses_total | Counter | Gateway |
| 限流 | ratelimit_rejects_total | Counter | Gateway |
| 队列 | mq_publish_total / mq_publish_failures_total | Counter | Gateway |
|      | mq_consume_total / mq_consume_failures_total | Counter | Dispatcher |
| 熔断 | circuit_breaker_open_total | Counter | Gateway + Dispatcher |

**双端口**：Gateway `:8080/metrics`，Dispatcher `:8081/metrics`。Prometheus 按 job label 区分两个服务。

### 面试官可能追问

**Q: 为什么 Gateway 和 Dispatcher 都注册了 provider 相关指标？**
> 两个服务都调 LLM。Gateway 同步 chat/completions 直接调；Dispatcher 异步队列消费也调。指标各自独立埋点，Prometheus scrape 两个 endpoint 后按 `job="gateway"` / `job="dispatcher"` label 区分。

**Q: Histogram bucket 怎么选的？**
> `DefaultBuckets = [5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000]` — 指数增长。LLM 调用是毫秒级，5ms 到 10s 覆盖冷启动 + 缓存命中 + 网络抖动全范围。默认 bucket 够用，简历里就写了 11 个。

**Q: Label 基数爆了吗？**
> 只留了 `provider` / `model` 两个 label。**没有**用 `user_id` 当 label——那会每个用户生成一个 time series，Prometheus 直接爆。高基数 label 是新手常踩的坑。

**Q: Gauge 指标在哪？**
> 项目里**没用到 Gauge**。Prometheus 14 个全是 Counter + Histogram。想加的话可以加 `dispatcher_worker_pool_size`（Gauge，当前空闲 worker 数），但没写进去因为想让指标聚焦在请求链路上。

---

## 简历条目 ④：异步请求 LPUSH 2ms，Dispatcher BRPOP 消费，全程无阻塞

### 逐字讲解

**时序**：
```
Gateway                         Redis List                      Dispatcher
  │                                │                               │
  │── LPUSH(msg) ───────────────►│                               │
  │   ~2ms                         │                               │
  │   立即返回 request_id ◄──────│                               │
  │                                │                               │
  │                                │◄──── BRPOP(timeout=0) ───────│  ← 阻塞等待
  │                                │   有数据立即返回              │
  │                                │                               │
  │                                │   BRPOP 原子 pop              │
  │                                │                               │
  │                                │                               │── 熔断检查 → 重试 → Provider.Chat()
  │                                │◄── SET async_result:id ──────│
  │                                │                               │
  │   ┌─ 客户端轮询 ─┐              │                               │
  │   │ GET /result  │──────────► Redis GET                         │
  │   │ :id          │◄────────── 返回完整结果                      │
  │   └──────────────┘              │                               │
```

**关键代码**：`internal/mq/redis.go`
- LPUSH：`LPush(ctx, queueKey, jsonBytes)` — O(1)，不关心后面谁消费，立即返回
- BRPOP：`BRPop(ctx, 0, queueKey)` — timeout=0 永久阻塞，Redis 单线程保证原子 pop

### 面试官可能追问

**Q: BRPOP 和普通 GET/轮询有什么区别？**
> BRPOP 是阻塞等待 — timeout=0 时 Dispatcher goroutine 直接挂起，CPU 不消耗。队列有数据时 Redis **主动** pop 并返回。GET + 轮询要写 `for { GET; 没数据 sleep; }`，空转耗 CPU，延迟还不确定。

**Q: Redis 挂了怎么办？消息会丢吗？**
> 会丢。Redis List 内存存储，宕机就没了。生产要开 AOF + `appendfsync always`（每条都写磁盘），或者换成 RabbitMQ / Kafka。简历里没吹"可靠队列"——诚实。

**Q: Dispatcher 怎么优雅退出？**
> `signal.Notify` + context cancel。main goroutine 收到 Ctrl+C → cancel context → 所有 worker goroutine 退出循环 → 最多等 10s 让正在处理的请求收尾（`dispatcher/main.go` 里有 drain 等待）。

**Q: 死信队列怎么用？**
> Worker 消费失败后（3 次重试全挂）→ 不丢，BRPOPLPUSH 转到 `smartproxy:queue:dlq`（死信 List）。运维可以单独写脚本扫描 dlq 里的消息重新入队，或者报警人工介入。

---

## 简历条目 ⑤：Dispatcher 8-worker pool + 指数退避重试（3 次，100ms 倍增）

### 逐字讲解

**Worker Pool**（`dispatcher/main.go:34`）：
```go
WorkerPoolSize: 8    // 并发 worker 数
PrefetchCount:  1    // QoS prefetch，每个 worker 一次只拿 1 条
```
BRPOP 主循环 → 拿到消息 → 投递给 `taskChan`（带缓冲 channel）→ 8 个 worker goroutine 同时消费。Prefetch=1 确保多个 worker 均匀分布任务，不让某个快 worker 一次抢太多。

**指数退避**（`resilience/retry.go:22-24, 71-73`）：
```go
MaxAttempts:   3
InitialDelay:  100ms
MaxDelay:      2s        // cap at 2s
BackoffFactor: 2.0

// 延迟计算:
// attempt=0 → 100ms * 2^0 = 100ms
// attempt=1 → 100ms * 2^1 = 200ms
// attempt=2 → 100ms * 2^2 = 400ms
// cap: 超过 2s 就卡 2s（当前 3 次没触发 cap，演示用）
delay = min(InitialDelay * BackoffFactor^attempt, MaxDelay)
```

### 面试官可能追问

**Q: Worker Pool 为什么选 8？不是越多越好？**
> Dispatcher 里真正耗时的是 LLM 调用（网络 IO）。LLM 响应在 700-1000ms 之间，一个 worker 一秒最多处理 ~1 个请求。8 个 worker 理论并发 ~8 QPS。生产 QPS 再高可以横向扩 Dispatcher 进程——Redis List 天生支持多消费者竞争消费。**没做过压力测试**——诚实说。

**Q: Retry MaxDelay 默认 0 怎么办？**
> 我写了兜底（`retry.go:46-48`）：
```go
if opt.MaxDelay <= 0 {
    opt.MaxDelay = opt.InitialDelay * 8  // 100ms * 8 = 800ms
}
```
Bug 历史：之前没这个兜底 → delay cap 成 0 → `time.After(0)` 瞬间返回 → 10 次重试 1ms 跑完 → 后端没任何喘息时间。

**Q: Context 取消能立即停重试吗？**
> 能。`retry.go:52` 的 `for` 循环里每次都检查 `ctx.Err()`：
```go
for attempt := 0; attempt < opt.MaxAttempts; attempt++ {
    select {
    case <-ctx.Done():
        return ctx.Err()
    default:
    }
    // ... 执行 attempt，失败后 sleep delay
    select {
    case <-time.After(delay):
    case <-ctx.Done():
        return ctx.Err()
    }
}
```
测试 `TestRetry_ContextCancel` PASS 证实了。

**Q: 重试和熔断的顺序？**
> **先重试，重试全挂再计数失败**。一次 Provider 调用失败 → DoWithRetry 重试 3 次 → 还失败 → 返回 error → 调用方 `cb.RecordFailure()` → 熔断器失败计数 +1。这样避免单次瞬时网络抖动就开闸。

---

## 简历条目 ⑥：`router.Register()` 一行代码扩展新 LLM，当前已接入 DeepSeek

### 逐字讲解

```go
// gateway/main.go:70-74  — 当前注册 DeepSeek
router.Register(
    llm.NewOpenAIProvider(
        "deepseek",
        config.OpenAIKey,
        config.OpenAIBaseURL,   // https://api.deepseek.com
        config.OpenAIModel,     // deepseek-chat
    ),
    "deepseek-chat",           // model 字符串 → Provider 的映射 key
)
```

**加通义/豆包就是改注册行**：
```go
// 加通义
router.Register(
    llm.NewOpenAIProvider(
        "qwen",
        os.Getenv("QWEN_API_KEY"),
        "https://dashscope.aliyuncs.com/compatible-mode/v1",
        "qwen-plus",
    ),
    "qwen-plus",
)
```

**为什么 OpenAI 兼容格式能适配所有 LLM？** 通义、豆包、DeepSeek 都做了 OpenAI `/v1/chat/completions` 兼容 endpoint——因为 OpenAI 格式成了事实标准。所以只需要一个 `openai_provider.go` 通用实现，不用每个 LLM 写一个。

### 面试官可能追问

**Q: Router.Pick 内部怎么路由？**
> `map[string]Provider`。`model` 字符串 → Provider 接口。`provider.Name()` 返回 "deepseek"/"qwen"，用来在熔断器 Group 里 `Get("deepseek")` 拿独立熔断器，以及做 Prometheus label。

**Q: 那为什么还写了个 Provider 接口？直接 struct 硬编码不行吗？**
> 面试技巧——接口解耦。`Provider` 只有 `Chat(ctx, req) (LLMResponse, error)` 和 `ChatStream(ctx, req)` 两个方法。以后接一个**不兼容 OpenAI 的 LLM**（比如 Anthropic Claude 原生格式），只要实现这两个方法，Router 无感切换。

**Q: Fallback 自动切换做了吗？**
> 没做。当前只有熔断，没有 fallback。设计思路：Provider 声明 `Capabilities{Fallbacks: []string{"qwen-plus", "doubao-pro"}}`，熔断触发时 Router 依次尝试 fallback。面试官问就诚实说"这是下一阶段"，别硬吹。

---

## 简历条目 ⑦：LPUSH 入队客户端 ~3ms

### 逐字讲解

**LPUSH** 是 Redis O(1) 操作——队头插入一条消息，不关心后面谁消费、队列有多长。所以从 Gateway 调用 `LPUSH` 到 Redis 返回 `OK`，本地到 localhost 延迟 + Redis 单线程处理 ≈ 3ms。

对比：
| 同步调用 | 异步调用 |
|---------|---------|
| Gateway → Provider → 700-1000ms | Gateway → Redis LPUSH → 3ms |
| 用户一直等 | 用户拿 request_id 立即返回 → 后台慢慢处理 |

### 面试官可能追问

**Q: 客户端怎么知道异步结果好了？**
> 轮询。客户端拿 `request_id` → 隔 2 秒 GET `/api/v1/chat/completions/result/:id` → Dispatcher 写回 Redis 的 `async_result:{request_id}` → 有结果返回完整 JSON，没有返回 `{"status":"pending"}`。

**Q: 为什么不用 WebSocket/SSE 推送结果？**
> 架构简化考虑。轮询实现最简单，兼容性最好（Postman/curl 都能测）。SSE 要保持长连接，Gateway 要维护连接池 + 等 Dispatcher 完成后推送，复杂度翻倍。面试说"轮询够用，生产可以升级 SSE"。

**Q: LPUSH vs RPUSH？**
> LPUSH 队头 + BRPOP 队尾 = **LIFO**（后进先出）。换成 RPUSH + BRPOP 是 FIFO（先进先出）。**当前没区分**——简历里写的 LPUSH/BRPOP 队头队尾都用，消息顺序不关键。

---

## 通用后端高频问题（不局限简历条目）

### Go 语言

**Q: goroutine 泄漏怎么防？**
> 三个手段：① Context cancel（面试代码里 `select { case <-ctx.Done(): return }` 到处有）② channel 有界缓冲（不无限扩）③ defer wg.Done() 在 goroutine 入口。Dispatcher worker pool 用 context 统一管理退出，main goroutine 收到 SIGTERM → cancel ctx → 所有 worker 退出。

**Q: interface 底层结构？**
> 两个指针：type (指向类型信息表) + data (指向实际数据)。nil interface 和 (T)(nil) 的区别：`var i interface{}; i == nil` → true；`var p *MyError = nil; var err error = p; err == nil` → false。**面试代码 Provider 接口没踩这个坑**，但备用。

**Q: channel 无界缓冲会 OOM？**
> 会。`make(chan int)` 是同步无缓冲（默认）；`make(chan int, 100)` 有界缓冲。面试代码里 Dispatcher 的 `taskChan` 是有缓冲的，不会无限涨。

### Redis

**Q: 为什么用 Lua 不用 WATCH/MULTI 做限流？**
> 令牌桶加令牌 + 扣令牌 + 设过期这三步必须原子性。Lua 脚本**一次 RTT** 搞定。WATCH/MULTI 在高并发下会产生大量事务冲突重试（乐观锁竞争），性能差远了。

**Q: Redis Pipeline 和 Lua 的区别？**
> Pipeline 是**多条命令批量发送**，节省 RTT，但每条命令之间不原子。Lua 是**单脚本执行**，命令之间原子。面试代码限流用 Lua（需要原子），没用到 Pipeline（简单 GET/SET 够用）。

**Q: Redis 数据结构选哪种？**
> - 限流：Hash（存 tokens + last 两个字段）
> - 缓存：String（直接 SET 整个 JSON）
> - 队列：List（LPUSH/BRPOP）
> - 结果：String（SET async_result:{id}）

### 分布式/系统设计

**Q: 单机瓶颈到了怎么办？**
> 从小到大：① Gateway 加本地令牌桶二级缓存（减轻 Redis 压力）② Redis Cluster 分片（缓存 key 按 model 分片）③ 多 Gateway + Nginx 负载均衡 ④ 多 Dispatcher（Redis List 天生多消费者竞争）⑤ LLM TPM 才是最终瓶颈，横向扩 Provider 实例。**诚实说没做压力测试，这些是设计上想过的方向**。

**Q: 幂等性怎么做？**
> JWT 有 jti（JWT ID）—— Gateway 可以存已处理的 jti，重复请求直接返回缓存结果。当前没做，面试说"如果接生产环境，这是第一个加的功能"。

**Q: 跨进程调用 Gateway→Dispatcher 用 RPC 好还是 Redis 消息好？**
> 用了 Redis List。理由：Gateway 异步入口只需要"扔进去立即返回"，不需要等待结果。同步 RPC（gRPC）会阻塞 Gateway 接流量。如果以后需要 Gateway 主动拉 Dispatcher 做健康检查，再加 gRPC。

**Q: 为什么不拆成微服务？**
> 两个原因：① 实习生项目，**复杂度要可控**——微服务 + Kubernetes 调试成本太高；② Redis 三职责已经足够解耦，双进程比微服务更轻但功能上等价。面试说"架构演进路径：单体 Gateway → 双进程 Gateway+Dispatcher（当前）→ 真微服务（下一阶段）"。

### 安全

**Q: 你的 DeepSeek API Key 在代码里吗？**
> **不在**。`.env` 里有真实 Key，`.gitignore` 排除了 `.env` 和 `.env.local`。GitHub 上只有 `.env.example` 模板。Docker compose 用 `env_file: .env`，镜像里不打包密钥。

**Q: JWT secret 硬编码了吗？**
> 没。`config.JWT_SECRET` 从 `.env` 读，默认 `change-me-in-production-please`。启动时如果是默认值会打 warn 日志提醒。

---

## 附：简历 vs 代码行号速查

| 简历数字 | 代码位置 | 验证命令 |
|---------|---------|---------|
| **双进程 Gateway+Dispatcher** | `cmd/gateway/main.go` :8080, `cmd/dispatcher/main.go` :8081 | `netstat -ano | findstr "8080 8081"` |
| **熔断 5 次 → Open 30s** | `gateway/main.go:67` `dispatcher/main.go:53` `NewGroup(5, 30*time.Second)` | grep `NewGroup` |
| **Worker Pool = 8** | `dispatcher/main.go:34` `WorkerPoolSize: envInt(..., 8)` | grep `WorkerPoolSize` |
| **重试 3 次 / 100ms / 2s cap** | `resilience/retry.go:22-25` | grep `MaxAttempts` |
| **14 个 Prometheus 指标** | `metrics/metrics.go:226-249` | `curl :8080/metrics | findstr "^# HELP" | measure` |
| **指数退避 100ms 倍增** | `retry.go:71` `float64(InitialDelay) * math.Pow(2, attempt)` | grep `BackoffFactor` |
| **router.Register 一行扩新 LLM** | `gateway/main.go:70-74` | grep `router.Register` |
| **Redis Lua 令牌桶** | `ratelimiter/bucket.go:82-113` | grep -A30 `script =` |
| **LPUSH 入队** | `mq/redis.go:68` `LPush(ctx, queueKey, msg)` | grep `LPush` |
| **BRPOP 阻塞消费** | `mq/redis.go:79` `BRPop(ctx, 0, queueKey)` | grep `BRPop` |
| **Prompt MD5 缓存 key** | `cache/cache.go:150-154` `md5.Sum(json.Marshal(reqBody))` | grep `cacheKey` |
| **三态熔断器** | `circuit.go:13-16` state = Closed/Open/HalfOpen | grep `State` |
| **三态熔断按 provider 隔离** | `circuit.go:99-100` `type Group map[string]*CircuitBreaker` | grep `type Group` |
