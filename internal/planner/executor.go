// executor.go — DAG 计划执行器。
//
// ============================================================================
//
//	设 计 原 理
//
// ============================================================================
//
// 【ReAct vs Planner 两种模式】
//
//	ReAct（orchestrator.reactLoop）：每步问 LLM "下一步干啥"，一边想一边做。
//	  适合探索性任务（用户意图模糊、需要试错）。
//	Planner（本包）：先让 LLM 产出完整 DAG（节点=操作，边=依赖），
//	  Executor 按拓扑顺序批量执行，彼此独立的节点可并发。
//	  适合批量任务（"给这个项目的所有 .go 文件加版权头"、"把 X 库升到 Y 版本"）。
//
// 【DAG 表达】
//
//	一个 Plan 是 []Step。每个 Step 有 ID、Tool、Args、DependsOn []string。
//	Executor:
//	  1. 拓扑排序（kahn 算法），找出所有无依赖的 step；
//	  2. 并发执行这一层，WaitGroup 同步；
//	  3. 把完成的 step 从依赖图摘掉，继续找下一层；
//	  4. 任一 step 失败 → 保存 checkpoint，剩余 step 取消（不执行）。
//
// 【checkpoint & resume】
//
//	每完成一个 step 把 state 写到 Redis（或 Postgres）。如果 Executor 进程
//	被 kill，重启后从 checkpoint 继续，已完成的不再跑。和 Temporal 类似
//	但更轻——不需要独立 workflow server。
//
// 【何时用 Planner 而不是 ReAct】
//
//	orchestrator 里的 planner_bridge.go 做 intent 判断：
//	  · "分析+重构/迁移/批量"类 → Planner
//	  · 其他 → ReAct
//
// ============================================================================
package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// ─── Step Executor ──────────────────────────────────────────────────────────

// StepExecutor is a function that executes a single plan step and returns its output.
type StepExecutor func(ctx context.Context, step Step) (output string, err error)

// Executor runs a Plan's steps in topological order with optional parallelism.
//
// Within a single DAG level, independent steps run concurrently. The
// MaxParallelism knob bounds that fan-out so that a wide plan (e.g. "search
// these 50 symbols in parallel") doesn't flood the downstream sandbox / LLM
// with requests. A value of 0 or less means "unlimited" and preserves the
// historical behaviour for tests that rely on it.
type Executor struct {
	planner        *Planner
	stepExec       StepExecutor
	maxRevision    int // maximum number of plan revisions on failure
	maxParallelism int // upper bound on concurrent step executions per level
	logger         *zap.Logger
}

// NewExecutor creates a plan executor with the given step handler.
// The default MaxParallelism is 4 — a safe value for mixed workloads where
// each step might open a sandbox container or issue an LLM call.
func NewExecutor(planner *Planner, stepExec StepExecutor, logger *zap.Logger) *Executor {
	return &Executor{
		planner:        planner,
		stepExec:       stepExec,
		maxRevision:    2,
		maxParallelism: 4,
		logger:         logger,
	}
}

// SetMaxParallelism configures the upper bound on concurrent step execution.
// Values ≤ 0 are treated as "unlimited".
func (e *Executor) SetMaxParallelism(n int) {
	e.maxParallelism = n
}

// ExecutionResult holds the final state after executing a plan.
type ExecutionResult struct {
	Plan          *Plan             `json:"plan"`
	Success       bool              `json:"success"`
	StepOutputs   map[string]string `json:"step_outputs"`
	FailedStepIDs []string          `json:"failed_step_ids,omitempty"`
	Summary       string            `json:"summary"`
}

