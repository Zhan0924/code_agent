// react_core.go extracts the shared ReAct loop logic used by both
// the synchronous reactLoop and the streaming ProcessMessageStreamFull.
package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/agent/code_agent/internal/agentloop"
	"github.com/agent/code_agent/internal/llm"
	"github.com/agent/code_agent/internal/models"
	"go.uber.org/zap"
)

// reactEventSink receives events during the ReAct loop execution.
type reactEventSink interface {
	Emit(event models.ReactStreamEvent)
}

// noopSink discards all events (used by synchronous reactLoop).
type noopSink struct{}

func (noopSink) Emit(models.ReactStreamEvent) {}

// channelSink sends events to a buffered channel (used by streaming).
//
// droppedCtx 可选；当它被 cancel（典型场景：SSE 客户端断连）后，Emit 静默丢弃事件。
// 这样上游业务 goroutine 可以继续推进而不会因 buffered channel 满（cap=64）而阻塞挂死。
// 业务的终态结果由调用方在 sink 之外保留（写 session）。
type channelSink struct {
	ch         chan<- models.ReactStreamEvent
	droppedCtx context.Context // nil → 永远阻塞写入（同步路径或希望事件不丢的场景）
}

func (s *channelSink) Emit(e models.ReactStreamEvent) {
	if s.droppedCtx == nil {
		s.ch <- e
		return
	}
	select {
	case <-s.droppedCtx.Done():
		// 客户端已断；丢弃事件让业务继续推进。
	case s.ch <- e:
	}
}

// reactCoreOpts configures the shared ReAct loop.
type reactCoreOpts struct {
	task           *models.Task
	messages       []models.Message
	tools          []models.ToolDefinition
	maxSteps       int
	startStep      int // for auto-continue: the global step offset
	interruptCh    chan InterruptSignal
	responseFormat *models.ResponseFormat
	// tx is the per-session ToolTransaction that captures file
	// modifications. When an interrupt arrives at a step boundary, the loop
	// calls tx.Rollback() to restore dirty files to their pre-step state.
	// nil disables rollback (used by callers that don't write files).
	tx *ToolTransaction
}

// reactCoreResult holds the outcome of a single batch of ReAct steps.
type reactCoreResult struct {
	content      string
	messages     []models.Message // updated messages after loop
	done         bool             // true if LLM produced a final answer
	stepsUsed    int
	hitStepLimit bool
	// toolsUsed is the in-order sequence of tool *names* invoked during
	// the loop (one entry per tool call, dedupe-then-execute already
	// applied). Used by memory_bridge.recordTaskEpisodeAsync to capture
	// the trajectory in the per-task episodic memory; downstream consumers
	// must treat this as best-effort (some early-exit paths don't append).
	toolsUsed []string
}

