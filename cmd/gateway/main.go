package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"smartproxy/internal/auth"
	"smartproxy/internal/cache"
	"smartproxy/internal/config"
	"smartproxy/internal/llm"
	"smartproxy/internal/metrics"
	"smartproxy/internal/mq"
	"smartproxy/internal/ratelimiter"
	"smartproxy/internal/resilience"
	"smartproxy/internal/stats"
	"smartproxy/internal/utils"
)

// App 依赖聚合
type App struct {
	cfg     *config.Config
	cache   *cache.Cache
	limiter *ratelimiter.TokenBucket
	router  *llm.Router
	stats   *stats.Recorder
	mq      *mq.Client
	cbGroup *resilience.Group // 按 provider 隔离的熔断器
}

func main() {
	cfg := config.Load()

	// === 初始化依赖 ===
	redisCache := cache.New(cfg)
	redisOK := redisCache.Ping(context.Background()) == nil
	if redisOK {
		log.Println("[gateway] redis connected")
	} else {
		log.Println("[gateway] redis NOT reachable — using in-memory fallback (cache/ratelimit/stats)")
		redisCache.UseMemory()
	}

	var limiter *ratelimiter.TokenBucket
	var recorder *stats.Recorder
	if redisOK {
		limiter = ratelimiter.New(cfg, redisCache.RDB())
		recorder = stats.New(redisCache.RDB())
	} else {
		limiter = ratelimiter.New(cfg, nil)
		recorder = stats.New(nil)
	}

	// Provider Router + 熔断器组（每个 provider 独立熔断，5 次失败开闸 30s 恢复）
	router := llm.NewRouter()
	cbGroup := resilience.NewGroup(5, 30*time.Second)

	if cfg.OpenAIAPIKey != "" {
		router.Register(
			llm.NewOpenAIProvider("openai", cfg.OpenAIAPIKey, cfg.OpenAIBaseURL, cfg.OpenAIModel),
			cfg.OpenAIModel,
		)
	}

	// MQ 连接策略：RabbitMQ > Redis 队列 > 跳过
	var mqClient *mq.Client
	if cfg.RabbitMQURL != "" && len(cfg.RabbitMQURL) >= 5 && cfg.RabbitMQURL[:5] == "amqp:" {
		if m, err := mq.New(cfg.RabbitMQURL); err != nil {
			log.Printf("[gateway] rabbitmq connect failed: %v (fallback to redis queue)", err)
		} else {
			mqClient = m
			log.Printf("[gateway] mq connected: %s", m.Name())
		}
	}

	// 没 RabbitMQ 但有 Redis → 用 Redis 当 MQ
	if mqClient == nil && !redisCache.IsMemory() {
		if m, err := mq.New(cfg.RedisAddr); err != nil {
			log.Printf("[gateway] redis queue connect failed: %v", err)
		} else {
			mqClient = m
			log.Printf("[gateway] mq connected: %s (async ready)", m.Name())
		}
	}

	if mqClient == nil {
		log.Println("[gateway] mq not connected (async mode disabled)")
	}

	app := &App{
		cfg:     cfg,
		cache:   redisCache,
		limiter: limiter,
		router:  router,
		stats:   recorder,
		mq:      mqClient,
		cbGroup: cbGroup,
	}

	// === Gin 路由 ===
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery(), gin.Logger())
	r.Use(app.metricsMiddleware) // 全局请求埋点

	r.GET("/", app.index)
	r.GET("/healthz", app.healthz)
	r.GET("/metrics", wrapHandler(metrics.Handler()))

	r.POST("/api/v1/login", app.login)

	api := r.Group("/api/v1", app.authMiddleware)
	{
		api.POST("/chat/completions", app.chatCompletions)
		api.POST("/chat/completions/async", app.chatCompletionsAsync)
		api.GET("/chat/completions/result/:request_id", app.getAsyncResult)
		api.GET("/stats/daily", app.dailyStats)
	}

	srv := &http.Server{
		Addr:         ":" + portStr(cfg.GatewayPort),
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 5 * time.Minute,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[gateway] server error: %v", err)
		}
	}()
	log.Printf("[gateway] listening on :%d | /metrics ready", cfg.GatewayPort)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("[gateway] shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("[gateway] forced shutdown: %v", err)
	}
	_ = redisCache.Close()
	if mqClient != nil {
		_ = mqClient.Close()
	}
	log.Println("[gateway] done")
}

