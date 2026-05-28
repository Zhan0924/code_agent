// Package llm provides an abstraction over LLM providers with circuit breaker
// pattern and automatic fallback to ensure high availability of AI capabilities.
//
// ============================================================================
//
//	设 计 原 理
//
// ============================================================================
//
// 【为什么需要"一层抽象"】
//
//	ReAct 循环一步可能调 LLM 几十次。如果直接把 openai 客户端散布到 orchestrator
//	/ planner / generator 各处，任何一次协议变动都要改几十处。Provider 接口
//	把 LLM 调用收窄到三个方法：ChatCompletion / ChatCompletionStream / Name。
//
// 【双供应商 + 熔断的设计】
//
//	生产里 LLM 不稳定是必然事件（流量高峰 / 供应商单集群抖动）。我们维护：
//	  · Primary：主力（Anthropic/OpenAI），能力强但偶尔抖；
//	  · Fallback：兜底（Ollama/本地量化模型），能力弱但可用；
//	  · Local Circuit Breaker（gobreaker）：单进程内失败累积到阈值就跳开，
//	    防止连锁等待；
//	  · Shared Circuit Breaker（shared_breaker.go，可选）：跨副本 Redis
//	    计数，防止 N 个 Pod 各自独立地重复砸同一个 degraded upstream。
//
//	任一熔断跳开后：优先 fallback；fallback 也失败时直接返回 error。
//
// 【为什么 ChatCompletionStream 的 fallback 只在"setup 失败"时生效】
//
//	一旦流开始，中间出错（HTTP 500 mid-stream）已经吐给前端半截内容了。
//	重跑 fallback 会让用户看到两段互相矛盾的响应。更好的做法是让前端感知
//	错误并选择重试整个请求——当前实现符合这一权衡。
//
// 【EstimateTokens 的用途】
//
//	Token 预算的**粗略**计量，用于：
//	  · orchestrator.pruneMessages 判断何时丢弃旧消息；
//	  · context.prompt_builder 切分上下文块；
//	精度要求到"不至于低估一倍"就够；精确计数需要 tiktoken-go（见 P1 #20）。
//	当前实现见 client.go:209+（rune 分类权重法）。
//
// 【Metrics】
//
//	每次调用打：
//	  code_agent_llm_request_total{provider, model, status}
//	  code_agent_llm_request_duration_seconds{provider, model}
//	  code_agent_llm_tokens_used_total{provider, kind=prompt|completion}
//	  code_agent_llm_circuit_breaker_state{provider}
//
// ============================================================================
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/agent/code_agent/internal/config"
	"github.com/agent/code_agent/internal/metrics"
	"github.com/agent/code_agent/internal/models"
	"github.com/sony/gobreaker"
	"go.uber.org/zap"
)

// Provider defines the interface that all LLM backends must implement.
type Provider interface {
	// ChatCompletion sends a chat completion request and returns the response.
	ChatCompletion(ctx context.Context, req *ChatRequest) (*ChatResponse, error)
	// ChatCompletionStream sends a streaming chat completion request.
	ChatCompletionStream(ctx context.Context, req *ChatRequest) (<-chan StreamChunk, error)
	// Name returns the provider name for logging.
	Name() string
}

// ChatRequest is the unified request format sent to any LLM provider.
type ChatRequest struct {
	Messages       []models.Message        `json:"messages"`
	Tools          []models.ToolDefinition `json:"tools,omitempty"`
	MaxTokens      int                     `json:"max_tokens,omitempty"`
	Temperature    float32                 `json:"temperature,omitempty"`
	Model          string                  `json:"model,omitempty"`
	ResponseFormat *models.ResponseFormat  `json:"response_format,omitempty"`
}

// ChatResponse is the unified response from an LLM provider.
type ChatResponse struct {
	Content   string            `json:"content"`
	ToolCalls []models.ToolCall `json:"tool_calls,omitempty"`
	Usage     Usage             `json:"usage"`
	Model     string            `json:"model"`
}

// Usage reports token consumption.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// StreamChunk represents a single piece of a streaming response.
type StreamChunk struct {
	Content   string            `json:"content,omitempty"`
	ToolCalls []models.ToolCall `json:"tool_calls,omitempty"`
	Done      bool              `json:"done"`
	Err       error             `json:"-"`
}

// Client is the main LLM client with circuit breaker and fallback support.
// The local gobreaker opens on per-process failure concentration; the
// optional SharedBreaker aggregates failures across replicas via Redis so
// distributed degradation (each pod sees few failures, fleet-wide is lots)
// also trips the circuit. See shared_breaker.go for the full rationale.
type Client struct {
	primary        Provider
	fallback       Provider
	breaker        *gobreaker.CircuitBreaker
	sharedBreaker  *SharedCircuitBreaker // may be nil — local breaker only
	logger         *zap.Logger
}

