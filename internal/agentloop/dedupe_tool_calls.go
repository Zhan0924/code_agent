// Package agentloop — dedupe_tool_calls.go drops duplicate ToolCalls
// emitted by the LLM within a single step's tool_calls array.
//
// Lives in agentloop (not orchestrator) so the standalone Runner
// (agentloop/runner.go) and the full ReAct loop (orchestrator/react_core.go)
// can both call the same canonical implementation without an import cycle —
// orchestrator already imports agentloop for AdaptiveFeedback and friends.
//
// Why this is needed:
//
// In long ReAct loops (step 40+) the LLM occasionally emits the same
// (Name, Args) pair twice in one response — sometimes a model artefact,
// sometimes a retry hallucination after a tool error in a prior step. The
// downstream code paths break in different ways without dedupe:
//
//   - Tool-level HITL (tool_approval.go:waitToolApproval) keys its pending
//     channel by task.ID. A second identical call attempted concurrently
//     fails the dup-detection branch with an error fed back to the LLM as a
//     synthetic tool result, which the model then often re-emits, locking
//     the loop. Even when the calls run sequentially, the user has to click
//     Approve twice for the same operation, which is confusing and slow.
//
//   - Parallel idempotent batches (canParallelExecute) waste workers on
//     redundant reads.
//
//   - The SSE UI renders one card per emitted tool_call event, so duplicates
//     produce phantom rows that imply the model is running a tool twice
//     when it isn't.
//
// Strategy: dedupe by a canonical key built from the tool name plus a JSON
// canonicalisation of the args (so semantically-equivalent argument blobs
// with different key orderings still collapse). Keep the first call's ID so
// the assistant message → tool result correspondence stays 1:1, log the drop
// for observability.
package agentloop

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/agent/code_agent/internal/models"
	"go.uber.org/zap"
)

// DedupeToolCalls returns a slice with duplicates removed in original order.
// A duplicate is defined as having an identical (Name, canonical-Args-JSON)
// key. The first occurrence wins; subsequent occurrences are dropped with an
// info-level log entry so we can observe the rate in production.
//
// If logger is nil, the function still works — it just doesn't log.
func DedupeToolCalls(calls []models.ToolCall, logger *zap.Logger) []models.ToolCall {
	if len(calls) <= 1 {
		return calls
	}
	seen := make(map[string]string, len(calls))
	out := make([]models.ToolCall, 0, len(calls))
	for _, tc := range calls {
		key := tc.Name + "\x00" + canonicalArgsKey(tc.Args)
		if firstID, dup := seen[key]; dup {
			if logger != nil {
				logger.Info("dropped duplicate tool call within step",
					zap.String("tool", tc.Name),
					zap.String("dropped_id", tc.ID),
					zap.String("kept_id", firstID),
				)
			}
			continue
		}
		seen[key] = tc.ID
		out = append(out, tc)
	}
	return out
}

// canonicalArgsKey returns a stable hash of the args JSON. The hash is over
// (sorted-key form, value) so `{"a":1,"b":2}` and `{"b":2,"a":1}` collide
// — JSON key order is unstable in LLM output and we don't want that to
// defeat dedupe.
//
// For non-JSON or malformed args we fall back to a hash of the raw bytes —
// good enough because two raw-equal blobs still match.
func canonicalArgsKey(args []byte) string {
	if len(args) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(args, &v); err != nil {
		// Not valid JSON — hash the raw bytes verbatim so byte-equal calls
		// still match. Worst-case behaviour: a malformed-args duplicate gets
		// merged. Acceptable.
		sum := sha256.Sum256(args)
		return hex.EncodeToString(sum[:])
	}
	canon, err := marshalCanonicalJSON(v)
	if err != nil {
		sum := sha256.Sum256(args)
		return hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}

// marshalCanonicalJSON re-encodes v with object keys sorted recursively so
// the output bytes are stable regardless of input key order. Built on the
// stdlib JSON encoder; the only non-trivial path is the map[string]any case
// where we sort keys before re-serialising.
func marshalCanonicalJSON(v any) ([]byte, error) {
	canon := canonicalise(v)
	return json.Marshal(canon)
}

func canonicalise(v any) any {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		// Encoding map[string]any directly gives unsorted output (Go
		// guarantees nothing about map iteration order). Re-wrap as a slice
		// of [key, value] pairs and emit as a JSON object via a custom
		// encode would be cleaner, but stdlib json.Marshal on a
		// map[string]any sorts keys lexicographically as of Go 1.12. Still,
		// we recurse into values so nested maps get canonicalised too.
		out := make(map[string]any, len(x))
		for _, k := range keys {
			out[k] = canonicalise(x[k])
		}
		return out
	case []any:
		for i, e := range x {
			x[i] = canonicalise(e)
		}
		return x
	default:
		return v
	}
}
