# SmartProxy — 多模型 LLM API 网关

> **定位**：后端基建方向的工程级 LLM API 网关
> **一句话**：统一入口，把鉴权 / 限流 / 缓存 / 熔断 / 异步解耦 / 可观测全扛起来
> **仓库**：https://github.com/Hellowwwwgiy/llm-gateway

---

## 一、项目简介

SmartProxy 是一个 Go + Gin 实现的多模型 LLM API 网关。用户调一个 OpenAI 兼容接口，后端负责 **JWT 鉴权、Redis Lua 令牌桶限流、Prompt MD5 去重缓存、三态熔断（按 provider 隔离）、指数退避重试、Redis List 异步队列、Prometheus 全链路指标**。

当前已接入 **DeepSeek**（真实 LLM 调用），架构支持一行代码扩展通义 / 豆包 / GPT-4 等任意 OpenAI 兼容模型。

### 核心数据（全部来自真实运行，可复现）

| 指标 | 实测值 | 来源 |
|------|--------|------|
| 冷启动 LLM 调用 | 700-1000ms | Gin 服务端 handler 内部耗时日志 |
| 缓存命中 | ~500µs | Gin 服务端日志 |
| 加速比 | ~1400x | 714ms / 0.5ms |
| Prometheus 指标 | 14 个 | `metrics.go:226-249`（HELP 行精确计数） |
| 异步入队 | ~3ms | LPUSH Redis List |
| Worker Pool | 8 | `dispatcher/main.go:34` |
| 单元测试 | 10/10 PASS | `go test ./internal/...` |

---

## 二、顶层架构设计

### 2.1 架构总览

```
┌──────────────────────────────────────────────────────────────────────┐
│                          用户 / 客户端                                │
└────────────────────────────┬─────────────────────────────────────────┘
                             │ POST /api/v1/chat/completions
                             ▼
┌──────────────────────────────────────────────────────────────────────┐
│                   Gateway :8080 (Gin)                                 │
│                                                                      │
│  请求链路：                                                           │
│  ① JWT Middleware → ② Metrics → ③ Router.Pick(model)                │
│  → ④ 令牌桶限流(Redis Lua) → ⑤ Prompt MD5 缓存                      │
│  → ⑥ 熔断器检查(含 fallback 自动切换) → ⑦ 指数退避重试               │
│  → ⑧ Provider.Chat / ChatStream                                      │
│                                                                      │
│  异步入口：POST /chat/completions/async                              │
│  → LPUSH Redis List → 立即返回 request_id（~3ms，不阻塞）           │
└──────────────┬───────────────────────────────┬───────────────────────┘
               │                               │
               │ BRPOP（阻塞等待）              │ 限流/缓存/结果
               ▼                               ▼
┌──────────────────────────┐      ┌──────────────────────────────────┐
│  Dispatcher :8081         │      │          Redis :6379             │
│  Worker Pool = 8          │      │                                  │
│  + 熔断 + 重试 + trace_id │      │  ① Lua 令牌桶限流                │
│                           │      │  ② Prompt MD5 缓存               │
│  BRPOP → 反序列化        │      │  ③ 异步结果 async_result:{id}    │
│  → 熔断检查 → 重试       │      │  ④ 主队列 smartproxy:queue:async  │
│  → Provider.Chat()       │      │  ⑤ 死信队列 smartproxy:queue:dlq  │
│  → SET async_result      │      │                                  │
└─────────────┬────────────┘      └──────────────────────────────────┘
              │
              ▼
   ┌─────────────────────┐
   │  LLM Provider       │
   │  DeepSeek (真实)    │
   └─────────────────────┘
```

### 2.2 为什么双进程？

| 维度 | 单体 | 双进程（Gateway + Dispatcher） |
|------|------|------------------------------|
| 扩缩容 | 只能整体扩 | Gateway 接流量、Dispatcher 消费，独立扩 |
| 可观测 | 指标混在一起 | Gateway/Dispatcher 各一份 metrics，Prometheus 按 job 区分 |
| 故障隔离 | 一处挂全挂 | Dispatcher 消费异常不影响 Gateway 接请求 |
| 职责 | 混在一起 | Gateway 扛 HTTP，Dispatcher 扛异步消费 |

### 2.3 为什么 Redis 一肩三挑？

| 职责 | 数据结构 | 为什么用 Redis |
|------|---------|---------------|
| 限流 | Hash (tokens + last) | Lua 原子脚本，一次 RTT 完成加令牌+扣令牌 |
| 缓存 | String (JSON) | SET/GET 毫秒级，TTL 自动过期 |
| 队列 | List (LPUSH/BRPOP) | 零额外组件，Sidekiq/Bull/Celery 同款方案 |

