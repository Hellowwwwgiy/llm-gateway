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
| 缓存命中 | ~500-800µs | Gin 服务端日志 |
| 加速比 | ~1000x | 754ms / 0.8ms |
| Prometheus 指标 | 14 个 | `metrics.go:226-249`（HELP 行精确计数） |
| 异步入队 | ~1ms | Gin 服务端日志（LPUSH Redis List） |
| Worker Pool | 8 | `dispatcher/main.go:34` |
| 单元测试 | 10/10 PASS | `go test ./internal/...` |

---

## 二、顶层架构设计

### 2.1 架构总览

```
┌──────────────────────────────────────────────────────────────────────┐
│用户 / 客户端                                                         │
└───────────────────────────┬──────────────────────────────────────────┘
                            │
                            ▼
┌──────────────────────────────────────────────────────────────────────┐
│  Gateway :8080 (Gin)                                                 │
│                                                                      │
│  ① JWT → ② Metrics → ③ Router.Pick                                   │
│  → ④ Lua 令牌桶限流 → ⑤ MD5 缓存                                     │
│  → ⑥ 熔断检查(fallback) → ⑦ 指数退避重试                             │
│  → ⑧ Provider.Chat / ChatStream                                      │
│                                                                      │
│  异步入口: POST /chat/completions/async                              │
│  → LPUSH Redis List → 返回 request_id (~1ms)                         │
└──────────────────────────────────────────────────────────────────────┘
└────────────────────┬───────────────────────────────────┬────────────┘
                     │                                   │
                     ▼                                   ▼
      ┌────────────────────────────┐      ┌────────────────────────────┐
      │  Dispatcher :8081          │      │  Redis :6379               │
      │  Worker Pool = 8           │      │                            │
      │  + 熔断 + 重试             │      │  ① Lua 令牌桶限流          │
      │  + trace_id                │      │  ② Prompt MD5 缓存         │
      │                            │      │  ③ 异步结果存储            │
      │  BRPOP → 反序列化          │      │  ④ 主队列 async_chat       │
      │  → 熔断 → 重试             │      │  ⑤ 死信队列 dlq            │
      │  → Provider.Chat()         │      └────────────────────────────┘
      │  → SET async_result        │
      └────────────────────────────┘
                    │
                    ▼
        ┌────────────────────────┐
        │  LLM Provider          │
        │  DeepSeek (真实)       │
        └────────────────────────┘
```

### 2.2 双进程架构

Gateway 负责承接 HTTP 流量，Dispatcher 负责异步消息消费，两个进程独立部署、独立扩缩容、互不影响。这样 Dispatcher 消费积压或异常时不会拖垮 Gateway 的同步接口；Prometheus 也能按 job 区分采集两个进程的指标，故障定位更清晰。

### 2.3 Redis 一肩三挑

项目已经依赖 Redis 做限流和缓存，再引入 RabbitMQ 会显著增加运维成本，因此队列也复用 Redis List（LPUSH/BRPOP）。这套方案在 Sidekiq、Bull、Celery 等成熟项目中被广泛验证。限流用 Hash 存 tokens + last 配合 Lua 原子脚本，缓存用 String 存 JSON 并设 TTL，队列用 List + 死信 List。`mq.New(url)` 会自动判断协议——传 `amqp://` 自动切 RabbitMQ，生产要换 MQ 只需改一行配置。

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
| 可观测 | **Prometheus 格式（自研）** | — | 手写 exposition 格式，14 指标含 Histogram，无 client_golang 依赖，Gateway + Dispatcher 双端口 |
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
├── go.mod / go.sum              # Go 1.21
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
│       YES → cache_hits_total++ → 直接返回  ← 加速 ~1000x
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
Gateway                     Redis List                 Dispatcher
 │                          │                          │
 ├──────────────────────────►  LPUSH(msg)  ~1ms
 │                          │                          │
 ◄──────────────────────────┤  返回 request_id
 │                          │                          │
 │                          │                          │
                            ◄──────────────────────────┤  BRPOP(timeout=0)  阻塞等待
 │                          │                          │
                            ├──────────────────────────►  返回消息（原子 pop）
 │                          │                          │
 │                          │                          │
                                                       │── json.Unmarshal
                                                       │── trace_id 贯穿日志
                                                       │── 熔断检查 → 重试
                                                       │── Provider.Chat()
 │                          │                          │
                            ◄──────────────────────────┤  SET async_result:id
 │                          │                          │
 │                          │                          │
  ┌─ 客户端轮询 ─┐
 ├──────────────────────────►  GET /result/:id
 ◄──────────────────────────┤  返回完整结果
  └──────────────┘
