package llm

// ===== 统一请求/响应类型 =====

// Role 消息角色
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// ChatMessage 一条对话消息
type ChatMessage struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
}

// LLMRequest 统一的模型请求
type LLMRequest struct {
	Model       string                 `json:"model"`
	Messages    []ChatMessage          `json:"messages"`
	Temperature float64                `json:"temperature,omitempty"`
	MaxTokens   int                    `json:"max_tokens,omitempty"`
	Stream      bool                   `json:"stream"`
	Extra       map[string]interface{} `json:"extra,omitempty"` // provider-specific
}

// LLMResponse 统一的非流式响应
type LLMResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Content string `json:"content"`
	// 可选字段
	Reasoning  string  `json:"reasoning,omitempty"`
	FinishReason string `json:"finish_reason,omitempty"`
	Usage     *Usage  `json:"usage,omitempty"`
	Latency   int64   `json:"latency_ms"`
}

// Usage token 用量统计
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// StreamChunk 统一的流式分片
type StreamChunk struct {
	Kind    ChunkKind `json:"kind"`
	Content string    `json:"content,omitempty"`
	Reason  string    `json:"reasoning,omitempty"`
	Done    bool      `json:"done"`
	Error   string    `json:"error,omitempty"`
}

// ChunkKind 分片类型
type ChunkKind string

const (
	ChunkContent    ChunkKind = "content"
	ChunkReasoning  ChunkKind = "reasoning"
	ChunkDone       ChunkKind = "done"
	ChunkError      ChunkKind = "error"
)

// ===== Provider 能力声明 =====

// Capabilities 声明该 provider 支持什么
type Capabilities struct {
	SupportsStream    bool
	SupportsReasoning bool
	SupportsJSONMode  bool
	MaxContextTokens  int
}

// Provider 统一的模型提供者接口
type Provider interface {
	// Name provider 标识，用于日志/路由
	Name() string
	// Capabilities 返回该 provider 能力
	Capabilities() Capabilities
	// Chat 非流式调用
	Chat(req *LLMRequest) (*LLMResponse, error)
	// ChatStream 流式调用，返回 channel，消费完需 close
	ChatStream(req *LLMRequest) (<-chan *StreamChunk, error)
}
