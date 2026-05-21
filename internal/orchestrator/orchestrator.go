// Package orchestrator implements the core Agent brain - the FSM-based task orchestrator
// with ReAct multi-step reasoning, intent parsing, and Human-in-the-Loop (HITL) support.
package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/agent/code_agent/internal/config"
	agentctx "github.com/agent/code_agent/internal/context"
	"github.com/agent/code_agent/internal/llm"
	"github.com/agent/code_agent/internal/mcp"
	"github.com/agent/code_agent/internal/metrics"
	"github.com/agent/code_agent/internal/models"
	"github.com/agent/code_agent/internal/rag"
	"github.com/agent/code_agent/internal/sandbox"
	"github.com/agent/code_agent/internal/session"
	"github.com/agent/code_agent/internal/store"
	"github.com/agent/code_agent/internal/toollearn"
	"github.com/agent/code_agent/internal/workspace"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// [P2-10] Adaptive step limits based on task complexity.
// getMaxSteps returns the step limit for a given intent.
func getMaxSteps(intent models.TaskIntent) int {
	switch intent {
	case models.IntentCodeQuery:
		return 10 // simple Q&A: search + answer
	case models.IntentCodeExecute:
		return 20 // single script: write + run + fix
	case models.IntentDiagnose:
		return 25 // diagnose: search + run diagnostics
	case models.IntentMCPCall:
		return 15 // external tool: call + format
	case models.IntentDeploy:
		return 20 // deploy: validate + execute
	default:
		return 50 // conversation/coding: full Document-First workflow
	}
}

// Orchestrator is the central brain of the Agent system.
type Orchestrator struct {
	llmClient      *llm.Client
	sessionMgr     *session.Manager
	ragEngine      *rag.Engine
	sandboxMgr     *sandbox.Manager
	mcpGateway     *mcp.Gateway
	promptBuilder  *agentctx.PromptBuilder
	securityCfg    *config.SecurityConfig
	sensitiveRules []*regexp.Regexp
	workspaceMgr   *workspace.Manager
	skillRegistry  interface {
		GetToolDefinitions() []models.ToolDefinition
		FindSkill(string) (string, bool)
		Execute(context.Context, string, json.RawMessage) (*models.ToolResult, error)
	}
	store  *store.Store // PostgreSQL persistence (nil = disabled)
	logger *zap.Logger

	// [P0] Precision edit engine with unique-match, backup, and lint
	editEngine *EditEngine
	// [P0] Auto-test runner for TDD self-verification loop
	autoTestRunner *AutoTestRunner
	// [P0-3] Speculative tool result cache for idempotent read tools
	toolCache *SpeculativeToolCache
	// [P1-D] Project rules loader for workspace-specific LLM guidance
	ruleLoader *RuleLoader

	// [P2-D] Tool learning: feedback collector + advisor
	toolCollector *toollearn.Collector
	toolAdvisor   *toollearn.Advisor

	// HITL: mutex-protected map of taskID → approval channel
	approvalMu sync.RWMutex
	approvalCh map[string]chan models.ApprovalResponse

	// [P2-E1] Interrupt: per-session interrupt signal channel
	interruptMu sync.RWMutex
	interruptCh map[string]chan InterruptSignal

	// [P2-E2] Per-session tool transactions for rollback on interrupt
	txMu sync.RWMutex
	txMap map[string]*ToolTransaction

	// [OPT-10] Intent cache: avoid redundant LLM calls for repeated intents
	intentCacheMu sync.RWMutex
	intentCache   map[string]intentCacheEntry

	// Optional Planner bridge — nil unless AttachPlanner has been called.
	// See planner_bridge.go for the behaviour.
	planner *plannerComponents

	// Optional Temporal client for durable HITL workflows.
	// When set, suspendForApproval uses Temporal instead of in-process channels.
	temporalClient TemporalClient
}

// intentCacheEntry stores a cached intent classification result.
type intentCacheEntry struct {
	intent    models.TaskIntent
	expiresAt time.Time
}

// NewOrchestrator creates a new orchestrator with all subsystem dependencies.
func NewOrchestrator(
	llmClient *llm.Client,
	sessionMgr *session.Manager,
	ragEngine *rag.Engine,
	sandboxMgr *sandbox.Manager,
	mcpGateway *mcp.Gateway,
	securityCfg *config.SecurityConfig,
	logger *zap.Logger,
	pgStore ...*store.Store,
) *Orchestrator {
	var rules []*regexp.Regexp
	for _, pattern := range securityCfg.SensitivePatterns {
		re, err := regexp.Compile("(?i)" + pattern)
		if err != nil {
			logger.Warn("bad sensitive pattern", zap.String("pattern", pattern), zap.Error(err))
			continue
		}
		rules = append(rules, re)
	}

	pb := agentctx.NewPromptBuilder(&agentctx.PromptBuilderConfig{
		SystemPrompt: `You are a highly capable code intelligence agent. You can:
1. Answer technical questions about code and architecture
2. Execute code in a sandboxed environment (use execute_code tool)
3. Search and retrieve relevant code snippets (use search_code tool)
4. Interact with external tools (GitHub, Jira, etc.) via MCP
5. Help diagnose and troubleshoot production issues
Always use tools when they would produce better answers. After receiving tool results, synthesize them into a clear answer.`,
		MaxTotalTokens: 128000, // ⬆ 8K→128K: Claude Opus has 200K context; use it
	}, logger)

	orch := &Orchestrator{
		llmClient:      llmClient,
		sessionMgr:     sessionMgr,
		ragEngine:      ragEngine,
		sandboxMgr:     sandboxMgr,
		mcpGateway:     mcpGateway,
		promptBuilder:  pb,
		securityCfg:    securityCfg,
		sensitiveRules: rules,
		logger:         logger.With(zap.String("component", "orchestrator")),
		approvalCh:     make(map[string]chan models.ApprovalResponse),
		interruptCh:    make(map[string]chan InterruptSignal),
		txMap:          make(map[string]*ToolTransaction),
		intentCache:    make(map[string]intentCacheEntry),
		toolCache:      NewSpeculativeToolCache(0, logger),
		ruleLoader:     NewRuleLoader(logger),
	}

	// [P2-D] Initialize tool learning subsystem
	collector := toollearn.NewCollector(nil, logger)
	extractor := toollearn.NewExtractor(collector, logger)
	orch.toolCollector = collector
	orch.toolAdvisor = toollearn.NewAdvisor(extractor, logger)
	if len(pgStore) > 0 && pgStore[0] != nil {
		orch.store = pgStore[0]
	}
	return orch
}

// ═══════════════════════════════════════════════════════════════════════════════
// F1: ReAct Multi-Step Reasoning Loop
// ═══════════════════════════════════════════════════════════════════════════════

