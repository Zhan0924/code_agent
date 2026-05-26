// activities.go — Temporal activity 实现。
//
// ============================================================================
//
//	设 计 原 理
//
// ============================================================================
//
// 【workflow vs activity 的边界】
//
//	Temporal 铁律：**workflow 代码必须是确定性的**（任意重放产生相同结果）。
//	有副作用的事情一律丢进 activity。本文件里的 activity：
//	  · ParseIntentActivity    — 关键词分类 intent（deploy/conversation）
//	  · SecurityCheckActivity  — 正则匹配，本身是确定性，但放这里便于独立重试
//	  · ExecuteTaskActivity    — 调 orchestrator 真正跑任务
//
// 【Activity 失败后的重试】
//
//	workflow 层配置 RetryPolicy（见 workflows.go 的 activityOpts）：
//	指数退避、最多 3 次。所以 activity 内部**不要自己再重试**，让 Temporal
//	统一管理——重试次数、延迟、超时都可视化。
//
// 【为什么把 orchestrator 塞进 Activities 结构】
//
//	activity 本质是函数，但 Temporal 支持绑定到 receiver 上的方法 activity。
//	Activities struct 持有 orchestrator / securityCfg / logger，方法里直接
//	引用，比全局变量干净。main.go 里 worker.RegisterActivity(activities) 把
//	所有方法一次性注册。
//
// ============================================================================
package temporal

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/agent/code_agent/internal/config"
	"github.com/agent/code_agent/internal/llm"
	"github.com/agent/code_agent/internal/models"
	"github.com/agent/code_agent/internal/orchestrator"
	"go.uber.org/zap"
)

// Activities holds the dependencies needed by Temporal activities.
type Activities struct {
	Orchestrator *orchestrator.Orchestrator
	SecurityCfg  *config.SecurityConfig
	LLMClient    LLMClient // Optional: for LLM-based intent classification
	Logger       *zap.Logger
	// [OPT-20] Pre-compiled regex patterns to avoid re-compilation per call.
	sensitivePatterns []*regexp.Regexp
}

// LLMClient is a minimal interface for LLM calls needed by activities.
type LLMClient interface {
	ChatCompletion(ctx context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error)
}

// NewActivities creates Activities with pre-compiled sensitive patterns.
func NewActivities(orch *orchestrator.Orchestrator, secCfg *config.SecurityConfig, logger *zap.Logger) *Activities {
	a := &Activities{
		Orchestrator: orch,
		SecurityCfg:  secCfg,
		Logger:       logger,
	}
	for _, pattern := range secCfg.SensitivePatterns {
		re, err := regexp.Compile("(?i)" + pattern)
		if err != nil {
			logger.Warn("invalid sensitive pattern, skipping",
				zap.String("pattern", pattern), zap.Error(err))
			continue
		}
		a.sensitivePatterns = append(a.sensitivePatterns, re)
	}
	return a
}

// ParseIntentActivity classifies the user's intent using LLM (if available) or keyword matching.
func (a *Activities) ParseIntentActivity(ctx context.Context, input AgentTaskInput) (*IntentResult, error) {
	a.Logger.Info("parsing intent", zap.String("task_id", input.TaskID))

	var intent models.TaskIntent
	var err error

	// Try LLM classification first if available
	if a.LLMClient != nil {
		intent, err = a.classifyIntentWithLLM(ctx, input.UserMessage)
		if err != nil {
			a.Logger.Warn("LLM intent classification failed, falling back to keywords",
				zap.String("task_id", input.TaskID),
				zap.Error(err))
			intent = classifyIntent(input.UserMessage)
		}
	} else {
		// Fallback: keyword matching
		intent = classifyIntent(input.UserMessage)
	}

	a.Logger.Info("intent classified",
		zap.String("task_id", input.TaskID),
		zap.String("intent", string(intent)),
	)

	return &IntentResult{
		Intent: intent,
	}, nil
}

func (a *Activities) classifyIntentWithLLM(ctx context.Context, message string) (models.TaskIntent, error) {
	prompt := fmt.Sprintf(`Classify the user's intent as either "deploy" or "conversation".

User message: %s

Respond with ONLY one word: "deploy" or "conversation".`, message)

	resp, err := a.LLMClient.ChatCompletion(ctx, &llm.ChatRequest{
		Messages:  []models.Message{{Role: models.RoleUser, Content: prompt}},
		MaxTokens: 10,
	})
	if err != nil {
		return "", err
	}

	content := strings.ToLower(strings.TrimSpace(resp.Content))
	if strings.Contains(content, "deploy") {
		return models.IntentDeploy, nil
	}
	return models.IntentConversation, nil
}

// deployKeywords are phrases that indicate a deployment intent.
var deployKeywords = []string{
	"deploy", "部署", "发布", "上线", "rollout", "release to prod",
	"push to production", "ship it", "go live",
}

// classifyIntent determines the task intent from the user message.
func classifyIntent(message string) models.TaskIntent {
	lower := strings.ToLower(message)

	for _, kw := range deployKeywords {
		if strings.Contains(lower, kw) {
			return models.IntentDeploy
		}
	}

	return models.IntentConversation
}

// SecurityCheckActivity checks if the user message contains sensitive operations.
func (a *Activities) SecurityCheckActivity(ctx context.Context, input SecurityCheckInput) (*SecurityCheckResult, error) {
	a.Logger.Info("checking security", zap.String("task_id", input.TaskID))

	result := &SecurityCheckResult{
		RequiresApproval: false,
		RiskLevel:        "low",
	}

	// [OPT-20] Check against pre-compiled sensitive patterns
	for _, re := range a.sensitivePatterns {
		if re.MatchString(input.UserMessage) {
			result.RequiresApproval = true
			result.RiskLevel = "high"
			result.Reason = "Matched sensitive pattern: " + re.String()
			break
		}
	}

	// Check deployment intents
	if input.Intent == models.IntentDeploy {
		result.RequiresApproval = true
		result.RiskLevel = "critical"
		result.Reason = "Deployment operation requires approval"
	}

	return result, nil
}

// ExecuteTaskActivity executes the actual task via the orchestrator.
func (a *Activities) ExecuteTaskActivity(ctx context.Context, input ExecuteTaskInput) (*ExecutionResult, error) {
	a.Logger.Info("executing task",
		zap.String("task_id", input.TaskID),
		zap.String("intent", string(input.Intent)),
	)

	// Use SkipHITL context to prevent recursive approval loops —
	// the workflow already obtained approval before reaching this activity.
	execCtx := orchestrator.ContextWithSkipHITL(ctx)
	resp, err := a.Orchestrator.ProcessMessage(execCtx, input.SessionID, input.UserMessage)
	if err != nil {
		return nil, err
	}

	return &ExecutionResult{
		Output: resp.Message,
	}, nil
}

// Ensure Activities methods match the function signatures used in workflows.
// These are the function references used in workflow.ExecuteActivity.
var (
	ParseIntentActivity   = (*Activities).ParseIntentActivity
	SecurityCheckActivity = (*Activities).SecurityCheckActivity
	ExecuteTaskActivity   = (*Activities).ExecuteTaskActivity
)
