# SmartProxy — 多模型 LLM API 网关

> 统一 LLM API 网关（Go + Gin + Redis），用户调一个接口，后端负责 **JWT 鉴权、Redis Lua 限流、Prompt MD5 缓存、三态熔断、异步 LPUSH/BRPOP 队列、Prometheus 可观测**。当前已接入 DeepSeek，架构支持一行代码扩展通义/豆包。

---

## 🏗️ 架构图

```
                    ┌─────────────────────────────┐
  用户请求 ──────►  │  🚪 Gateway :8080 (Gin)      │
                    │  ┌─────────────────────────┐ │
                    │  │ JWT → Router → 限流     │ │
                    │  │              → 缓存     │ │
                    │  │              → 熔断     │ │
                    │  │              → LLM 调用 │ │
                    │  └────────────┬────────────┘ │
                    │               │              │
                    │  (异步: LPUSH)│              │
                    └──────────────┼──────────────┘
                                   │
                    ┌──────────────┼──────────────┐
                    ▼              ▼              ▼
             ┌──────────┐  ┌──────────┐  ┌──────────┐
             │  Redis   │  │  Redis   │  │  Redis   │
             │ Lua 限流  │  │ MD5 缓存 │  │ List 队列 │
             └──────────┘  └──────────┘  └────┬─────┘
                                               │ BRPOP
                                               ▼
                                        ┌──────────────┐
                                        │ 🔧 Dispatcher │
                                        │  :8081 8worker│
                                        │  +熔断 +重试  │
                                        └──────┬───────┘
                                               │
                                               ▼
                                        ┌──────────────┐
                                        │  LLM Provider │
                                        │  (DeepSeek)   │
                                        └──────────────┘
```

## 🔗 核心链路

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
| 可观测 | **手写 Prometheus 指标** | 14 个指标含 Histogram bucket（Gateway + Dispatcher 双端口） |
| 容器 | **Docker Compose** | 一键拉起全链路 |
| 构建 | **-trimpath -ldflags="-s -w"** | 防路径泄露、体积 -30%、减少杀毒误报 |

## 📦 项目结构

```
smartproxy/
├── run-all.bat                  # Windows 一键 start|stop|restart|status|logs
├── stop.bat                     # 强制杀进程 + 释放端口（支持 --redis）
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
│   ├── metrics/metrics.go       # Prometheus 指标（14 个，Gateway + Dispatcher 各一份）
│   └── llm/
│       ├── types.go             # LLMRequest / LLMResponse / Provider 接口
│       ├── openai_provider.go   # OpenAI 兼容实现
│       └── router.go            # model → provider 路由
├── scripts/
│   ├── build.bat                # Windows 一键构建
│   ├── build.sh                 # Linux / macOS 构建
│   ├── run-gw.bat               # Windows 快速启动 gateway
│   ├── mock_llm.py              # 本地 mock LLM（非流式 + SSE）
│   └── test_full.py             # 端到端测试脚本
├── .run/                        # 运行时日志（gitignored，bat 自动创建）
│   ├── gateway.log / gateway-err.log
│   ├── dispatcher.log / dispatcher-err.log
│   └── redis.log / redis-err.log
├── dist/                        # 构建产物（gitignored，bat 自动构建）
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

### 方式二：一键启动（推荐 — Windows）

```powershell
# 1. 准备 .env
copy .env.example .env
# 填你自己的 DeepSeek API Key

# 2. 一键启动（构建 + 启 Redis + Gateway :8080 + Dispatcher :8081）
.\run-all.bat start

#    所有服务 up 后会自动打开前端页面: http://localhost:8080/
#    前端页面里嵌了 Live Metrics —— tab 切换 Gateway/Dispatcher
#    日志目录:   .run\                  （gateway.log / dispatcher.log / redis.log）
#    停止:       .\stop.bat             （杀 Gateway + Dispatcher，保留 Redis）
#    全杀（含 Redis）: .\stop.bat --redis
#    端口自动漂移: 如果 8080/8081 被占，自动往后找
```

### 方式三：手动分进程

```powershell
# 1. 起 Redis（Windows 免安装版）
redis-server.exe --port 6379 --save "" --appendonly no

# 2. 构建
scripts\build.bat all

# 3. Gateway（终端 A）
copy .env.example .env       # 填 Key 后
$env:Path='D:\Go\bin;' + $env:Path
$env:GOROOT='D:\Go'
dist\smartproxy-gateway.exe

# 4. Dispatcher（终端 B）
dist\smartproxy-dispatcher.exe
```

## 📡 API 速查

| 方法 | 路径 | 说明 | 鉴权 |
|------|------|------|------|
| POST | `/api/v1/login` | `{"user_id":"alice"}` → 返回 JWT | ❌ |
| POST | `/api/v1/chat/completions` | OpenAI 兼容接口 | ✅ Bearer |
| POST | `/api/v1/chat/completions/async` | 异步入队，返回 request_id | ✅ Bearer |
| GET  | `/api/v1/chat/completions/result/:id` | 轮询异步结果 | ✅ Bearer |
| GET  | `/api/v1/stats/daily` | 当日用量统计 | ✅ Bearer |
| GET  | `/metrics` | Gateway Prometheus 指标（text/plain，14 HELP） | ❌ |
| GET  | `/healthz` | 健康检查 | ❌ |
| GET  | `/api/v1/metrics/dispatcher` | Dispatcher Prometheus 指标（Gateway 反向代理，14 HELP） | ❌ |
| GET  | `/api/v1/metrics/html?src=gateway\|dispatcher` | 前端 iframe 用（text/html，白字黑底） | ❌ |

### curl 示例

```powershell
# 1. 拿 Token
$resp = Invoke-RestMethod -Method POST -Uri http://localhost:8080/api/v1/login `
    -ContentType "application/json" -Body '{"user_id":"alice"}'
$token = $resp.token

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

## 📊 Prometheus 指标（14 个）

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