func portStr(p int) string {
	if p == 0 {
		return "8080"
	}
	return strconv.Itoa(p)
}

// ====== 通用 helper ======

func wrapHandler(h http.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		h.ServeHTTP(c.Writer, c.Request)
	}
}

// ====== 中间件 ======

func (a *App) metricsMiddleware(c *gin.Context) {
	start := time.Now()
	c.Next()
	lat := metrics.ObserveLatency(start)
	metrics.ReqTotal.Inc()
	metrics.ReqLatency.Observe(lat)
	if c.Writer.Status() >= 500 {
		metrics.ReqErrors.Inc()
	}
}

func (a *App) authMiddleware(c *gin.Context) {
	raw := c.GetHeader("Authorization")
	if len(raw) < 8 || raw[:7] != "Bearer " {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	claims, err := auth.Parse(a.cfg.JWTSecret, raw[7:])
	if err != nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token: " + err.Error()})
		return
	}
	c.Set("user_id", claims.UserID)
	c.Set("role", claims.Role)
	c.Next()
}

// ====== Handler ======

func (a *App) healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (a *App) index(c *gin.Context) {
	var redisStatus string
	if a.cache.IsMemory() {
		redisStatus = "in-memory fallback"
	} else {
		redisStatus = "connected"
	}
	mqStatus := "not connected (async disabled)"
	if a.mq != nil {
		mqStatus = "connected (" + a.mq.Name() + ")"
	}
	providers := a.router.RegisteredProviders()
	models := a.router.RegisteredModels()

	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(indexHTML(redisStatus, mqStatus, providers, models)))
}

