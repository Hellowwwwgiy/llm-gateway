# SmartProxy — 多模型 LLM API 网关

> 一个统一接入多家大模型（OpenAI / 通义 / 豆包）的 API 网关，用户调你一个接口，你负责 **路由、限流、缓存、熔断、异步批处理、用量统计**。

---

## 🏗️ 架构图

```
                    ┌─────────────────────┐
  用户请求 ──────►  │    🚪 API 网关服务    │  ← 统一入口 / 鉴权 / 路由 / SSE
                    └─────────┬───────────┘
                              │
                 ┌────────────┼────────────┐
                 ▼            ▼            ▼
          ┌──────────┐  ┌──────────┐  ┌──────────┐
          │ ⚡ 限流服务 │  │ 💾 缓存服务 │  │ 📊 统计服务 │
          │(Redis Lua)│  │(Prompt去重)│  │(Pipeline) │
          └──────────┘  └──────────┘  └──────────┘
                 │            │            │
                 └────────────┼────────────┘
                              ▼
                    ┌─────────────────────┐
                    │  📨 Redis 消息队列     │  ← LPUSH/BRPOP  + 死信 List
                    └─────────┬───────────┘
                              ▼
                    ┌─────────────────────┐
                    │  🔧 异步调度消费者     │  ← Worker Pool + 熔断 + 重试
                    └─────────┬───────────┘
                              ▼
                    ┌─────────────────────┐
                    │  🌐 LLM Provider x N  │  ← OpenAI / 通义 / 豆包 ...
                    └─────────────────────┘
```

## 🔗 核心链路（面试必考）

```
POST /api/v1/chat/completions
  │
  ├─ ① JWT Auth Middleware → 校验 Bearer Token
  ├─ ② Metrics Middleware  → http_requests_total++ / latency_histogram
  ├─ ③ Router.Pick(model)  → 按 model 字符串选对应 Provider
  ├─ ④ 令牌桶限流 (Redis Lua 原子) → 无令牌 → 429 + ratelimit_rejects_total++
  ├─ ⑤ Prompt 去重缓存 (Redis MD5) → 命中 → cache_hits_total++ → 直接返回
  ├─ ⑥ Provider 熔断器检查 → Open → 503 + circuit_breaker_open_total++
  ├─ ⑦ 指数退避重试 (最多 3 次)
  ├─ ⑧ Provider.Chat / ChatStream
  │     ├─ 非流式 → JSON 响应 → 异步写缓存 + 异步统计
  │     └─ 流式   → SSE 透传 → 逐 chunk 输出
  └─ ⑨ 最终 Metrics: provider_calls++ / provider_latency 记 bucket
```

## 🛠 技术选型

| 层 | 组件 | 选择理由 |
|----|------|----------|
| 语言 | **Go 1.21+** | 高并发原生支持、单二进制、后端岗加分 |
| 框架 | **Gin** | 轻量、性能强、生态成熟 |
| 缓存 | **Redis + go-redis** | 限流计数 / prompt 去重 / 异步结果 |
| 队列 | **Redis List (LPUSH/BRPOP)** | 零额外组件、死信 List、阻塞消费、Sidekiq/Bull/Celery 同款方案 |
| 鉴权 | **JWT (golang-jwt/jwt/v5)** | 无状态、适合分布式 |
| 限流 | **Redis Lua 令牌桶** | 原子性脚本、分布式安全 |
| 熔断重试 | **自研 resilience 包** | 三态状态机 + 指数退避 |
| 可观测 | **手写 Prometheus 指标** | 16 个指标含 Histogram bucket |
| 容器 | **Docker Compose** | 一键拉起全链路 |
| 构建 | **-trimpath -ldflags="-s -w"** | 防路径泄露、体积 -30%、减少杀毒误报 |

## 📦 项目结构

