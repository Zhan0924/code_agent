// router.go implements intelligent model routing that selects the best LLM
// based on task complexity, intent type, and token budget.
//
// ============================================================================
//
//	设 计 原 理
//
// ============================================================================
//
// 【为什么要做模型路由（Model Router）？】
//
//	LLM 是 Agent 最大的成本中心和延迟来源。但不同任务对模型的要求悬殊：
//	  · "帮我总结一下这段日志"        → 小模型（GPT-3.5 / Claude Haiku）就够
//	  · "重构这 5 个服务的鉴权逻辑"   → 必须用旗舰模型（GPT-4 / Claude Sonnet）
//	  · "从 10k 行日志中找异常"       → 大 context 模型（Claude Sonnet 200k）
//	一刀切用最强模型 = 烧钱；一刀切用弱模型 = 效果差。
//
// 【路由决策维度】
//
//  1. Intent 类型      : qa / edit / plan / review / summarize 等
//
//  2. 输入 token 长度  : 小 → 快速模型；大 → 长 context 模型
//
//  3. 复杂度启发式     : 是否涉及多文件 / 含代码 / 含错误栈
//
//  4. 成本预算         : 租户配额 / 月度上限
//
//  5. 可用性           : 主模型熔断时自动降级到备用
//
//     ┌────────────┐  vote  ┌─────────────────────────┐
//     │ Intent 分类 ├───────▶│                         │
//     └────────────┘        │   Router.Select()        │
//     ┌────────────┐        │   （加权决策）            │─▶ 选中的 Provider
//     │ Token 估算 ├───────▶│                         │
//     └────────────┘        └─────────────────────────┘
//     ┌────────────┐        ▲
//     │ 熔断器状态 ├────────┘
//     └────────────┘
//
// 【熔断与降级（Circuit Breaker）】
//
//	每个 Provider 维护独立的熔断器（滑动窗口失败率）：
//	  · Closed  : 正常；
//	  · Open    : 失败率 > 50% 触发，60s 内全部 fast-fail 到备用模型；
//	  · Half-Open: 超时后放 1 个请求试探；成功则回 Closed。
//	指数退避重试：1s → 2s → 4s → 最多 3 次。
//
// 【与 Skill Registry / Orchestrator 的边界】
//
//	· Registry 负责"工具有什么"；
//	· 本包 Router 负责"用哪个大脑想"；
//	· Orchestrator 整合两者得出 "何时用 → 选谁 → 用什么想"。
//
// ============================================================================
package llm

import (
	"context"
	"strings"
	"sync"

	"go.uber.org/zap"
)

// ─── Model Tier ─────────────────────────────────────────────────────────────

// ModelTier classifies models by capability/cost.
type ModelTier string

const (
	TierHeavy  ModelTier = "heavy"  // Claude Opus, GPT-4o — complex reasoning, multi-file edits
	TierMedium ModelTier = "medium" // Claude Sonnet, GPT-4o-mini — single-file edits, Q&A
	TierLight  ModelTier = "light"  // Haiku, GPT-3.5, local — summaries, intent parsing, simple tasks
)

// ModelRoute specifies which model to use for a request.
type ModelRoute struct {
	Tier      ModelTier `json:"tier"`
	Model     string    `json:"model"`
	Reason    string    `json:"reason"`
	MaxTokens int       `json:"max_tokens"`
}

// ─── Router ─────────────────────────────────────────────────────────────────

// RouterConfig defines model assignments per tier.
type RouterConfig struct {
	HeavyModel  string `json:"heavy_model"`  // e.g. "claude-sonnet-4-20250514"
	MediumModel string `json:"medium_model"` // e.g. "claude-sonnet-4-20250514"
	LightModel  string `json:"light_model"`  // e.g. "claude-3-5-haiku-20241022"

	// Token limits per tier
	HeavyMaxTokens  int `json:"heavy_max_tokens"`
	MediumMaxTokens int `json:"medium_max_tokens"`
	LightMaxTokens  int `json:"light_max_tokens"`
}

// Router dynamically selects the appropriate model based on task attributes.
type Router struct {
	config RouterConfig
	logger *zap.Logger
	mu     sync.RWMutex

	// Stats for observability
	routeCount map[ModelTier]int64
}

// NewRouter creates a new model router.
func NewRouter(config RouterConfig, logger *zap.Logger) *Router {
	// Apply defaults
	if config.HeavyMaxTokens == 0 {
		config.HeavyMaxTokens = 16384
	}
	if config.MediumMaxTokens == 0 {
		config.MediumMaxTokens = 8192
	}
	if config.LightMaxTokens == 0 {
		config.LightMaxTokens = 4096
	}

	return &Router{
		config:     config,
		logger:     logger,
		routeCount: make(map[ModelTier]int64),
	}
}