// ProcessMessage is the main entry point. It implements a ReAct loop:
//
//	while not done (max 10 iterations):
//	    LLM(messages + tool_results) → response
//	    if response.has_tool_calls:
//	        execute each tool → append results to messages
//	    else:
//	        return response.content  // LLM decided it's done
func (o *Orchestrator) ProcessMessage(ctx context.Context, sessionID, userMessage string) (*models.ChatResponse, error) {
	task := &models.Task{
		ID:        uuid.New().String(),
		SessionID: sessionID,
		UserInput: userMessage,
		State:     models.TaskStatePending,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	o.logger.Info("processing message", zap.String("task_id", task.ID), zap.String("session_id", sessionID))

	// Persist task to DB
	o.persistTaskCreate(ctx, task)

	// [Fix-1] Auto-continue: detect "continue" message and inject context recovery
	lowerMsg := strings.ToLower(strings.TrimSpace(userMessage))
	if lowerMsg == "continue" || lowerMsg == "继续" || lowerMsg == "go on" {
		userMessage = o.buildContinuationPrompt(userMessage)
	}

	// Store user message
	if err := o.sessionMgr.AddMessage(ctx, sessionID, models.Message{
		Role: models.RoleUser, Content: userMessage,
	}); err != nil {
		return nil, fmt.Errorf("store user message: %w", err)
	}

	// Parse intent
	task.State = models.TaskStatePlanning
	intent, err := o.parseIntent(ctx, sessionID, userMessage)
	if err != nil {
		return nil, fmt.Errorf("intent parsing: %w", err)
	}
	task.Intent = intent
	o.logger.Info("intent parsed", zap.String("task_id", task.ID), zap.String("intent", string(intent)))

	// Security check — if sensitive, suspend for HITL approval (F3)
	// Skip when called from Temporal ExecuteTaskActivity (already approved).
	if !skipHITL(ctx) && (o.containsSensitiveContent(userMessage) || intent == models.IntentDeploy) {
		o.persistTaskState(ctx, task.ID, models.TaskStateSuspended)
		return o.suspendForApproval(ctx, task)
	}

	// [P1-C] Planner path: complex tasks get a DAG plan instead of flat ReAct
	if resp, used, err := o.MaybeUsePlanner(ctx, task); used {
		if err != nil {
			task.State = models.TaskStateFailed
			return nil, err
		}
		task.State = models.TaskStateCompleted
		now := time.Now()
		task.CompletedAt = &now
		o.persistTaskState(ctx, task.ID, models.TaskStateCompleted)
		_ = o.sessionMgr.AddMessage(ctx, sessionID, models.Message{
			Role: models.RoleAssistant, Content: resp.Message,
		})
		return resp, nil
	}

	// Execute with ReAct loop
	task.State = models.TaskStateExecuting
	o.persistTaskState(ctx, task.ID, models.TaskStateExecuting)
	response, err := o.reactLoop(ctx, task)
	if err != nil {
		task.State = models.TaskStateFailed
		return nil, err
	}

	// Store assistant response
	task.State = models.TaskStateCompleted
	now := time.Now()
	task.CompletedAt = &now
	o.persistTaskState(ctx, task.ID, models.TaskStateCompleted)
	_ = o.sessionMgr.AddMessage(ctx, sessionID, models.Message{
		Role: models.RoleAssistant, Content: response,
	})

	return &models.ChatResponse{
		SessionID: sessionID, TaskID: task.ID,
		Message: response, State: models.TaskStateCompleted,
	}, nil
}

// ═══════════════════════════════════════════════════════════════════════════════
// Store Persistence Helpers
// ═══════════════════════════════════════════════════════════════════════════════

// persistTaskCreate writes a new task record to PostgreSQL (best-effort).
func (o *Orchestrator) persistTaskCreate(ctx context.Context, task *models.Task) {
	if o.store == nil {
		return
	}
	record := &store.TaskRecord{
		ID:        task.ID,
		SessionID: task.SessionID,
		Intent:    string(task.Intent),
		State:     string(task.State),
		UserInput: task.UserInput,
		CreatedAt: task.CreatedAt,
		UpdatedAt: task.UpdatedAt,
	}
	if err := o.store.CreateTask(ctx, record); err != nil {
		o.logger.Warn("failed to persist task", zap.String("task_id", task.ID), zap.Error(err))
	}
}

// persistTaskState updates the task state in PostgreSQL (best-effort).
func (o *Orchestrator) persistTaskState(ctx context.Context, taskID string, state models.TaskState) {
	if o.store == nil {
		return
	}
	if err := o.store.UpdateTaskState(ctx, taskID, string(state), nil); err != nil {
		o.logger.Warn("failed to update task state", zap.String("task_id", taskID), zap.Error(err))
	}
}

// persistAudit writes an audit log entry (best-effort).
func (o *Orchestrator) persistAudit(ctx context.Context, taskID, userID, action, riskLevel string) {
	if o.store == nil {
		return
	}
	record := &store.AuditRecord{
		TaskID:    taskID,
		UserID:    userID,
		Action:    action,
		RiskLevel: riskLevel,
	}
	if err := o.store.InsertAuditLog(ctx, record); err != nil {
		o.logger.Warn("failed to persist audit log", zap.String("task_id", taskID), zap.Error(err))
	}
}

// reactLoop implements the core ReAct (Reason + Act) cycle.
// The LLM can call tools, receive results, then reason further — up to maxReActSteps.
func (o *Orchestrator) reactLoop(ctx context.Context, task *models.Task) (string, error) {
	// [P2-E1] Register interrupt channel for this session
	interruptCh := o.registerInterrupt(task.SessionID)
	defer o.unregisterInterrupt(task.SessionID)

	// [P2-E2] Register tool transaction for rollback support
	tx := NewToolTransaction(task.SessionID, o.logger)
	o.txMu.Lock()
	o.txMap[task.SessionID] = tx
	o.txMu.Unlock()
	defer func() {
		o.txMu.Lock()
		delete(o.txMap, task.SessionID)
		o.txMu.Unlock()
	}()

	// [OPT-19] Use PromptBuilder for KV-cache-friendly, pruned prompt assembly.
	sess, _ := o.sessionMgr.Get(ctx, task.SessionID)

	// Retrieve RAG code chunks for code-related intents
	var codeChunks []models.CodeChunk
	var relevanceScores []float64
	if task.Intent == models.IntentCodeQuery || task.Intent == models.IntentDiagnose {
		if o.ragEngine != nil {
			results, err := o.ragEngine.Retrieve(ctx, task.UserInput, nil)
			if err == nil && len(results) > 0 {
				metrics.RAGChunksReturned.Observe(float64(len(results)))
				for _, r := range results {
					codeChunks = append(codeChunks, r.Chunk)
					relevanceScores = append(relevanceScores, r.Score)
				}
			}
		}
	}

	// Update PromptBuilder's system prompt based on intent
	o.promptBuilder.UpdateLongTermMemory(func() string {
		if sess != nil {
			return sess.Summary
		}
		return ""
	}())

	messages := o.promptBuilder.BuildPrompt(sess, codeChunks, relevanceScores, task.UserInput)

	// Override system prompt with intent-specific prompt
	if len(messages) > 0 && messages[0].Role == models.RoleSystem {
		messages[0] = o.buildSystemMessage(task.Intent)
	}

	tools := o.getAvailableTools()

	// [OPT-7] Token budget: track cumulative tokens to prevent exceeding model context window.
	// ⬆ 7K→128K: Claude Opus supports 200K input; reserve 16K for output + 56K headroom.
	const maxContextTokens = 128000

	// [Fix-4] Initialize failure tracker for fix loop detection
	failTracker := &consecutiveFailureTracker{}

	maxSteps := getMaxSteps(task.Intent)
	for step := 0; step < maxSteps; step++ {
		o.logger.Debug("ReAct step", zap.String("task_id", task.ID), zap.Int("step", step+1))

		// [Fix-2] Inject reflection checkpoint every 10 steps
		if reflection := o.reflectionCheckpoint(step, maxSteps); reflection != nil {
			o.logger.Info("injecting reflection checkpoint", zap.Int("step", step+1))
			messages = append(messages, *reflection)
		}

		// Check token budget before calling LLM
		totalTokens := 0
		for _, m := range messages {
			totalTokens += llm.EstimateTokens(m.Content)
		}
		if totalTokens > maxContextTokens {
			o.logger.Warn("ReAct token budget exceeded, pruning oldest messages",
				zap.String("task_id", task.ID), zap.Int("tokens", totalTokens))
			messages = o.pruneMessages(messages, maxContextTokens)
		}

		// [P0-2] LLM call with retry — recover from transient API failures
		var resp *llm.ChatResponse
		var llmErr error
		for attempt := 0; attempt < 3; attempt++ {
			resp, llmErr = o.llmClient.ChatCompletion(ctx, &llm.ChatRequest{
				Messages: messages, Tools: tools,
			})
			if llmErr == nil {
				break
			}
			o.logger.Warn("LLM call failed, retrying",
				zap.String("task_id", task.ID), zap.Int("step", step+1),
				zap.Int("attempt", attempt+1), zap.Error(llmErr))
			if attempt < 2 {
				// Exponential backoff: 2s, 4s
				select {
				case <-time.After(time.Duration(2<<attempt) * time.Second):
				case <-ctx.Done():
					return "", fmt.Errorf("context cancelled during LLM retry: %w", ctx.Err())
				}
			}
		}
		if llmErr != nil {
			// All retries exhausted — save progress and return partial result
			o.saveProgressForContinuation(ctx, task)
			return "", fmt.Errorf("LLM call failed at step %d after 3 retries: %w\n\n"+
				"⚠️ Your workspace files are intact. Send 'continue' in the same session to resume.", step+1, llmErr)
		}

		// If no tool calls → LLM is done reasoning, return the final answer
		if len(resp.ToolCalls) == 0 {
			return resp.Content, nil
		}

		// Append assistant message with tool calls to conversation
		assistantMsg := models.Message{
			Role:      models.RoleAssistant,
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		}
		messages = append(messages, assistantMsg)

		// Execute each tool and append results as tool-role messages
		var editedFilePaths []string // [P0] Track files modified for auto-test

		for _, tc := range resp.ToolCalls {
			// [P2-E1] Check for interrupt signal between tool calls
			if action, interrupted := o.checkInterrupt(ctx, interruptCh, task); interrupted {
				if action.cancel {
					// [P2-E2] Rollback dirty files on cancel
					if tx.HasDirtyFiles() {
						rolled, _ := tx.Rollback()
						o.logger.Info("rolled back on interrupt", zap.Int("files", rolled))
					}
					return action.response, nil
				}
				if action.redirect {
					tx.Clear()
					task.UserInput = action.newMessage
					return o.reactLoop(ctx, task)
				}
			}

			o.logger.Info("tool call", zap.String("task_id", task.ID),
				zap.String("tool", tc.Name), zap.Int("step", step+1))

			result, execErr := o.executeTool(ctx, tc)
			content := result.Content
			if execErr != nil {
				content = fmt.Sprintf("Error: %v", execErr)
			}

			// [P0] Track edited file paths for auto-test
			if (tc.Name == "edit_file" || tc.Name == "write_file" || tc.Name == "patch_file") && execErr == nil && !result.IsError {
				var pathReq struct {
					Path string `json:"path"`
				}
				if json.Unmarshal(tc.Args, &pathReq) == nil && pathReq.Path != "" {
					editedFilePaths = append(editedFilePaths, pathReq.Path)
				}
			}

			// [OPT-7] Smart truncation: keep HEAD + TAIL to preserve error details
			if llm.EstimateTokens(content) > 8000 {
				runes := []rune(content)
				if len(runes) > 32000 {
					headSize := 8000  // first 8K chars: test names, setup info
					tailSize := 12000 // last 12K chars: error messages, FAIL details
					content = string(runes[:headSize]) +
						"\n\n... [middle truncated — " + fmt.Sprintf("%d", len(runes)-headSize-tailSize) + " chars omitted] ...\n\n" +
						string(runes[len(runes)-tailSize:])
				}
			}

			// Tool result message (fed back to LLM for next reasoning step)
			messages = append(messages, models.Message{
				Role:       models.RoleTool,
				Content:    content,
				ToolCallID: tc.ID,
			})

			// [Fix-4] Track consecutive failures and inject "step back" prompt if stuck
			isErr := (execErr != nil) || (result != nil && result.IsError) || strings.Contains(content, "❌ Command FAILED")
			if failTracker.track(tc.Name, isErr) {
				o.logger.Warn("fix loop detected", zap.String("tool", tc.Name), zap.Int("failures", failTracker.failCount))
				msg := failTracker.stepBackMessage()
				messages = append(messages, msg)
			}
		}

		// [P0] Auto-test: after file modifications, run related tests and inject results
		if len(editedFilePaths) > 0 {
			if testResult := o.RunAutoTestAfterEdit(ctx, editedFilePaths); testResult != nil {
				autoTestMsg := testResult.FormatForLLM()
				if autoTestMsg != "" {
					o.logger.Info("auto-test result injected",
						zap.Strings("files", editedFilePaths),
						zap.Bool("passed", testResult.Passed))
					messages = append(messages, models.Message{
						Role:    models.RoleSystem,
						Content: autoTestMsg,
					})
				}
			}
		}
	}

	// Save progress for continuation — write .progress.json to workspace
	o.saveProgressForContinuation(ctx, task)

	return "⚠️ I reached the maximum reasoning steps (50). Your workspace files and .plan.md are intact.\n\n" +
		"**To continue from where I left off**, send a follow-up message saying `continue` in the same session. " +
		"I will read .plan.md and .progress.json to resume the remaining steps.\n\n" +
		"Here's what I completed so far based on the tool results above.", nil
}

// saveProgressForContinuation writes a .progress.json to the workspace
// so the agent can resume from where it left off in a follow-up message.
func (o *Orchestrator) saveProgressForContinuation(ctx context.Context, task *models.Task) {
	ws := o.resolveWorkspace("")
	if ws == nil {
		return
	}

	progress := map[string]interface{}{
		"task_id":    task.ID,
		"session_id": task.SessionID,
		"intent":     string(task.Intent),
		"status":     "paused_at_step_limit",
		"message":    "Task paused at 50 step limit. Send 'continue' to resume.",
		"timestamp":  time.Now().Format(time.RFC3339),
	}

	data, err := json.MarshalIndent(progress, "", "  ")
	if err != nil {
		o.logger.Warn("failed to marshal progress", zap.Error(err))
		return
	}

	if err := o.workspaceMgr.WriteFile(ws, ".progress.json", string(data)); err != nil {
		o.logger.Warn("failed to save progress", zap.Error(err))
	} else {
		o.logger.Info("saved progress for continuation", zap.String("task_id", task.ID))
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// [Fix-1] Auto-Continue Context Recovery
// ═══════════════════════════════════════════════════════════════════════════════

// buildContinuationPrompt enriches a simple "continue" message with workspace context.
// It reads .progress.json and .plan.md from the workspace and injects their content
// so the LLM has full context to resume from where it left off.
func (o *Orchestrator) buildContinuationPrompt(originalMsg string) string {
	ws := o.resolveWorkspace("")
	if ws == nil {
		return originalMsg
	}

	var parts []string
	parts = append(parts, "The user wants to continue the previous task. Here is the saved context:\n")

	// Read .progress.json
	if progress, err := o.workspaceMgr.ReadFile(ws, ".progress.json"); err == nil {
		parts = append(parts, "## .progress.json\n```json\n"+progress+"\n```\n")
	}

	// Read .plan.md
	if plan, err := o.workspaceMgr.ReadFile(ws, ".plan.md"); err == nil {
		// Truncate plan if too long (keep first 8K)
		if len(plan) > 8000 {
			plan = plan[:8000] + "\n... [plan truncated, use read_file to see full plan]"
		}
		parts = append(parts, "## .plan.md\n"+plan+"\n")
	}

	// List workspace files for orientation
	if tree := o.workspaceMgr.TreeString(ws); tree != "" {
		if len(tree) > 3000 {
			tree = tree[:3000] + "\n... [tree truncated]"
		}
		parts = append(parts, "## Workspace files\n```\n"+tree+"\n```\n")
	}

	parts = append(parts, "\nPlease continue executing the remaining unchecked steps from .plan.md. "+
		"Start by reading .plan.md to find which steps are already completed (checked), then proceed with the next unchecked step.")

	return strings.Join(parts, "\n")
}

// ═══════════════════════════════════════════════════════════════════════════════
// [Fix-2] Periodic Reflection Checkpoint
// ═══════════════════════════════════════════════════════════════════════════════

// reflectionCheckpoint injects a reflection prompt every N steps to keep the agent on track.
// Returns a message to inject, or nil if no reflection needed at this step.
func (o *Orchestrator) reflectionCheckpoint(step, maxSteps int) *models.Message {
	// Inject reflection every 10 steps, starting from step 10
	if step == 0 || step%10 != 0 {
		return nil
	}

	remaining := maxSteps - step
	return &models.Message{
		Role: models.RoleSystem,
		Content: fmt.Sprintf(
			"[REFLECTION CHECKPOINT — Step %d/%d, %d steps remaining]\n"+
				"Pause and assess your progress:\n"+
				"1. Read .plan.md to check which steps are completed (✅) and which remain (☐)\n"+
				"2. Are you still on track with the plan? If not, adjust your approach.\n"+
				"3. Are there any errors from previous steps that need fixing before continuing?\n"+
				"4. Prioritize: with %d steps remaining, focus on the most critical remaining items.\n"+
				"Continue with the next unchecked step from .plan.md.",
			step, maxSteps, remaining, remaining),
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// [Fix-4] Fix Loop Limiter
// ═══════════════════════════════════════════════════════════════════════════════

// consecutiveFailureTracker tracks consecutive failures of similar tool calls
// to detect and break out of fix loops (e.g., repeatedly failing the same go build).
type consecutiveFailureTracker struct {
	lastFailedTool string
	failCount      int
}

// consecutiveFailureTracker and pruneMessages have been extracted into
// failure_tracker.go and message_pruner.go respectively to keep this file
// focused on the main ReAct loop.

// ═══════════════════════════════════════════════════════════════════════════════
// F3: Complete HITL Approval Flow
// ═══════════════════════════════════════════════════════════════════════════════

// suspendForApproval creates a pending approval and waits for human decision.
// When approved, it continues execution. When rejected, it cancels.
func (o *Orchestrator) suspendForApproval(ctx context.Context, task *models.Task) (*models.ChatResponse, error) {
	task.State = models.TaskStateSuspended

	// Prefer Temporal-backed HITL when available (durable across restarts)
	if o.temporalClient != nil {
		return o.suspendForApprovalTemporal(ctx, task)
	}

	return o.suspendForApprovalInProcess(ctx, task)
}

// suspendForApprovalInProcess uses in-process channels for HITL (non-durable).
func (o *Orchestrator) suspendForApprovalInProcess(ctx context.Context, task *models.Task) (*models.ChatResponse, error) {
	metrics.HITLPendingGauge.Inc()

	// Create approval channel
	ch := make(chan models.ApprovalResponse, 1)
	o.approvalMu.Lock()
	o.approvalCh[task.ID] = ch
	o.approvalMu.Unlock()

	approval := &models.ApprovalRequest{
		TaskID:      task.ID,
		SessionID:   task.SessionID,
		Action:      fmt.Sprintf("Execute %s operation", task.Intent),
		RiskLevel:   "high",
		Details:     task.UserInput,
		RequestedAt: time.Now(),
	}

	// Start a goroutine that waits for approval then continues execution
	go func() {
		defer func() {
			metrics.HITLPendingGauge.Dec()
			o.approvalMu.Lock()
			delete(o.approvalCh, task.ID)
			o.approvalMu.Unlock()
		}()

		// Wait for approval with 30-minute timeout
		waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()

		select {
		case resp := <-ch:
			if resp.Approved {
				metrics.HITLApprovalTotal.WithLabelValues("approved").Inc()
				o.logger.Info("task approved, executing", zap.String("task_id", task.ID))
				result, err := o.reactLoop(waitCtx, task)
				if err != nil {
					o.logger.Error("post-approval execution failed", zap.Error(err))
					return
				}
				_ = o.sessionMgr.AddMessage(waitCtx, task.SessionID, models.Message{
					Role: models.RoleAssistant, Content: result,
				})
			} else {
				metrics.HITLApprovalTotal.WithLabelValues("rejected").Inc()
				o.logger.Info("task rejected", zap.String("task_id", task.ID))
			}
		case <-waitCtx.Done():
			metrics.HITLApprovalTotal.WithLabelValues("timeout").Inc()
			o.logger.Warn("approval timed out", zap.String("task_id", task.ID))
		}
	}()

	return &models.ChatResponse{
		SessionID: task.SessionID, TaskID: task.ID,
		State:    models.TaskStateSuspended,
		Message:  "⚠️ This operation requires approval. Please review and confirm.",
		Approval: approval,
	}, nil
}

// HandleApproval processes an approval/rejection for a suspended task.
func (o *Orchestrator) HandleApproval(ctx context.Context, resp models.ApprovalResponse) (*models.ChatResponse, error) {
	// Temporal path: when available, signal the workflow directly
	if o.temporalClient != nil {
		return o.HandleApprovalTemporal(ctx, resp)
	}

	// In-process fallback
	o.approvalMu.RLock()
	ch, ok := o.approvalCh[resp.TaskID]
	o.approvalMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("no pending approval for task: %s", resp.TaskID)
	}

	// Send approval to the waiting goroutine (non-blocking due to buffer=1)
	select {
	case ch <- resp:
	default:
		return nil, fmt.Errorf("approval already submitted for task: %s", resp.TaskID)
	}

	if !resp.Approved {
		return &models.ChatResponse{
			TaskID: resp.TaskID, Message: "Operation cancelled by user.",
			State: models.TaskStateCancelled,
		}, nil
	}

	return &models.ChatResponse{
		TaskID: resp.TaskID, Message: "Operation approved. Executing in background...",
		State: models.TaskStateExecuting,
	}, nil
}

// ═══════════════════════════════════════════════════════════════════════════════
// F4: Streaming Support
// ═══════════════════════════════════════════════════════════════════════════════

// ProcessMessageStream processes a message with streaming LLM output.
// It returns a channel that emits response chunks in real-time.
func (o *Orchestrator) ProcessMessageStream(ctx context.Context, sessionID, userMessage string) (<-chan llm.StreamChunk, error) {
	// Store user message
	_ = o.sessionMgr.AddMessage(ctx, sessionID, models.Message{
		Role: models.RoleUser, Content: userMessage,
	})

	contextMsgs, _ := o.sessionMgr.GetContextWindow(ctx, sessionID)
	systemMsg := models.Message{
		Role:    models.RoleSystem,
		Content: `You are a highly capable code intelligence agent. Be concise and accurate.`,
	}
	messages := append([]models.Message{systemMsg}, contextMsgs...)

	ch, err := o.llmClient.ChatCompletionStream(ctx, &llm.ChatRequest{
		Messages: messages,
	})
	if err != nil {
		return nil, err
	}

	// Wrap to collect full response for session storage
	outCh := make(chan llm.StreamChunk, 64)
	go func() {
		defer close(outCh)
		var fullContent strings.Builder
		for chunk := range ch {
			if chunk.Content != "" {
				fullContent.WriteString(chunk.Content)
			}
			outCh <- chunk
			if chunk.Done {
				// Store complete response in session
				if fullContent.Len() > 0 {
					_ = o.sessionMgr.AddMessage(ctx, sessionID, models.Message{
						Role: models.RoleAssistant, Content: fullContent.String(),
					})
				}
			}
		}
	}()

	return outCh, nil
}

// ProcessMessageStreamFull processes a message with the full ReAct loop,
// emitting structured ReactStreamEvent for each step (intent, thinking, tool_call, tool_result, message).
// This provides the frontend with complete visibility into the agent's reasoning process.
func (o *Orchestrator) ProcessMessageStreamFull(ctx context.Context, sessionID, userMessage string) (<-chan models.ReactStreamEvent, error) {
	eventCh := make(chan models.ReactStreamEvent, 64)

	// Store user message
	_ = o.sessionMgr.AddMessage(ctx, sessionID, models.Message{
		Role: models.RoleUser, Content: userMessage,
	})

	go func() {
		defer close(eventCh)

		// Register interrupt channel for this session
		interruptCh := o.registerInterrupt(sessionID)
		defer o.unregisterInterrupt(sessionID)

		// 1. Parse intent
		intent, err := o.parseIntent(ctx, sessionID, userMessage)
		if err != nil {
			eventCh <- models.ReactStreamEvent{Type: "error", Content: "Failed to parse intent: " + err.Error()}
			return
		}

		// 2. Create task
		task := &models.Task{
			ID:        uuid.New().String(),
			SessionID: sessionID,
			UserInput: userMessage,
			Intent:    intent,
			State:     models.TaskStatePlanning,
			CreatedAt: time.Now(),
		}

		// Security check — suspend for HITL if sensitive
		if !skipHITL(ctx) && (o.containsSensitiveContent(userMessage) || intent == models.IntentDeploy) {
			task.State = models.TaskStateSuspended
			o.persistTaskCreate(ctx, task)
			o.persistTaskState(ctx, task.ID, models.TaskStateSuspended)

			var wfID string
			if o.temporalClient != nil {
				wfID, _ = o.temporalClient.StartHITLWorkflow(ctx, task.ID, sessionID, userMessage)
			}
			_ = wfID

			eventCh <- models.ReactStreamEvent{
				Type:    "approval_request",
				TaskID:  task.ID,
				Content: fmt.Sprintf("⚠️ This operation requires approval. Risk: high. Action: %s", userMessage),
			}
			eventCh <- models.ReactStreamEvent{Type: "done", TaskID: task.ID}
			return
		}

		maxSteps := getMaxSteps(intent)

		// Emit intent event (show absoluteMaxSteps as the total capacity)
		const absoluteMaxSteps = 200
		eventCh <- models.ReactStreamEvent{
			Type: "step_start", Intent: string(intent),
			TaskID: task.ID, MaxSteps: absoluteMaxSteps,
		}

		// 3. Run a simplified ReAct loop with event emission
		sess, _ := o.sessionMgr.Get(ctx, sessionID)

		// RAG retrieval
		var codeChunks []models.CodeChunk
		var relevanceScores []float64
		if task.Intent == models.IntentCodeQuery || task.Intent == models.IntentDiagnose {
			if o.ragEngine != nil {
				results, ragErr := o.ragEngine.Retrieve(ctx, task.UserInput, nil)
				if ragErr == nil && len(results) > 0 {
					for _, r := range results {
						codeChunks = append(codeChunks, r.Chunk)
						relevanceScores = append(relevanceScores, r.Score)
					}
					eventCh <- models.ReactStreamEvent{
						Type:    "rag_context",
						Content: fmt.Sprintf("Retrieved %d code chunks from RAG", len(results)),
					}
				}
			}
		}

		o.promptBuilder.UpdateLongTermMemory(func() string {
			if sess != nil {
				return sess.Summary
			}
			return ""
		}())

		messages := o.promptBuilder.BuildPrompt(sess, codeChunks, relevanceScores, task.UserInput)
		if len(messages) > 0 && messages[0].Role == models.RoleSystem {
			messages[0] = o.buildSystemMessage(task.Intent)
		}

		tools := o.getAvailableTools()

		// Auto-continue: use an outer loop with absolute hard limit to prevent infinite loops.
		// Each "batch" runs up to maxSteps. When a batch exhausts, we inject a continuation
		// prompt and start a new batch, up to absoluteMaxSteps total.
		globalStep := 0

		for globalStep < absoluteMaxSteps {
			batchLimit := maxSteps
			if globalStep+batchLimit > absoluteMaxSteps {
				batchLimit = absoluteMaxSteps - globalStep
			}

			for batchStep := 0; batchStep < batchLimit; batchStep++ {
				globalStep++

				select {
				case <-ctx.Done():
					eventCh <- models.ReactStreamEvent{Type: "error", Content: "Request cancelled"}
					return
				case sig := <-interruptCh:
					eventCh <- models.ReactStreamEvent{
						Type:    "message",
						Content: fmt.Sprintf("Task interrupted (%s).", sig.Type),
					}
					eventCh <- models.ReactStreamEvent{Type: "done", TaskID: task.ID}
					return
				default:
				}

				eventCh <- models.ReactStreamEvent{
					Type: "step_start", Step: globalStep, MaxSteps: absoluteMaxSteps, TaskID: task.ID,
				}

				// LLM call
				var resp *llm.ChatResponse
				var llmErr error
				for attempt := 0; attempt < 3; attempt++ {
					resp, llmErr = o.llmClient.ChatCompletion(ctx, &llm.ChatRequest{
						Messages: messages, Tools: tools,
					})
					if llmErr == nil {
						break
					}
					if attempt < 2 {
						select {
						case <-time.After(time.Duration(2<<attempt) * time.Second):
						case <-ctx.Done():
							eventCh <- models.ReactStreamEvent{Type: "error", Content: "Timeout during LLM retry"}
							return
						}
					}
				}
				if llmErr != nil {
					eventCh <- models.ReactStreamEvent{Type: "error", Content: "LLM call failed: " + llmErr.Error()}
					return
				}

				// Emit thinking/content if present
				if resp.Content != "" {
					if len(resp.ToolCalls) > 0 {
						// Content with tool calls = thinking
						eventCh <- models.ReactStreamEvent{Type: "thinking", Step: globalStep, Content: resp.Content}
					} else {
						// No tool calls = final answer
						eventCh <- models.ReactStreamEvent{Type: "message", Step: globalStep, Content: resp.Content}
						// Store in session
						_ = o.sessionMgr.AddMessage(ctx, sessionID, models.Message{
							Role: models.RoleAssistant, Content: resp.Content,
						})
						eventCh <- models.ReactStreamEvent{Type: "done", TaskID: task.ID}
						return
					}
				}

				// No tool calls = done
				if len(resp.ToolCalls) == 0 {
					eventCh <- models.ReactStreamEvent{Type: "done", TaskID: task.ID}
					return
				}

				// Append assistant message
				assistantMsg := models.Message{
					Role: models.RoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls,
				}
				messages = append(messages, assistantMsg)

				// Execute tools
				for _, tc := range resp.ToolCalls {
					argsStr := string(tc.Args)
					eventCh <- models.ReactStreamEvent{
						Type: "tool_call", Step: globalStep,
						ToolName: tc.Name, ToolArgs: argsStr, ToolCallID: tc.ID,
					}

					result, execErr := o.executeTool(ctx, tc)
					content := result.Content
					if execErr != nil {
						content = fmt.Sprintf("Error: %v", execErr)
					}

					// Truncate large output
					if llm.EstimateTokens(content) > 8000 {
						runes := []rune(content)
						if len(runes) > 32000 {
							headSize := 8000
							tailSize := 12000
							content = string(runes[:headSize]) +
								"\n\n... [truncated " + fmt.Sprintf("%d", len(runes)-headSize-tailSize) + " chars] ...\n\n" +
								string(runes[len(runes)-tailSize:])
						}
					}

					isErr := (execErr != nil) || (result != nil && result.IsError)
					eventCh <- models.ReactStreamEvent{
						Type: "tool_result", Step: globalStep,
						ToolName: tc.Name, ToolCallID: tc.ID,
						Content: content, IsError: isErr,
					}

					messages = append(messages, models.Message{
						Role: models.RoleTool, Content: content, ToolCallID: tc.ID,
					})
				}
			}

			// Batch exhausted — auto-continue by injecting a continuation message
			o.logger.Info("auto-continue: batch exhausted, injecting continuation prompt",
				zap.Int("global_step", globalStep), zap.Int("absolute_max", absoluteMaxSteps))

			eventCh <- models.ReactStreamEvent{
				Type:    "thinking",
				Step:    globalStep,
				Content: fmt.Sprintf("Auto-continuing... (%d/%d steps used)", globalStep, absoluteMaxSteps),
			}

			// Inject a system-level continuation prompt to keep the LLM focused
			messages = append(messages, models.Message{
				Role: models.RoleUser,
				Content: "You have used " + fmt.Sprintf("%d", globalStep) + " steps so far. " +
					"Continue executing the remaining tasks. Focus on completing the current work efficiently. " +
					"If you have completed all tasks, provide a final summary response without any tool calls.",
			})
		}

		// Absolute hard limit reached
		eventCh <- models.ReactStreamEvent{
			Type:    "message",
			Content: fmt.Sprintf("⚠️ Reached absolute maximum of %d reasoning steps. Task progress has been saved.", absoluteMaxSteps),
		}
		eventCh <- models.ReactStreamEvent{Type: "done", TaskID: task.ID}
	}()

	return eventCh, nil
}

// ═══════════════════════════════════════════════════════════════════════════════
// Intent Parsing
// ═══════════════════════════════════════════════════════════════════════════════

// intentCacheTTL controls how long a cached intent is valid (per session+message).
const intentCacheTTL = 2 * time.Minute

// intentCacheMaxEntries bounds the in-memory intent cache across all sessions.
// A long-lived process with many distinct (session, message) pairs would
// otherwise grow unboundedly — lightweight pruning is triggered on insert.
const intentCacheMaxEntries = 2048

// intentCacheKey derives the cache key from both sessionID and the user
// message. The previous implementation keyed on sessionID alone, which meant a
// first message classified as `code_query` caused a later "deploy to prod"
// in the same session to hit the cache and silently skip the HITL-gated
// `deploy` intent path. Message content is included via SHA-256 to avoid
// unbounded key size and to canonicalize whitespace at the hashed level.
func intentCacheKey(sessionID, userMessage string) string {
	h := sha256.Sum256([]byte(userMessage))
	return sessionID + ":" + hex.EncodeToString(h[:16])
}

func (o *Orchestrator) parseIntent(ctx context.Context, sessionID, userMessage string) (models.TaskIntent, error) {
	// [OPT-10] Check intent cache first to avoid redundant LLM calls.
	// Cache is scoped per (session, message-hash) — see intentCacheKey for why.
	cacheKey := intentCacheKey(sessionID, userMessage)
	o.intentCacheMu.RLock()
	if entry, ok := o.intentCache[cacheKey]; ok && time.Now().Before(entry.expiresAt) {
		o.intentCacheMu.RUnlock()
		o.logger.Debug("intent cache hit", zap.String("session_id", sessionID), zap.String("intent", string(entry.intent)))
		return entry.intent, nil
	}
	o.intentCacheMu.RUnlock()

	contextMsgs, err := o.sessionMgr.GetContextWindow(ctx, sessionID)
	if err != nil {
		return "", err
	}

	systemPrompt := models.Message{
		Role: models.RoleSystem,
		Content: `Classify the user's message into exactly one category:
- code_query: Questions about code, docs, APIs, technical concepts
- code_execute: Requests to run/execute code, scripts, commands
- diagnose: Diagnose issues, check logs, troubleshoot
- deploy: Deployment operations (kubectl, docker push, terraform)
- mcp_call: External tool requests (GitHub, Jira, GitLab)
- conversation: General conversation
Respond with ONLY the category name.`,
	}

	messages := append([]models.Message{systemPrompt}, contextMsgs...)
	messages = append(messages, models.Message{Role: models.RoleUser, Content: userMessage})

	resp, err := o.llmClient.ChatCompletion(ctx, &llm.ChatRequest{
		Messages: messages, MaxTokens: 20, Temperature: 0.0,
	})
	if err != nil {
		return models.IntentConversation, nil
	}

	intent := strings.TrimSpace(strings.ToLower(resp.Content))
	var result models.TaskIntent
	switch models.TaskIntent(intent) {
	case models.IntentCodeQuery, models.IntentCodeExecute, models.IntentDiagnose,
		models.IntentDeploy, models.IntentMCPCall, models.IntentConversation:
		result = models.TaskIntent(intent)
	default:
		result = models.IntentConversation
	}

	// [OPT-10] Cache the result. If the cache has grown past its bound,
	// evict a batch of expired-or-oldest entries to keep memory bounded —
	// otherwise a long-running process with many distinct conversations would
	// leak.
	o.intentCacheMu.Lock()
	if len(o.intentCache) >= intentCacheMaxEntries {
		o.evictIntentCacheLocked()
	}
	o.intentCache[cacheKey] = intentCacheEntry{intent: result, expiresAt: time.Now().Add(intentCacheTTL)}
	o.intentCacheMu.Unlock()

	return result, nil
}

// evictIntentCacheLocked purges expired entries first; if still at capacity,
// it drops the oldest quarter of entries by expiresAt. Caller MUST hold
// intentCacheMu for writing.
func (o *Orchestrator) evictIntentCacheLocked() {
	now := time.Now()
	for k, v := range o.intentCache {
		if !now.Before(v.expiresAt) {
			delete(o.intentCache, k)
		}
	}
	if len(o.intentCache) < intentCacheMaxEntries {
		return
	}
	// Fallback: drop oldest ~25% by expiresAt.
	type kv struct {
		key string
		exp time.Time
	}
	all := make([]kv, 0, len(o.intentCache))
	for k, v := range o.intentCache {
		all = append(all, kv{k, v.expiresAt})
	}
	// Partial sort is fine; len is bounded by intentCacheMaxEntries so this
	// stays cheap even at the cap.
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && all[j-1].exp.After(all[j].exp); j-- {
			all[j-1], all[j] = all[j], all[j-1]
		}
	}
	drop := len(all) / 4
	for i := 0; i < drop; i++ {
		delete(o.intentCache, all[i].key)
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// F6: Specialized Intent Handlers (via system prompts)
// ═══════════════════════════════════════════════════════════════════════════════

// buildSystemMessage returns an intent-specific system prompt that guides the LLM
// to use the right tools and approach for each task type.
func (o *Orchestrator) buildSystemMessage(intent models.TaskIntent) models.Message {
	var prompt string
	switch intent {
	case models.IntentCodeQuery:
		prompt = `You are an expert code intelligence assistant. Answer questions about code accurately.
If code context is provided, use it to ground your answers. Cite specific files and line numbers.
Use the search_code tool if you need to find more code. Use execute_code to verify behavior.`

	case models.IntentCodeExecute:
		prompt = `You are a code execution assistant. When the user wants to run code:
1. Extract the language and code from their message
2. Use the execute_code tool to run it in a sandboxed container
3. Explain the output clearly
Supported languages: python, go, bash, node.`

	case models.IntentDiagnose:
		prompt = `You are a production diagnostics expert. To diagnose issues:
1. Use search_code to find relevant error handlers and log patterns
2. Use execute_code to run diagnostic scripts (curl, grep logs, check endpoints)
3. Synthesize findings into a root-cause analysis with recommended fixes
Be systematic: check logs → trace code path → identify root cause → suggest fix.`

	case models.IntentMCPCall:
		prompt = `You are a tool integration assistant with access to external services via MCP.
Available MCP tools will appear in your tool list. Use them to:
- Query GitHub (PRs, issues, commits)
- Interact with Jira (create/update tickets)
- Access other connected services
Always use the appropriate tool rather than guessing information.`

	case models.IntentDeploy:
		prompt = `You are a deployment assistant. Help the user plan and execute deployments.
Important: All deployment operations require human approval.
Help the user prepare by reviewing configs, running dry-runs, and validating prerequisites.
Use execute_code for dry-run commands (e.g., kubectl diff, terraform plan).`

	default:
		prompt = `You are a highly capable code intelligence agent with full agentic autonomy.
You follow a strict "Document-First" methodology: NEVER write code immediately. Always generate a
detailed technical design document BEFORE any implementation.

═══════════════════════════════════════════════════════════════
MANDATORY TWO-PHASE WORKFLOW
═══════════════════════════════════════════════════════════════

## PHASE 1: TECHNICAL DESIGN DOCUMENT (必须先完成)

Before writing ANY code, you MUST create a file called '.plan.md' in the workspace root using write_file.
This document MUST contain ALL 6 sections below. Do NOT skip any section.

### Section 1: 目标与边界 (Context & Scope)
- **核心目标**: 用一两句话明确本次任务的最终产出
- **非目标 (Out of Scope)**: 明确不做什么，防止过度工程

### Section 2: 架构与依赖约定 (Architecture & Constraints)
- **技术栈与版本**: 语言、框架、运行环境
- **基础设施限制**: 数据库、缓存、网络等约束
- **依赖库**: 必须使用的包，禁止引入的外部依赖

### Section 3: 接口与数据契约 (Contracts & Schemas)
- **API / 协议定义**: RESTful 路由、gRPC Protobuf、或 Tool Schema
- **数据模型**: 表结构 DDL、接口定义、缓存 Key 规范

### Section 4: 核心逻辑与模块拆解 (Module Breakdown)
- **流程流转**: 数据从输入到输出的完整链路
- **关键算法思路**: 性能优化策略、时间/空间复杂度分析

### Section 5: 异常处理与容错机制 (Error Handling & Resilience)
- **失败模式**: 网络超时、连接断开、限流时的 fallback/重试策略
- **日志与监控**: 关键节点的日志规范和格式

### Section 6: 分步执行清单 (Execution Roadmap)
- 将任务拆解为 Step-by-Step 的 Checkbox 列表
- 每个 Step 应该是一个可独立验证的最小单元
- 示例:
  - [ ] Step 1: 初始化模块并定义目录结构
  - [ ] Step 2: 实现接口契约与数据模型定义
  - [ ] Step 3: 编写核心业务逻辑
  - [ ] Step 4: 编写单元测试覆盖核心路径
  - [ ] Step 5: 运行测试并修复问题
  - [ ] Step 6: 最终验证与清理

## PHASE 2: 按清单逐步实现 (STRICT Step-by-Step Execution)

After .plan.md is written, execute EACH step from the Execution Roadmap IN ORDER:
1. Read .plan.md to confirm the current step
2. Implement that step using write_file / patch_file
3. Use run_workspace_cmd to verify (e.g. 'go build ./...', 'go test ./... -v -count=1')
4. If tests fail, read errors, fix code, re-run until the step passes
5. Move to the next step only after current step is verified
6. After ALL steps pass, proceed to Phase 3

## PHASE 3: 编译二进制 + 集成测试 (Build Binary & Integration Test)

After all unit tests pass, you MUST:

### 3a. 编译为二进制文件
Use run_workspace_cmd to compile the project into a binary:
- Go: 'go build -o <binary_name> ./...'
- Node: 'npm run build' or 'npx tsc'
- Python: verify 'python3 main.py --help' works

### 3b. 启动二进制 + 集成测试
If the project is a server/service, write an integration test script and run it:
- IMPORTANT: Use port 19090 for integration tests (ports 8080, 18080 are already taken by the agent itself)
- Use run_workspace_cmd to start the binary, wait for it, test endpoints, then kill it. Example pattern:
  './<binary> -addr :19090 &' to start in background (or set PORT env)
  'sleep 2' to wait for startup
  'curl -s http://localhost:19090/health' to test endpoints
  'kill %1' to stop the server
- Combine these into a single shell command or write a test_integration.sh script.
- Verify HTTP status codes, JSON response bodies, error handling.

### 3c. 构造边界测试用例
Generate test cases that cover:
- Happy path: normal requests with valid data
- Error path: malformed JSON, missing fields, wrong HTTP methods
- Boundary: empty body, very large payload, special characters
- Concurrency: multiple simultaneous requests (use '&' to parallelize curl)

## PHASE 4: 对照方案验证 + 迭代修复 (Spec Verification & Iterative Fix Loop)

After Phase 3 completes, you MUST verify results against the .plan.md specification:

### 4a. 对照技术方案验证 (Compare Against Spec)
1. Re-read .plan.md — especially Section 3 (接口契约) and Section 5 (异常处理)
2. Compare ACTUAL test output (stdout/stderr from run_workspace_cmd) against EXPECTED behavior:
   - Do HTTP status codes match the spec? (e.g., spec says 404 for missing key, does test confirm?)
   - Do response JSON bodies match the data model defined in Section 3?
   - Are error messages consistent with Section 5?
   - Are all endpoints from Section 3 covered by integration tests?

### 4b. 迭代修复循环 (Fix-Retest Loop)
If ANY test result does NOT match the specification:
1. Identify the SPECIFIC discrepancy (e.g., "spec says 400 for empty body, but server returns 500")
2. Use read_file to examine the relevant source code
3. Use patch_file or write_file to fix the root cause
4. Re-compile: 'go build -o <binary> .'
5. Re-run the failing test(s) to confirm the fix
6. Repeat until ALL tests match the spec

### 4c. 关键修复模式 (Common Fix Patterns)
- **Status code mismatch**: Check handler logic for correct http.StatusXxx usage
- **Response body mismatch**: Check JSON struct tags and encoding
- **Missing endpoint**: Add handler registration in router/mux setup
- **Test script bug**: Sometimes the test_integration.sh itself has bugs — fix the test AND the code
- **Race condition**: If concurrent tests fail intermittently, check sync.Mutex usage

### 4d. 完成标准 (Definition of Done)
Only declare task complete when ALL of these are true:
✅ go build / compile succeeds
✅ go test -race passes (ALL unit tests green)
✅ Binary starts and responds to curl requests (integration test exit_code=0)
✅ Error cases return correct status codes matching .plan.md Section 3
✅ Every endpoint defined in the spec is covered by at least one integration test
✅ No FAIL lines in integration test output

If you cannot achieve all criteria within the step limit, report which items are pending.

═══════════════════════════════════════════════════════════════
AVAILABLE TOOLS
═══════════════════════════════════════════════════════════════
- list_files / read_file: Explore existing code before planning
- write_file / patch_file: Create or modify files
- create_directory: Create directories
- run_workspace_cmd: Execute shell commands directly (PREFERRED for go test, go build, curl, python, npm)
  * Supports: compilation, test execution, starting servers, curl testing, shell scripts
  * Max timeout: 2 minutes per command
  * For server integration tests, use: './binary & sleep 2 && curl ... && kill %1'
- run_tests: Execute in Docker sandbox (fallback only)
- search_code: Semantic code search via RAG

IMPORTANT: Always use run_workspace_cmd (not run_tests) for compilation and testing.`
	}

	// [P1-D] Inject project-specific rules from workspace
	if o.ruleLoader != nil && o.workspaceMgr != nil {
		if ws := o.resolveWorkspace(""); ws != nil {
			if rules := o.ruleLoader.Load(ws.RootDir); rules != nil {
				if text := rules.FormatForSystemPrompt(); text != "" {
					prompt += text
				}
			}
		}
	}

	return models.Message{Role: models.RoleSystem, Content: prompt}
}

// ═══════════════════════════════════════════════════════════════════════════════
// RAG Context Retrieval
// ═══════════════════════════════════════════════════════════════════════════════

func (o *Orchestrator) retrieveRAGContext(ctx context.Context, query string) string {
	if o.ragEngine == nil {
		return ""
	}
	results, err := o.ragEngine.Retrieve(ctx, query, nil)
	if err != nil {
		o.logger.Warn("RAG retrieval failed", zap.Error(err))
		return ""
	}
	if len(results) == 0 {
		return ""
	}

	metrics.RAGChunksReturned.Observe(float64(len(results)))

	var parts []string
	for _, r := range results {
		parts = append(parts, fmt.Sprintf("--- %s (%s) [score: %.3f] ---\n%s",
			r.Chunk.FilePath, r.Chunk.SymbolName, r.Score, r.Chunk.Content))
	}
	return strings.Join(parts, "\n\n")
}

// ═══════════════════════════════════════════════════════════════════════════════
// Tool Execution
// ═══════════════════════════════════════════════════════════════════════════════

func (o *Orchestrator) executeTool(ctx context.Context, tc models.ToolCall) (*models.ToolResult, error) {
	start := time.Now()

	// [P0-3] Speculative cache: return cached result for idempotent read tools
	scope := o.cacheScope()
	if o.toolCache != nil {
		if cached, hit := o.toolCache.Get(scope, tc.Name, tc.Args); hit {
			return cached, nil
		}
	}

	// [P2-E2] Capture file state before write operations for rollback
	o.captureForTransaction(tc)

	result, err := o.dispatchTool(ctx, tc, start)

	// [P2-D] Record tool feedback for learning
	if o.toolCollector != nil {
		success := err == nil && (result == nil || !result.IsError)
		errMsg := ""
		if err != nil {
			errMsg = err.Error()
		} else if result != nil && result.IsError {
			errMsg = result.Content
		}
		o.toolCollector.Record(tc.Name, tc.Args, success, time.Since(start), errMsg, "")
	}

	// [P0-3] Cache write: store successful idempotent results
	if o.toolCache != nil && err == nil && result != nil && !result.IsError {
		o.toolCache.Put(scope, tc.Name, tc.Args, result)
	}
	// [P0-3] Cache invalidation: write tools invalidate the scope
	if o.toolCache != nil && ShouldInvalidateAfter(tc.Name) {
		o.toolCache.Invalidate(scope)
	}

	return result, err
}

func (o *Orchestrator) dispatchTool(ctx context.Context, tc models.ToolCall, start time.Time) (*models.ToolResult, error) {
	// Check MCP tools first
	if o.mcpGateway != nil {
		if serverName, ok := o.mcpGateway.FindServerForTool(tc.Name); ok {
			result, err := o.mcpGateway.CallTool(ctx, serverName, tc.Name, tc.Args)
			metrics.MCPCallTotal.WithLabelValues(serverName, tc.Name, statusLabel(err)).Inc()
			metrics.MCPCallDuration.WithLabelValues(serverName).Observe(time.Since(start).Seconds())
			return result, err
		}
	}

	// Built-in tools
	switch tc.Name {
	case "execute_code":
		return o.toolExecuteCode(ctx, tc.Args)
	case "search_code":
		return o.toolSearchCode(ctx, tc.Args)
	// Autonomous file tools — LLM decides when to use these
	case "read_file":
		return o.toolReadFile(ctx, tc.Args)
	case "write_file":
		return o.toolWriteFile(ctx, tc.Args)
	case "patch_file":
		return o.toolPatchFile(ctx, tc.Args)
	case "edit_file":
		return o.toolEditFile(ctx, tc.Args)
	case "list_files":
		return o.toolListFiles(ctx, tc.Args)
	case "create_directory":
		return o.toolCreateDirectory(ctx, tc.Args)
	case "run_tests":
		return o.toolRunTests(ctx, tc.Args)
	case "run_workspace_cmd":
		return o.toolRunWorkspaceCmd(ctx, tc.Args)
	case "git_status", "git_diff", "git_commit", "git_log", "git_branch":
		return o.executeGitTool(ctx, tc)
	default:
		// Check dynamic skills
		if o.skillRegistry != nil {
			if _, ok := o.skillRegistry.FindSkill(tc.Name); ok {
				return o.skillRegistry.Execute(ctx, tc.Name, tc.Args)
			}
		}
		return &models.ToolResult{Content: fmt.Sprintf("Unknown tool: %s", tc.Name), IsError: true}, nil
	}
}

func (o *Orchestrator) toolExecuteCode(ctx context.Context, args json.RawMessage) (*models.ToolResult, error) {
	var req models.SandboxRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return &models.ToolResult{Content: "Invalid arguments: " + err.Error(), IsError: true}, nil
	}
	if o.sandboxMgr == nil {
		return &models.ToolResult{Content: "Sandbox not available (Docker not connected)", IsError: true}, nil
	}

	start := time.Now()
	result, err := o.sandboxMgr.Execute(ctx, &req)
	lang := strings.ToLower(req.Language)

	if err != nil {
		metrics.SandboxExecutionTotal.WithLabelValues(lang, "error").Inc()
		return &models.ToolResult{Content: "Execution failed: " + err.Error(), IsError: true}, nil
	}

	// Track metrics
	status := "success"
	if result.Killed {
		status = "timeout"
	} else if result.ExitCode != 0 {
		status = "failed"
	}
	metrics.SandboxExecutionTotal.WithLabelValues(lang, status).Inc()
	metrics.SandboxExecutionDuration.WithLabelValues(lang).Observe(time.Since(start).Seconds())

	// Format output
	var out strings.Builder
	fmt.Fprintf(&out, "Exit code: %d | Duration: %s\n", result.ExitCode, result.Duration)
	if result.Stdout != "" {
		fmt.Fprintf(&out, "STDOUT:\n%s\n", result.Stdout)
	}
	if result.Stderr != "" {
		fmt.Fprintf(&out, "STDERR:\n%s\n", result.Stderr)
	}
	if result.Killed {
		out.WriteString("⚠️ Process was killed (timeout or OOM)\n")
	}

	return &models.ToolResult{Content: out.String()}, nil
}

func (o *Orchestrator) toolSearchCode(ctx context.Context, args json.RawMessage) (*models.ToolResult, error) {
	var req struct {
		Query   string            `json:"query"`
		Filters map[string]string `json:"filters,omitempty"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return &models.ToolResult{Content: "Invalid arguments", IsError: true}, nil
	}
	if o.ragEngine == nil {
		return &models.ToolResult{Content: "Code search not available (RAG engine disabled)", IsError: true}, nil
	}

	results, err := o.ragEngine.Retrieve(ctx, req.Query, req.Filters)
	if err != nil {
		return &models.ToolResult{Content: "Search failed: " + err.Error(), IsError: true}, nil
	}
	if len(results) == 0 {
		return &models.ToolResult{Content: "No matching code found for: " + req.Query}, nil
	}

	var parts []string
	for _, r := range results {
		parts = append(parts, fmt.Sprintf("[%s:%d-%d] %s (score: %.3f)\n%s",
			r.Chunk.FilePath, r.Chunk.StartLine, r.Chunk.EndLine,
			r.Chunk.SymbolName, r.Score, r.Chunk.Content))
	}
	return &models.ToolResult{Content: strings.Join(parts, "\n---\n")}, nil
}

// ═══════════════════════════════════════════════════════════════════════════════
// Tool Registry
// ═══════════════════════════════════════════════════════════════════════════════

func (o *Orchestrator) getAvailableTools() []models.ToolDefinition {
	tools := []models.ToolDefinition{
		{
			Name:        "execute_code",
			Description: "Execute code in a sandboxed Docker container. Returns stdout, stderr, and exit code.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"language":{"type":"string","enum":["python","go","bash","node"],"description":"Programming language"},"code":{"type":"string","description":"Code to execute"}},"required":["language","code"]}`),
			Source:      "builtin",
		},
		{
			Name:        "search_code",
			Description: "Search the indexed codebase using semantic and keyword search. Returns matching code snippets with file paths and line numbers.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Natural language search query or exact symbol name"}},"required":["query"]}`),
			Source:      "builtin",
		},
	}
	// File tools — only available when workspace manager is connected
	if o.workspaceMgr != nil {
		tools = append(tools, fileToolDefinitions()...)
		tools = append(tools, gitToolDefinitions()...)
	}
	if o.mcpGateway != nil {
		tools = append(tools, o.mcpGateway.GetAvailableTools()...)
	}
	// Dynamic skills — added at runtime via REST API
	if o.skillRegistry != nil {
		tools = append(tools, o.skillRegistry.GetToolDefinitions()...)
	}
	return tools
}

// SetSkillRegistry injects a skill registry (satisfying the interface) after construction.
func (o *Orchestrator) SetSkillRegistry(sr interface {
	GetToolDefinitions() []models.ToolDefinition
	FindSkill(string) (string, bool)
	Execute(context.Context, string, json.RawMessage) (*models.ToolResult, error)
}) {
	o.skillRegistry = sr
}

// ═══════════════════════════════════════════════════════════════════════════════
// Security
// ═══════════════════════════════════════════════════════════════════════════════

// cacheScope returns the scope key for the speculative tool cache.
// Uses workspace ID so that writes in any session sharing the same workspace
// correctly invalidate cached reads.
func (o *Orchestrator) cacheScope() string {
	if ws := o.resolveWorkspace(""); ws != nil {
		return ws.ID
	}
	return "_global"
}

// captureForTransaction records file state before write tools execute,
// enabling rollback on interrupt.
func (o *Orchestrator) captureForTransaction(tc models.ToolCall) {
	switch tc.Name {
	case "write_file", "edit_file", "patch_file":
	default:
		return
	}

	var pathReq struct {
		Path        string `json:"path"`
		WorkspaceID string `json:"workspace_id"`
	}
	if json.Unmarshal(tc.Args, &pathReq) != nil || pathReq.Path == "" {
		return
	}

	ws := o.resolveWorkspace(pathReq.WorkspaceID)
	if ws == nil {
		return
	}

	absPath := pathReq.Path
	if !filepath.IsAbs(absPath) {
		absPath = filepath.Join(ws.RootDir, absPath)
	}

	// Find the active transaction for any session currently running
	o.txMu.RLock()
	for _, tx := range o.txMap {
		tx.CaptureBeforeWrite(absPath)
	}
	o.txMu.RUnlock()
}

func (o *Orchestrator) containsSensitiveContent(input string) bool {
	for _, re := range o.sensitiveRules {
		if re.MatchString(input) {
			o.logger.Warn("sensitive content detected", zap.String("pattern", re.String()))
			return true
		}
	}
	return false
}

func statusLabel(err error) string {
	if err != nil {
		return "error"
	}
	return "success"
}
