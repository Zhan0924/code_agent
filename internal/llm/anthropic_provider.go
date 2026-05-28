// anthropic_provider.go — Anthropic 原生 Messages API Provider，支持 streaming、
// cache_control、tool_use。
//
// ============================================================================
//
//	设 计 原 理
//
// ============================================================================
//
// 【为什么需要原生 Provider】
//
//	虽然 openai_provider 可以通过代理（new-api / litellm）访问 Claude，但：
//	  · cache_control 在 OpenAI 协议里没有对应字段，代理层需要自己映射；
//	  · Anthropic 的 system 消息是独立参数，不在 messages 数组里；
//	  · 原生 SDK 的错误处理、重试、超时策略更贴合 Anthropic 的实际行为。
//	原生 Provider 绕过代理层，直接调 Anthropic Messages API，获得：
//	  · 原生 prompt caching 支持（40-90% 成本节省 + 10x 延迟降低）；
//	  · 更精确的 token 计数（Usage.CacheCreationInputTokens / CacheReadInputTokens）；
//	  · 更稳定的流式协议（Anthropic SSE 格式与 OpenAI 有细微差异）。
//
// 【System 消息的特殊处理】
//
//	Anthropic API 要求：
//	  · system 消息不在 messages 数组里，而是单独的 System 参数（[]TextBlockParam）；
//	  · messages 数组必须以 user 消息开头，且 user/assistant 交替。
//	convertMessages 会：
//	  1. 提取所有 role=system 的消息，合并成 System 参数；
//	  2. 剩余消息映射成 MessageParam 数组；
//	  3. 如果第一条不是 user，插入空 user 消息（Anthropic 要求）。
//
// 【Cache Control 的映射】
//
//	models.Message.CacheControl != nil 时，在对应的 ContentBlockParam 上设置
//	CacheControl: {Type: "ephemeral"}。Anthropic 会在该位置打 cache breakpoint，
//	后续请求复用该前缀可节省 90% prompt token 成本。仅在 cfg.EnablePromptCaching
//	为 true 时启用。
//
// 【Tool Calls 的双向映射】
//
//	Request:  models.ToolDefinition → anthropic.ToolParam（InputSchema 是 JSON Schema）
//	Response: anthropic.ToolUseBlock → models.ToolCall（ID / Name / Input）
//	Anthropic 的 tool_use 格式与 OpenAI 基本一致，只是字段名略有不同。
//
// 【Streaming 协议】
//
//	Anthropic SSE 事件类型：
//	  · message_start    — 初始 Message 对象（含 Usage）
//	  · content_block_start / content_block_delta / content_block_stop
//	  · message_delta    — 增量 Usage（CacheCreationInputTokens 等）
//	  · message_stop     — 流结束
//	我们订阅 SDK 的 ssestream.Stream，解析 MessageStreamEventUnion，把 text delta
//	和 tool_use 翻成统一的 StreamChunk。Tool input 通过 input_json_delta 增量累积。
//
// ============================================================================
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/agent/code_agent/internal/config"
	"github.com/agent/code_agent/internal/models"
	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"go.uber.org/zap"
)

// anthropicProvider implements the Provider interface using Anthropic's native Messages API.
type anthropicProvider struct {
	client anthropic.Client
	cfg    *config.LLMProviderConfig
	logger *zap.Logger
}

// newAnthropicProvider creates a new Anthropic native provider.
func newAnthropicProvider(cfg *config.LLMProviderConfig, httpClient *http.Client, logger *zap.Logger) (*anthropicProvider, error) {
	opts := []option.RequestOption{
		option.WithAPIKey(cfg.APIKey),
	}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	if httpClient != nil {
		opts = append(opts, option.WithHTTPClient(httpClient))
	}

	client := anthropic.NewClient(opts...)

	return &anthropicProvider{
		client: client,
		cfg:    cfg,
		logger: logger.With(zap.String("provider", "anthropic")),
	}, nil
}

func (p *anthropicProvider) Name() string {
	return fmt.Sprintf("anthropic/%s", p.cfg.Model)
}

