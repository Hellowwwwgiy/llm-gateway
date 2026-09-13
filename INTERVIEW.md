# SmartProxy — 面试弹药库

> **定位**：后端基建方向的多模型 LLM API 网关
> **一句话**：统一入口，把鉴权/限流/缓存/熔断/异步解耦/可观测都扛起来
> **耗时**：约 2 周（2026.09.01 - 2026.09.10）
> **仓库**：https://github.com/Hellowwwwgiy/llm-gateway

---

## 零、简历项目条目（已审计，零虚假）

**项目名称：** SmartProxy — LLM API 网关平台

**项目简介：** Go + Gin 工程级 LLM API 网关，Gateway + Dispatcher 双进程架构，Redis 同时承担 Lua 限流、Prompt MD5 缓存、LPUSH/BRPOP 异步队列三职责，三态熔断按 provider 隔离。

**项目业绩：** 冷启动真实 LLM 调用 700-1000ms 起（Gin 服务端日志实测，波动随 prompt 长度变化），同 prompt 缓存命中稳定 ~500µs，加速 **1400x**。双端口暴露 **14 个** Prometheus 指标覆盖全链路。异步请求 LPUSH 入队客户端 ~3ms / 服务端 500µs → Dispatcher BRPOP 消费 + LLM 调用 → 写回 Redis，全程无阻塞。Dispatcher 8-worker pool + 指数退避重试（3 次，100ms 倍增），`router.Register()` 一行代码扩展新 LLM，当前已接入 DeepSeek。

---

## 一、项目背景

1. **多个 AI 平台 API 不统一** — OpenAI / 通义 / 豆包 各一套
2. **成本不可控** — 同一 prompt 反复调 LLM 没有缓存，token 浪费
3. **没有统一鉴权和限流** — 各家 API Key 直接塞给前端，风控为零
4. **调用失败没降级** — 主模型挂了，整个业务跟着死

目标：做 OpenAI 兼容的单一入口，屏蔽后端模型差异。

---

## 二、架构总览

```
┌─────────────────────────────────────────────────────────────────┐
│                         用户 / 客户端                             │
└───────────────────────────┬─────────────────────────────────────┘
                            │ POST /api/v1/chat/completions
                            ▼
┌─────────────────────────────────────────────────────────────────┐
│                    Gateway :8080 (Gin)                           │
│  ① JWT → ② Metrics → ③ Router.Pick → ④ 限流 → ⑤ 缓存          │
│                                              → ⑥ 熔断          │
│                                              → ⑦ 重试          │
│                                              → ⑧ Provider      │
│  异步: POST /api/v1/chat/completions/async                      │
│    → ⑤ LPUSH Redis List → 立即返回 request_id                   │
└───────────────────────────┬─────────────────────────────────────┘
                            │ BRPOP（阻塞等待）
                            ▼
┌─────────────────────────────────────────────────────────────────┐
│                  Dispatcher :8081 (Worker Pool)                  │
│  BRPOP smartproxy:queue:async_chat                               │
│    → 反序列化 → 熔断器检查 → 重试 → Provider.Chat()              │
│    → Redis SetAsyncResult → 继续 BRPOP                          │
└───────────────────────────┬─────────────────────────────────────┘
                            │
                            ▼
┌─────────────────────────────────────────────────────────────────┐
│                        Redis :6379                               │
│  ├─ Lua 令牌桶限流 (HMGET/HMSET/EXPIRE)                          │
│  ├─ Prompt MD5 缓存 llmcache:{model}:{md5}                      │
│  ├─ 异步结果 async_result:{request_id}                           │
│  ├─ 主队列 smartproxy:queue:async_chat (List)                    │
│  └─ 死信队列 smartproxy:queue:dlq (List)                         │
└───────────────────────────┬─────────────────────────────────────┘
                            │
                            ▼
                    DeepSeek API（真实 LLM）
```

---

## 三、核心链路逐行讲解

### 同步 chat/completions