// NewClient creates a new LLM client with circuit breaker wrapping the primary provider.
func NewClient(cfg *config.LLMConfig, logger *zap.Logger) (*Client, error) {
	return NewClientWithOptions(cfg, nil, nil, logger)
}

// NewClientWithSharedBreaker is the same as NewClient but also wires in a
// cross-replica SharedBreaker. Pass nil for shared to disable — the client
// then behaves exactly like NewClient. Kept as a separate constructor so
// existing callers that don't have Redis access don't have to change.
func NewClientWithSharedBreaker(cfg *config.LLMConfig, shared *SharedCircuitBreaker, logger *zap.Logger) (*Client, error) {
	return NewClientWithOptions(cfg, shared, nil, logger)
}

// NewClientWithOptions creates a new LLM client with all optional dependencies.
// httpClient is used for SSRF-protected outbound requests (pass nil for default).
func NewClientWithOptions(cfg *config.LLMConfig, shared *SharedCircuitBreaker, httpClient *http.Client, logger *zap.Logger) (*Client, error) {
	var primary Provider
	var err error

	// Dispatch to the appropriate provider based on configuration
	switch cfg.Primary.Provider {
	case "anthropic":
		primary, err = newAnthropicProvider(&cfg.Primary, httpClient, logger)
	default:
		// Default to OpenAI-compatible provider (supports openai, ollama, vllm, etc.)
		primary, err = newOpenAIProvider(&cfg.Primary, httpClient, logger)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to create primary LLM provider: %w", err)
	}

	var fallback Provider
	if cfg.Fallback.BaseURL != "" {
		switch cfg.Fallback.Provider {
		case "anthropic":
			fallback, err = newAnthropicProvider(&cfg.Fallback, httpClient, logger)
		default:
			fallback, err = newOpenAIProvider(&cfg.Fallback, httpClient, logger)
		}
		if err != nil {
			logger.Warn("failed to create fallback LLM provider, running without fallback",
				zap.Error(err))
		}
	}

	// Configure circuit breaker with exponential backoff characteristics
	cbSettings := gobreaker.Settings{
		Name:        "llm-primary",
		MaxRequests: uint32(cfg.CircuitBreaker.HalfOpenMaxReqs),
		Interval:    cfg.CircuitBreaker.Timeout,
		Timeout:     cfg.CircuitBreaker.Timeout,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return int(counts.ConsecutiveFailures) >= cfg.CircuitBreaker.MaxFailures
		},
		OnStateChange: func(name string, from gobreaker.State, to gobreaker.State) {
			logger.Warn("circuit breaker state changed",
				zap.String("name", name),
				zap.String("from", from.String()),
				zap.String("to", to.String()),
			)
		},
	}

	return &Client{
		primary:       primary,
		fallback:      fallback,
		breaker:       gobreaker.NewCircuitBreaker(cbSettings),
		sharedBreaker: shared,
		logger:        logger,
	}, nil
}

// ChatCompletion executes a chat completion with circuit breaker protection.
// On primary failure, it automatically falls back to the secondary provider.
// (F5) Prometheus metrics are recorded for every call: latency, token usage, success/failure.
func (c *Client) ChatCompletion(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	start := time.Now()
	provider := c.primary.Name()

	// Cross-replica check: if the fleet-wide failure count for this
	// provider is over the shared threshold, short-circuit directly to
	// fallback without hitting the degraded upstream. See shared_breaker.go.
	if c.sharedBreaker != nil && !c.sharedBreaker.Allow(ctx, provider) {
		c.logger.Warn("shared circuit breaker open, bypassing primary",
			zap.String("primary", provider))
		if c.fallback != nil {
			return c.fallbackChat(ctx, req, start, nil)
		}
		return nil, fmt.Errorf("LLM primary tripped by shared circuit breaker and no fallback configured")
	}

	// Try primary through the local circuit breaker
	result, err := c.breaker.Execute(func() (interface{}, error) {
		return c.primary.ChatCompletion(ctx, req)
	})

	// Record circuit breaker state
	c.recordBreakerState()

	model := req.Model
	if model == "" {
		model = "default"
	}

	if err == nil {
		resp := result.(*ChatResponse)
		duration := time.Since(start).Seconds()
		metrics.LLMRequestTotal.WithLabelValues(provider, model, "success").Inc()
		metrics.LLMRequestDuration.WithLabelValues(provider, model).Observe(duration)
		metrics.LLMTokensUsed.WithLabelValues(provider, "prompt").Add(float64(resp.Usage.PromptTokens))
		metrics.LLMTokensUsed.WithLabelValues(provider, "completion").Add(float64(resp.Usage.CompletionTokens))
		return resp, nil
	}

	// Record the failure to the shared counter so other replicas trip.
	if c.sharedBreaker != nil {
		c.sharedBreaker.RecordFailure(ctx, provider)
	}

	metrics.LLMRequestTotal.WithLabelValues(provider, model, "error").Inc()
	metrics.LLMRequestDuration.WithLabelValues(provider, model).Observe(time.Since(start).Seconds())

	c.logger.Warn("primary LLM failed, attempting fallback",
		zap.String("primary", provider),
		zap.Error(err),
	)

	if c.fallback != nil {
		return c.fallbackChat(ctx, req, start, err)
	}

	return nil, fmt.Errorf("LLM request failed (no fallback configured): %w", err)
}

