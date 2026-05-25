// openai_provider.go — OpenAI 兼容客户端（同时支持 OpenAI / Anthropic 代理 /
// Ollama / vLLM / Azure OpenAI 等任何实现了 OpenAI 协议的 endpoint）。
//
// ============================================================================
//
//	设 计 原 理
//
// ============================================================================
//
// 【为什么只有一个 Provider 类型？】
//
//	OpenAI 协议事实上是 LLM 业界标准。Anthropic 官方 API 格式不同，但我们
//	通过支持 OpenAI 协议的代理（new-api / openrouter / litellm 等）用统一
//	接口访问 Claude。好处：主备切换只改 base_url + api_key + model 名字，
//	不需要维护多套 SDK。
//
// 【流式协议】
//
//	OpenAI 的 SSE 协议：每个 chunk 是 `data: {json}\n\n`，最后 `data: [DONE]\n\n`。
//	go-openai SDK 已经处理了这层。我们订阅 chunk 然后把 Delta.Content / ToolCalls
//	翻成统一的 StreamChunk 类型发到 channel。
//
// 【Tool calls 的两种格式】
//
//	OpenAI 老 schema：function_call（单个）
//	OpenAI 新 schema：tool_calls（数组，支持并行调用）
//	我们只用新 schema。
//
// 【错误处理的边界】
//
//	这里不做重试（上层 Client 用 gobreaker）。网络层错误直接返回 wrapped error。
//	HTTP 状态码错误（4xx/5xx）go-openai 会翻成 APIError，保留在 wrapped error
//	里让 Client 的 metric 打点可以区分 status。
//
// ============================================================================
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/agent/code_agent/internal/config"
	"github.com/agent/code_agent/internal/models"
	openai "github.com/sashabaranov/go-openai"
	"go.uber.org/zap"
)

// openaiProvider implements the Provider interface using the OpenAI-compatible API.
// It supports both OpenAI and any OpenAI-compatible endpoint (e.g., Ollama, vLLM).
type openaiProvider struct {
	client *openai.Client
	cfg    *config.LLMProviderConfig
	logger *zap.Logger
}

// cacheControlJSON is the JSON representation of cache_control for Anthropic-compatible APIs.
type cacheControlJSON struct {
	Type string `json:"type"`
}

// cachedMessage wraps an OpenAI message with optional cache_control for Anthropic providers.
type cachedMessage struct {
	openai.ChatCompletionMessage
	CacheControl *cacheControlJSON `json:"cache_control,omitempty"`
}

// newOpenAIProvider creates a new OpenAI-compatible provider.
func newOpenAIProvider(cfg *config.LLMProviderConfig, logger *zap.Logger) (*openaiProvider, error) {
	clientCfg := openai.DefaultConfig(cfg.APIKey)
	if cfg.BaseURL != "" {
		clientCfg.BaseURL = cfg.BaseURL
	}

	return &openaiProvider{
		client: openai.NewClientWithConfig(clientCfg),
		cfg:    cfg,
		logger: logger.With(zap.String("provider", cfg.Provider)),
	}, nil
}

func (p *openaiProvider) Name() string {
	return fmt.Sprintf("%s/%s", p.cfg.Provider, p.cfg.Model)
}

// ChatCompletion sends a non-streaming chat completion request.
func (p *openaiProvider) ChatCompletion(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	messages := p.convertMessages(req.Messages)
	model := req.Model
	if model == "" {
		model = p.cfg.Model
	}

	apiReq := openai.ChatCompletionRequest{
		Model:       model,
		Messages:    messages,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	}

	// Add tools if provided
	if len(req.Tools) > 0 {
		apiReq.Tools = p.convertTools(req.Tools)
	}

	// Add response format if provided
	if req.ResponseFormat != nil {
		apiReq.ResponseFormat = p.convertResponseFormat(req.ResponseFormat)
	}

	if apiReq.MaxTokens == 0 {
		apiReq.MaxTokens = p.cfg.MaxTokens
	}
	if apiReq.Temperature == 0 {
		apiReq.Temperature = p.cfg.Temperature
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()

	resp, err := p.client.CreateChatCompletion(timeoutCtx, apiReq)
	if err != nil {
		return nil, fmt.Errorf("openai chat completion failed: %w", err)
	}

	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("openai returned empty choices")
	}

	choice := resp.Choices[0]
	result := &ChatResponse{
		Content: choice.Message.Content,
		Usage: Usage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		},
		Model: resp.Model,
	}

	// Extract tool calls if present
	if len(choice.Message.ToolCalls) > 0 {
		for _, tc := range choice.Message.ToolCalls {
			result.ToolCalls = append(result.ToolCalls, models.ToolCall{
				ID:   tc.ID,
				Name: tc.Function.Name,
				Args: json.RawMessage(tc.Function.Arguments),
			})
		}
	}

	return result, nil
}