```
smartproxy/
├── cmd/
│   ├── gateway/main.go          # API 网关（主服务）
│   └── dispatcher/main.go       # 异步调度消费者
├── internal/
│   ├── config/config.go         # 环境变量配置加载
│   ├── auth/jwt.go              # JWT 签发 / 解析
│   ├── cache/cache.go           # Redis + 内存 fallback
│   ├── ratelimiter/bucket.go    # 令牌桶（Redis Lua / 本地）
│   ├── mq/                     # 消息队列（抽象 backend）
│   │   ├── mq.go               # Client + backend interface
│   │   ├── rabbitmq.go         # RabbitMQ backend (可选)
│   │   └── redis.go            # Redis List backend (默认)
│   ├── stats/recorder.go       # Pipeline 异步统计
│   ├── utils/id.go              # request_id
│   ├── resilience/
│   │   ├── circuit.go           # 三态熔断器（Closed→Open→HalfOpen）
│   │   └── retry.go             # 指数退避重试
│   ├── metrics/metrics.go       # Prometheus 指标（16 个）
│   └── llm/
│       ├── types.go             # LLMRequest / LLMResponse / Provider 接口
│       ├── openai_provider.go   # OpenAI 兼容实现
│       └── router.go            # model → provider 路由
├── scripts/
│   ├── build.bat                # Windows 一键构建
│   ├── build.sh                 # Linux / macOS 构建
│   ├── run-gw.bat               # Windows 快速启动 gateway
│   ├── mock_llm.py              # 本地 mock LLM（非流式 + SSE）
│   └── test_sse.py              # SSE 端到端测试
├── go.mod / go.sum
├── docker-compose.yml           # Redis + Gateway + Dispatcher (RabbitMQ 可选)
├── Dockerfile                   # 多阶段构建
├── Makefile
└── .env.example
```

## 🚀 快速启动

### 方式一：内存 fallback（零依赖，最快）

```powershell
# 1. 构建
scripts\build.bat all

# 2. 启动 mock LLM（另一个终端）
python scripts\mock_llm.py

# 3. 启动 gateway（指向 mock）
$env:OPENAI_API_KEY="sk-mock"
$env:OPENAI_BASE_URL="http://localhost:9999"
$env:OPENAI_MODEL="mock-gpt"
scripts\run-gw.bat
```

Gateway 启动后提示：
```
[gateway] redis NOT reachable — using in-memory fallback
[gateway] mq connect failed (async mode disabled)
[gateway] listening on :8080
```

### 方式二：真实 Redis + DeepSeek（推荐）

```powershell
# 1. 起 Redis（Windows 免安装版，已在项目 docs 里说明）
#    或 Docker Desktop: docker compose up -d redis

# 2. 配置真实 DeepSeek
copy .env.example .env
# .env 里已经配好 DeepSeek Key，gateway 自动读

# 3. 启动 gateway + dispatcher
scripts\build.bat all
dist\smartproxy-gateway.exe      # 另一个终端
dist\smartproxy-dispatcher.exe   # Redis BRPOP 消费
```

Gateway 启动日志：
```
[config] loaded .env
[gateway] redis connected (127.0.0.1:6379)
[MQ] redis queue connected on 127.0.0.1:6379 (lpush/brpop, key=smartproxy:queue:async_chat)
[gateway] mq connected: redis-queue (async ready)
[gateway] listening on :8080
```

## 📡 API 速查

| 方法 | 路径 | 说明 | 鉴权 |
|------|------|------|------|
| POST | `/api/v1/login` | `{"user_id":"alice"}` → 返回 JWT | ❌ |
| POST | `/api/v1/chat/completions` | OpenAI 兼容接口 | ✅ Bearer |
| POST | `/api/v1/chat/completions/async` | 异步入队，返回 request_id | ✅ Bearer |
| GET  | `/api/v1/chat/completions/result/:id` | 轮询异步结果 | ✅ Bearer |
| GET  | `/api/v1/stats/daily` | 当日用量统计 | ✅ Bearer |
| GET  | `/metrics` | Prometheus 指标 | ❌ |
| GET  | `/healthz` | 健康检查 | ❌ |

### curl 示例

```powershell
# 1. 拿 Token
$resp = Invoke-RestMethod -Method POST -Uri http://localhost:8080/api/v1/login `
    -ContentType "application/json" -Body '{"user_id":"alice"}'
$token = $resp.token