func (a *App) login(c *gin.Context) {
	var req struct {
		UserID string `json:"user_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	token, err := auth.Generate(a.cfg.JWTSecret, req.UserID, "user", a.cfg.JWTExpiry)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"token": token})
}

// ====== chat completions 核心链路 ======

type chatRequest struct {
	Model       string            `json:"model"`
	Messages    []llm.ChatMessage `json:"messages" binding:"required"`
	Temperature float64           `json:"temperature"`
	MaxTokens   int               `json:"max_tokens"`
	Stream      bool              `json:"stream"`
}

func (a *App) chatCompletions(c *gin.Context) {
	userID, _ := c.Get("user_id")
	ctx := c.Request.Context()

	var raw chatRequest
	if err := c.ShouldBindJSON(&raw); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 1. 选 provider
	provider, err := a.router.Pick(raw.Model)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no provider for model: " + raw.Model})
		return
	}
	providerName := provider.Name()
	cb := a.cbGroup.Get(providerName)

	// 2. 限流
	allowed, _ := a.limiter.Allow(ctx, userID.(string))
	if !allowed {
		metrics.RateLimitRejects.Inc()
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "rate limit exceeded"})
		return
	}

	llmReq := &llm.LLMRequest{
		Model:       raw.Model,
		Messages:    raw.Messages,
		Temperature: raw.Temperature,
		MaxTokens:   raw.MaxTokens,
		Stream:      raw.Stream,
	}

	// 3. 缓存（非流式，用同一个 llmReq 做 key 保证命中）
	if !raw.Stream {
		var cached llm.LLMResponse
		if a.cache.GetLLM(ctx, raw.Model, llmReq, &cached) {
			metrics.CacheHits.Inc()
			go a.stats.RecordCall(context.Background(), userID.(string), raw.Model, &cached)
			c.JSON(http.StatusOK, cached)
			return
		}
		metrics.CacheMisses.Inc()
	}

	// 4. 熔断器检查
	if !cb.Allow() {
		metrics.CircuitOpen.Inc()
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":         "provider circuit open, try later",
			"provider":      providerName,
			"circuit_state": string(cb.State()),
		})
		return
	}

	if raw.Stream {
		a.handleStream(c, provider, llmReq, userID.(string), cb)
	} else {
		a.handleSync(c, provider, llmReq, userID.(string), cb)
	}
}

func (a *App) handleSync(c *gin.Context, p llm.Provider, req *llm.LLMRequest, userID string, cb *resilience.CircuitBreaker) {
	start := time.Now()
	metrics.ProviderCalls.Inc()

	// 带重试的 provider 调用（非流式）
	opt := resilience.DefaultRetryOption()
	opt.MaxAttempts = 3
	opt.Retryable = func(err error) bool {
		// 网络错误/超时/5xx 才重试；4xx 业务错误不重试
		return err != nil
	}

	var resp *llm.LLMResponse
	err := resilience.DoWithRetry(c.Request.Context(), func() error {
		var e error
		resp, e = p.Chat(req)
		return e
	}, opt)

	lat := metrics.ObserveLatency(start)
	metrics.ProviderLatency.Observe(lat)

	if err != nil {
		metrics.ProviderFailures.Inc()
		cb.RecordFailure()
		c.JSON(http.StatusBadGateway, gin.H{"error": "provider failed after retries: " + err.Error()})
		return
	}
	cb.RecordSuccess()

	// 写缓存 + 统计（异步）
	go func() {
		cacheCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = a.cache.SetLLM(cacheCtx, req.Model, req, resp, time.Duration(a.cfg.CacheTTL)*time.Second)
		_ = a.stats.RecordCall(cacheCtx, userID, req.Model, resp)
	}()

	c.JSON(http.StatusOK, resp)
}

func (a *App) handleStream(c *gin.Context, p llm.Provider, req *llm.LLMRequest, userID string, cb *resilience.CircuitBreaker) {
	metrics.ProviderCalls.Inc()

	ch, err := p.ChatStream(req)
	if err != nil {
		metrics.ProviderFailures.Inc()
		cb.RecordFailure()
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	cb.RecordSuccess()

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	// 初始响应
	c.SSEvent("", `{"id":"`+utils.NewRequestID()+`","object":"chat.completion","model":"`+req.Model+`","choices":[]}`)

	var fullContent string
	start := time.Now()

	for chunk := range ch {
		if chunk.Error != "" {
			c.SSEvent("", `{"error":"`+escapeJSON(chunk.Error)+`"}`)
			return
		}
		if chunk.Content != "" {
			fullContent += chunk.Content
		}
		if chunk.Done {
			c.SSEvent("", `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
			c.SSEvent("", "[DONE]")
			metrics.ProviderLatency.Observe(metrics.ObserveLatency(start))
			go func() {
				_ = a.stats.RecordCall(context.Background(), userID, req.Model, &llm.LLMResponse{
					Content: fullContent, Model: req.Model, Latency: time.Since(start).Milliseconds(),
				})
			}()
			return
		}
		data := `{"choices":[{"delta":{"content":"` + escapeJSON(chunk.Content) + `"},"index":0}]}`
		c.SSEvent("", data)
	}
}

func escapeJSON(s string) string {
	out := make([]byte, 0, len(s)+2)
	for _, b := range []byte(s) {
		switch b {
		case '"':
			out = append(out, '\\', '"')
		case '\\':
			out = append(out, '\\', '\\')
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		case '\t':
			out = append(out, '\\', 't')
		default:
			out = append(out, b)
		}
	}
	return string(out)
}

// ====== 异步 chat completions ======

func (a *App) chatCompletionsAsync(c *gin.Context) {
	if a.mq == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "async mode not available (mq not connected)"})
		return
	}
	userID, _ := c.Get("user_id")

	var raw chatRequest
	if err := c.ShouldBindJSON(&raw); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	requestID := utils.NewRequestID()
	llmReq := &llm.LLMRequest{
		Model: raw.Model, Messages: raw.Messages,
		Temperature: raw.Temperature, MaxTokens: raw.MaxTokens,
	}
	reqBytes, _ := json.Marshal(llmReq)
	msg := &mq.RequestMessage{
		RequestID: requestID,
		UserID:    userID.(string),
		Model:     raw.Model,
		Req:       reqBytes,
	}

	if err := a.mq.Publish(c.Request.Context(), msg); err != nil {
		metrics.MQPublishFail.Inc()
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	metrics.MQPublishOk.Inc()

	c.JSON(http.StatusAccepted, gin.H{
		"request_id": requestID,
		"status":     "pending",
		"poll_url":   "/api/v1/chat/completions/result/" + requestID,
	})
}