// ChatCompletionStream sends a streaming chat completion request.
func (p *openaiProvider) ChatCompletionStream(ctx context.Context, req *ChatRequest) (<-chan StreamChunk, error) {
	messages := p.convertMessages(req.Messages)
	model := req.Model
	if model == "" {
		model = p.cfg.Model
	}

	apiReq := openai.ChatCompletionRequest{
		Model:       model,
		Messages:    messages,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		Stream:      true,
	}

	if len(req.Tools) > 0 {
		apiReq.Tools = p.convertTools(req.Tools)
	}

	// Add response format if provided
	if req.ResponseFormat != nil {
		apiReq.ResponseFormat = p.convertResponseFormat(req.ResponseFormat)
	}

	if apiReq.MaxTokens == 0 {
		apiReq.MaxTokens = p.cfg.MaxTokens
	}
	if apiReq.Temperature == 0 {
		apiReq.Temperature = p.cfg.Temperature
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)

	stream, err := p.client.CreateChatCompletionStream(timeoutCtx, apiReq)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("openai stream failed: %w", err)
	}

	ch := make(chan StreamChunk, 64)

	go func() {
		defer close(ch)
		defer cancel()
		defer stream.Close()

		for {
			resp, err := stream.Recv()
			if err != nil {
				if err == io.EOF {
					ch <- StreamChunk{Done: true}
					return
				}
				ch <- StreamChunk{Err: err, Done: true}
				return
			}

			if len(resp.Choices) == 0 {
				continue
			}

			delta := resp.Choices[0].Delta
			chunk := StreamChunk{
				Content: delta.Content,
			}

			// Handle streaming tool calls
			if len(delta.ToolCalls) > 0 {
				for _, tc := range delta.ToolCalls {
					chunk.ToolCalls = append(chunk.ToolCalls, models.ToolCall{
						ID:   tc.ID,
						Name: tc.Function.Name,
						Args: json.RawMessage(tc.Function.Arguments),
					})
				}
			}

			select {
			case ch <- chunk:
			case <-timeoutCtx.Done():
				ch <- StreamChunk{Err: timeoutCtx.Err(), Done: true}
				return
			}
		}
	}()

	return ch, nil
}

// convertMessages converts internal Message types to OpenAI message format.
func (p *openaiProvider) convertMessages(msgs []models.Message) []openai.ChatCompletionMessage {
	result := make([]openai.ChatCompletionMessage, 0, len(msgs))
	for _, m := range msgs {
		msg := openai.ChatCompletionMessage{
			Role:    string(m.Role),
			Content: m.Content,
		}

		// Set ToolCallID for tool result messages (required by Claude/Bedrock)
		if m.ToolCallID != "" {
			msg.ToolCallID = m.ToolCallID
		}

		// Convert tool calls
		if len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				msg.ToolCalls = append(msg.ToolCalls, openai.ToolCall{
					ID:   tc.ID,
					Type: openai.ToolTypeFunction,
					Function: openai.FunctionCall{
						Name:      tc.Name,
						Arguments: string(tc.Args),
					},
				})
			}
		}

		result = append(result, msg)
	}
	return result
}

// convertMessagesWithCache converts messages and injects cache_control markers
// for Anthropic-compatible providers. Returns JSON-serializable messages.
func (p *openaiProvider) convertMessagesWithCache(msgs []models.Message) []cachedMessage {
	result := make([]cachedMessage, 0, len(msgs))
	for _, m := range msgs {
		msg := openai.ChatCompletionMessage{
			Role:    string(m.Role),
			Content: m.Content,
		}

		if m.ToolCallID != "" {
			msg.ToolCallID = m.ToolCallID
		}

		if len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				msg.ToolCalls = append(msg.ToolCalls, openai.ToolCall{
					ID:   tc.ID,
					Type: openai.ToolTypeFunction,
					Function: openai.FunctionCall{
						Name:      tc.Name,
						Arguments: string(tc.Args),
					},
				})
			}
		}

		cm := cachedMessage{ChatCompletionMessage: msg}
		if m.CacheControl != nil {
			cm.CacheControl = &cacheControlJSON{Type: m.CacheControl.Type}
		}
		result = append(result, cm)
	}
	return result
}

// convertTools converts internal ToolDefinition to OpenAI tool format.
func (p *openaiProvider) convertTools(tools []models.ToolDefinition) []openai.Tool {
	result := make([]openai.Tool, 0, len(tools))
	for _, t := range tools {
		var params json.RawMessage
		if len(t.Parameters) > 0 {
			params = t.Parameters
		} else {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}

		result = append(result, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		})
	}
	return result
}

// convertResponseFormat converts internal ResponseFormat to OpenAI response_format.
func (p *openaiProvider) convertResponseFormat(rf *models.ResponseFormat) *openai.ChatCompletionResponseFormat {
	if rf == nil {
		return nil
	}

	result := &openai.ChatCompletionResponseFormat{
		Type: openai.ChatCompletionResponseFormatType(rf.Type),
	}

	if rf.JSONSchema != nil {
		result.JSONSchema = &openai.ChatCompletionResponseFormatJSONSchema{
			Name:        rf.JSONSchema.Name,
			Description: rf.JSONSchema.Description,
			Schema:      json.RawMessage(rf.JSONSchema.Schema),
			Strict:      rf.JSONSchema.Strict,
		}
	}

	return result
}