> 已依赖 Redis 做限流和缓存，再加 RabbitMQ 运维成本翻倍。`mq.New(url)` 自动判断协议（`amqp://` 走 RabbitMQ），生产要换 MQ 一行配置。

---

## 三、核心技术栈

| 层 | 组件 | 版本 | 选型理由 |
|----|------|------|----------|
| 语言 | **Go** | 1.21 | goroutine 原生高并发、单二进制静态链接、后端岗加分 |
| Web 框架 | **Gin** | v1.10 | 轻量、性能强、中间件生态成熟 |
| 缓存/存储 | **Redis** | 7.x | 限流计数 / prompt 去重 / 异步队列，三职责复用 |
| 队列 | **Redis List** | — | LPUSH/BRPOP + 死信 List，零额外组件 |
| 鉴权 | **JWT** | golang-jwt v5 | 无状态、适合分布式 Gateway 横向扩展 |
| 限流 | **Redis Lua 令牌桶** | — | 原子性脚本，分布式安全 |
| 熔断 | **自研 resilience** | — | 三态状态机 + 按 provider 隔离 + fallback 自动切换 |
| 重试 | **自研 resilience** | — | 指数退避（100ms 倍增，cap 2s）+ context 可取消 |
| 可观测 | **Prometheus** | client_golang | 14 个指标含 Histogram，Gateway + Dispatcher 双端口 |
| 容器 | **Docker Compose** | — | 一键拉起 Redis + Gateway + Dispatcher |
| CI | **GitHub Actions** | — | push 自动 go build + vet + test |
| 构建 | `-trimpath -ldflags="-s -w"` | — | 去路径去符号表，体积 -30%，减少杀毒误报 |

---

## 四、项目结构

```
smartproxy/
├── run-all.bat                  # 一键 start|stop|restart|status|logs（本地开发）
├── stop.bat                     # 强制杀进程 + 释放端口（--redis 全杀）
├── docker-up.bat / docker-down.bat  # Docker 一键部署
├── Dockerfile                   # 多阶段构建
├── docker-compose.yml           # Redis + Gateway + Dispatcher
├── .github/workflows/ci.yml     # CI: build + vet + test
├── cmd/
│   ├── gateway/main.go          # :8080 Gin HTTP，全链路中间件
│   └── dispatcher/main.go       # :8081 Worker Pool=8，BRPOP 消费
├── internal/
│   ├── config/config.go         # 环境变量加载
│   ├── auth/jwt.go              # JWT 签发/解析
│   ├── cache/cache.go           # Redis MD5 缓存 + 内存 fallback + SingleFlight
│   ├── ratelimiter/bucket.go    # 令牌桶（Redis Lua 原子 / 本地 fallback）
│   ├── mq/                      # 消息队列抽象
│   │   ├── mq.go                # Client + backend interface + Envelope
│   │   ├── redis.go             # Redis List LPUSH/BRPOP（默认）
│   │   └── rabbitmq.go          # RabbitMQ（可选）
│   ├── stats/recorder.go        # Pipeline 异步统计
│   ├── utils/id.go              # request_id / trace_id 生成
│   ├── resilience/
│   │   ├── circuit.go           # 三态熔断器（Closed→Open→HalfOpen，按 provider 隔离）
│   │   ├── retry.go             # 指数退避重试（3 次，100ms 倍增，cap 2s）
│   │   ├── circuit_test.go      # 3/3 PASS
│   │   └── retry_test.go        # 4/4 PASS
│   ├── ratelimiter/bucket_test.go  # 3/3 PASS
│   ├── metrics/metrics.go       # 14 个 Prometheus 指标
│   └── llm/
│       ├── types.go             # LLMRequest / LLMResponse / Provider 接口
│       ├── openai_provider.go   # OpenAI 兼容实现（已接 DeepSeek）
│       └── router.go            # model→provider 路由 + fallback 声明
├── scripts/
│   ├── build.bat / build.sh     # 构建脚本
│   ├── mock_llm.py              # 本地 mock LLM
│   └── test_full.py             # E2E 测试
├── .run/                        # 运行日志（gitignored）
├── dist/                        # 构建产物（gitignored）
├── .env.example                 # 环境变量模板
├── INTERVIEW.md                 # 面试弹药库
├── INTERVIEW_PREP.md            # 简历逐条深挖 + 追问题链
└── README.md
```

---

## 五、核心链路详解

