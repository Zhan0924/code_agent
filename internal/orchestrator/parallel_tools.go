package orchestrator

import (
	"context"
	"sync"

	"github.com/agent/code_agent/internal/models"
)

// toolExecResult holds the outcome of a single parallel tool execution.
type toolExecResult struct {
	index   int
	tc      models.ToolCall
	result  *models.ToolResult
	execErr error
}

// canParallelExecute returns true when all tool calls in the batch are idempotent read-only tools.
func canParallelExecute(toolCalls []models.ToolCall) bool {
	if len(toolCalls) <= 1 {
		return false
	}
	for _, tc := range toolCalls {
		if !IsIdempotentTool(tc.Name) {
			return false
		}
	}
	return true
}

// parallelExecuteTools runs all tool calls concurrently and returns results in original order.
//
// Cancellation: previously wg.Wait was bare, so a hung worker (e.g., a slow
// RAG retrieval inside a read_file call) would pin the loop even after the
// caller's ctx was cancelled. Now wg.Wait races against ctx.Done — when ctx
// is cancelled we return immediately with whatever results already landed.
// Late-arriving goroutines still write into their own index slot (per-index
// slot isolation makes the late write race-free), but the caller has already
// taken its copy of `results` and moved on.
func (o *Orchestrator) parallelExecuteTools(ctx context.Context, toolCalls []models.ToolCall) []toolExecResult {
	results := make([]toolExecResult, len(toolCalls))
	var wg sync.WaitGroup
	wg.Add(len(toolCalls))

	for i, tc := range toolCalls {
		go func(idx int, call models.ToolCall) {
			defer wg.Done()
			result, err := o.executeTool(ctx, call)
			results[idx] = toolExecResult{
				index:   idx,
				tc:      call,
				result:  result,
				execErr: err,
			}
		}(i, tc)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		// Caller cancelled; surface whatever has already completed. Empty
		// slots stay zero-valued (ToolCall name empty), and the calling
		// loop in react_core treats those as a no-op rather than feeding
		// half-finished tool output back to the LLM.
	}
	return results
}