```

**消息可靠性**：消息体带 `retry_count`，业务失败 +1，< 3 次重新入队，≥ 3 次进死信队列并写错误结果。熔断时不算业务失败，`retry_count` 不 +1，等熔断恢复。

---

## 六、技术实现要点

### 6.1 分布式限流：Redis Lua 原子令牌桶

令牌桶的"补充 + 扣减"必须原子执行，否则高并发下会超发。这里用 Redis Lua 脚本一次 RTT 完成全部逻辑，比 WATCH/MULTI 乐观锁更高效——后者在高并发下会产生大量事务冲突重试。脚本逻辑：从 Hash 读 tokens 和 last，按 `elapsed * qps` 补充令牌（cap 到容量），够用就扣减写回并返回 1，否则返回 0。

限流 key 设计为 `ratelimit:user:{userId}:model:{model}`，每个用户对每个模型拥有独立的令牌桶，避免某个模型流量大时把其他模型的额度一起用光。

### 6.2 缓存击穿防护：SingleFlight

热点 key 过期瞬间，大量并发请求同时 miss 会同时打到 LLM 后端，造成 token 浪费和后端压力。这里自研了 SingleFlight 组件（不依赖 golang.org/x/sync），核心是用 `map[string]*call` + `sync.WaitGroup` 记录正在执行的调用：同 key 的后续请求直接等待第一个请求的结果，只有第一个请求真正调用 LLM。调用完成后从 map 删除 key，让下一批请求重新执行。

缓存 key 基于 `model + md5(json.Marshal(LLMRequest))` 生成，保证相同 prompt 命中同一缓存。

### 6.3 三态熔断 + 按 provider 隔离 + fallback

熔断器是经典三态状态机：Closed 放行并记录结果，连续失败 5 次切 Open 直接拒绝，30 秒后进入 HalfOpen 放一个试探请求，成功回 Closed、失败回 Open。

熔断器按 provider 隔离：`resilience.NewGroup(5, 30s)` 维护一个 map，`group.Get("deepseek")` 和 `group.Get("qwen")` 各拿各的实例。这样一家 LLM 挂了不影响其他家。在此基础上，`router.RegisterFallback("deepseek", "qwen-plus", "doubao-pro")` 声明备用 provider，主 provider 熔断时自动遍历 fallback 列表找第一个未熔断的，全部熔断才返回 503。

### 6.4 异步队列：消息重试 + 死信

消息体携带 `retry_count` 字段。消费失败时 retry_count +1：小于 3 次重新入队（带着更新后的 retry_count），达到 3 次进入死信队列 `smartproxy:queue:dlq` 并把错误结果写入 `async_result:{id}`，客户端轮询时拿到明确的失败信息而非永远 pending。熔断场景不算业务失败，retry_count 不递增，等熔断恢复后自然重试。

### 6.5 链路追踪：trace_id

Gateway 和 Dispatcher 是两个独立进程，日志无法直接关联。Gateway 的 auth middleware 为每个请求生成 trace_id 并注入 context，异步入队时写进消息体；Dispatcher 消费时取出 trace_id，在 processing、失败、完成三条关键日志里都带上 `trace=xxx`，实现跨进程的日志串联。

### 6.6 Prometheus 指标设计

全链路共 14 个指标，覆盖 HTTP、Provider、缓存、限流、队列、熔断六类。label 只保留 `provider` 和 `model` 两个维度——绝对不能放 `user_id`，否则每个用户产生独立 time series，基数爆炸。Histogram 用 11 个 bucket（5ms → 10s）覆盖 LLM 调用的延迟范围。Gateway 和 Dispatcher 各自注册独立的 metrics endpoint（8080/8081），Prometheus 按 job 区分。

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

## 十、后续扩展方向

- [ ] 接入通义 / 豆包 Provider（OpenAI 兼容，加一个 provider 文件）
- [ ] Redis Sentinel / Cluster 高可用
- [ ] PostgreSQL 持久化用户 / API Key / 用量明细
- [ ] Prometheus + Grafana 监控大盘
- [ ] Kubernetes 部署清单

---

## License

MIT
