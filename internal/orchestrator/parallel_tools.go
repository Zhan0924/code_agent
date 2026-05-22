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

	wg.Wait()
	return results
}
