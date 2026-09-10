package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAIProvider OpenAI 兼容接口的 provider（同时适用于通义/豆包的兼容端点）
type OpenAIProvider struct {
	name    string
	apiKey  string
	baseURL string
	model   string
	client  *http.Client
}

// NewOpenAIProvider 创建一个 OpenAI 兼容 provider
// name: 标识名（如 "openai", "qwen", "doubao"）
// baseURL: API 基础地址（如 "https://api.openai.com/v1"）
// model: 默认模型（请求里可覆盖）
func NewOpenAIProvider(name, apiKey, baseURL, model string) *OpenAIProvider {
	return &OpenAIProvider{
		name:    name,
		apiKey:  apiKey,
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		client:  &http.Client{Timeout: 120 * time.Second},
	}
}

func (p *OpenAIProvider) Name() string { return p.name }

func (p *OpenAIProvider) Capabilities() Capabilities {
	return Capabilities{
		SupportsStream:    true,
		SupportsReasoning: false, // 部分兼容端点不支持，按需开启
		SupportsJSONMode:  true,
		MaxContextTokens:  128000,
	}
}

// ===== 内部：构造底层请求体 =====

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIRequest struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	Temperature float64         `json:"temperature,omitempty"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Stream      bool            `json:"stream"`
}

func (p *OpenAIProvider) buildRequest(req *LLMRequest) openAIRequest {
	messages := make([]openAIMessage, len(req.Messages))
	for i, m := range req.Messages {
		messages[i] = openAIMessage{Role: string(m.Role), Content: m.Content}
	}
	model := req.Model
	if model == "" {
		model = p.model
	}
	return openAIRequest{
		Model:       model,
		Messages:    messages,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Stream:      req.Stream,
	}
}

// ===== 非流式 Chat =====

type openAIResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int `json:"index"`
		Message      struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
}

func (p *OpenAIProvider) Chat(req *LLMRequest) (*LLMResponse, error) {
	start := time.Now()
	req.Stream = false

	body := p.buildRequest(req)
	payload, _ := json.Marshal(body)

	httpReq, err := http.NewRequest("POST", p.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http call: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("provider error %d: %s", resp.StatusCode, string(b))
	}

	var parsed openAIResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	result := &LLMResponse{
		ID:       parsed.ID,
		Model:    parsed.Model,
		Latency:  time.Since(start).Milliseconds(),
		Usage:    parsed.Usage,
	}
	if len(parsed.Choices) > 0 {
		result.Content = parsed.Choices[0].Message.Content
		result.FinishReason = parsed.Choices[0].FinishReason
	}
	return result, nil
}

// ===== 流式 ChatStream =====

func (p *OpenAIProvider) ChatStream(req *LLMRequest) (<-chan *StreamChunk, error) {
	req.Stream = true

	body := p.buildRequest(req)
	payload, _ := json.Marshal(body)

	httpReq, err := http.NewRequest("POST", p.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build stream request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http stream: %w", err)
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("provider stream error %d: %s", resp.StatusCode, string(b))
	}

	out := make(chan *StreamChunk, 64)
	go func() {
		defer close(out)
		defer resp.Body.Close()

		// 简单的 SSE 解析：逐行读，找 "data: {...}"
		buf := make([]byte, 4096)
		var lineBuf strings.Builder
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				// 按 \n 切行
				for _, b := range chunk {
					if b == '\n' {
						line := strings.TrimSpace(lineBuf.String())
						lineBuf.Reset()
						if line != "" {
							p.parseSSELine(line, out)
						}
					} else {
						lineBuf.WriteByte(b)
					}
				}
			}
			if err != nil {
				if err != io.EOF {
					out <- &StreamChunk{Kind: ChunkError, Error: err.Error(), Done: true}
				} else {
					out <- &StreamChunk{Kind: ChunkDone, Done: true}
				}
				return
			}
		}
	}()

	return out, nil
}

type openAIStreamDelta struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

func (p *OpenAIProvider) parseSSELine(line string, out chan<- *StreamChunk) {
	if !strings.HasPrefix(line, "data:") {
		return
	}
	data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if data == "[DONE]" {
		out <- &StreamChunk{Kind: ChunkDone, Done: true}
		return
	}
	var delta openAIStreamDelta
	if err := json.Unmarshal([]byte(data), &delta); err != nil {
		return // 忽略解析失败的行
	}
	if len(delta.Choices) > 0 {
		if delta.Choices[0].FinishReason != nil {
			out <- &StreamChunk{Kind: ChunkDone, Done: true}
			return
		}
		if content := delta.Choices[0].Delta.Content; content != "" {
			out <- &StreamChunk{Kind: ChunkContent, Content: content}
		}
	}
}
