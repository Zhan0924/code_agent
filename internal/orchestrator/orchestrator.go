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

	"github.com/agent/code_agent/internal/agentloop"
	"github.com/agent/code_agent/internal/config"
	agentctx "github.com/agent/code_agent/internal/context"
	"github.com/agent/code_agent/internal/llm"
	"github.com/agent/code_agent/internal/mcp"
	"github.com/agent/code_agent/internal/memory"
	"github.com/agent/code_agent/internal/metrics"
	"github.com/agent/code_agent/internal/models"
	"github.com/agent/code_agent/internal/multiagent"
	"github.com/agent/code_agent/internal/rag"
	"github.com/agent/code_agent/internal/sandbox"
	"github.com/agent/code_agent/internal/session"
	"github.com/agent/code_agent/internal/store"
	"github.com/agent/code_agent/internal/toollearn"
	"github.com/agent/code_agent/internal/tools"
	"github.com/agent/code_agent/internal/workspace"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Context keys for orchestrator-local request metadata.
//
// Note: the (sessionID, userID, projectID) triple has moved to
// `models.WithSessionContext` / `models.SessionIDFromContext` etc. so that
// downstream packages (tools / memory / observability / audit) can read
// the same values without re-fetching the session by ID.
// The previous orchestrator-local `ctxKeySessionID` has been deleted —
// any new orchestrator-local key should use this typed wrapper.
type contextKey string

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
	llmRouter      *llm.Router // Optional intent-based model tier router; nil = disabled.
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

	// Unified tool registry for all tool dispatch
	toolRegistry *tools.Registry

	// [P0] Precision edit engine with unique-match, backup, and lint
	editEngine *EditEngine
	// [P0] Auto-test runner for TDD self-verification loop
	autoTestRunner *AutoTestRunner
	// [P0-3] Speculative tool result cache for idempotent read tools
	toolCache *SpeculativeToolCache
	// [P1-D] Project rules loader for workspace-specific LLM guidance
	ruleLoader *RuleLoader

	// [P1] Persistent PTY session manager for stateful shell execution (nil = disabled)
	ptyManager PTYManager

	// [P1] LSP client for type-aware code intelligence (nil = disabled)
	lspClient LSPClient

	// [P1] Tree-sitter parser for multi-language AST analysis (nil = disabled)
	tsParser TreeSitterParser

	// [P2-D] Tool learning: feedback collector + advisor + adaptive policy + distiller
	toolCollector *toollearn.Collector
	toolAdvisor   *toollearn.Advisor
	toolPolicy    *toollearn.AdaptivePolicy
	toolDistiller *toollearn.Distiller

	// run_workspace_cmd 单次 exec 硬上限。0 = 取 defaultWorkspaceCmdTimeout
	// 兜底。LLM 通过 tool args.timeout_seconds 可在 [0, 此值] 内自定义更
	// 短上限;超此值被钳制。来源:config.WorkspaceConfig.CmdTimeout,经
	// main.go 调 SetWorkspaceCmdTimeout 注入。
	workspaceCmdTimeout time.Duration

	// HITL: mutex-protected map of taskID → approval channel
	approvalMu sync.RWMutex
	approvalCh map[string]chan models.ApprovalResponse

	// Tool-level HITL: per-task pending approval for a single high-risk tool
	// call. Distinct from approvalCh (which suspends the whole task) — this
	// only blocks executeTool until the user decides.
	toolApprovalMu sync.RWMutex
	toolApprovalCh map[string]chan models.ApprovalResponse

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

	// Optional multi-agent Supervisor — nil unless AttachSupervisor has been called.
	// See multiagent_bridge.go for the behaviour.
	supervisor *multiagent.Supervisor

	// Optional Temporal client for durable HITL workflows.
	// When set, suspendForApproval uses Temporal instead of in-process channels.
	temporalClient TemporalClient

	// Optional long-term memory store for cross-session context.
	memoryStore MemoryRetriever
	// Optional memory extractor for learning from interactions.
	memoryExtractor *memory.Extractor
	// Optional MemGPT-style core memory (always-on persona / human / project
	// sections). When wired, buildLongTermMemory pulls these sections and
	// prepends them to the long-term-memory slot of the prompt so the LLM
	// actually *reads* whatever core_memory_append / core_memory_replace
	// tools wrote earlier. Without this read path, those tools were a pure
	// write-only blackhole (the original bug).
	coreMemory memory.CoreMemoryManager

	// Trajectory memory: records successful tool sequences per intent.
	trajectoryMem agentloop.TrajectoryStore

	// Dynamic context window budget (from LLM provider config)
	maxContextTokens int

	// Compaction mode: "truncate" (default) or "summarize" (LLM-based)
	compactionMode string

	// Optional Redis-backed stream cache. When wired, ProcessMessageStreamFull
	// mirrors every emitted ReactStreamEvent to a per-session Redis Stream so
	// /chat/react-stream/resume can replay+follow after a page refresh. nil ⇒
	// streaming持续工作但刷新后无法重连进行中任务。
	streamCache *StreamCache
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
	return NewOrchestratorWithConfig(llmClient, sessionMgr, ragEngine, sandboxMgr, mcpGateway, securityCfg, nil, logger, pgStore...)
}

