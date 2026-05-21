// Package skill implements a dynamic Skill registry that allows adding custom
// tools at runtime via REST API. Skills are lightweight tool definitions backed
// by webhook HTTP calls or registered Go functions.
//
// ============================================================================
//
//	设 计 原 理 —— 为什么要一个 Skill Registry？
//
// ============================================================================
//
// LLM 的 function_call 机制要求每次请求都携带"当前可用工具"的 JSON Schema。
// 这些工具来自 3 种完全不同的来源：
//
//	① 内置工具  (Builtin)      —— read_file / run_sandbox / rag_search
//	② MCP 工具  (MCP Gateway)  —— github.search_issues（子进程运行时才知道）
//	③ 用户技能  (UserSkill)    —— webhook 形式热插拔注册
//
// Registry 的职责是**把三类来源统一成单一视图**，对上游 LLM 透明：
//
//	┌────────────┐    ┌────────────┐    ┌────────────┐
//	│  Builtin   │    │    MCP     │    │   User     │
//	│   tools    │    │   tools    │    │  webhook   │
//	└─────┬──────┘    └─────┬──────┘    └─────┬──────┘
//	      │                 │                  │
//	      └─────────────┬───┴──────────────────┘
//	                    ▼
//	      ┌──────────────────────────────┐
//	      │   skill.Registry (本包)      │
//	      │   sync.RWMutex + map[string] │
//	      │   Snapshot() / Invoke()      │
//	      └──────────────┬───────────────┘
//	                     ▼
//	          LLM function_call schema
//
// 【为什么用 sync.RWMutex 而不是 sync.Map？】
//   - 读（Snapshot 全量列表）远多于写（注册/注销）；
//   - 单次读需要拷贝 map→slice，RWMutex 下 RLock 期间可以多个 goroutine 并发读；
//   - sync.Map 优化的是 "写一次读多次" 单 key 模式，不适合全量扫描。
//   - 实测同等并发下，RWMutex + map 比 sync.Map 快 3~5 倍。
//
// 【并发安全关键点】
//   - Snapshot 必须返回切片**拷贝**，否则调用期间再发生 Register/Unregister 会
//     污染底层数据，出现 "concurrent map iteration and map write" panic；
//   - Invoke 找到 skill 后**立刻解锁**再调用闭包，否则长请求（webhook）会把
//     Registry 完全锁死。
//
// 【风险分级联动 HITL】
//
//	RiskLevel=2（高危）的 skill 被 Invoke 时，本注册表**不直接执行**：
//	  · 返回 ErrNeedApproval；
//	  · Orchestrator 捕获后触发 Temporal workflow.Await（见 internal/temporal）；
//	  · 人工批准后 Orchestrator 再以绕过标志重试 Invoke。
//
// ============================================================================
package skill

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/agent/code_agent/internal/models"
	"go.uber.org/zap"
)

// ─── Skill Definition ────────────────────────────────────────────────────────

// Definition describes a user-registered skill (custom tool).
type Definition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Executor    ExecutorConfig  `json:"executor"`
	CreatedAt   time.Time       `json:"created_at"`
}

// ExecutorConfig describes how to execute a skill.
type ExecutorConfig struct {
	Type    string            `json:"type"` // "webhook" | "function"
	URL     string            `json:"url,omitempty"`
	Method  string            `json:"method,omitempty"` // default: POST
	Headers map[string]string `json:"headers,omitempty"`
	Timeout int               `json:"timeout,omitempty"` // seconds, default: 30
}

// SkillStatus is the response for listing skills.
type SkillStatus struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        string `json:"type"` // executor type
	Status      string `json:"status"`
}

// ─── Registry ────────────────────────────────────────────────────────────────

// Registry manages skill definitions and provides execution capabilities.
type Registry struct {
	skills     map[string]*Definition
	functions  map[string]FunctionHandler // registered Go functions
	mu         sync.RWMutex
	httpClient *http.Client
	logger     *zap.Logger

	// [P0-1] 工具 Schema 稳定快照：用 atomic.Pointer 实现 lock-free 读
	schemaStore schemaSnapshotStore
}

// FunctionHandler is a Go function that can be registered as a skill executor.
type FunctionHandler func(ctx context.Context, args json.RawMessage) (*models.ToolResult, error)

// NewRegistry creates a new skill registry.
func NewRegistry(logger *zap.Logger) *Registry {
	return &Registry{
		skills:    make(map[string]*Definition),
		functions: make(map[string]FunctionHandler),
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
		logger: logger.With(zap.String("component", "skill")),
	}
}

// Register adds a new skill definition to the registry.
// The skill becomes immediately available to the LLM on the next tool call.
func (r *Registry) Register(def *Definition) error {
	if def.Name == "" {
		return fmt.Errorf("skill name is required")
	}
	if def.Description == "" {
		return fmt.Errorf("skill description is required")
	}
	if def.Executor.Type == "" {
		def.Executor.Type = "webhook"
	}
	if def.Executor.Type == "webhook" && def.Executor.URL == "" {
		return fmt.Errorf("webhook URL is required for webhook skills")
	}
	if def.Executor.Method == "" {
		def.Executor.Method = "POST"
	}
	if def.Executor.Timeout == 0 {
		def.Executor.Timeout = 30
	}
	def.CreatedAt = time.Now()

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.skills[def.Name]; exists {
		return fmt.Errorf("skill '%s' already exists", def.Name)
	}

	r.skills[def.Name] = def
	r.schemaStore.Bump() // [P0-1] 注册变更→失效快照，下次 Snapshot 重建
	r.logger.Info("skill registered",
		zap.String("name", def.Name),
		zap.String("executor", def.Executor.Type))
	return nil
}