// Execute runs a plan to completion, revising on failure up to maxRevision times.
func (e *Executor) Execute(ctx context.Context, plan *Plan) (*ExecutionResult, error) {
	result := &ExecutionResult{
		Plan:        plan,
		StepOutputs: make(map[string]string),
	}

	for revision := 0; revision <= e.maxRevision; revision++ {
		e.logger.Info("executing plan",
			zap.String("plan_id", plan.ID),
			zap.Int("version", plan.Version),
			zap.Int("revision_attempt", revision),
		)

		// Get topological order
		levels, err := TopologicalSort(plan.Steps)
		if err != nil {
			return nil, fmt.Errorf("topological sort failed: %w", err)
		}

		// Execute level by level
		var failures []Step
		for _, level := range levels {
			levelFailures := e.executeLevel(ctx, plan, level, result.StepOutputs)
			failures = append(failures, levelFailures...)
		}

		if len(failures) == 0 {
			result.Success = true
			result.Summary = fmt.Sprintf("Plan completed successfully: %d steps executed", len(plan.Steps))
			return result, nil
		}

		// Collect failed step IDs
		for _, f := range failures {
			result.FailedStepIDs = append(result.FailedStepIDs, f.ID)
		}

		// If we can still revise, ask the planner
		if revision < e.maxRevision {
			failureSummary := buildFailureSummary(failures)
			revised, err := e.planner.RevisePlan(ctx, plan, failureSummary)
			if err != nil {
				e.logger.Warn("plan revision failed, stopping", zap.Error(err))
				break
			}
			plan = revised
			result.Plan = plan
		}
	}

	result.Success = false
	result.Summary = fmt.Sprintf("Plan failed after %d revision(s): %d steps failed",
		e.maxRevision, len(result.FailedStepIDs))
	return result, nil
}

// executeLevel runs all steps at one DAG level, potentially in parallel.
func (e *Executor) executeLevel(ctx context.Context, plan *Plan, level []Step, outputs map[string]string) []Step {
	// Skip already-completed steps (from prior revision)
	var toRun []Step
	for _, s := range level {
		if s.Status == StepCompleted || s.Status == StepSkipped {
			continue
		}
		// Check if all dependencies completed
		allDepsMet := true
		for _, dep := range s.DependsOn {
			depStep := findStep(plan, dep)
			if depStep == nil || depStep.Status != StepCompleted {
				allDepsMet = false
				break
			}
		}
		if !allDepsMet {
			updateStepStatus(plan, s.ID, StepSkipped, "", "dependency not met")
			continue
		}
		toRun = append(toRun, s)
	}

	if len(toRun) == 0 {
		return nil
	}

	// Run steps in parallel within the same level, bounded by maxParallelism.
	var mu sync.Mutex
	var failures []Step
	var wg sync.WaitGroup

	// Semaphore: bounded only if maxParallelism > 0.
	var sem chan struct{}
	if e.maxParallelism > 0 {
		sem = make(chan struct{}, e.maxParallelism)
	}

	for _, s := range toRun {
		wg.Add(1)
		go func(step Step) {
			defer wg.Done()
			if sem != nil {
				sem <- struct{}{}
				defer func() { <-sem }()
			}
			updateStepStatus(plan, step.ID, StepRunning, "", "")

			start := time.Now()
			output, err := e.stepExec(ctx, step)
			duration := time.Since(start)

			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				e.logger.Warn("step failed",
					zap.String("step_id", step.ID),
					zap.String("action", step.Action),
					zap.Error(err),
				)
				updateStepStatus(plan, step.ID, StepFailed, "", err.Error())
				failures = append(failures, step)
			} else {
				updateStepStatus(plan, step.ID, StepCompleted, output, "")
				outputs[step.ID] = output
			}

			// Update duration
			for i := range plan.Steps {
				if plan.Steps[i].ID == step.ID {
					plan.Steps[i].Duration = duration
				}
			}
		}(s)
	}

	wg.Wait()
	return failures
}

// ─── Helpers ────────────────────────────────────────────────────────────────

func findStep(plan *Plan, id string) *Step {
	for i := range plan.Steps {
		if plan.Steps[i].ID == id {
			return &plan.Steps[i]
		}
	}
	return nil
}

func updateStepStatus(plan *Plan, id string, status StepStatus, output, errMsg string) {
	for i := range plan.Steps {
		if plan.Steps[i].ID == id {
			plan.Steps[i].Status = status
			if output != "" {
				plan.Steps[i].Output = output
			}
			if errMsg != "" {
				plan.Steps[i].Error = errMsg
			}
			return
		}
	}
}

func buildFailureSummary(failures []Step) string {
	var sb strings.Builder
	for _, f := range failures {
		sb.WriteString(fmt.Sprintf("- Step %q (%s): %s\n", f.ID, f.Action, f.Error))
	}
	return sb.String()
}

// ─── Plan Complexity Estimation ─────────────────────────────────────────────

