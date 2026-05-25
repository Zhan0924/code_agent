package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/agent/code_agent/internal/models"
)

// ExecutorType 定义工具执行器类型
type ExecutorType string

const (
	ExecutorTypeWebhook ExecutorType = "webhook" // HTTP POST 到外部 URL
	ExecutorTypeInline  ExecutorType = "inline"  // 执行内联代码（沙箱）
)

// DynamicToolConfig 定义动态工具配置
type DynamicToolConfig struct {
	Name           string          `json:"name" binding:"required"`
	Description    string          `json:"description" binding:"required"`
	Parameters     json.RawMessage `json:"parameters" binding:"required"` // JSON Schema
	ExecutorType   ExecutorType    `json:"executor_type" binding:"required"`
	ExecutorConfig json.RawMessage `json:"executor_config" binding:"required"`
	RiskLevel      int             `json:"risk_level"` // 0=safe, 1=moderate, 2=high
	TTL            *time.Duration  `json:"ttl,omitempty"` // 可选过期时间
	CreatedAt      time.Time       `json:"created_at"`
}

// WebhookExecutorConfig 定义 webhook 执行器配置
type WebhookExecutorConfig struct {
	URL     string            `json:"url" binding:"required"`
	Method  string            `json:"method"` // 默认 POST
	Headers map[string]string `json:"headers,omitempty"`
	Timeout time.Duration     `json:"timeout"` // 默认 30s
}

// InlineExecutorConfig 定义内联代码执行器配置
type InlineExecutorConfig struct {
	Language string        `json:"language"` // "bash", "python", "javascript"
	Code     string        `json:"code" binding:"required"`
	Timeout  time.Duration `json:"timeout"` // 默认 10s
}

// dynamicTool 实现 Tool 接口
type dynamicTool struct {
	config   DynamicToolConfig
	executor func(context.Context, json.RawMessage) (*models.ToolResult, error)
}

func (dt *dynamicTool) Definition() models.ToolDefinition {
	return models.ToolDefinition{
		Name:        dt.config.Name,
		Description: dt.config.Description,
		Parameters:  dt.config.Parameters,
		Source:      "dynamic",
		RiskLevel:   dt.config.RiskLevel,
	}
}

func (dt *dynamicTool) Execute(ctx context.Context, args json.RawMessage) (*models.ToolResult, error) {
	return dt.executor(ctx, args)
}

// NewDynamicTool 创建动态工具
func NewDynamicTool(config DynamicToolConfig) (Tool, error) {
	// 验证 parameters 是有效的 JSON Schema
	var schema map[string]interface{}
	if err := json.Unmarshal(config.Parameters, &schema); err != nil {
		return nil, fmt.Errorf("invalid parameters schema: %w", err)
	}

	var executor func(context.Context, json.RawMessage) (*models.ToolResult, error)

	switch config.ExecutorType {
	case ExecutorTypeWebhook:
		var webhookCfg WebhookExecutorConfig
		if err := json.Unmarshal(config.ExecutorConfig, &webhookCfg); err != nil {
			return nil, fmt.Errorf("invalid webhook config: %w", err)
		}
		executor = createWebhookExecutor(webhookCfg)

	case ExecutorTypeInline:
		var inlineCfg InlineExecutorConfig
		if err := json.Unmarshal(config.ExecutorConfig, &inlineCfg); err != nil {
			return nil, fmt.Errorf("invalid inline config: %w", err)
		}
		executor = createInlineExecutor(inlineCfg)

	default:
		return nil, fmt.Errorf("unsupported executor type: %s", config.ExecutorType)
	}

	return &dynamicTool{
		config:   config,
		executor: executor,
	}, nil
}

func createWebhookExecutor(cfg WebhookExecutorConfig) func(context.Context, json.RawMessage) (*models.ToolResult, error) {
	if cfg.Method == "" {
		cfg.Method = "POST"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}

	return func(ctx context.Context, args json.RawMessage) (*models.ToolResult, error) {
		ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, cfg.Method, cfg.URL, bytes.NewReader(args))
		if err != nil {
			return &models.ToolResult{Content: err.Error(), IsError: true}, nil
		}

		req.Header.Set("Content-Type", "application/json")
		for k, v := range cfg.Headers {
			req.Header.Set(k, v)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return &models.ToolResult{Content: err.Error(), IsError: true}, nil
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 400 {
			return &models.ToolResult{
				Content: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body)),
				IsError: true,
			}, nil
		}

		return &models.ToolResult{Content: string(body), IsError: false}, nil
	}
}

func createInlineExecutor(cfg InlineExecutorConfig) func(context.Context, json.RawMessage) (*models.ToolResult, error) {
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}

	return func(ctx context.Context, args json.RawMessage) (*models.ToolResult, error) {
		// TODO: 集成 sandbox.Manager 执行代码
		// 当前简化实现：返回占位符
		return &models.ToolResult{
			Content: fmt.Sprintf("Inline executor not yet implemented (language=%s)", cfg.Language),
			IsError: true,
		}, nil
	}
}