func (a *App) getAsyncResult(c *gin.Context) {
	requestID := c.Param("request_id")
	var resp llm.LLMResponse
	err := a.cache.GetAsyncResult(c.Request.Context(), requestID, &resp)
	if err != nil {
		c.JSON(http.StatusAccepted, gin.H{"request_id": requestID, "status": "pending"})
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (a *App) dailyStats(c *gin.Context) {
	date := c.Query("date")
	summary, err := a.stats.DailySummary(c.Request.Context(), date)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, summary)
}

// ====== 首页 ======

func indexHTML(redisStatus, mqStatus string, providers, models []string) string {
	providerList := ""
	for _, p := range providers {
		providerList += "<span class='tag'>" + p + "</span> "
	}
	if providerList == "" {
		providerList = "<span class='tag warn'>none (mock only)</span>"
	}

	// 动态渲染 model 下拉框
	modelOptions := ""
	for _, m := range models {
		label := m
		if m == "deepseek-chat" || m == "deepseek-reasoner" {
			label = m + " (DeepSeek)"
		} else if m == "gpt-4o-mini" || m == "gpt-4o" {
			label = m + " (OpenAI)"
		} else if m == "qwen-plus" || m == "qwen-turbo" {
			label = m + " (通义)"
		} else if m == "doubao-pro-32k" {
			label = m + " (豆包)"
		}
		modelOptions += `<option value="` + m + `">` + label + `</option>`
	}
	if modelOptions == "" {
		modelOptions = `<option value="mock">mock (no real provider)</option>`
	}

	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="zh">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>SmartProxy - LLM Gateway</title>
<style>
  *{margin:0;padding:0;box-sizing:border-box}
  body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;background:#0f172a;color:#e2e8f0;min-height:100vh;padding:2rem}
  .wrap{max-width:960px;margin:0 auto}
  h1{font-size:2rem;margin-bottom:.25rem;background:linear-gradient(90deg,#60a5fa,#a78bfa);-webkit-background-clip:text;background-clip:text;color:transparent}
  .subtitle{color:#94a3b8;margin-bottom:2rem}
  .grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(220px,1fr));gap:1rem;margin-bottom:2rem}
  .card{background:#1e293b;border:1px solid #334155;border-radius:12px;padding:1.25rem;transition:border-color .2s}
  .card:hover{border-color:#60a5fa}
  .card h3{font-size:.8rem;color:#94a3b8;text-transform:uppercase;letter-spacing:.08em;margin-bottom:.5rem}
  .card .val{font-size:1.1rem;font-weight:600}
  .ok{color:#4ade80}.warn{color:#fbbf24}.err{color:#f87171}
  .tag{display:inline-block;background:#334155;color:#cbd5e1;padding:.25rem .6rem;border-radius:20px;font-size:.85rem;margin:.15rem}
  .links{display:flex;gap:.75rem;flex-wrap:wrap;margin-bottom:2rem}
  .link{background:#1e293b;border:1px solid #334155;color:#60a5fa;padding:.6rem 1rem;border-radius:8px;text-decoration:none;font-size:.9rem;transition:all .2s}
  .link:hover{background:#334155;border-color:#60a5fa}
  .panel{background:#1e293b;border:1px solid #334155;border-radius:12px;padding:1.5rem}
  .panel h2{font-size:1.1rem;margin-bottom:1rem}
  textarea{width:100%%;background:#0f172a;border:1px solid #334155;color:#e2e8f0;border-radius:8px;padding:.8rem;font-size:.95rem;resize:vertical;min-height:70px;font-family:inherit}
  .row{display:flex;gap:.75rem;margin-top:.75rem;align-items:center}
  select,button{background:#334155;border:1px solid #475569;color:#e2e8f0;padding:.55rem 1rem;border-radius:8px;font-size:.9rem;font-family:inherit}
  button{background:linear-gradient(135deg,#3b82f6,#8b5cf6);border:none;color:#fff;font-weight:600;cursor:pointer;flex-shrink:0}
  button:hover{opacity:.9}
  button:disabled{opacity:.5;cursor:not-allowed}
  .resp{margin-top:1rem;background:#0f172a;border:1px solid #334155;border-radius:8px;padding:1rem;min-height:60px;white-space:pre-wrap;font-family:'Consolas',monospace;font-size:.85rem;max-height:300px;overflow-y:auto}
  .resp.err{border-color:#ef4444;color:#fca5a5}
  .resp.ok{border-color:#22c55e}
</style>
</head>
<body>
<div class="wrap">
  <h1>SmartProxy</h1>
  <p class="subtitle">多模型 LLM API 网关 · 统一入口 · 路由 · 限流 · 缓存 · 熔断 · 异步</p>

  <div class="grid">
    <div class="card"><h3>Redis</h3><div class="val %s">%s</div></div>
    <div class="card"><h3>RabbitMQ</h3><div class="val %s">%s</div></div>
    <div class="card"><h3>Providers</h3><div class="val" style="margin-top:.35rem">%s</div></div>
  </div>

  <div class="links">
    <a class="link" href="/healthz" target="_blank">🩺 /healthz</a>
    <a class="link" href="/metrics" target="_blank">📊 /metrics (Prometheus)</a>
    <a class="link" href="https://github.com" target="_blank">📖 README</a>
  </div>

  <div class="panel">
    <h2>⚡ Quick Chat Test</h2>
    <textarea id="msg" placeholder="Type a message...">你好，请做个自我介绍</textarea>
    <div class="row">
      <select id="model">
        %s
      </select>
      <button id="btnSend" onclick="send()">Send</button>
      <button id="btnStream" onclick="sendStream()">Stream</button>
    </div>
    <div id="resp" class="resp">Response will appear here...</div>
  </div>
</div>

<script>
let token = null;
async function ensureToken(){
  if(token) return;
  const r = await fetch('/api/v1/login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({user_id:'demo'})});
  token = (await r.json()).token;
}
async function send(){
  await ensureToken();
  const resp = document.getElementById('resp');
  const btn = document.getElementById('btnSend'); btn.disabled=true; btn.textContent='Sending...';
  try{
    const r = await fetch('/api/v1/chat/completions',{
      method:'POST',
      headers:{Authorization:'Bearer '+token,'Content-Type':'application/json'},
      body:JSON.stringify({model:document.getElementById('model').value,messages:[{role:'user',content:document.getElementById('msg').value}]})
    });
    const j = await r.json();
    resp.className = 'resp '+(r.ok?'ok':'err');
    resp.textContent = r.ok ? (j.content || JSON.stringify(j,null,2)) : ('Error: '+JSON.stringify(j,null,2));
  }catch(e){resp.className='resp err'; resp.textContent='Network error: '+e.message;}
  btn.disabled=false; btn.textContent='Send';
}
async function sendStream(){
  await ensureToken();
  const resp = document.getElementById('resp');
  const btn = document.getElementById('btnStream'); btn.disabled=true; btn.textContent='Streaming...';
  resp.className='resp'; resp.textContent='';
  try{
    const r = await fetch('/api/v1/chat/completions',{
      method:'POST',
      headers:{Authorization:'Bearer '+token,'Content-Type':'application/json'},
      body:JSON.stringify({model:document.getElementById('model').value,messages:[{role:'user',content:document.getElementById('msg').value}],stream:true})
    });
    const reader = r.body.getReader();
    const dec = new TextDecoder(); let buf='';
    while(true){
      const {value,done} = await reader.read();
      if(done) break;
      buf += dec.decode(value,{stream:true});
      const lines = buf.split('\n'); buf=lines.pop();
      for(const line of lines){
        if(!line.startsWith('data:')) continue;
        const data = line.slice(5).trim();
        if(data==='[DONE]') continue;
        try{
          const j = JSON.parse(data);
          const d = j.choices?.[0]?.delta?.content;
          if(d){resp.textContent += d; resp.scrollTop = resp.scrollHeight;}
        }catch(e){}
      }
    }
    resp.className='resp ok';
  }catch(e){resp.className='resp err'; resp.textContent='Error: '+e.message;}
  btn.disabled=false; btn.textContent='Stream';
}
</script>
</body>
</html>`,
		statusClass(redisStatus), redisStatus,
		statusClass(mqStatus), mqStatus,
		providerList,
		modelOptions,
	)
}

func statusClass(s string) string {
	switch s {
	case "connected":
		return "ok"
	case "in-memory fallback":
		return "warn"
	default:
		return "warn"
	}
}