### 5.1 同步 chat/completions

```
POST /api/v1/chat/completions
│
├─ ① JWT Middleware (auth/jwt.go)
│     Bearer Token → claims.user_id → 生成 trace_id → 注入 context
│
├─ ② Metrics Middleware
│     http_requests_total++ + http_request_duration_ms observe
│
├─ ③ Router.Pick(model) → Provider
│     model 字符串查 modelIndex → 拿 Provider 实例
│
├─ ④ 令牌桶限流 (ratelimiter/bucket.go Lua 脚本)
│     key = "ratelimit:" + user_id + ":" + model   ← 按用户+模型独立桶
│     1 次 Redis EVAL → 返回 0/1 → 无令牌 429
│
├─ ⑤ Prompt 去重缓存 (cache/cache.go)
│     key = "llmcache:" + model + ":" + md5(json.Marshal(LLMRequest))
│     Redis GET → 命中？
│       YES → cache_hits_total++ → 直接返回  ← 加速 1400x
│       NO  → cache_misses_total++ → 继续
│
├─ ⑥ 熔断器检查 (resilience/circuit.go)
│     cb = cbGroup.Get(providerName)
│     Closed → 放行
│     Open → 尝试 fallback provider（router.Fallbacks）→ 全熔断才 503
│     HalfOpen → 放一个试探
│
├─ ⑦ SingleFlight 防缓存击穿 (cache/cache.go)
│     同 key 并发请求只执行一次 LLM 调用，其余等结果
│
├─ ⑧ 指数退避重试 (resilience/retry.go)
│     delay = 100ms * 2^attempt，cap at 2s
│     attempt=0→100ms, attempt=1→200ms, attempt=2→400ms
│
├─ ⑨ Provider.Chat() (llm/openai_provider.go)
│     http.Post → DeepSeek API → 返回 JSON
│     成功 → RecordSuccess() → 异步写缓存 + 统计
│     失败 → RecordFailure() → 重试 or 返回
│
└─ ⑩ 返回响应 + provider_calls_total++
```

### 5.2 异步队列 BRPOP 时序

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
  │                           │   BRPOP 原子 pop           │
  │                           │                          │
  │                           │                          │── json.Unmarshal
  │                           │                          │── trace_id 贯穿日志
  │                           │                          │── cbGroup.Get().Allow()
  │                           │                          │── DoWithRetry → Provider.Chat()
  │                           │                          │
  │                           │◄── SET async_result:id ─│
  │                           │                          │
  │   ┌─ 客户端轮询 ─┐        │                          │
  │   │ GET /result  │──────────► Redis GET ◄────────────│
  │   │ :id          │◄────────── 返回完整结果            │
  │   └──────────────┘        │                          │
```

**消息可靠性**：消息体带 `retry_count`，业务失败 +1，< 3 次重新入队，≥ 3 次进死信队列并写错误结果。熔断时不算业务失败，`retry_count` 不 +1，等熔断恢复。

---

## 六、技术难点与解决方案

### 6.1 分布式限流：Redis Lua 原子令牌桶

**难点**：加令牌 + 扣令牌必须原子性，否则高并发下超发。

**方案**：`ratelimiter/bucket.go:82-113` Lua 脚本一次 RTT 完成：

```lua
local bucket = redis.call('HMGET', key, 'tokens', 'last')
-- 计算补充的令牌
tokens = math.min(cap, tokens + elapsed * qps)
-- 扣令牌
if tokens >= requested then
    tokens = tokens - requested
    redis.call('HMSET', key, 'tokens', tokens, 'last', now)
    return 1  -- 放行
else
    return 0  -- 拒绝
