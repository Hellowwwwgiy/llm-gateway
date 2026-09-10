package config

import (
	"log"
	"os"
	"strconv"
	"sync"

	"github.com/joho/godotenv"
)

// Config 全局配置
type Config struct {
	// Server
	GatewayPort    int
	DispatcherPort int

	// Redis
	RedisAddr     string
	RedisPassword string
	RedisDB       int

	// RabbitMQ
	RabbitMQURL string

	// JWT
	JWTSecret string
	JWTExpiry int // hours

	// Providers - OpenAI 兼容
	OpenAIAPIKey  string
	OpenAIBaseURL string // 可切换到通义/豆包的兼容端点
	OpenAIModel   string

	// Rate Limit
	RateLimitQPS float64
	RateLimitCap int

	// Cache
	CacheTTL int // seconds
}

var (
	instance *Config
	once     sync.Once
)

// Load 从 .env 文件 + 环境变量加载配置（环境变量优先覆盖 .env）
func Load() *Config {
	once.Do(func() {
		// 尝试加载 .env（找不到就跳过，不报错）
		if err := godotenv.Load(); err != nil {
			log.Println("[config] no .env file found, using system env/defaults")
		} else {
			log.Println("[config] loaded .env")
		}

		instance = &Config{
			GatewayPort:    getEnvInt("GATEWAY_PORT", 8080),
			DispatcherPort: getEnvInt("DISPATCHER_PORT", 8081),

			RedisAddr:     getEnv("REDIS_ADDR", "localhost:6379"),
			RedisPassword: getEnv("REDIS_PASSWORD", ""),
			RedisDB:       getEnvInt("REDIS_DB", 0),

			RabbitMQURL: getEnv("RABBITMQ_URL", "amqp://guest:guest@localhost:5672/"),

			JWTSecret: getEnv("JWT_SECRET", "change-me-in-production"),
			JWTExpiry: getEnvInt("JWT_EXPIRY_HOURS", 24),

			OpenAIAPIKey:  getEnv("OPENAI_API_KEY", ""),
			OpenAIBaseURL: getEnv("OPENAI_BASE_URL", "https://api.openai.com/v1"),
			OpenAIModel:   getEnv("OPENAI_MODEL", "gpt-4o-mini"),

			RateLimitQPS: float64(getEnvInt("RATE_LIMIT_QPS", 10)),
			RateLimitCap: getEnvInt("RATE_LIMIT_CAP", 20),

			CacheTTL: getEnvInt("CACHE_TTL_SECONDS", 3600),
		}
	})
	return instance
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
