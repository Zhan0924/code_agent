package agentloop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/agent/code_agent/internal/llm"
	"github.com/agent/code_agent/internal/models"
	"go.uber.org/zap"
)

// RunOpts configures a single Run invocation.
type RunOpts struct {
	SystemPrompt string
	Messages     []models.Message
	TaskID       string
}

// RunResult holds the outcome of a ReAct loop run.
type RunResult struct {
	Content      string
	Messages     []models.Message
	Done         bool
	StepsUsed    int
	HitStepLimit bool
}

// Runner executes a ReAct loop using injected dependencies.
type Runner struct {
	llm      LLMCaller
	toolExec ToolExecutor
	toolProv ToolProvider
	config   Config
	logger   *zap.Logger
}

// NewRunner creates a new ReAct loop runner.
func NewRunner(llmCaller LLMCaller, toolExec ToolExecutor, toolProv ToolProvider, config Config, logger *zap.Logger) *Runner {
	return &Runner{
		llm:      llmCaller,
		toolExec: toolExec,
		toolProv: toolProv,
		config:   config,
		logger:   logger,
	}
}

// Run executes the ReAct loop until completion or step limit.
func (r *Runner) Run(ctx context.Context, opts RunOpts, sink EventSink) RunResult {
	if sink == nil {
		sink = NoopSink{}
	}

	messages := opts.Messages
	tools := r.toolProv.Definitions()

	failTracker := &ConsecutiveFailureTracker{}
	adaptiveFB := &AdaptiveFeedback{}
	meta := NewMetacognitiveState()
	lastToolNames := make(map[string]int)

	for step := range r.config.MaxSteps {
		select {
		case <-ctx.Done():
			return RunResult{Content: "Request cancelled", Messages: messages, StepsUsed: step, Done: true}
		default:
		}

		r.logger.Debug("agentloop step", zap.String("task_id", opts.TaskID), zap.Int("step", step+1))
		sink.Emit(models.ReactStreamEvent{Type: "step_start", Step: step + 1, TaskID: opts.TaskID, MaxSteps: r.config.MaxSteps})

		// Reflection checkpoint (if enabled)
		if r.config.EnableReflection {
			if step > 0 && step%10 == 0 {
				msg := meta.AdaptiveReflectionMessage(step+1, r.config.MaxSteps)
				messages = append(messages, *msg)
			}
			if meta.NeedsReflection() {
				messages = append(messages, *meta.AdaptiveReflectionMessage(step+1, r.config.MaxSteps))
			}
		}

		// Token budget check
		totalTokens := 0
		for _, m := range messages {
			totalTokens += llm.EstimateTokens(m.Content)
		}
		if totalTokens > r.config.MaxContextTokens {
			r.logger.Warn("token budget exceeded, pruning", zap.Int("tokens", totalTokens))
			messages = PruneMessages(messages, r.config.MaxContextTokens)
		}

		// LLM call with retry
		var resp *llm.ChatResponse
		var llmErr error
		retries := r.config.LLMRetries
		if retries < 1 {
			retries = 1
		}
		for attempt := range retries {
			resp, llmErr = r.llm.ChatCompletion(ctx, &llm.ChatRequest{
				Messages: messages, Tools: tools,
			})
			if llmErr == nil {
				break
			}
			r.logger.Warn("LLM call failed, retrying", zap.Int("attempt", attempt+1), zap.Error(llmErr))
			if attempt < retries-1 {
				select {
				case <-time.After(time.Duration(2<<attempt) * time.Second):
				case <-ctx.Done():
					return RunResult{Content: "Context cancelled during LLM retry", Messages: messages, StepsUsed: step, Done: true}
				}
			}
		}
		if llmErr != nil {
			sink.Emit(models.ReactStreamEvent{Type: "error", Content: "LLM call failed: " + llmErr.Error()})
			return RunResult{Content: "LLM call failed after retries: " + llmErr.Error(), Messages: messages, StepsUsed: step, Done: true}
		}

		// Emit thinking / final message
		if resp.Content != "" {
			if len(resp.ToolCalls) > 0 {
				sink.Emit(models.ReactStreamEvent{Type: "thinking", Step: step + 1, Content: resp.Content})
			} else {
				sink.Emit(models.ReactStreamEvent{Type: "message", Step: step + 1, Content: resp.Content})
			}
		}

		// No tool calls = final answer
		if len(resp.ToolCalls) == 0 {
			return RunResult{Content: resp.Content, Messages: messages, StepsUsed: step + 1, Done: true}
		}

		// Append assistant message
		messages = append(messages, models.Message{
			Role: models.RoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls,
		})

		// Execute tools
		for _, tc := range resp.ToolCalls {
			sink.Emit(models.ReactStreamEvent{
				Type: "tool_call", Step: step + 1,
				ToolName: tc.Name, ToolArgs: string(tc.Args), ToolCallID: tc.ID,
			})

			result, execErr := r.toolExec.Execute(ctx, tc)
			content := ""
			if result != nil {
				content = result.Content
			}
			if execErr != nil {
				content = fmt.Sprintf("Error: %v", execErr)
			}

			// Smart truncation
			if llm.EstimateTokens(content) > 8000 {
				runes := []rune(content)
				if len(runes) > 32000 {
					headSize := 8000
					tailSize := 12000
					content = string(runes[:headSize]) +
						"\n\n... [middle truncated — " + fmt.Sprintf("%d", len(runes)-headSize-tailSize) + " chars omitted] ...\n\n" +
						string(runes[len(runes)-tailSize:])
				}
			}

			isErr := (execErr != nil) || (result != nil && result.IsError) || strings.Contains(content, "❌ Command FAILED")

			// Structured error classification and adaptive feedback
			if isErr {
				toolErr := ClassifyToolError(tc.Name, result, execErr)
				adaptiveFB.Record(toolErr)
				feedback := adaptiveFB.BuildFeedback(toolErr)
				content += "\n\n[SYSTEM HINT] " + feedback
			}

			// Metacognitive tracking
			lastToolNames[tc.Name]++
			meta.RecordOutcome(tc.Name, !isErr, lastToolNames[tc.Name] > 1 && isErr)
			if isErr {
				meta.AddUncertainty("recent tool failure: " + tc.Name)
			}

			sink.Emit(models.ReactStreamEvent{
				Type: "tool_result", Step: step + 1,
				ToolName: tc.Name, ToolCallID: tc.ID,
				Content: content, IsError: isErr,
			})

			messages = append(messages, models.Message{
				Role: models.RoleTool, Content: content, ToolCallID: tc.ID,
			})

			// Failure tracking
			if failTracker.Track(tc.Name, isErr) {
				r.logger.Warn("fix loop detected", zap.String("tool", tc.Name), zap.Int("failures", failTracker.FailCount))
				messages = append(messages, failTracker.StepBackMessage())
			}
		}
	}

	return RunResult{Messages: messages, StepsUsed: r.config.MaxSteps, HitStepLimit: true}
}

// truncateToolResult is a helper used by the orchestrator adapter for
// extracting file paths from tool args.
func ExtractFilePaths(toolCalls []models.ToolCall) []string {
	var paths []string
	for _, tc := range toolCalls {
		if tc.Name == "edit_file" || tc.Name == "write_file" || tc.Name == "patch_file" {
			var pathReq struct {
				Path string `json:"path"`
			}
			if json.Unmarshal(tc.Args, &pathReq) == nil && pathReq.Path != "" {
				paths = append(paths, pathReq.Path)
			}
		}
	}
	return paths
}
