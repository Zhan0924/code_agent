package orchestrator

import (
	"testing"

	"github.com/agent/code_agent/internal/models"
)

// TestBuiltinToolsHaveMetadata is a CI guard: every builtin tool should
// declare at least one behavior metadata bit (IsFileWrite / IsIdempotentRead
// / TriggersAutoTest / InvalidatesCache), otherwise it falls through every
// downstream consumer (auto-test, transaction capture, cache invalidation,
// supervisor conflict detection) as a no-op — silently broken.
//
// `create_directory`, `run_tests`, `run_workspace_cmd`, `execute_code`,
// `search_code` are intentional exemptions: they are neither file writes
// nor cacheable reads. Any addition to this allowlist must be reviewed.
func TestBuiltinToolsHaveMetadata(t *testing.T) {
	exempt := map[string]struct{}{
		"create_directory":  {}, // mkdir only — no auto-test, not cacheable
		"run_tests":         {}, // sandboxed exec — not cacheable, not file-write
		"run_workspace_cmd": {}, // host exec — not cacheable, not file-write
		"execute_code":      {}, // sandboxed exec
		"search_code":       {}, // semantic search — non-deterministic enough that caching can hide reindex
	}

	defs := append([]models.ToolDefinition{}, fileToolDefinitions()...)
	defs = append(defs, gitToolDefinitions()...)

	for _, def := range defs {
		if def.Source != "builtin" {
			t.Errorf("builtin tool %q has Source=%q, expected 'builtin'", def.Name, def.Source)
		}
		if _, ok := exempt[def.Name]; ok {
			continue
		}
		hasAnyBit := def.IsFileWrite || def.IsIdempotentRead || def.TriggersAutoTest || def.InvalidatesCache
		if !hasAnyBit {
			t.Errorf("builtin tool %q declares no behavior metadata (IsFileWrite / IsIdempotentRead / TriggersAutoTest / InvalidatesCache). Add the appropriate bit at the definition site, or add to the exempt list with a comment.", def.Name)
		}
	}
}

// TestFileWriteToolsTriggerAutoTest checks the invariant that any tool
// marked IsFileWrite=true also has TriggersAutoTest=true and
// InvalidatesCache=true — otherwise writes silently slip past the auto-test
// runner or leave stale read cache behind (the exact bug that motivated
// centralization).
func TestFileWriteToolsTriggerAutoTest(t *testing.T) {
	defs := append([]models.ToolDefinition{}, fileToolDefinitions()...)
	defs = append(defs, gitToolDefinitions()...)

	for _, def := range defs {
		if !def.IsFileWrite {
			continue
		}
		if !def.TriggersAutoTest {
			t.Errorf("tool %q marks IsFileWrite=true but TriggersAutoTest=false — auto-test runner will skip changes from this tool", def.Name)
		}
		if !def.InvalidatesCache {
			t.Errorf("tool %q marks IsFileWrite=true but InvalidatesCache=false — speculative read cache will serve stale results after writes", def.Name)
		}
	}
}