// NewOrchestratorWithConfig creates a new orchestrator with explicit LLM provider config for dynamic budgets.
func NewOrchestratorWithConfig(
	llmClient *llm.Client,
	sessionMgr *session.Manager,
	ragEngine *rag.Engine,
	sandboxMgr *sandbox.Manager,
	mcpGateway *mcp.Gateway,
	securityCfg *config.SecurityConfig,
	llmCfg *config.LLMProviderConfig,
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

	// Determine context window from config or default
	contextWindow := 128000
	enableCaching := false
	if llmCfg != nil {
		if llmCfg.ContextWindow > 0 {
			contextWindow = llmCfg.ContextWindow
		}
		enableCaching = llmCfg.EnablePromptCaching
	}
	maxTokens := contextWindow * 95 / 100 // 5% safety margin

	pb := agentctx.NewPromptBuilder(&agentctx.PromptBuilderConfig{
		SystemPrompt: `You are a highly capable code intelligence agent. You can:
1. Answer technical questions about code and architecture
2. Execute code in a sandboxed environment (use execute_code tool)
3. Search and retrieve relevant code snippets (use search_code tool)
4. Interact with external tools (GitHub, Jira, etc.) via MCP
5. Help diagnose and troubleshoot production issues
Always use tools when they would produce better answers. After receiving tool results, synthesize them into a clear answer.

FINAL-ANSWER STYLE (strict — applies to the last assistant message that ends a task):
- Keep it to a brief summary: at most ~6 short bullets or ~150 words covering what changed, where, and any next step required from the user.
- Do NOT paste full file contents, large code blocks, or verification tables back into the chat — written files already live in the workspace and the UI surfaces an "Open in Workspace" button for the user to inspect them.
- When the task produced or modified files in the workspace, end with one line:
    Click "Open in Workspace" to browse the generated files.
- Code snippets are only acceptable in the final answer when the user explicitly asked to see code inline, or when fewer than ~20 lines are needed to illustrate the answer.`,
		MaxTotalTokens:      maxTokens,
		EnablePromptCaching: enableCaching,
	}, logger)

	orch := &Orchestrator{
		llmClient:           llmClient,
		sessionMgr:          sessionMgr,
		ragEngine:           ragEngine,
		sandboxMgr:          sandboxMgr,
		mcpGateway:          mcpGateway,
		promptBuilder:       pb,
		securityCfg:         securityCfg,
		sensitiveRules:      rules,
		maxContextTokens:    maxTokens,
		workspaceCmdTimeout: defaultWorkspaceCmdTimeout,
		logger:              logger.With(zap.String("component", "orchestrator")),
		approvalCh:          make(map[string]chan models.ApprovalResponse),
		toolApprovalCh:      make(map[string]chan models.ApprovalResponse),
		interruptCh:      make(map[string]chan InterruptSignal),
		txMap:            make(map[string]*ToolTransaction),
		intentCache:      make(map[string]intentCacheEntry),
		toolCache:        NewSpeculativeToolCache(0, logger),
		ruleLoader:       NewRuleLoader(logger),
		toolRegistry:     tools.NewRegistry(),
		trajectoryMem:    agentloop.NewTrajectoryMemory(),
	}

	// Register built-in tools into the unified registry
	if err := orch.RegisterBuiltinTools(orch.toolRegistry); err != nil {
		logger.Error("failed to register builtin tools", zap.Error(err))
	}

	// Wire the speculative tool cache's metadata lookup to the registry so
	// idempotent / write classification stays in sync with ToolDefinition
	// metadata (set in file_tools / git_tools / lsp_tools). Falls back to the
	// hardcoded whitelist for tools that aren't (yet) in the registry.
	orch.toolCache.SetMetadataLookup(func(name string) (bool, bool, bool) {
		def, ok := orch.toolMetadata(name)
		if !ok {
			return false, false, false
		}
		return def.IsIdempotentRead, def.IsFileWrite || def.InvalidatesCache, true
	})

	// [P2-D] Initialize tool learning subsystem
	collector := toollearn.NewCollector(nil, logger)
	extractor := toollearn.NewExtractor(collector, logger)
	orch.toolCollector = collector
	orch.toolAdvisor = toollearn.NewAdvisor(extractor, logger)
	orch.toolPolicy = toollearn.NewAdaptivePolicy(collector)
	orch.toolDistiller = toollearn.NewDistiller(collector, logger)
	if len(pgStore) > 0 && pgStore[0] != nil {
		orch.store = pgStore[0]
	}
	return orch
}

// SetStreamCache wires the Redis-backed stream event cache. Nil is a valid
// no-op state — streaming continues working without persistence/resume.
func (o *Orchestrator) SetStreamCache(c *StreamCache) {
	o.streamCache = c
}

// StreamCache exposes the wired cache so HTTP handlers can implement
// /chat/react-stream/status and /chat/react-stream/resume. Returns nil when
// streaming-persistence is disabled.
func (o *Orchestrator) StreamCache() *StreamCache {
	return o.streamCache
}

// SetToolLearnStore wires a persistent store into the tool-learning collector
// so feedback survives process restarts. Safe to call at any time after
// construction; nil is a no-op.
func (o *Orchestrator) SetToolLearnStore(s toollearn.Store) {
	if o.toolCollector == nil || s == nil {
		return
	}
	o.toolCollector.SetStore(s)
}