// ChatCompletion sends a non-streaming chat completion request to Anthropic Messages API.
func (p *anthropicProvider) ChatCompletion(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	messages, systemText := p.convertMessages(req.Messages)
	model := req.Model
	if model == "" {
		model = p.cfg.Model
	}

	maxTokens := int64(req.MaxTokens)
	if maxTokens == 0 {
		maxTokens = int64(p.cfg.MaxTokens)
	}

	temperature := req.Temperature
	if temperature == 0 {
		temperature = p.cfg.Temperature
	}

	params := anthropic.MessageNewParams{
		Model:     model,
		Messages:  messages,
		MaxTokens: maxTokens,
	}

	if temperature > 0 {
		params.Temperature = param.NewOpt(float64(temperature))
	}

	// Set system parameter if we have system messages
	if systemText != "" {
		systemBlock := anthropic.TextBlockParam{
			Text: systemText,
		}
		if p.cfg.EnablePromptCaching {
			systemBlock.CacheControl = anthropic.CacheControlEphemeralParam{
				Type: "ephemeral",
			}
		}
		params.System = []anthropic.TextBlockParam{systemBlock}
	}

	// Add tools if provided
	if len(req.Tools) > 0 {
		params.Tools = p.convertTools(req.Tools)
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()

	resp, err := p.client.Messages.New(timeoutCtx, params)
	if err != nil {
		return nil, fmt.Errorf("anthropic messages API failed: %w", err)
	}

	// Extract content and tool calls
	var content strings.Builder
	var toolCalls []models.ToolCall

	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			content.WriteString(block.Text)
		case "tool_use":
			toolCalls = append(toolCalls, models.ToolCall{
				ID:   block.ID,
				Name: block.Name,
				Args: block.Input,
			})
		}
	}

	result := &ChatResponse{
		Content:   content.String(),
		ToolCalls: toolCalls,
		Usage: Usage{
			PromptTokens:     int(resp.Usage.InputTokens),
			CompletionTokens: int(resp.Usage.OutputTokens),
			TotalTokens:      int(resp.Usage.InputTokens + resp.Usage.OutputTokens),
		},
		Model: resp.Model,
	}

	return result, nil
}

// ChatCompletionStream sends a streaming chat completion request to Anthropic Messages API.
func (p *anthropicProvider) ChatCompletionStream(ctx context.Context, req *ChatRequest) (<-chan StreamChunk, error) {
	messages, systemText := p.convertMessages(req.Messages)
	model := req.Model
	if model == "" {
		model = p.cfg.Model
	}

	maxTokens := int64(req.MaxTokens)
	if maxTokens == 0 {
		maxTokens = int64(p.cfg.MaxTokens)
	}

	temperature := req.Temperature
	if temperature == 0 {
		temperature = p.cfg.Temperature
	}

	params := anthropic.MessageNewParams{
		Model:     model,
		Messages:  messages,
		MaxTokens: maxTokens,
	}

	if temperature > 0 {
		params.Temperature = param.NewOpt(float64(temperature))
	}

	if systemText != "" {
		params.System = []anthropic.TextBlockParam{
			{Text: systemText},
		}
	}

	if len(req.Tools) > 0 {
		params.Tools = p.convertTools(req.Tools)
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)

	stream := p.client.Messages.NewStreaming(timeoutCtx, params)

	ch := make(chan StreamChunk, 64)

	go func() {
		defer close(ch)
		defer cancel()

		// Accumulate tool calls across deltas (indexed by content block index)
		toolCallsMap := make(map[int64]*models.ToolCall)

		for stream.Next() {
			event := stream.Current()

			switch event.Type {
			case "content_block_start":
				// New content block starting
				if event.ContentBlock.Type == "tool_use" {
					idx := event.Index
					toolCallsMap[idx] = &models.ToolCall{
						ID:   event.ContentBlock.ID,
						Name: event.ContentBlock.Name,
						Args: json.RawMessage(""),
					}
				}

			case "content_block_delta":
				delta := event.Delta

				// Text delta
				if delta.Type == "text_delta" {
					ch <- StreamChunk{
						Content: delta.Text,
					}
				}

				// Tool input delta (JSON accumulation)
				if delta.Type == "input_json_delta" {
					idx := event.Index
					if tc, exists := toolCallsMap[idx]; exists {
						tc.Args = append(tc.Args, []byte(delta.PartialJSON)...)
					}
				}

			case "content_block_stop":
				// Content block finished, emit tool call if any
				idx := event.Index
				if tc, exists := toolCallsMap[idx]; exists {
					ch <- StreamChunk{
						ToolCalls: []models.ToolCall{*tc},
					}
					delete(toolCallsMap, idx)
				}

			case "message_stop":
				ch <- StreamChunk{Done: true}
				return

			case "error":
				ch <- StreamChunk{
					Err:  fmt.Errorf("anthropic stream error: %v", event),
					Done: true,
				}
				return
			}
		}

		if err := stream.Err(); err != nil {
			ch <- StreamChunk{Err: err, Done: true}
		} else {
			ch <- StreamChunk{Done: true}
		}
	}()

	return ch, nil
}