// fallbackChat runs the secondary provider and records its outcome with
// metrics. primaryErr is the upstream error that triggered the fallback
// (nil if we fell back preemptively via the shared breaker). start is the
// wall-clock start used only for fallback latency measurement.
func (c *Client) fallbackChat(ctx context.Context, req *ChatRequest, _ time.Time, primaryErr error) (*ChatResponse, error) {
	model := req.Model
	if model == "" {
		model = "default"
	}
	fbStart := time.Now()
	fbProvider := c.fallback.Name()
	resp, fallbackErr := c.fallback.ChatCompletion(ctx, req)
	if fallbackErr != nil {
		metrics.LLMRequestTotal.WithLabelValues(fbProvider, model, "error").Inc()
		if primaryErr != nil {
			return nil, fmt.Errorf("both primary and fallback LLM failed: primary=%w, fallback=%v", primaryErr, fallbackErr)
		}
		return nil, fmt.Errorf("fallback LLM failed: %w", fallbackErr)
	}
	metrics.LLMRequestTotal.WithLabelValues(fbProvider, model, "success").Inc()
	metrics.LLMRequestDuration.WithLabelValues(fbProvider, model).Observe(time.Since(fbStart).Seconds())
	metrics.LLMTokensUsed.WithLabelValues(fbProvider, "prompt").Add(float64(resp.Usage.PromptTokens))
	metrics.LLMTokensUsed.WithLabelValues(fbProvider, "completion").Add(float64(resp.Usage.CompletionTokens))
	return resp, nil
}

// recordBreakerState publishes the circuit breaker state as a Prometheus gauge.
func (c *Client) recordBreakerState() {
	state := c.breaker.State()
	var val float64
	switch state {
	case gobreaker.StateClosed:
		val = 0
	case gobreaker.StateHalfOpen:
		val = 1
	case gobreaker.StateOpen:
		val = 2
	}
	metrics.LLMCircuitBreakerState.WithLabelValues(c.primary.Name()).Set(val)
}

// ChatCompletionStream executes a streaming chat completion.
func (c *Client) ChatCompletionStream(ctx context.Context, req *ChatRequest) (<-chan StreamChunk, error) {
	provider := c.primary.Name()

	if c.sharedBreaker != nil && !c.sharedBreaker.Allow(ctx, provider) {
		c.logger.Warn("shared circuit breaker open for streaming, bypassing primary",
			zap.String("primary", provider))
		if c.fallback != nil {
			return c.fallback.ChatCompletionStream(ctx, req)
		}
		return nil, fmt.Errorf("LLM primary tripped by shared circuit breaker (streaming)")
	}

	// Try primary through circuit breaker
	result, err := c.breaker.Execute(func() (interface{}, error) {
		ch, streamErr := c.primary.ChatCompletionStream(ctx, req)
		if streamErr != nil {
			return nil, streamErr
		}
		return ch, nil
	})

	if err == nil {
		return result.(<-chan StreamChunk), nil
	}

	c.logger.Warn("primary LLM stream failed, attempting fallback",
		zap.String("primary", c.primary.Name()),
		zap.Error(err),
	)

	if c.fallback != nil {
		return c.fallback.ChatCompletionStream(ctx, req)
	}

	return nil, fmt.Errorf("LLM stream request failed (no fallback configured): %w", err)
}

// EstimateTokens delegates to FastEstimate for backward compatibility.
func EstimateTokens(text string) int {
	return FastEstimate(text)
}

// BuildToolDefinitions converts tool definitions to the format expected by the LLM.
func BuildToolDefinitions(tools []models.ToolDefinition) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(tools))
	for _, t := range tools {
		var params interface{}
		if len(t.Parameters) > 0 {
			_ = json.Unmarshal(t.Parameters, &params)
		}
		result = append(result, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  params,
			},
		})
	}
	return result
}

// retryWithBackoff executes fn with exponential backoff retry logic.
func retryWithBackoff(ctx context.Context, maxRetries int, fn func() error) error {
	var err error
	for i := 0; i <= maxRetries; i++ {
		if err = fn(); err == nil {
			return nil
		}
		if i < maxRetries {
			backoff := time.Duration(1<<uint(i)) * 500 * time.Millisecond
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}
	}
	return err
}