// SetTrajectoryStore replaces the default in-memory TrajectoryMemory with
// any TrajectoryStore implementation — typically PGTrajectoryStore in
// production so successful tool sequences survive restarts and benefit
// from embedding-based intent recall. nil is a no-op so callers can wire
// optimistically without checking embedder presence.
func (o *Orchestrator) SetTrajectoryStore(s agentloop.TrajectoryStore) {
	if s == nil {
		return
	}
	o.trajectoryMem = s
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
// ProcessOptions holds optional parameters for ProcessMessage and streaming variants.
type ProcessOptions struct {
	OutputFormat *models.ResponseFormat
}

func (o *Orchestrator) ProcessMessage(ctx context.Context, sessionID, userMessage string, opts ...ProcessOptions) (*models.ChatResponse, error) {
	task := &models.Task{
		ID:        uuid.New().String(),
		SessionID: sessionID,
		UserInput: userMessage,
		State:     models.TaskStatePending,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if len(opts) > 0 {
		task.OutputFormat = opts[0].OutputFormat
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
		// Persist a failure-state assistant placeholder so the conversation
		// reflects WHY this turn produced no answer. Without this, the user
		// just sees no response, retries, and Redis ends up with runs of
		// consecutive RoleUser messages with no assistant in between.
		o.persistFailureAssistant(ctx, sessionID, task.ID, "⚠️ Failed to parse intent: "+err.Error())
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
	response, toolsUsed, err := o.reactLoop(ctx, task)
	if err != nil {
		task.State = models.TaskStateFailed
		o.persistFailureAssistant(ctx, sessionID, task.ID, "⚠️ Task failed: "+err.Error())
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

	// Extract memories from this interaction (async, non-blocking).
	// extractMemoriesAsync produces typed memories via the LLM (preference
	// / decision / knowledge / pattern), recordTaskEpisodeAsync persists
	// the raw trajectory as a single episodic memory for the Distiller
	// to consolidate later. Both run independently; either failing does
	// not affect the other.
	o.extractMemoriesAsync(ctx, sessionID, userMessage, response)
	o.recordTaskEpisodeAsync(ctx, sessionID, userMessage, response, toolsUsed)

	return &models.ChatResponse{
		SessionID: sessionID, TaskID: task.ID,
		Message: response, State: models.TaskStateCompleted,
	}, nil
}

// persistFailureAssistant writes a terminal RoleAssistant placeholder when the
// orchestrator can't produce a real answer (intent-parse fail, ReAct loop error,
// empty content, step-budget exhaustion). Without it, the session ends up with
// a RoleUser write paired with no assistant record; the user sees nothing, hits
// retry, and Redis fills with runs of consecutive RoleUser messages that look
// like duplicates. Best-effort — never propagates errors back to the caller
// because the caller is already on an error path.
func (o *Orchestrator) persistFailureAssistant(ctx context.Context, sessionID, taskID, message string) {
	if o.sessionMgr == nil || sessionID == "" {
		return
	}
	if err := o.sessionMgr.AddMessage(ctx, sessionID, models.Message{
		Role:    models.RoleAssistant,
		Content: message,
	}); err != nil {
		o.logger.Warn("failed to persist failure-state assistant message",
			zap.String("session_id", sessionID),
			zap.String("task_id", taskID),
			zap.Error(err),
		)
	}
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
// reactLoop returns (content, toolsUsed, error). toolsUsed is the
// in-order sequence of tool names invoked during the loop, used by
// recordTaskEpisodeAsync to capture the trajectory in per-task episodic
// memory. Best-effort: some early-exit paths may return nil tools.
func (o *Orchestrator) reactLoop(ctx context.Context, task *models.Task) (string, []string, error) {
	// [P2-E1] Register interrupt channel for this session
	interruptCh := o.registerInterrupt(task.SessionID)
	defer o.unregisterInterrupt(task.SessionID)

	// [P2-E2] Register tool transaction for rollback support
	tx, releaseTx := o.registerTx(task.SessionID)
	defer releaseTx()

	// Build prompt via PromptBuilder
	sess, _ := o.sessionMgr.Get(ctx, task.SessionID)

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

	o.promptBuilder.UpdateLongTermMemory(func() string {
		summary := ""
		if sess != nil {
			summary = sess.Summary
		}
		userID := ""
		projectID := ""
		if sess != nil {
			userID = sess.UserID
			projectID = sess.ProjectID
		}
		return o.buildLongTermMemory(ctx, summary, userID, projectID, task.UserInput)
	}())

	messages := o.promptBuilder.BuildPrompt(sess, codeChunks, relevanceScores, task.UserInput)
	if len(messages) > 0 && messages[0].Role == models.RoleSystem {
		messages[0] = o.buildSystemMessage(task.Intent)
	}

	tools := o.GetAvailableTools()

	// Delegate to shared core loop
	result := o.reactLoopCore(ctx, reactCoreOpts{
		task:           task,
		messages:       messages,
		tools:          tools,
		maxSteps:       getMaxSteps(task.Intent),
		startStep:      0,
		interruptCh:    interruptCh,
		responseFormat: task.OutputFormat,
		tx:             tx,
	}, noopSink{})

	if result.done {
		if o.shouldVerifyOutput(task.Intent, result.stepsUsed) && result.content != "" {
			vResult, vErr := o.verifyOutput(ctx, task.UserInput, result.content)
			if vErr == nil && !vResult.Passed {
				// Verification feedback is for internal observability only —
				// it is the *evaluator's* critique, not part of the assistant's
				// answer. Never concatenate to result.content (would leak the
				// "Please address the issues above before finalizing..." text
				// to the user) and never AddMessage as RoleAssistant.
				o.logger.Warn("output verification failed",
					zap.String("task_id", task.ID),
					zap.Float64("score", vResult.Score),
					zap.Strings("issues", vResult.Issues),
					zap.String("reasoning", vResult.Reasoning),
				)
			}
		}
		return result.content, result.toolsUsed, nil
	}

	// Step limit exhausted — save progress
	o.saveProgressForContinuation(ctx, task)
	return "⚠️ I reached the maximum reasoning steps. Your workspace files and .plan.md are intact.\n\n" +
		"**To continue from where I left off**, send a follow-up message saying `continue` in the same session. " +
		"I will read .plan.md and .progress.json to resume the remaining steps.", result.toolsUsed, nil
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
				result, toolsUsed, err := o.reactLoop(waitCtx, task)
				if err != nil {
					o.logger.Error("post-approval execution failed", zap.Error(err))
					return
				}
				_ = o.sessionMgr.AddMessage(waitCtx, task.SessionID, models.Message{
					Role: models.RoleAssistant, Content: result,
				})
				o.recordTaskEpisodeAsync(waitCtx, task.SessionID, task.UserInput, result, toolsUsed)
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

// HandleApproval processes an approval/rejection for a suspended task or a
// blocked tool call. Tool-level pending approvals (waitToolApproval) are
// dispatched first; falling through to task-level suspendForApproval matches
// the pre-existing behaviour.
func (o *Orchestrator) HandleApproval(ctx context.Context, resp models.ApprovalResponse) (*models.ChatResponse, error) {
	// Tool-level HITL: a single tool call inside a running ReAct loop. Resolve
	// before consulting Temporal because Temporal only tracks task-level
	// suspends.
	o.toolApprovalMu.RLock()
	toolCh, hasTool := o.toolApprovalCh[resp.TaskID]
	o.toolApprovalMu.RUnlock()
	if hasTool {
		select {
		case toolCh <- resp:
		default:
			return nil, fmt.Errorf("tool approval already submitted for task: %s", resp.TaskID)
		}
		state := models.TaskStateExecuting
		msg := "Tool approved. Continuing execution…"
		if !resp.Approved {
			msg = "Tool rejected. Agent will pick an alternative approach."
		}
		return &models.ChatResponse{TaskID: resp.TaskID, Message: msg, State: state}, nil
	}

	// Temporal path: when available, signal the workflow directly
	if o.temporalClient != nil {
		return o.HandleApprovalTemporal(ctx, resp)
	}

	// In-process fallback (task-level suspend)
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

	streamReq := &llm.ChatRequest{Messages: messages}
	o.applyModelRoute(streamReq, "conversation", userMessage, len(messages))
	ch, err := o.llmClient.ChatCompletionStream(ctx, streamReq)
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

// streamWorkCtxMaxDuration 是流式 ReAct 任务的"业务运行 ctx"硬上限。
// 与 HTTP 请求 ctx 解耦后，业务 ctx 不会因 SSE 断连而取消；30 分钟兜住极端 runaway。
// 与 temporal.workflow_timeout (30m) 对齐，便于 HITL 工作流降级时语义一致。
const streamWorkCtxMaxDuration = 30 * time.Minute

// ProcessMessageStreamFull processes a message with the full ReAct loop,
// emitting structured ReactStreamEvent for each step (intent, thinking, tool_call, tool_result, message).
// This provides the frontend with complete visibility into the agent's reasoning process.
//
// 业务 ctx 与 HTTP ctx 解耦（2026-06-04）：
//
//	入参 reqCtx 来自 gin handler，与 SSE TCP 连接强绑定。前一版本将 reqCtx 直接传给
//	parseIntent / reactLoopCore，导致连接因 write_timeout(600s)、移动网络抖动、负载
//	均衡 idle drop 而断时，整个 ReAct 链路被取消，工具调用半途撤销、HITL 审批通道
//	孤立、终态消息从未落 session —— UI 永远旋转。
//
//	新设计：
//	1. 函数级构造 workCtx (background + 30min 兜底)，所有业务调用一律走 workCtx；
//	2. 起一个独立桥接 goroutine：reqCtx 死时仅 cancel droppedCtx —— 业务继续，
//	   只是 channelSink 自此静默丢弃事件（防止 buffered channel 满后阻塞业务）；
//	3. workCtx 仅在以下情况被 cancel：(a) goroutine 自然返回（业务完成）；
//	   (b) 30min 超时（runaway 兜底）。reqCtx 死掉本身不传播到 workCtx。
//	4. 终态事件无论 reqCtx 是否还活，都先尝试入 channel —— 客户端可能在断连前
//	   收到，已断的则被 droppedCtx 丢弃。任务结果仍通过 sessionMgr.AddMessage
//	   持久化，重连后 GET /sessions/:id/messages 能查到完整 assistant 终态。
func (o *Orchestrator) ProcessMessageStreamFull(reqCtx context.Context, sessionID, userMessage string, opts ...ProcessOptions) (<-chan models.ReactStreamEvent, error) {
	eventCh := make(chan models.ReactStreamEvent, 64)

	// 业务 ctx —— background + 硬上限。reqCtx 死掉不影响它。
	workCtx, workCancel := context.WithTimeout(context.Background(), streamWorkCtxMaxDuration)

	// droppedCtx 仅在 reqCtx 死后触发，让 sink 把后续事件静默丢弃避免 buffered
	// channel 满阻塞业务。它独立于 workCtx，所以业务 goroutine 继续运行直到自然
	// 完成 / 30min 兜底 / 业务级中断。
	droppedCtx, dropCancel := context.WithCancel(context.Background())

	// Store user message —— 业务 ctx，redis 操作毫秒级，差异微乎其微但保持一致。
	_ = o.sessionMgr.AddMessage(workCtx, sessionID, models.Message{
		Role: models.RoleUser, Content: userMessage,
	})

	// 桥接 goroutine：reqCtx 死时只 drop，不 cancel workCtx。
	go func() {
		select {
		case <-reqCtx.Done():
			o.logger.Info("stream client disconnected; work context continues",
				zap.String("session_id", sessionID),
				zap.Error(reqCtx.Err()))
			dropCancel()
		case <-workCtx.Done():
			// 业务自然结束（成功或 30min 超时）；不再需要监听 reqCtx。
		}
	}()

	go func() {
		defer workCancel()
		defer dropCancel()
		defer close(eventCh)

		var sink reactEventSink = &channelSink{ch: eventCh, droppedCtx: droppedCtx}
		// 镜像每条 event 到 Redis Stream，让 /resume 能 Replay+Follow。
		// nil-streamCache 路径下 persistingSink 退化为内层 channelSink。
		if o.streamCache != nil {
			sink = &persistingSink{inner: sink, cache: o.streamCache, sessionID: sessionID, ctx: workCtx}
		}

		interruptCh := o.registerInterrupt(sessionID)
		defer o.unregisterInterrupt(sessionID)

		// Register a ToolTransaction for the streaming path. Without this,
		// the o.txMap iteration in CaptureBeforeWrite (orchestrator.go,
		// preWriteCapture) skipped streaming-path writes entirely, so file
		// edits from /chat/react-stream could not be rolled back on
		// interrupt. Mirrors the registration in reactLoop.
		streamTx, releaseStreamTx := o.registerTx(sessionID)
		defer releaseStreamTx()

		// 提前分配 task.ID 并标记 running，否则 parseIntent（一次 LLM 调用，
		// 可能几秒）期间刷新页面，/status 会返回 running:false，前端就放弃了重连。
		// MarkDone 在所有 sink.Emit 之后由 defer 触发，保证 Follow 端不会过早退出。
		taskID := uuid.New().String()
		if o.streamCache != nil {
			o.streamCache.MarkRunning(workCtx, sessionID, taskID)
			defer o.streamCache.MarkDone(workCtx, sessionID)
		}

		intent, err := o.parseIntent(workCtx, sessionID, userMessage)
		if err != nil {
			msg := "⚠️ Failed to parse intent: " + err.Error()
			sink.Emit(models.ReactStreamEvent{Type: "error", Content: msg})
			// Pair the just-written user message with a failure-state assistant
			// record so the user sees the error after refresh and doesn't retry.
			o.persistFailureAssistant(workCtx, sessionID, "", msg)
			return
		}

		task := &models.Task{
			ID:        taskID,
			SessionID: sessionID,
			UserInput: userMessage,
			Intent:    intent,
			State:     models.TaskStatePlanning,
			CreatedAt: time.Now(),
		}
		if len(opts) > 0 {
			task.OutputFormat = opts[0].OutputFormat
		}

		if !skipHITL(workCtx) && (o.containsSensitiveContent(userMessage) || intent == models.IntentDeploy) {
			task.State = models.TaskStateSuspended
			o.persistTaskCreate(workCtx, task)
			o.persistTaskState(workCtx, task.ID, models.TaskStateSuspended)

			if o.temporalClient != nil {
				if _, err := o.temporalClient.StartHITLWorkflow(workCtx, task.ID, sessionID, userMessage); err != nil {
					o.logger.Warn("temporal HITL failed in stream, falling back to in-process",
						zap.String("task_id", task.ID), zap.Error(err))
				}
			}

			sink.Emit(models.ReactStreamEvent{
				Type:    "approval_request",
				TaskID:  task.ID,
				Content: fmt.Sprintf("⚠️ This operation requires approval. Risk: high. Action: %s", userMessage),
			})
			sink.Emit(models.ReactStreamEvent{Type: "done", TaskID: task.ID})
			return
		}

		maxSteps := getMaxSteps(intent)
		const absoluteMaxSteps = 200
		sink.Emit(models.ReactStreamEvent{
			Type: "step_start", Intent: string(intent),
			TaskID: task.ID, MaxSteps: absoluteMaxSteps,
		})

		sess, _ := o.sessionMgr.Get(workCtx, sessionID)
		var codeChunks []models.CodeChunk
		var relevanceScores []float64
		if task.Intent == models.IntentCodeQuery || task.Intent == models.IntentDiagnose {
			if o.ragEngine != nil {
				results, ragErr := o.ragEngine.Retrieve(workCtx, task.UserInput, nil)
				if ragErr == nil && len(results) > 0 {
					for _, r := range results {
						codeChunks = append(codeChunks, r.Chunk)
						relevanceScores = append(relevanceScores, r.Score)
					}
					sink.Emit(models.ReactStreamEvent{Type: "rag_context", Content: fmt.Sprintf("Retrieved %d code chunks from RAG", len(results))})
				}
			}
		}

		o.promptBuilder.UpdateLongTermMemory(func() string {
			summary := ""
			if sess != nil {
				summary = sess.Summary
			}
			userID := ""
			projectID := ""
			if sess != nil {
				userID = sess.UserID
				projectID = sess.ProjectID
			}
			return o.buildLongTermMemory(workCtx, summary, userID, projectID, task.UserInput)
		}())

		messages := o.promptBuilder.BuildPrompt(sess, codeChunks, relevanceScores, task.UserInput)
		if len(messages) > 0 && messages[0].Role == models.RoleSystem {
			messages[0] = o.buildSystemMessage(task.Intent)
		}
		tools := o.GetAvailableTools()

		globalStep := 0
		for globalStep < absoluteMaxSteps {
			batchLimit := maxSteps
			if globalStep+batchLimit > absoluteMaxSteps {
				batchLimit = absoluteMaxSteps - globalStep
			}

			result := o.reactLoopCore(workCtx, reactCoreOpts{
				task:           task,
				messages:       messages,
				tools:          tools,
				maxSteps:       batchLimit,
				startStep:      globalStep,
				interruptCh:    interruptCh,
				responseFormat: task.OutputFormat,
				tx:             streamTx,
			}, sink)

			messages = result.messages
			globalStep += result.stepsUsed

			if result.done {
				if result.content == "" {
					// done==true but no content: model returned an empty final
					// answer (rare — usually an upstream stop signal). Without
					// a placeholder the session would silently lose this turn.
					placeholder := "⚠️ Agent finished without producing a response."
					o.persistFailureAssistant(workCtx, sessionID, task.ID, placeholder)
					// Also push the placeholder onto the live stream so the user
					// sees it immediately; without this they only learn about the
					// empty turn on a page refresh that re-reads the session.
					sink.Emit(models.ReactStreamEvent{
						Type:    "message",
						Content: placeholder,
						TaskID:  task.ID,
					})
					sink.Emit(models.ReactStreamEvent{Type: "done", TaskID: task.ID})
					return
				}
				if o.shouldVerifyOutput(task.Intent, globalStep) {
					vResult, vErr := o.verifyOutput(workCtx, task.UserInput, result.content)
					if vErr == nil && !vResult.Passed {
						// Emit a structured warning so the front-end can show the
						// score + issues to the user. Until this commit the
						// critique was logged only and the human-facing answer
						// silently shipped — verifier said 0.2 / "no actual code"
						// and the user saw nothing.
						warnPayload, canRetry := decideVerificationFollowup(
							task.VerificationRetried, globalStep, absoluteMaxSteps, vResult,
						)

						o.logger.Warn("output verification failed",
							zap.String("task_id", task.ID),
							zap.Float64("score", vResult.Score),
							zap.Strings("issues", vResult.Issues),
							zap.String("reasoning", vResult.Reasoning),
							zap.Bool("retrying", canRetry),
						)

						if raw, mErr := json.Marshal(warnPayload); mErr == nil {
							sink.Emit(models.ReactStreamEvent{
								Type:     "verification_warning",
								Step:     globalStep,
								TaskID:   task.ID,
								Metadata: json.RawMessage(raw),
							})
						}

						if canRetry {
							task.VerificationRetried = true
							// Push the previous assistant answer + the verifier's
							// feedback back onto the conversation and continue the
							// outer for loop. The next reactLoopCore iteration sees
							// the critique as a user instruction and is free to
							// invoke tools again to address the cited issues.
							messages = append(messages,
								models.Message{Role: models.RoleAssistant, Content: result.content},
								models.Message{Role: models.RoleUser, Content: formatVerificationFeedback(vResult)},
							)
							continue
						}
					}
				}
				_ = o.sessionMgr.AddMessage(workCtx, sessionID, models.Message{
					Role: models.RoleAssistant, Content: result.content,
				})
				o.extractMemoriesAsync(workCtx, sessionID, task.UserInput, result.content)
				o.recordTaskEpisodeAsync(workCtx, sessionID, task.UserInput, result.content, result.toolsUsed)
				sink.Emit(models.ReactStreamEvent{Type: "done", TaskID: task.ID})
				return
			}

			if result.hitStepLimit && globalStep < absoluteMaxSteps {
				sink.Emit(models.ReactStreamEvent{
					Type:    "thinking",
					Step:    globalStep,
					Content: fmt.Sprintf("Auto-continuing... (%d/%d steps used)", globalStep, absoluteMaxSteps),
				})
				messages = append(messages, models.Message{
					Role: models.RoleUser,
					Content: "You have used " + fmt.Sprintf("%d", globalStep) + " steps so far. " +
						"Continue executing the remaining tasks. Focus on completing the current work efficiently. " +
						"If you have completed all tasks, provide a final summary response without any tool calls.",
				})
			}
		}

		exhausted := fmt.Sprintf("⚠️ Reached absolute maximum of %d reasoning steps. Task progress has been saved.", absoluteMaxSteps)
		sink.Emit(models.ReactStreamEvent{Type: "message", Content: exhausted})
		// Persist a terminal assistant record for the step-exhausted exit so the
		// session doesn't leave the user message dangling.
		o.persistFailureAssistant(workCtx, sessionID, task.ID, exhausted)
		sink.Emit(models.ReactStreamEvent{Type: "done", TaskID: task.ID})
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

	// [P7] Fast-path: keyword-based classification for unambiguous patterns.
	// Avoids an LLM round-trip for ~60% of typical developer messages.
	if fastIntent := classifyIntentByKeywords(userMessage); fastIntent != "" {
		o.logger.Debug("intent fast-path hit", zap.String("intent", string(fastIntent)))
		o.cacheIntent(cacheKey, fastIntent)
		return fastIntent, nil
	}

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

	intentReq := &llm.ChatRequest{
		Messages: messages, MaxTokens: 20, Temperature: 0.0,
	}
	// Intent parsing is the canonical "internal utility" route — the router
	// classifies this as Light tier, so we use a cheap+fast model when
	// available.
	o.applyModelRoute(intentReq, "_intent_parse", userMessage, len(messages))
	resp, err := o.llmClient.ChatCompletion(ctx, intentReq)
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

	// [OPT-10] Cache the result.
	o.cacheIntent(cacheKey, result)

	return result, nil
}

// cacheIntent stores a classified intent in the bounded cache.
func (o *Orchestrator) cacheIntent(key string, intent models.TaskIntent) {
	o.intentCacheMu.Lock()
	if len(o.intentCache) >= intentCacheMaxEntries {
		o.evictIntentCacheLocked()
	}
	o.intentCache[key] = intentCacheEntry{intent: intent, expiresAt: time.Now().Add(intentCacheTTL)}
	o.intentCacheMu.Unlock()
}

// classifyIntentByKeywords performs fast keyword-based intent classification.
// Returns empty string if no confident match — caller should fall through to LLM.
func classifyIntentByKeywords(msg string) models.TaskIntent {
	lower := strings.ToLower(msg)

	// Deploy: strong signals
	for _, kw := range []string{"deploy", "kubectl", "terraform", "helm install", "docker push", "k8s", "发布", "部署", "上线"} {
		if strings.Contains(lower, kw) {
			return models.IntentDeploy
		}
	}

	// Code execute: explicit run/execute requests
	for _, kw := range []string{"run ", "execute ", "运行", "执行代码", "跑一下", "编译并运行"} {
		if strings.Contains(lower, kw) {
			return models.IntentCodeExecute
		}
	}

	// Diagnose: troubleshooting signals
	for _, kw := range []string{"debug", "diagnose", "troubleshoot", "check logs", "查日志", "排查", "诊断", "为什么报错", "error log"} {
		if strings.Contains(lower, kw) {
			return models.IntentDiagnose
		}
	}

	// MCP: external tool references
	for _, kw := range []string{"github issue", "jira", "gitlab", "create pr", "open issue", "slack", "创建issue", "提交pr"} {
		if strings.Contains(lower, kw) {
			return models.IntentMCPCall
		}
	}

	// Code query: question patterns about code
	for _, kw := range []string{"how does", "what is", "explain", "where is", "show me", "这段代码", "怎么实现", "是什么意思", "解释一下", "哪个文件"} {
		if strings.Contains(lower, kw) {
			return models.IntentCodeQuery
		}
	}

	return ""
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
  * Default timeout: 5 minutes (server-side cap workspace.cmd_timeout). Pass timeout_seconds for known long suites.
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

	// [P5] Tool-level HITL: high-risk tools pause for human approval via SSE
	// → /tasks/:id/approve. Without task+sink in ctx (multiagent / planner
	// callers) we fall back to the legacy "block with error" behaviour so the
	// safety check still trips.
	if !skipHITL(ctx) {
		if def, ok := o.getToolRiskLevel(tc.Name); ok && def.RiskLevel >= 2 {
			// Fail-fast if the parent SSE stream is already gone — otherwise
			// waitToolApproval would block until the user clicks Approve, the
			// approval would land on an abandoned channel slot, and the ReAct
			// loop would silently terminate. Returning IsError=true gives the
			// upper layer a clean cancellation path.
			if err := ctx.Err(); err != nil {
				o.logger.Info("high-risk tool aborted: caller cancelled before approval",
					zap.String("tool", tc.Name), zap.Int("risk_level", def.RiskLevel), zap.Error(err))
				return &models.ToolResult{
					ToolCallID: tc.ID,
					Content:    fmt.Sprintf("⚠️ Tool '%s' aborted: request cancelled (%v) before approval", tc.Name, err),
					IsError:    true,
				}, nil
			}
			task := taskFromCtx(ctx)
			sink := sinkFromCtx(ctx)
			if task != nil && sink != nil {
				approved, err := o.waitToolApproval(ctx, task, tc, def, sink)
				if err != nil {
					o.logger.Warn("tool approval failed",
						zap.String("tool", tc.Name),
						zap.Int("risk_level", def.RiskLevel),
						zap.Error(err))
					return &models.ToolResult{
						ToolCallID: tc.ID,
						Content:    fmt.Sprintf("⚠️ Tool '%s' approval failed: %v", tc.Name, err),
						IsError:    true,
					}, nil
				}
				if !approved {
					return &models.ToolResult{
						ToolCallID: tc.ID,
						Content:    fmt.Sprintf("❌ Tool '%s' rejected by user. Choose a different approach.", tc.Name),
						IsError:    true,
					}, nil
				}
				// fall through and execute normally
			} else {
				o.logger.Info("high-risk tool blocked pending approval (no task/sink in ctx)",
					zap.String("tool", tc.Name), zap.Int("risk_level", def.RiskLevel))
				return &models.ToolResult{
					ToolCallID: tc.ID,
					Content: fmt.Sprintf("⚠️ Tool '%s' requires approval (risk_level=%d). "+
						"This operation modifies system state. Please confirm execution.", tc.Name, def.RiskLevel),
					IsError: true,
				}, nil
			}
		}
	}

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
		sessionID := models.SessionIDFromContext(ctx)
		o.toolCollector.Record(tc.Name, tc.Args, success, time.Since(start), errMsg, sessionID)
	}

	// [P0-3] Cache write: store successful idempotent results
	if o.toolCache != nil && err == nil && result != nil && !result.IsError {
		o.toolCache.Put(scope, tc.Name, tc.Args, result)
	}
	// [P0-3] Cache invalidation: write tools invalidate the scope
	if o.toolCache != nil && o.toolCache.shouldInvalidate(tc.Name) {
		o.toolCache.Invalidate(scope)
	}

	return result, err
}

func (o *Orchestrator) getToolRiskLevel(name string) (models.ToolDefinition, bool) {
	if o.toolRegistry == nil {
		return models.ToolDefinition{}, false
	}
	tool, ok := o.toolRegistry.Get(name)
	if !ok {
		return models.ToolDefinition{}, false
	}
	return tool.Definition(), true
}

func (o *Orchestrator) dispatchTool(ctx context.Context, tc models.ToolCall, start time.Time) (*models.ToolResult, error) {
	// Check MCP tools first (they may shadow built-in names)
	if o.mcpGateway != nil {
		if serverName, ok := o.mcpGateway.FindServerForTool(tc.Name); ok {
			result, err := o.mcpGateway.CallTool(ctx, serverName, tc.Name, tc.Args)
			metrics.MCPCallTotal.WithLabelValues(serverName, tc.Name, statusLabel(err)).Inc()
			metrics.MCPCallDuration.WithLabelValues(serverName).Observe(time.Since(start).Seconds())
			return result, err
		}
	}

	// Unified registry dispatch for built-in + file + git tools
	if o.toolRegistry != nil {
		if _, ok := o.toolRegistry.Get(tc.Name); ok {
			return o.toolRegistry.Execute(ctx, tc.Name, tc.Args)
		}
	}

	// Dynamic skills as fallback
	if o.skillRegistry != nil {
		if _, ok := o.skillRegistry.FindSkill(tc.Name); ok {
			return o.skillRegistry.Execute(ctx, tc.Name, tc.Args)
		}
	}

	return &models.ToolResult{Content: fmt.Sprintf("Unknown tool: %s", tc.Name), IsError: true}, nil
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

// ToolRegistry returns the internal tool registry for testing and external execution.
func (o *Orchestrator) ToolRegistry() *tools.Registry {
	return o.toolRegistry
}

// GetAvailableTools returns all available tool definitions (builtin + MCP + dynamic + skills)
func (o *Orchestrator) GetAvailableTools() []models.ToolDefinition {
	tools := make([]models.ToolDefinition, 0, 16)
	if o.toolRegistry != nil {
		tools = append(tools, o.toolRegistry.Definitions()...)
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

// SetCompactionMode sets the context compaction mode ("truncate" or "summarize").
func (o *Orchestrator) SetCompactionMode(mode string) {
	o.compactionMode = mode
}

// SetWorkspaceCmdTimeout 覆盖 run_workspace_cmd 单次 exec 的硬上限。
// 0 或负值忽略。配置项 workspace.cmd_timeout(config.go)经 main.go 接线
// 到这里;LLM 通过 tool args.timeout_seconds 可在 [0, 此值] 内提出更短
// 上限,超过此值会被钳制。
func (o *Orchestrator) SetWorkspaceCmdTimeout(d time.Duration) {
	if d > 0 {
		o.workspaceCmdTimeout = d
	}
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
// enabling rollback on interrupt. Uses centralized IsFileWrite metadata bit
// (set in fileToolDefinitions / lsp_tools) so new write tools opt in by
// declaring the bit, not by editing this switch.
func (o *Orchestrator) captureForTransaction(tc models.ToolCall) {
	if !o.isFileWriteTool(tc.Name) {
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

// RegisterDynamicTool registers a dynamic tool into the tool registry.
func (o *Orchestrator) RegisterDynamicTool(tool tools.Tool) error {
	if o.toolRegistry == nil {
		return fmt.Errorf("tool registry not initialized")
	}
	return o.toolRegistry.Register(tool)
}

// UnregisterDynamicTool removes a dynamic tool from the registry by name.
func (o *Orchestrator) UnregisterDynamicTool(name string) bool {
	if o.toolRegistry == nil {
		return false
	}
	return o.toolRegistry.Unregister(name)
}

// GetTool retrieves a tool by name from the registry.
func (o *Orchestrator) GetTool(name string) (tools.Tool, bool) {
	if o.toolRegistry == nil {
		return nil, false
	}
	return o.toolRegistry.Get(name)
}