// Route determines the best model for the given task parameters.
func (r *Router) Route(intent string, complexityScore int, messageCount int) ModelRoute {
	route := r.classify(intent, complexityScore, messageCount)

	r.mu.Lock()
	r.routeCount[route.Tier]++
	r.mu.Unlock()

	r.logger.Debug("model routed",
		zap.String("tier", string(route.Tier)),
		zap.String("model", route.Model),
		zap.String("reason", route.Reason),
		zap.String("intent", intent),
		zap.Int("complexity", complexityScore),
	)

	return route
}

// classify performs the actual routing logic.
func (r *Router) classify(intent string, complexityScore int, messageCount int) ModelRoute {
	// Rule 1: High complexity always gets heavy model
	if complexityScore >= 7 {
		return ModelRoute{
			Tier:      TierHeavy,
			Model:     r.config.HeavyModel,
			Reason:    "high complexity score",
			MaxTokens: r.config.HeavyMaxTokens,
		}
	}

	// Rule 2: Deploy and diagnose tasks get heavy model (safety critical)
	if intent == "deploy" || intent == "diagnose" {
		return ModelRoute{
			Tier:      TierHeavy,
			Model:     r.config.HeavyModel,
			Reason:    "safety-critical intent: " + intent,
			MaxTokens: r.config.HeavyMaxTokens,
		}
	}

	// Rule 3: Code execution with medium+ complexity
	if intent == "code_execute" && complexityScore >= 4 {
		return ModelRoute{
			Tier:      TierHeavy,
			Model:     r.config.HeavyModel,
			Reason:    "complex code execution",
			MaxTokens: r.config.HeavyMaxTokens,
		}
	}

	// Rule 4: Simple Q&A and conversation → light model
	if intent == "conversation" && complexityScore < 3 {
		return ModelRoute{
			Tier:      TierLight,
			Model:     r.config.LightModel,
			Reason:    "simple conversation",
			MaxTokens: r.config.LightMaxTokens,
		}
	}

	// Rule 5: Intent parsing (internal use) → light model
	if intent == "_intent_parse" || intent == "_summarize" {
		return ModelRoute{
			Tier:      TierLight,
			Model:     r.config.LightModel,
			Reason:    "internal utility task",
			MaxTokens: r.config.LightMaxTokens,
		}
	}

	// Rule 6: Code query with low complexity → medium model
	if intent == "code_query" && complexityScore < 5 {
		return ModelRoute{
			Tier:      TierMedium,
			Model:     r.config.MediumModel,
			Reason:    "standard code query",
			MaxTokens: r.config.MediumMaxTokens,
		}
	}

	// Rule 7: Long conversations (many messages) → might need heavy for context
	if messageCount > 20 {
		return ModelRoute{
			Tier:      TierHeavy,
			Model:     r.config.HeavyModel,
			Reason:    "long conversation context",
			MaxTokens: r.config.HeavyMaxTokens,
		}
	}

	// Default: medium model
	return ModelRoute{
		Tier:      TierMedium,
		Model:     r.config.MediumModel,
		Reason:    "default routing",
		MaxTokens: r.config.MediumMaxTokens,
	}
}

// ApplyRoute modifies a ChatRequest to use the routed model.
func (r *Router) ApplyRoute(req *ChatRequest, route ModelRoute) {
	if route.Model != "" {
		req.Model = route.Model
	}
	if route.MaxTokens > 0 && (req.MaxTokens == 0 || req.MaxTokens > route.MaxTokens) {
		req.MaxTokens = route.MaxTokens
	}
}

// Stats returns routing statistics.
func (r *Router) Stats() map[ModelTier]int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make(map[ModelTier]int64, len(r.routeCount))
	for k, v := range r.routeCount {
		result[k] = v
	}
	return result
}

// ─── Complexity Estimation (shared with planner) ────────────────────────────

// QuickComplexity does a fast complexity estimate from the raw message.
// This is lighter than planner.EstimateComplexity but uses same heuristics.
func QuickComplexity(msg string) int {
	score := 0
	lower := strings.ToLower(msg)
	words := len(strings.Fields(msg))

	if words > 50 {
		score += 2
	}
	if words > 100 {
		score += 2
	}

	complexKeywords := []string{
		"refactor", "multiple files", "implement", "create", "build",
		"重构", "多个文件", "实现", "创建", "开发",
		"then", "after that", "finally", "step by step",
		"然后", "接着", "最后", "首先",
	}
	for _, kw := range complexKeywords {
		if strings.Contains(lower, kw) {
			score += 2
		}
	}

	return score
}

// RouteForMessage is a convenience method that estimates complexity and routes.
func (r *Router) RouteForMessage(_ context.Context, intent, message string, messageCount int) ModelRoute {
	complexity := QuickComplexity(message)
	return r.Route(intent, complexity, messageCount)
}
