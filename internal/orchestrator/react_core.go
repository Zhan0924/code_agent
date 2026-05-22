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
type channelSink struct {
	ch chan<- models.ReactStreamEvent
}

func (s *channelSink) Emit(e models.ReactStreamEvent) { s.ch <- e }

// reactCoreOpts configures the shared ReAct loop.
type reactCoreOpts struct {
	task        *models.Task
	messages    []models.Message
	tools       []models.ToolDefinition
	maxSteps    int
	startStep   int // for auto-continue: the global step offset
	interruptCh chan InterruptSignal
}

// reactCoreResult holds the outcome of a single batch of ReAct steps.
type reactCoreResult struct {
	content      string
	messages     []models.Message // updated messages after loop
	done         bool             // true if LLM produced a final answer
	stepsUsed    int
	hitStepLimit bool
}

// reactLoopCore runs the shared inner ReAct loop. Both reactLoop (sync) and
// ProcessMessageStreamFull (streaming) delegate here for the step-by-step
// LLM → tool → observe cycle.
func (o *Orchestrator) reactLoopCore(ctx context.Context, opts reactCoreOpts, sink reactEventSink) reactCoreResult {
	// Inject session ID into context for downstream use (e.g., tool feedback recording)
	ctx = context.WithValue(ctx, ctxKeySessionID, opts.task.SessionID)
	messages := opts.messages

	// Trigger knowledge distillation on exit (regardless of success/failure/interrupt)
	defer func() {
		if o.toolDistiller != nil {
			o.toolDistiller.Distill()
		}
	}()
	failTracker := &consecutiveFailureTracker{}
	adaptiveFB := &agentloop.AdaptiveFeedback{}
	meta := NewMetacognitiveState()
	lastToolNames := make(map[string]int) // track tool call frequency for repeat detection
	var lastToolName string               // most recent tool executed (for sequence hints)
	globalStep := opts.startStep

	for step := range opts.maxSteps {
		globalStep++

		// Check context cancellation
		select {
		case <-ctx.Done():
			return reactCoreResult{content: "Request cancelled", messages: messages, stepsUsed: step, done: true}
		default:
		}

		// Check interrupt
		if opts.interruptCh != nil {
			select {
			case sig := <-opts.interruptCh:
				sink.Emit(models.ReactStreamEvent{Type: "message", Content: fmt.Sprintf("Task interrupted (%s).", sig.Type)})
				return reactCoreResult{content: fmt.Sprintf("Task interrupted (%s).", sig.Type), messages: messages, stepsUsed: step, done: true}
			default:
			}
		}

		o.logger.Debug("ReAct step", zap.String("task_id", opts.task.ID), zap.Int("step", globalStep))

		sink.Emit(models.ReactStreamEvent{Type: "step_start", Step: globalStep, TaskID: opts.task.ID, MaxSteps: opts.startStep + opts.maxSteps})

		// Proactive context compaction for long-running loops
		compactEarlyMessages(messages, step)

		// Reflection checkpoint every 10 steps, plus adaptive reflection when confidence drops
		if reflection := o.reflectionCheckpoint(step, opts.maxSteps); reflection != nil {
			messages = append(messages, *reflection)
		}
		if meta.NeedsReflection() {
			messages = append(messages, *meta.AdaptiveReflectionMessage(globalStep, opts.startStep+opts.maxSteps))
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

		// Token budget check
		const maxContextTokens = 128000
		totalTokens := 0
		for _, m := range messages {
			totalTokens += llm.EstimateTokens(m.Content)
		}
		if totalTokens > maxContextTokens {
			o.logger.Warn("token budget exceeded, pruning", zap.Int("tokens", totalTokens))
			messages = o.pruneMessages(messages, maxContextTokens)
		}

		// LLM call with retry
		var resp *llm.ChatResponse
		var llmErr error
		for attempt := range 3 {
			resp, llmErr = o.llmClient.ChatCompletion(ctx, &llm.ChatRequest{
				Messages: messages, Tools: opts.tools,
			})
			if llmErr == nil {
				break
			}
			o.logger.Warn("LLM call failed, retrying", zap.Int("attempt", attempt+1), zap.Error(llmErr))
			if attempt < 2 {
				select {
				case <-time.After(time.Duration(2<<attempt) * time.Second):
				case <-ctx.Done():
					return reactCoreResult{content: "Context cancelled during LLM retry", messages: messages, stepsUsed: step, done: true}
				}
			}
		}
		if llmErr != nil {
			sink.Emit(models.ReactStreamEvent{Type: "error", Content: "LLM call failed: " + llmErr.Error()})
			return reactCoreResult{content: "LLM call failed after 3 retries: " + llmErr.Error(), messages: messages, stepsUsed: step, done: true}
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
			return reactCoreResult{content: resp.Content, messages: messages, stepsUsed: step + 1, done: true}
		}

		// Append assistant message
		messages = append(messages, models.Message{
			Role: models.RoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls,
		})

		// Execute tools — parallel for read-only batches, sequential otherwise
		var editedFilePaths []string

		// Emit all tool_call events upfront
		for _, tc := range resp.ToolCalls {
			sink.Emit(models.ReactStreamEvent{
				Type: "tool_call", Step: globalStep,
				ToolName: tc.Name, ToolArgs: string(tc.Args), ToolCallID: tc.ID,
			})
		}

		type processedResult struct {
			tc      models.ToolCall
			content string
			isErr   bool
		}

		var processed []processedResult

		if canParallelExecute(resp.ToolCalls) {
			// All tools are idempotent — execute concurrently
			parallelResults := o.parallelExecuteTools(ctx, resp.ToolCalls)
			for _, pr := range parallelResults {
				content := pr.result.Content
				if pr.execErr != nil {
					content = fmt.Sprintf("Error: %v", pr.execErr)
				}
				processed = append(processed, processedResult{tc: pr.tc, content: content, isErr: (pr.execErr != nil) || (pr.result != nil && pr.result.IsError)})
			}
			o.logger.Debug("parallel tool execution", zap.Int("count", len(resp.ToolCalls)))
		} else {
			// Sequential execution for write tools or single calls
			for _, tc := range resp.ToolCalls {
				result, execErr := o.executeTool(ctx, tc)
				content := result.Content
				if execErr != nil {
					content = fmt.Sprintf("Error: %v", execErr)
				}

				// Track edited files for auto-test
				if (tc.Name == "edit_file" || tc.Name == "write_file" || tc.Name == "patch_file") && execErr == nil && !result.IsError {
					var pathReq struct {
						Path string `json:"path"`
					}
					if json.Unmarshal(tc.Args, &pathReq) == nil && pathReq.Path != "" {
						editedFilePaths = append(editedFilePaths, pathReq.Path)
					}
				}

				isErr := (execErr != nil) || (result != nil && result.IsError) || strings.Contains(content, "❌ Command FAILED")
				processed = append(processed, processedResult{tc: tc, content: content, isErr: isErr})
			}
		}

		// Post-process all results uniformly
		for i := range processed {
			pr := &processed[i]

			// Smart truncation
			if llm.EstimateTokens(pr.content) > 8000 {
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
			}

			// Record outcome in metacognitive state
			lastToolNames[pr.tc.Name]++
			meta.RecordOutcome(pr.tc.Name, !pr.isErr, lastToolNames[pr.tc.Name] > 1 && pr.isErr)
			if pr.isErr {
				meta.AddUncertainty("recent tool failure: " + pr.tc.Name)
			}

			sink.Emit(models.ReactStreamEvent{
				Type: "tool_result", Step: globalStep,
				ToolName: pr.tc.Name, ToolCallID: pr.tc.ID,
				Content: pr.content, IsError: pr.isErr,
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
	return reactCoreResult{messages: messages, stepsUsed: opts.maxSteps, hitStepLimit: true}
}
