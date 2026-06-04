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
//
// Race-free snapshot: each worker publishes through its own buffer-1 channel
// rather than writing into a shared slice. After ctx.Done we drain whatever
// has already been delivered into a freshly-allocated []toolExecResult and
// return; late workers send into their buffered slot and exit (no blocking,
// no goroutine retains a reference to the returned slice). This closes the
// gap the prior "per-index slot isolation" assumption left open — the caller
// could iterate the returned slice concurrently with a late worker's write.
func (o *Orchestrator) parallelExecuteTools(ctx context.Context, toolCalls []models.ToolCall) []toolExecResult {
	// Per-slot channel so a late worker writing after we return doesn't race
	// the caller iterating the returned slice. Buffer=1 so the worker can
	// always send and exit even if no one drains the channel.
	slots := make([]chan toolExecResult, len(toolCalls))
	for i := range slots {
		slots[i] = make(chan toolExecResult, 1)
	}
	var wg sync.WaitGroup
	wg.Add(len(toolCalls))

	for i, tc := range toolCalls {
		go func(idx int, call models.ToolCall) {
			defer wg.Done()
			result, err := o.executeTool(ctx, call)
			slots[idx] <- toolExecResult{
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
		// Caller cancelled; fall through to the drain loop below. Workers
		// that have not yet finished will eventually publish into their
		// buffered slot and exit on their own.
	}

	out := make([]toolExecResult, len(toolCalls))
	for i, ch := range slots {
		select {
		case r := <-ch:
			out[i] = r
		default:
			// Worker hasn't published yet — leave zero-valued. react_core
			// skips entries with empty tc.Name to avoid feeding half-baked
			// results back to the LLM.
		}
	}
	return out
}