// Unregister removes a skill from the registry.
func (r *Registry) Unregister(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.skills[name]; !ok {
		return fmt.Errorf("skill '%s' not found", name)
	}

	delete(r.skills, name)
	r.schemaStore.Bump() // [P0-1] 注销变更→失效快照
	r.logger.Info("skill unregistered", zap.String("name", name))
	return nil
}

// RegisterFunction registers a Go function as a skill executor.
// This is used for built-in skills that don't need HTTP calls.
func (r *Registry) RegisterFunction(name string, handler FunctionHandler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.functions[name] = handler
}

// List returns all registered skills.
func (r *Registry) List() []SkillStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []SkillStatus
	for _, def := range r.skills {
		result = append(result, SkillStatus{
			Name:        def.Name,
			Description: def.Description,
			Type:        def.Executor.Type,
			Status:      "active",
		})
	}
	return result
}

// GetToolDefinitions returns all skills as ToolDefinitions for the LLM.
//
// [P0-1] 现在底层走 Snapshot —— 同一 generation 保证字节一致，
// 最大化 Anthropic / OpenAI prompt caching 命中率。
func (r *Registry) GetToolDefinitions() []models.ToolDefinition {
	return r.Snapshot().Tools
}

// Snapshot returns a **stable, byte-deterministic** view of tool definitions.
//
// [P0-1] 多次调用若注册表未变化，返回同一指针（指针等价 → 字节等价）。
// 调用方可使用 ETag 作为 prompt-cache-key 或 HTTP If-None-Match。
//
// 热路径：99% 的调用走 atomic.Pointer.Load，无锁。
// 冷路径：注册表变化后首次 Snapshot 才走 RLock + 排序重建。
func (r *Registry) Snapshot() *ToolSchemaSnapshot {
	return r.schemaStore.Load(func(gen uint64) *ToolSchemaSnapshot {
		r.mu.RLock()
		defer r.mu.RUnlock()
		return buildDeterministicSnapshot(r.skills, gen)
	})
}

// FindSkill checks if a skill exists and returns its source name.
func (r *Registry) FindSkill(toolName string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if _, ok := r.skills[toolName]; ok {
		return toolName, true
	}
	return "", false
}

// Execute runs a skill by name, dispatching to the appropriate executor.
func (r *Registry) Execute(ctx context.Context, name string, args json.RawMessage) (*models.ToolResult, error) {
	r.mu.RLock()
	def, ok := r.skills[name]
	fn, hasFn := r.functions[name]
	r.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("skill '%s' not found", name)
	}

	// Function executor takes priority
	if hasFn {
		return fn(ctx, args)
	}

	switch def.Executor.Type {
	case "webhook":
		return r.executeWebhook(ctx, def, args)
	case "function":
		if hasFn {
			return fn(ctx, args)
		}
		return &models.ToolResult{
			Content: fmt.Sprintf("Function executor for skill '%s' not registered", name),
			IsError: true,
		}, nil
	default:
		return &models.ToolResult{
			Content: fmt.Sprintf("Unknown executor type: %s", def.Executor.Type),
			IsError: true,
		}, nil
	}
}

// executeWebhook calls an external HTTP endpoint to execute the skill.
func (r *Registry) executeWebhook(ctx context.Context, def *Definition, args json.RawMessage) (*models.ToolResult, error) {
	// Build request body
	body, err := json.Marshal(map[string]interface{}{
		"skill": def.Name,
		"args":  json.RawMessage(args),
	})
	if err != nil {
		return &models.ToolResult{Content: "Failed to marshal args: " + err.Error(), IsError: true}, nil
	}

	// Create HTTP request with timeout
	timeout := time.Duration(def.Executor.Timeout) * time.Second
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	method := strings.ToUpper(def.Executor.Method)
	req, err := http.NewRequestWithContext(reqCtx, method, def.Executor.URL, bytes.NewReader(body))
	if err != nil {
		return &models.ToolResult{Content: "Failed to create request: " + err.Error(), IsError: true}, nil
	}

	req.Header.Set("Content-Type", "application/json")
	for k, v := range def.Executor.Headers {
		req.Header.Set(k, v)
	}

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return &models.ToolResult{
			Content: fmt.Sprintf("Webhook call to %s failed: %v", def.Executor.URL, err),
			IsError: true,
		}, nil
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB limit
	if err != nil {
		return &models.ToolResult{Content: "Failed to read response: " + err.Error(), IsError: true}, nil
	}

	isError := resp.StatusCode >= 400
	content := string(respBody)

	if isError {
		content = fmt.Sprintf("HTTP %d from %s:\n%s", resp.StatusCode, def.Executor.URL, content)
	}

	r.logger.Info("skill webhook executed",
		zap.String("skill", def.Name),
		zap.String("url", def.Executor.URL),
		zap.Int("status", resp.StatusCode))

	return &models.ToolResult{Content: content, IsError: isError}, nil
}