// reactLoopCore runs the shared inner ReAct loop. Both reactLoop (sync) and
// ProcessMessageStreamFull (streaming) delegate here for the step-by-step
// LLM → tool → observe cycle.
func (o *Orchestrator) reactLoopCore(ctx context.Context, opts reactCoreOpts, sink reactEventSink) reactCoreResult {
	// Inject (sessionID, userID, projectID) triple into ctx — downstream
	// consumers (tools, memory_tools, observability, audit) pull these from
	// models.SessionIDFromContext / UserIDFromContext / ProjectIDFromContext
	// so no downstream code needs to re-resolve the session by ID.
	// We resolve userID/projectID once here from the cheapest source
	// (sessionMgr.Get). If the lookup fails we still inject sessionID so
	// the tool feedback / workspace resolution paths keep working.
	userID, projectID := "", ""
	if o.sessionMgr != nil && opts.task.SessionID != "" {
		if sess, err := o.sessionMgr.Get(ctx, opts.task.SessionID); err == nil && sess != nil {
			userID = sess.UserID
			projectID = sess.ProjectID
		}
	}
	ctx = models.WithSessionContext(ctx, opts.task.SessionID, userID, projectID)
	// Tool-level HITL plumbing: executeTool reads task + sink from ctx to
	// decide whether to surface an approval_request event or fall back to a
	// blocking error. Keep this scoped to the loop's ctx; downstream callers
	// (multiagent / planner) build their own ctx and won't pick it up.
	ctx = withToolApprovalContext(ctx, opts.task, sink)
	messages := opts.messages

	// execCtx is a tool-execution context derived from ctx that the interrupt
	// watchdog can cancel mid-tool. Previously the interrupt signal was only
	// observed at step boundaries, so a long-running shell_exec / sandbox.Run
	// continued until natural completion even after the user cancelled.
	//
	// Wire: watchdog reads one signal from opts.interruptCh, cancels execCtx
	// on Cancel/Pause, then re-injects the signal so the step-boundary select
	// at line ~114 also sees it (and can roll the ToolTransaction back). The
	// re-inject is non-blocking — the channel is buffered=1 and the boundary
	// reader either already consumed it (success) or is about to (we lose the
	// race but execCtx is cancelled so the next boundary check still trips on
	// ctx.Done via the toolCtx of executeTool).
	execCtx, execCancel := context.WithCancel(ctx)
	defer execCancel()
	if opts.interruptCh != nil {
		go func() {
			select {
			case sig := <-opts.interruptCh:
				if sig.Type == InterruptCancel || sig.Type == InterruptPause {
					execCancel()
				}
				select {
				case opts.interruptCh <- sig:
				default:
				}
			case <-execCtx.Done():
			}
		}()
	}

	failTracker := &consecutiveFailureTracker{}
	adaptiveFB := &agentloop.AdaptiveFeedback{}
	meta := NewMetacognitiveState()
	lastToolNames := make(map[string]int) // track tool call frequency for repeat detection
	var lastToolName string               // most recent tool executed (for sequence hints)
	var toolSequence []string             // ordered tool names for trajectory recording
	var verificationAttempted bool        // only attempt low-confidence verification once
	var loopDone bool                     // tracks if loop ended with a final answer
	globalStep := opts.startStep

	// Trigger knowledge distillation and trajectory recording on exit
	defer func() {
		if o.toolDistiller != nil {
			o.toolDistiller.Distill()
		}
		if o.trajectoryMem != nil && opts.task.Intent != "" && len(toolSequence) > 0 {
			// Errors are intentionally swallowed: trajectory recording is
			// a best-effort learning signal, not a request-blocking step.
			_ = o.trajectoryMem.Record(ctx, string(opts.task.Intent), toolSequence, loopDone)
		}
	}()

	// Inject trajectory hint from historical successful patterns
	if o.trajectoryMem != nil && opts.task.Intent != "" {
		if hint := agentloop.FormatTrajectoryHint(ctx, o.trajectoryMem, string(opts.task.Intent)); hint != "" {
			messages = append(messages, models.Message{Role: models.RoleSystem, Content: hint})
		}
	}

	for step := range opts.maxSteps {
		globalStep++

		// Check context cancellation.
		//
		// 终态事件契约（2026-06-04）：之前这里静默 return，前端永远收不到收尾信号，
		// SSE 流挂死直到 write_timeout 撕掉。现在补 emit 一条 error，让 handler 与
		// 前端都能感知到 task 已结束（loading=false / 重试入口）。
		select {
		case <-ctx.Done():
			sink.Emit(models.ReactStreamEvent{
				Type:    "error",
				TaskID:  opts.task.ID,
				Step:    globalStep,
				Content: "Task cancelled: " + ctx.Err().Error(),
			})
			return reactCoreResult{content: "Request cancelled", messages: messages, stepsUsed: step, done: true, toolsUsed: toolSequence}
		default:
		}

		// Check interrupt. Cancel and Pause both terminate the current step
		// run; on either, roll back any files touched during this step batch
		// so the user sees a clean tree. Redirect signals are not handled at
		// this layer yet (handlers pass them through, no Rollback semantics).
		if opts.interruptCh != nil {
			select {
			case sig := <-opts.interruptCh:
				content := fmt.Sprintf("Task interrupted (%s).", sig.Type)
				if (sig.Type == InterruptCancel || sig.Type == InterruptPause) && opts.tx != nil && opts.tx.HasDirtyFiles() {
					n, errs := opts.tx.Rollback()
					o.logger.Info("rolled back tool transaction on interrupt",
						zap.String("session_id", opts.task.SessionID),
						zap.String("interrupt_type", string(sig.Type)),
						zap.Int("files_rolled", n),
						zap.Int("rollback_errors", len(errs)))
					content = fmt.Sprintf("Task %s by user. Rolled back %d file(s).", sig.Type, n)
				}
				sink.Emit(models.ReactStreamEvent{Type: "message", Content: content})
				return reactCoreResult{content: content, messages: messages, stepsUsed: step, done: true, toolsUsed: toolSequence}
			default:
			}
		}

		o.logger.Debug("ReAct step", zap.String("task_id", opts.task.ID), zap.Int("step", globalStep))

		sink.Emit(models.ReactStreamEvent{Type: "step_start", Step: globalStep, TaskID: opts.task.ID, MaxSteps: opts.startStep + opts.maxSteps})

		// Proactive context compaction for long-running loops
		if o.compactionMode == "summarize" {
			compactEarlyMessages(messages, step, withSummarizeMode(o.llmClient, o.logger))
		} else {
			compactEarlyMessages(messages, step)
		}

		// Reflection checkpoint every 10 steps, plus adaptive reflection when confidence drops
		if reflection := o.reflectionCheckpoint(step, opts.maxSteps); reflection != nil {
			messages = append(messages, *reflection)
		}
		if meta.NeedsReflection() {
			messages = append(messages, *meta.AdaptiveReflectionMessage(globalStep, opts.startStep+opts.maxSteps))
		}

		// Micro-plan: periodically ask LLM to state its plan
		if plan := microPlanPrompt(step, opts.maxSteps); plan != nil {
			messages = append(messages, *plan)
		}

		// Tool learning: inject context hints from adaptive policy
		if o.toolPolicy != nil && step > 0 {
			if hint := o.toolPolicy.FormatContextHint(lastToolName); hint != "" {
				messages = append(messages, models.Message{Role: models.RoleSystem, Content: hint})
			}
		}

		// Knowledge distillation: inject strategy recommendation on first step
		if o.toolDistiller != nil && step == 0 {
			if rec := o.toolDistiller.FormatRecommendation(opts.task.UserInput); rec != "" {
				messages = append(messages, models.Message{Role: models.RoleSystem, Content: rec})
			}
		}

		// Token budget check (use dynamic context window from config)
		maxContextTokens := o.maxContextTokens
		if maxContextTokens <= 0 {
			maxContextTokens = 128000
		}
		totalTokens := 0
		for _, m := range messages {
			totalTokens += llm.ExactTokenCount(m.Content)
		}
		if totalTokens > maxContextTokens {
			o.logger.Warn("token budget exceeded, pruning", zap.Int("tokens", totalTokens), zap.Int("budget", maxContextTokens))
			messages = o.pruneMessages(messages, maxContextTokens)
		}

		// LLM call with retry
		var resp *llm.ChatResponse
		var llmErr error
		llmReq := &llm.ChatRequest{
			Messages:       messages,
			Tools:          opts.tools,
			ResponseFormat: opts.responseFormat,
		}
		o.applyModelRoute(llmReq, string(opts.task.Intent), opts.task.UserInput, len(messages))
		for attempt := range 3 {
			// LLM 进度三件套：started → 周期 progress → completed。
			// 目的：当 finalize 阶段一次 ChatCompletion 可能阻塞数分钟乃至 20+ 分钟
			// 时（非流式接口），前端 UI 会看到稳定的 elapsed_ms 心跳，不再呈现假死。
			// 进度事件通过 persistingSink 自动入 Redis Stream，resume 链路一样可见。
			resp, llmErr = callLLMWithProgress(ctx, func(c context.Context) (*llm.ChatResponse, error) {
				return o.llmClient.ChatCompletion(c, llmReq)
			}, sink, opts.task.ID, globalStep, attempt, len(messages), len(opts.tools))
			if llmErr == nil {
				break
			}
			// Skip the backoff loop on deterministic errors (auth, malformed
			// request, hard quota). Retrying them only wastes the 2^n backoff
			// budget without any chance of success. See internal/llm/retryable.go
			// for the full classification matrix.
			if !llm.IsRetryable(llmErr) {
				o.logger.Warn("LLM call failed (non-retryable), giving up",
					zap.Int("attempt", attempt+1), zap.Error(llmErr))
				break
			}
			o.logger.Warn("LLM call failed, retrying", zap.Int("attempt", attempt+1), zap.Error(llmErr))
			if attempt < 2 {
				select {
				case <-time.After(time.Duration(2<<attempt) * time.Second):
				case <-ctx.Done():
					// 终态契约：补 emit error，避免 SSE 静默挂死
					sink.Emit(models.ReactStreamEvent{
						Type:    "error",
						TaskID:  opts.task.ID,
						Step:    globalStep,
						Content: "Context cancelled during LLM retry: " + ctx.Err().Error(),
					})
					return reactCoreResult{content: "Context cancelled during LLM retry", messages: messages, stepsUsed: step, done: true, toolsUsed: toolSequence}
				}
			}
		}
		if llmErr != nil {
			sink.Emit(models.ReactStreamEvent{Type: "error", Content: "LLM call failed: " + llmErr.Error()})
			return reactCoreResult{content: "LLM call failed after 3 retries: " + llmErr.Error(), messages: messages, stepsUsed: step, done: true, toolsUsed: toolSequence}
		}

		// Emit thinking if content present with tool calls
		if resp.Content != "" {
			if len(resp.ToolCalls) > 0 {
				sink.Emit(models.ReactStreamEvent{Type: "thinking", Step: globalStep, Content: resp.Content})
			} else {
				sink.Emit(models.ReactStreamEvent{Type: "message", Step: globalStep, Content: resp.Content})
			}
		}

		// No tool calls = final answer
		if len(resp.ToolCalls) == 0 {
			// Low confidence verification: ask LLM to reconsider before committing
			if meta.NeedsReflection() && step > 2 && !verificationAttempted {
				verificationAttempted = true
				messages = append(messages, models.Message{
					Role: models.RoleAssistant, Content: resp.Content,
				})
				messages = append(messages, models.Message{
					Role:    models.RoleSystem,
					Content: "[VERIFICATION] 当前置信度较低。在给出最终回答前，请确认：(1) 是否已充分使用工具验证信息？(2) 是否有遗漏的关键步骤？如果确定当前答案正确，重复输出即可。",
				})
				continue
			}
			loopDone = true
			return reactCoreResult{content: resp.Content, messages: messages, stepsUsed: step + 1, done: true, toolsUsed: toolSequence}
		}

		// Dedupe identical ToolCalls within a single LLM step. Long ReAct loops
		// occasionally emit the same (Name, Args) twice — most commonly when a
		// high-risk tool needs HITL approval, the user approves once, and the
		// loop discovers there's a second identical call still queued and
		// stalls waiting for a second click. Dedupe upstream so both the
		// SSE stream and the LLM tool-call/tool-result correspondence stay
		// 1:1. See PR after audit: tool_approval.toolApprovalCh is keyed by
		// task.ID and rejects concurrent registrations, so a duplicate would
		// previously bubble back as an error to the LLM mid-step.
		dedupedCalls := agentloop.DedupeToolCalls(resp.ToolCalls, o.logger)

		// Append assistant message using the deduped set so the LLM history
		// shows exactly the calls we're going to execute (otherwise the model
		// sees N tool_calls in the assistant turn but only K < N matching
		// tool results and may re-emit the missing ones).
		messages = append(messages, models.Message{
			Role: models.RoleAssistant, Content: resp.Content, ToolCalls: dedupedCalls,
		})

		// Execute tools — parallel for read-only batches, sequential otherwise
		var editedFilePaths []string

		// Emit all tool_call events upfront (over deduped set so the UI
		// doesn't render phantom duplicate cards).
		for _, tc := range dedupedCalls {
			sink.Emit(models.ReactStreamEvent{
				Type: "tool_call", Step: globalStep,
				ToolName: tc.Name, ToolArgs: string(tc.Args), ToolCallID: tc.ID,
			})
		}

		type processedResult struct {
			tc       models.ToolCall
			content  string
			isErr    bool
			metadata json.RawMessage
		}

		var processed []processedResult

		if canParallelExecute(dedupedCalls) {
			// All tools are idempotent — execute concurrently. Pass execCtx
			// so cancel/pause cuts hung workers, not just step boundaries.
			parallelResults := o.parallelExecuteTools(execCtx, dedupedCalls)
			for _, pr := range parallelResults {
				// parallelExecuteTools may return zero-valued slots if execCtx
				// was cancelled before that worker finished. Skip those —
				// feeding empty tool results back to the LLM would confuse it.
				if pr.tc.Name == "" {
					continue
				}
				var content string
				var meta json.RawMessage
				if pr.result != nil {
					content = pr.result.Content
					meta = pr.result.Metadata
				}
				if pr.execErr != nil {
					content = fmt.Sprintf("Error: %v", pr.execErr)
				}
				processed = append(processed, processedResult{tc: pr.tc, content: content, isErr: (pr.execErr != nil) || (pr.result != nil && pr.result.IsError), metadata: meta})
			}
			o.logger.Debug("parallel tool execution", zap.Int("count", len(dedupedCalls)))
		} else {
			// Sequential execution for write tools or single calls
			for _, tc := range dedupedCalls {
				// Step-internal interrupt gate: when a step has N tool calls
				// and a cancel arrives between call 1 and call 2, we want to
				// stop without firing call 2. Before this check, the for loop
				// ran to completion and only the next step-boundary select
				// noticed the cancellation.
				select {
				case <-execCtx.Done():
					o.logger.Info("tool batch cancelled mid-sequence", zap.String("session_id", opts.task.SessionID))
				default:
				}
				if execCtx.Err() != nil {
					break
				}
				// Inject progress callback so long-running tools can stream output
				toolCtx := WithProgressCallback(execCtx, func(chunk string) {
					sink.Emit(models.ReactStreamEvent{
						Type:       "tool_progress",
						Step:       globalStep,
						ToolCallID: tc.ID,
						ToolName:   tc.Name,
						Content:    chunk,
					})
				})
				result, execErr := o.executeTool(toolCtx, tc)
				content := result.Content
				if execErr != nil {
					content = fmt.Sprintf("Error: %v", execErr)
				}

				// Track edited files for auto-test. Uses the centralized
				// TriggersAutoTest metadata bit instead of a hardcoded name list,
				// so adding a new file-write tool (rename_symbol, future ones)
				// requires no edits here.
				if execErr == nil && !result.IsError && o.triggersAutoTest(tc.Name) {
					var pathReq struct {
						Path string `json:"path"`
					}
					if json.Unmarshal(tc.Args, &pathReq) == nil && pathReq.Path != "" {
						editedFilePaths = append(editedFilePaths, pathReq.Path)
					}
				}

				var meta json.RawMessage
				if result != nil {
					meta = result.Metadata
				}
				isErr := (execErr != nil) || (result != nil && result.IsError) || strings.Contains(content, "❌ Command FAILED")
				processed = append(processed, processedResult{tc: tc, content: content, isErr: isErr, metadata: meta})
			}
		}

		// Post-process all results uniformly
		for i := range processed {
			pr := &processed[i]

			// Smart truncation
			if llm.FastEstimate(pr.content) > 8000 {
				runes := []rune(pr.content)
				if len(runes) > 32000 {
					headSize := 8000
					tailSize := 12000
					pr.content = string(runes[:headSize]) +
						"\n\n... [middle truncated — " + fmt.Sprintf("%d", len(runes)-headSize-tailSize) + " chars omitted] ...\n\n" +
						string(runes[len(runes)-tailSize:])
				}
			}

			if !pr.isErr {
				pr.isErr = strings.Contains(pr.content, "❌ Command FAILED")
			}

			// Structured error classification and adaptive feedback
			if pr.isErr {
				toolErr := agentloop.ClassifyToolError(pr.tc.Name, &models.ToolResult{Content: pr.content, IsError: true}, nil)
				adaptiveFB.Record(toolErr)
				feedback := adaptiveFB.BuildFeedback(toolErr)
				pr.content += "\n\n[SYSTEM HINT] " + feedback
			} else {
				adaptiveFB.RecordSuccess(pr.tc.Name)
			}

			// Record outcome in metacognitive state
			lastToolNames[pr.tc.Name]++
			meta.RecordOutcome(pr.tc.Name, !pr.isErr, lastToolNames[pr.tc.Name] > 1 && pr.isErr)
			if pr.isErr {
				meta.AddUncertainty("recent tool failure: " + pr.tc.Name)
			}

			// Pass tool-level structured payload through unchanged so the UI
			// can render artifacts (e.g. edit_file's unified diff) alongside
			// the human-readable content. ReactStreamEvent.Metadata is
			// `interface{}` but the SSE encoder marshals json.RawMessage
			// verbatim, so the front-end sees the original object shape.
			sink.Emit(models.ReactStreamEvent{
				Type: "tool_result", Step: globalStep,
				ToolName: pr.tc.Name, ToolCallID: pr.tc.ID,
				Content: pr.content, IsError: pr.isErr,
				Metadata: pr.metadata,
			})

			messages = append(messages, models.Message{
				Role: models.RoleTool, Content: pr.content, ToolCallID: pr.tc.ID,
			})

			// Failure tracking
			if failTracker.track(pr.tc.Name, pr.isErr) {
				o.logger.Warn("fix loop detected", zap.String("tool", pr.tc.Name), zap.Int("failures", failTracker.failCount))
				messages = append(messages, failTracker.stepBackMessage())
			}
			lastToolName = pr.tc.Name
			toolSequence = append(toolSequence, pr.tc.Name)

			// Hard abort: the step-back prompt was already injected at the
			// warn threshold; if the tool keeps failing past the abort
			// threshold the LLM is clearly stuck (hallucinated tool name,
			// non-existent file, broken env) — bail out instead of letting
			// max_steps auto-expand and burn the whole budget.
			if failTracker.shouldAbort() {
				o.logger.Warn("fix loop hard abort",
					zap.String("tool", pr.tc.Name),
					zap.Int("failures", failTracker.failCount),
					zap.Int("step", globalStep))
				sink.Emit(models.ReactStreamEvent{
					Type:    "fix_loop_abort",
					Step:    globalStep,
					Content: fmt.Sprintf("ReAct loop aborted: tool '%s' failed %d times in a row.", pr.tc.Name, failTracker.failCount),
				})
				return reactCoreResult{messages: messages, stepsUsed: step + 1, hitStepLimit: false, toolsUsed: toolSequence}
			}
		}

		// Auto-test after file edits
		if len(editedFilePaths) > 0 {
			if testResult := o.RunAutoTestAfterEdit(ctx, editedFilePaths); testResult != nil {
				if msg := testResult.FormatForLLM(); msg != "" {
					messages = append(messages, models.Message{Role: models.RoleSystem, Content: msg})
				}
			}
		}

		// Update tool policy every 5 steps
		if o.toolPolicy != nil && step > 0 && step%5 == 0 {
			o.toolPolicy.Update()
		}
	}

	// Step limit exhausted
	return reactCoreResult{messages: messages, stepsUsed: opts.maxSteps, hitStepLimit: true, toolsUsed: toolSequence}
}