```
POST /api/v1/chat/completions
│
├─ ① JWT Middleware (auth/jwt.go)
│     jwt.Parse → Claims.user_id → 无 token 401
│
├─ ② Metrics Middleware (metrics.go)
│     http_requests_total++ + latency_histogram observe
│
├─ ③ 缓存检查 (cache.go:GetLLM)
│     key = "llmcache:" + model + ":" + md5(Marshal(reqBody))
│     Redis GET → 命中？
│       YES → cache_hits_total++ → 直接返回  ← 加速关键
│       NO  → cache_misses_total++ → 继续
│
├─ ④ 令牌桶限流 (ratelimiter/bucket.go Lua 脚本)
│     key = "ratelimit:user:" + userId
│     1次 Redis EVAL → 返回 0/1 → 无令牌 429
│
├─ ⑤ Router.Pick(model) → Provider.CircuitBreaker.Allow()
│     cbGroup.Get("openai") → Allow()
│     Closed → 放行；Open → 503；HalfOpen → 放一个试探
│
├─ ⑥ 指数退避重试 (resilience/retry.go DoWithRetry)
│     delay = 100ms * 2^attempt，cap at 2s
│     attempt=0 → 100ms, attempt=1 → 200ms, attempt=2 → 400ms
│
├─ ⑦ Provider.Chat() (llm/openai_provider.go)
│     http.Post → OpenAI 兼容格式 → 返回 JSON
│     成功 → RecordSuccess() → 异步写缓存
│     失败 → RecordFailure() → 重试 or 返回
│
└─ 返回响应 + provider_calls_total++
```

### 异步队列 BRPOP 时序图

```
Gateway                    Redis List                  Dispatcher
  │                           │                          │
  │── LPUSH(msg) ──────────►│                            │
  │     ~3ms                  │                            │
  │   返回 request_id ◄──────│                          │
  │                           │                          │
  │                           │◄──── BRPOP(timeout=0) ───│  ← 阻塞等待
  │                           │   有数据立即返回           │
  │                           │                          │
  │                           │   BRPOP 原子性 pop         │
  │                           │                          │
  │                           │                          │── json.Unmarshal(msg)
  │                           │                          │── cbGroup.Get("openai").Allow()
  │                           │                          │── DoWithRetry → Provider.Chat()
  │                           │                          │
  │                           │◄── SET async_result:id ─│
  │                           │                          │
  │   ┌─ 客户端轮询 ─┐        │                          │
  │   │ GET /result  │──────────► Redis GET ◄────────────│
  │   │ :id          │◄────────── 返回完整结果            │
  │   └──────────────┘        │                          │
```

代码位置：`internal/mq/redis.go:75-124`

---

## 四、底层技术细节

### 4.1 Redis Lua 令牌桶限流

`internal/ratelimiter/bucket.go:82-113`

```lua
-- KEYS[1] = "ratelimit:user:{userId}"
-- ARGV = [qps, cap, now(ms), requested(=1)]

local bucket = redis.call('HMGET', key, 'tokens', 'last')
local tokens = bucket[1]
local last   = bucket[2]

if not tokens then
    tokens = cap
    last = now
else
    local elapsed = (now - last) / 1000
    tokens = math.min(cap, tokens + elapsed * qps)
end

if tokens >= requested then
    tokens = tokens - requested
    redis.call('HMSET', key, 'tokens', tostring(tokens), 'last', tostring(now))
    redis.call('EXPIRE', key, cap * 10)
    return 1
else
    redis.call('HMSET', key, 'tokens', tostring(tokens), 'last', tostring(now))
    redis.call('EXPIRE', key, cap * 10)
    return 0
end
```

**为什么 Lua 不是 WATCH/MULTI：** 加令牌 + 扣令牌必须原子性。Lua 一次 RTT 搞定，WATCH 高并发下会产生大量事务冲突重试。

### 4.2 Prompt MD5 缓存 key

`internal/cache/cache.go:150-154`

```go
func cacheKey(model string, reqBody interface{}) string {
    raw, _ := json.Marshal(reqBody)
    sum := md5.Sum(raw)
    return fmt.Sprintf("llmcache:%s:%s", model, hex.EncodeToString(sum[:]))
}
```

**Bug 历史：** 之前用 handler 层 `chatRequest` 做序列化，后来改成 `internal/llm.LLMRequest`——两个 struct 字段一样但 JSON tag 顺序不同 → md5 不同 → 永远 miss。

### 4.3 三态熔断器

`internal/resilience/circuit.go`