// EstimateComplexity analyzes a user message to decide if it needs a plan.
// Returns a complexity score; higher scores indicate more complex tasks.
func EstimateComplexity(userMessage string) int {
	score := 0
	lower := strings.ToLower(userMessage)

	// Length-based scoring
	words := len(strings.Fields(userMessage))
	if words > 50 {
		score += 2
	}
	if words > 100 {
		score += 2
	}

	// Multi-file indicators
	multiFileKeywords := []string{
		"multiple files", "several files", "across files", "refactor",
		"多个文件", "重构", "批量", "all files", "每个文件",
		"entire codebase", "whole project", "all modules",
	}
	for _, kw := range multiFileKeywords {
		if strings.Contains(lower, kw) {
			score += 3
			break
		}
	}

	// Multi-step indicators
	multiStepKeywords := []string{
		"then", "after that", "next", "finally", "step by step",
		"first", "second", "third", "然后", "接着", "最后", "首先",
		"and also", "additionally", "并且", "同时",
	}
	stepCount := 0
	for _, kw := range multiStepKeywords {
		if strings.Contains(lower, kw) {
			stepCount++
		}
	}
	if stepCount > 0 {
		score += 2
	}
	if stepCount >= 3 {
		score += 2
	}

	// Implementation indicators
	implKeywords := []string{
		"implement", "create", "build", "develop", "add feature",
		"实现", "创建", "开发", "添加功能", "新增",
		"integrate", "migrate", "upgrade",
	}
	for _, kw := range implKeywords {
		if strings.Contains(lower, kw) {
			score += 2
			break
		}
	}

	// Test indicators
	testKeywords := []string{
		"test", "verify", "ensure", "validate",
		"测试", "验证", "确保",
	}
	for _, kw := range testKeywords {
		if strings.Contains(lower, kw) {
			score++
			break
		}
	}

	// Deployment/infrastructure indicators (inherently multi-step)
	deployKeywords := []string{
		"deploy", "deployment", "部署", "上线",
		"docker", "kubernetes", "k8s", "terraform",
		"ci/cd", "pipeline", "release",
		"production", "staging", "生产环境",
	}
	deployHits := 0
	for _, kw := range deployKeywords {
		if strings.Contains(lower, kw) {
			deployHits++
		}
	}
	if deployHits > 0 {
		score += 3
	}
	if deployHits >= 2 {
		score += 2
	}

	// Database/migration indicators
	dbKeywords := []string{
		"database", "migration", "schema", "数据库", "迁移",
		"sql", "postgres", "mysql", "mongodb",
	}
	for _, kw := range dbKeywords {
		if strings.Contains(lower, kw) {
			score += 2
			break
		}
	}

	// API/integration indicators
	apiKeywords := []string{
		"api", "endpoint", "integration", "webhook",
		"接口", "集成", "对接",
	}
	for _, kw := range apiKeywords {
		if strings.Contains(lower, kw) {
			score += 2
			break
		}
	}

	// Action verb count (multiple actions = multi-step)
	actionVerbs := []string{
		"add", "remove", "update", "modify", "change", "fix",
		"create", "delete", "refactor", "optimize", "improve",
		"implement", "integrate", "migrate", "deploy", "test",
		"添加", "删除", "更新", "修改", "修复", "优化",
	}
	verbCount := 0
	for _, verb := range actionVerbs {
		if strings.Contains(lower, verb) {
			verbCount++
		}
	}
	if verbCount >= 3 {
		score += 2
	} else if verbCount >= 2 {
		score++
	}

	// File path mentions (e.g., "internal/api/handler.go")
	pathPattern := regexp.MustCompile(`[a-z_]+/[a-z_]+`)
	pathMatches := pathPattern.FindAllString(lower, -1)
	if len(pathMatches) >= 3 {
		score += 2
	} else if len(pathMatches) >= 2 {
		score++
	}

	// Conditional/branching indicators (adds complexity)
	conditionalKeywords := []string{
		"if", "unless", "except", "when", "in case",
		"如果", "除非", "当", "假如",
	}
	for _, kw := range conditionalKeywords {
		if strings.Contains(lower, kw) {
			score++
			break
		}
	}

	return score
}

// NeedsPlanning returns true if the complexity score exceeds the planning threshold.
func NeedsPlanning(userMessage string) bool {
	return EstimateComplexity(userMessage) >= 5
}

// ─── Plan Serialization ─────────────────────────────────────────────────────

// ToJSON serializes a plan for persistence or transmission.
func (p *Plan) ToJSON() ([]byte, error) {
	return json.MarshalIndent(p, "", "  ")
}

// PlanFromJSON deserializes a plan from JSON.
func PlanFromJSON(data []byte) (*Plan, error) {
	var plan Plan
	if err := json.Unmarshal(data, &plan); err != nil {
		return nil, err
	}
	return &plan, nil
}