// llmProgressInterval 控制 llm_call_progress 心跳节奏。
// 3s 是 UX 与 Redis Stream 体积的折中：长任务 (20min) 仅 ~400 条事件、~32KB,
// 远低于 streamMaxLen=2000。改更短会增加 Redis 压力;更长则 UI 感知会变迟钝。
// 包级 var(非 const)是为了让测试可以缩短 interval 验证 progress 真在跳。
var llmProgressInterval = 3 * time.Second

// callLLMWithProgress 包一层 llm_call_started/progress/completed 事件,
// 让前端在 finalize 阶段(LLM 非流式调用阻塞数分钟)仍能看到稳定 elapsed_ms 心跳。
// 进度 goroutine 在 LLM 返回后立即收敛(progressCancel + 等 progressDone),
// 不留泄漏。事件经由 persistingSink 自动入 Redis Stream,resume 链路同样可见。
//
// call 参数注入实际 LLM 调用 —— 让本函数纯粹只管进度三件套,不绑 orchestrator
// 状态,便于单元测试用 stub 注入慢响应验证 progress 心跳真的在跳。
func callLLMWithProgress(
	ctx context.Context,
	call func(context.Context) (*llm.ChatResponse, error),
	sink reactEventSink,
	taskID string,
	globalStep int,
	attempt int,
	messageCount int,
	toolCount int,
) (*llm.ChatResponse, error) {
	startedAt := time.Now()
	sink.Emit(models.ReactStreamEvent{
		Type: "llm_call_started", TaskID: taskID, Step: globalStep,
		Content: fmt.Sprintf(`{"attempt":%d,"messages":%d,"tools":%d}`, attempt+1, messageCount, toolCount),
	})

	progressCtx, progressCancel := context.WithCancel(ctx)
	progressDone := make(chan struct{})
	go func() {
		defer close(progressDone)
		ticker := time.NewTicker(llmProgressInterval)
		defer ticker.Stop()
		for {
			select {
			case <-progressCtx.Done():
				return
			case <-ticker.C:
				sink.Emit(models.ReactStreamEvent{
					Type: "llm_call_progress", TaskID: taskID, Step: globalStep,
					Content: fmt.Sprintf(`{"attempt":%d,"elapsed_ms":%d}`, attempt+1, time.Since(startedAt).Milliseconds()),
				})
			}
		}
	}()

	resp, err := call(ctx)
	progressCancel()
	<-progressDone

	sink.Emit(models.ReactStreamEvent{
		Type: "llm_call_completed", TaskID: taskID, Step: globalStep,
		Content: fmt.Sprintf(`{"attempt":%d,"elapsed_ms":%d,"err":%t}`, attempt+1, time.Since(startedAt).Milliseconds(), err != nil),
	})
	return resp, err
}