# 2. 同步调用
Invoke-RestMethod -Method POST -Uri http://localhost:8080/api/v1/chat/completions `
    -Headers @{Authorization="Bearer $token"; "Content-Type"="application/json"} `
    -Body '{"model":"mock-gpt","messages":[{"role":"user","content":"hi"}]}'

# 3. 异步调用 + 轮询
$async = Invoke-RestMethod -Method POST -Uri http://localhost:8080/api/v1/chat/completions/async `
    -Headers @{Authorization="Bearer $token"; "Content-Type"="application/json"} `
    -Body '{"model":"mock-gpt","messages":[{"role":"user","content":"hi"}]}'

Start-Sleep -Seconds 2
Invoke-RestMethod -Uri "http://localhost:8080/api/v1/chat/completions/result/$($async.request_id)" `
    -Headers @{Authorization="Bearer $token"}
```

## 🧪 测试覆盖

```
=== RUN   TestCircuitBreaker_StateTransition   → PASS  (三态转换)
=== RUN   TestCircuitBreaker_Concurrent        → PASS  (并发安全)
=== RUN   TestGroup_Isolation                   → PASS  (多 provider 隔离)
=== RUN   TestRetry_Success                     → PASS  (重试后成功)
=== RUN   TestRetry_AllFail                     → PASS  (全部失败)
=== RUN   TestRetry_Backoff                     → PASS  (指数退避 + MaxDelay cap)
=== RUN   TestRetry_ContextCancel               → PASS  (ctx 取消立即停止)
=== RUN   TestLocalBucket_Allow                 → PASS  (令牌桶放行/拒绝)
=== RUN   TestLocalBucket_Refill                → PASS  (令牌自动补充)
=== RUN   TestLocalBucket_KeyIsolation          → PASS  (多 key 隔离)

10/10 PASS
```

## 📊 Prometheus 指标（16 个）

| 分类 | 指标 | 类型 |
|------|------|------|
| HTTP | `http_requests_total` | Counter |
|      | `http_request_errors_total` | Counter |
|      | `http_request_duration_ms` | Histogram (12 bucket: 5ms → 10s) |
| Provider | `provider_calls_total` | Counter |
|          | `provider_failures_total` | Counter |
|          | `provider_call_duration_ms` | Histogram |
| 缓存 | `cache_hits_total` / `cache_misses_total` | Counter |
| 限流 | `ratelimit_rejects_total` | Counter |
| 队列 | `mq_publish_total` / `mq_publish_failures_total` | Counter |
|      | `mq_consume_total` / `mq_consume_failures_total` | Counter |
| 熔断 | `circuit_breaker_open_total` | Counter |

## 🧠 面试能聊的点

| 模块 | 面试考点 |
|------|----------|
| 网关模式 | 统一入口、按能力路由（长文本→便宜模型、复杂推理→GPT-4）|
| 分布式限流 | Redis Lua 令牌桶原子性 vs 漏桶、QPS vs RPM vs TPM |
| 缓存策略 | Prompt 哈希去重、缓存穿透/击穿/雪崩、Redis Pipeline |
| 异步解耦 | Redis List LPUSH/BRPOP + 死信 List + 手动 ack、为什么不同步统计、为什么 Sidekiq/Bull/Celery 都用 Redis |
| 熔断降级 | 三态状态机 (Closed→Open→HalfOpen)、按 provider 隔离、fallback 策略 |
| 高并发 | Worker Pool + prefetch、goroutine 泄漏防护、context 传递 |
| 可观测 | Prometheus Histogram bucket 设计、label 基数控制 |
| 微服务 | Docker Compose 一键编排、优雅关闭（signal.Notify + timeout）|

## 📝 后续扩展方向

- [ ] 接入真实通义 / 豆包 Provider（都兼容 OpenAI 格式，加一个 provider 文件即可）
- [ ] PostgreSQL 持久化用户 / API Key / 用量明细
- [ ] ClickHouse 存储时序统计数据
- [ ] Prometheus + Grafana 监控大盘
- [ ] Kubernetes 部署清单

## 📄 License

MIT