```
Closed ──(failures >= 5)──► Open
  ▲                           │
  │  RecordSuccess()           │ time.Since(lastFailure) >= 30s
  │                           ▼
  │                      HalfOpen
  │                           │
  │    试探请求成功 ───────────┤  试探请求失败
  │                           ▼
  └─────────────────────► Open
```

**halfOpenAllowed 标志位：** HalfOpen 状态只放一个试探请求。

**为什么不用 sony/gobreaker：** 面试自己写更能讲清楚原理。

### 4.4 指数退避重试

`internal/resilience/retry.go:36-83`

```go
delay = 100ms * 2^attempt   // 100ms → 200ms → 400ms
delay = min(delay, 2s)       // cap at 2s
```

**Bug 历史：** `MaxDelay` 默认 0 → delay cap 成 0 → `time.After(0)` 瞬间返回 → 10 次重试 1ms 跑完。修复加 `if MaxDelay <= 0 { MaxDelay = InitialDelay * 8 }`。

---

## 五、技术决策（7 个 Why）

**Q1: 为什么 Redis 当 MQ？**
> 零额外组件（已经依赖 Redis 做限流和缓存）。Sidekiq/Bull/Celery 同款方案。`mq.New(url)` 自动判断协议，Gateway/Dispatcher 无感切换 RabbitMQ。

**Q2: 为什么熔断按 provider 隔离？**
> 全局熔断会导致一家挂了全都不能用。实现用 `resilience.NewGroup(5, 30*time.Second)` → `group.Get("openai")` / `group.Get("qwen")` 各拿各的。

**Q3: 缓存为什么不做流式？**
> 流式每次 chunk 边界不同，没法稳定做 key。而且流式是用户交互场景，重复率低。

**Q4: 为什么 14 个 Prometheus 指标？**
> 覆盖全链路（HTTP 3 + Provider 3 + 缓存 2 + 限流 1 + 队列 4 + 熔断 1）。Label 基数只留 provider/model，不让 cardinality 爆掉。

**Q5: 为什么做内存 fallback？**
> 开发机零依赖能跑。生产 Redis 挂了自动切本地令牌桶，保证可用性优先。

**Q6: 为什么双进程？**
> 独立扩缩、独立 metrics（Prometheus 按 job label 区分）、职责清晰。

**Q7: 为什么 Go？**
> 高并发原生支持、单二进制静态链接 exe、大厂后端 Go 份额越来越高。

---

## 六、熔断诚实边界（主动说加分）

**我没真触发过熔断** — 测试时不想浪费 DeepSeek token。但代码逻辑完整：
- Gateway 同步调用：handler 里 `cbGroup.Get("openai").Allow()`
- Dispatcher 异步消费：worker 里同样

**备答口径：** "我没真调挂 DeepSeek 测试熔断——怕浪费 token。但逻辑完整：5 次失败开闸，30s 后 HalfOpen 放一个试探，成功回 Closed，失败再 Open。`circuit_breaker_open_total` 计数器已经埋点了。"

---

## 七、加速比 1400x 计算推导

### Gin 服务端日志实测

```
冷启动（走真实 DeepSeek LLM）:
  17:29:41 | 714ms  | POST chat/completions   ← 最快冷启动
  17:34:34 | 1.01s  | POST chat/completions
  17:37:51 | 2.65s  | POST chat/completions

缓存命中（直接 Redis GET）:
  17:29:41 | 507µs  | POST chat/completions
  17:34:44 | 546µs  | POST chat/completions
  17:34:57 | 520µs  | POST chat/completions
```

### 计算

```
加速比 = 最快冷启动 / 稳定缓存命中
       = 714ms / 0.5ms = 1428 → 简历写 1400x
```

**为什么用最快的冷启动做分母？** 面试官自己 curl 测到 1000ms，加速比就是 2000x——只会比简历写的更高。

---

## 八、核心代码行号速查表