end
```

**为什么不用 WATCH/MULTI**：乐观锁在高并发下产生大量事务冲突重试，性能差。Lua 脚本在 Redis 单线程内原子执行，无竞争。

**限流维度**：`ratelimit:user:{userId}:model:{model}` —— 每个用户每个模型独立令牌桶，避免一个模型用光额度影响其他模型。

### 6.2 缓存击穿防护：SingleFlight

**难点**：热点 key 过期瞬间，100 个并发请求同时 miss → 100 个请求同时调 LLM → 浪费 token + 压垮后端。

**方案**：自研 SingleFlight（`cache/cache.go:189-236`，无额外依赖）：

```go
func (sf *SingleFlight) Do(key string, fn func() (interface{}, error)) (interface{}, error) {
    sf.mu.Lock()
    if c, ok := sf.m[key]; ok {
        // 已有同 key 调用在进行，等待结果
        sf.mu.Unlock()
        c.wg.Wait()
        return c.val, c.err
    }
    // 第一个请求，创建 call 并执行 fn
    c := &sfCall{}
    c.wg.Add(1)
    sf.m[key] = c
    sf.mu.Unlock()
    c.val, c.err = fn()
    c.wg.Done()
    // 执行完移除 key
    sf.mu.Lock()
    delete(sf.m, key)
    sf.mu.Unlock()
    return c.val, c.err
}
```

同 prompt 的并发请求只有第一个调 LLM，其余阻塞等待第一个完成后拿到同样结果。

### 6.3 三态熔断 + 按 provider 隔离 + fallback

**难点**：全局熔断会导致一家 LLM 挂了全都不能用；熔断后直接 503 用户体验差。

**方案**：`resilience/circuit.go` + `llm/router.go`

```
Closed ──(连续失败 ≥ 5)──► Open
  ▲                           │
  │  RecordSuccess()           │ 30s 后
  │                           ▼
  │                      HalfOpen
  │                           │
  │    试探成功 ───────────────┤  试探失败
  │                           ▼
  └─────────────────────► Open
```

- **隔离**：`resilience.NewGroup(5, 30*time.Second)` → `group.Get("deepseek")` / `group.Get("qwen")` 各拿各的熔断器
- **fallback**：`router.RegisterFallback("deepseek", "qwen-plus", "doubao-pro")` 声明备用，主 provider 熔断时自动遍历 fallback 找未熔断的，全熔断才返回 503

### 6.4 异步队列：消息重试 + 死信

**难点**：消费失败不能无限重试，也不能直接丢。

**方案**：消息体带 `retry_count` 字段：

```
消费失败 → retry_count++
  ├─ < 3 → NackRequeue（带新 retry_count 重新入队）
  └─ ≥ 3 → NackDead（进死信队列 + 写错误结果到 async_result）
```

死信队列 `smartproxy:queue:dlq` 可人工扫描重新入队或报警。

### 6.5 链路追踪：trace_id

**难点**：Gateway 入队和 Dispatcher 消费是两个进程，日志无法关联。

**方案**：Gateway auth middleware 生成 `trace_id`，写进消息体，Dispatcher 所有日志打印 `trace=xxx`：

```
Gateway:  [gateway] trace=abc123 async request_id=xyz...
Dispatcher: [worker 3] trace=abc123 processing request_id=xyz...
Dispatcher: [worker 3] trace=abc123 done latency_ms=1327
```

### 6.6 Prometheus 指标设计

**难点**：指标要覆盖全链路，但 label 基数不能爆。

**方案**：14 个指标，只留 `provider` / `model` 两个 label，绝不放 `user_id`（否则每个用户一个 time series）：

| 分类 | 指标 | 类型 |
|------|------|------|
| HTTP | http_requests_total | Counter |
|      | http_request_errors_total | Counter |
|      | http_request_duration_ms | Histogram (11 bucket: 5ms→10s) |
| Provider | provider_calls_total | Counter |
|          | provider_failures_total | Counter |
|          | provider_call_duration_ms | Histogram |
| 缓存 | cache_hits_total / cache_misses_total | Counter |
| 限流 | ratelimit_rejects_total | Counter |
| 队列 | mq_publish_total / mq_publish_failures_total | Counter |
|      | mq_consume_total / mq_consume_failures_total | Counter |
| 熔断 | circuit_breaker_open_total | Counter |

---

## 七、快速启动

### 方式一：run-all.bat（本地开发，最推荐）

```powershell
copy .env.example .env    # 填 DeepSeek API Key
.\run-all.bat start       # 自动构建 + 启 Redis + Gateway :8080 + Dispatcher :8081
# 端口自动漂移（被占就往后找）
# 日志: .run\gateway.log / dispatcher.log / redis.log

.\stop.bat                # 停 Gateway + Dispatcher，保留 Redis
.\stop.bat --redis        # 全杀
```

### 方式二：Docker 一键化

```powershell
copy .env.example .env
.\docker-up.bat           # build + compose up -d，等 healthy 后打印地址
.\docker-down.bat         # 停容器，保留数据卷
.\docker-down.bat --clean # 停容器 + 清数据卷
```

compose 自动处理：`REDIS_ADDR=redis:6379`（容器内 service DNS）、healthcheck 依赖链（Redis → Gateway → Dispatcher）。

### 方式三：手动分进程

```powershell
# Redis
redis-server.exe --port 6379 --save "" --appendonly no