// convertMessages converts internal Message types to Anthropic MessageParam format.
// Returns (messages, systemText) where systemText is the merged system prompt.
func (p *anthropicProvider) convertMessages(msgs []models.Message) ([]anthropic.MessageParam, string) {
	var systemParts []string
	var messages []anthropic.MessageParam

	for _, m := range msgs {
		// Extract system messages separately
		if m.Role == models.RoleSystem {
			systemParts = append(systemParts, m.Content)
			continue
		}

		// Convert role
		var role anthropic.MessageParamRole
		switch m.Role {
		case models.RoleUser:
			role = anthropic.MessageParamRoleUser
		case models.RoleAssistant:
			role = anthropic.MessageParamRoleAssistant
		default:
			// Tool results are sent as user messages with tool_result blocks
			role = anthropic.MessageParamRoleUser
		}

		var content []anthropic.ContentBlockParamUnion

		// Handle tool result messages
		if m.Role == models.RoleTool && m.ToolCallID != "" {
			toolResultBlock := anthropic.ToolResultBlockParam{
				ToolUseID: m.ToolCallID,
				Content: []anthropic.ToolResultBlockParamContentUnion{
					{
						OfText: &anthropic.TextBlockParam{
							Text: m.Content,
						},
					},
				},
			}
			if m.CacheControl != nil && p.cfg.EnablePromptCaching {
				toolResultBlock.CacheControl = anthropic.CacheControlEphemeralParam{
					Type: "ephemeral",
				}
			}
			content = append(content, anthropic.ContentBlockParamUnion{
				OfToolResult: &toolResultBlock,
			})
		} else if len(m.ToolCalls) > 0 {
			// Assistant message with tool calls
			if m.Content != "" {
				content = append(content, anthropic.ContentBlockParamUnion{
					OfText: &anthropic.TextBlockParam{
						Text: m.Content,
					},
				})
			}

			// Add tool use blocks
			for _, tc := range m.ToolCalls {
				content = append(content, anthropic.ContentBlockParamUnion{
					OfToolUse: &anthropic.ToolUseBlockParam{
						ID:    tc.ID,
						Name:  tc.Name,
						Input: tc.Args,
					},
				})
			}
		} else {
			// Regular text message
			textBlock := anthropic.TextBlockParam{
				Text: m.Content,
			}
			if m.CacheControl != nil && p.cfg.EnablePromptCaching {
				textBlock.CacheControl = anthropic.CacheControlEphemeralParam{
					Type: "ephemeral",
				}
			}
			content = append(content, anthropic.ContentBlockParamUnion{
				OfText: &textBlock,
			})
		}

		messages = append(messages, anthropic.MessageParam{
			Role:    role,
			Content: content,
		})
	}

	// Anthropic requires messages to start with user role
	if len(messages) > 0 && messages[0].Role == anthropic.MessageParamRoleAssistant {
		messages = append([]anthropic.MessageParam{
			{
				Role: anthropic.MessageParamRoleUser,
				Content: []anthropic.ContentBlockParamUnion{
					{
						OfText: &anthropic.TextBlockParam{
							Text: "(continue)",
						},
					},
				},
			},
		}, messages...)
	}

	// Merge system messages
	systemText := strings.Join(systemParts, "\n\n")

	return messages, systemText
}

// convertTools converts internal ToolDefinition to Anthropic ToolUnionParam format.
func (p *anthropicProvider) convertTools(tools []models.ToolDefinition) []anthropic.ToolUnionParam {
	result := make([]anthropic.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		var inputSchema anthropic.ToolInputSchemaParam
		if len(t.Parameters) > 0 {
			// Parse the JSON schema
			var schemaMap map[string]interface{}
			if err := json.Unmarshal(t.Parameters, &schemaMap); err == nil {
				// Extract properties and required fields
				if props, ok := schemaMap["properties"].(map[string]interface{}); ok {
					inputSchema.Properties = props
				}
				if req, ok := schemaMap["required"].([]interface{}); ok {
					required := make([]string, 0, len(req))
					for _, r := range req {
						if s, ok := r.(string); ok {
							required = append(required, s)
						}
					}
					inputSchema.Required = required
				}
			}
		}

		toolParam := anthropic.ToolParam{
			Name:        t.Name,
			InputSchema: inputSchema,
		}
		if t.Description != "" {
			toolParam.Description = param.NewOpt(t.Description)
		}
		result = append(result, anthropic.ToolUnionParam{
			OfTool: &toolParam,
		})
	}
	return result
}