| 声明 | 文件:行号 |
|------|----------|
| Gin 框架 | `cmd/gateway/main.go:111` |
| 熔断 5 次 → Open 30s | `gateway/main.go:65`, `dispatcher/main.go:53` `NewGroup(5, 30*time.Second)` |
| Worker pool = 8 | `dispatcher/main.go:34` |
| 重试 3 次 / 100ms / 2s cap | `resilience/retry.go:22-24` |
| Redis Lua 令牌桶脚本 | `ratelimiter/bucket.go:82-113` |
| Prompt MD5 缓存 key | `cache/cache.go:150-154` |
| LPUSH 入队 | `mq/redis.go:68` |
| BRPOP 阻塞消费 | `mq/redis.go:79` `BRPop(ctx, 0, queueKey)` |
| router.Register + DeepSeek | `gateway/main.go:68-71` |
| 14 个 Prometheus 指标 | `metrics/metrics.go:226-249` |
| Dispatcher :8081 metrics | `dispatcher/main.go:37` |

---

## 九、面试高频问题预答

**Q1: 和 One API / OpenRouter 有什么区别？**
> One API 是多 key 管理后台，重点在 key 轮询。我的 SmartProxy 重点在高并发基建——Redis Lua 限流、三态熔断、Prometheus 指标、Redis List 异步队列。One API 没有熔断和指标体系。

**Q2: QPS 从 100 涨到 1000？**
> Redis 会成瓶颈。解决：限流用本地令牌桶做二级缓存、缓存命中率 > 60% 时 Redis QPS 可接受、横向扩 Gateway + Nginx + Redis Cluster。LLM TPM 才是最终瓶颈。

**Q3: Redis 挂了怎么办？**
> 自动 fallback 内存模式。生产做 Sentinel + AOF。

**Q4: 接新 LLM 改几行？**
> 两步：.env 加三个变量 + `router.Register(NewOpenAIProvider("qwen", key, baseURL, model), "qwen-plus")` 一行。

**Q5: BRPOP 和 GET 有什么区别？**
> BRPOP 是阻塞等待——timeout=0 时一直等到队列有数据才返回，Dispatcher 主循环就是 `for { BRPOP → handler }`，CPU 空闲不消耗。普通 GET 没数据立即返回 nil，要自己写轮询。另外 BRPOP 原子 pop，ack 是 no-op。

**Q6: 熔断 fallback 自动切换？**
> 当前只实现了熔断，fallback 还没写。设计：Provider 声明 `Capabilities{Fallbacks: []string}`，熔断触发时 Router 从 Fallbacks 挑备用。

**Q7: 限流 key 是 user_id + model？**
> 当前只有 `ratelimit:user:{userId}` — 一个用户所有模型共享 10 QPS。生产应该改成 `ratelimit:user:{userId}:model:{model}` 两个维度独立桶。

---

## 十、快速复现

```powershell
# 1. Redis
cd D:\agent\other1\redis-tmp\Redis-7.4.4-Windows-x64-msys2
.\redis-server.exe --port 6379 --save "" --appendonly no

# 2. 构建
cd D:\agent\llm-gateway
$env:Path = 'D:\Go\bin;' + $env:Path
$env:GOROOT = 'D:\Go'
go build -trimpath -ldflags "-s -w" -o dist\smartproxy-gateway.exe ./cmd/gateway
go build -trimpath -ldflags "-s -w" -o dist\smartproxy-dispatcher.exe ./cmd/dispatcher

# 3. Gateway
.\dist\smartproxy-gateway.exe

# 4. Dispatcher
.\dist\smartproxy-dispatcher.exe

# 5. 测试
curl -X POST http://localhost:8080/api/v1/login -H "Content-Type: application/json" -d "{\"user_id\":\"test\"}"
# 拿 token 后
curl -X POST http://localhost:8080/api/v1/chat/completions -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -d "{\"model\":\"deepseek-chat\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}"

# 6. Prometheus 指标
curl http://localhost:8080/metrics   # Gateway
curl http://localhost:8081/metrics   # Dispatcher
```

---

## 十一、面试备答策略

1. **简历每个数字都能 grep 到代码行号** — 带 GitHub 链接让面试官自己点
2. **诚实说没做压力测试** — 面试 demo 加速比是真实的，别吹生产 QPS
3. **主动暴露熔断没真触发** — 面试官反而觉得你靠谱
4. **加速比分母用最快冷启动** — 面试官自己测只会更高
5. **Redis 当 MQ 是 Sidekiq/Bull/Celery 同款** — 不是拍脑袋