# Gateway（终端 A）
$env:Path='D:\Go\bin;' + $env:Path; $env:GOROOT='D:\Go'
scripts\build.bat all
dist\smartproxy-gateway.exe

# Dispatcher（终端 B）
dist\smartproxy-dispatcher.exe
```

### 启动后验证

```powershell
curl http://localhost:8080/healthz                          # 健康检查
curl http://localhost:8080/metrics | findstr "^# HELP"       # 14 HELP 指标
curl http://localhost:8081/metrics | findstr "mq_consume"    # Dispatcher 指标
```

---

## 八、API 速查

| 方法 | 路径 | 说明 | 鉴权 |
|------|------|------|------|
| POST | `/api/v1/login` | `{"user_id":"alice"}` → 返回 JWT | ❌ |
| POST | `/api/v1/chat/completions` | OpenAI 兼容同步/流式 | ✅ |
| POST | `/api/v1/chat/completions/async` | 异步入队，返回 request_id | ✅ |
| GET | `/api/v1/chat/completions/result/:id` | 轮询异步结果 | ✅ |
| GET | `/api/v1/stats/daily` | 当日用量统计 | ✅ |
| GET | `/metrics` | Gateway Prometheus 指标 | ❌ |
| GET | `/healthz` | 健康检查 | ❌ |
| GET | `/api/v1/metrics/dispatcher` | Dispatcher 指标（Gateway 代理） | ❌ |
| GET | `/api/v1/metrics/html?src=gateway\|dispatcher` | 前端 iframe 用（白字黑底） | ❌ |

### 调用示例

```powershell
# 1. 拿 Token
$token = (Invoke-RestMethod -Method POST -Uri http://localhost:8080/api/v1/login `
    -ContentType "application/json" -Body '{"user_id":"alice"}').token

# 2. 同步调用
Invoke-RestMethod -Method POST -Uri http://localhost:8080/api/v1/chat/completions `
    -Headers @{Authorization="Bearer $token"; "Content-Type"="application/json"} `
    -Body '{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}'

# 3. 异步调用 + 轮询
$async = Invoke-RestMethod -Method POST -Uri http://localhost:8080/api/v1/chat/completions/async `
    -Headers @{Authorization="Bearer $token"; "Content-Type"="application/json"} `
    -Body '{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}'
Start-Sleep -Seconds 2
Invoke-RestMethod -Uri "http://localhost:8080/api/v1/chat/completions/result/$($async.request_id)" `
    -Headers @{Authorization="Bearer $token"}
```

---

## 九、测试覆盖

```
=== RUN   TestCircuitBreaker_StateTransition   → PASS  三态转换
=== RUN   TestCircuitBreaker_Concurrent        → PASS  并发安全
=== RUN   TestGroup_Isolation                   → PASS  多 provider 隔离
=== RUN   TestRetry_Success                     → PASS  重试后成功
=== RUN   TestRetry_AllFail                     → PASS  全部失败
=== RUN   TestRetry_Backoff                     → PASS  指数退避 + MaxDelay cap
=== RUN   TestRetry_ContextCancel               → PASS  ctx 取消立即停止
=== RUN   TestLocalBucket_Allow                 → PASS  令牌桶放行/拒绝
=== RUN   TestLocalBucket_Refill                → PASS  令牌自动补充
=== RUN   TestLocalBucket_KeyIsolation          → PASS  多 key 隔离

10/10 PASS
```

---

## 十、面试考点速查

| 模块 | 考点 |
|------|------|
| 分布式限流 | Redis Lua 原子性 vs WATCH/MULTI、令牌桶 vs 漏桶、QPS vs TPM |
| 缓存策略 | MD5 去重、穿透/击穿/雪崩、SingleFlight 合并并发 |
| 熔断降级 | 三态状态机、按 provider 隔离、fallback 自动切换 |
| 异步解耦 | Redis List LPUSH/BRPOP、死信队列、retry_count 语义 |
| 高并发 | Worker Pool + prefetch、goroutine 泄漏防护、context 传递 |
| 可观测 | Prometheus Histogram bucket 设计、label 基数控制、trace_id 链路 |
| 工程化 | Docker Compose 编排、优雅关闭、GitHub Actions CI |

---

## 十一、后续扩展方向

- [ ] 接入通义 / 豆包 Provider（OpenAI 兼容，加一个 provider 文件）
- [ ] Redis Sentinel / Cluster 高可用
- [ ] PostgreSQL 持久化用户 / API Key / 用量明细
- [ ] Prometheus + Grafana 监控大盘
- [ ] Kubernetes 部署清单

---

## License

MIT
