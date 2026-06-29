package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/agent/code_agent/internal/memory"
)

// staticMemoryRetriever returns a hard-coded set of MemoryEntry rows for
// any Retrieve / RetrieveByType call so we can assert on the prompt /
// audit log without spinning up Redis or PG.
type staticMemoryRetriever struct {
	byType  map[string][]MemoryEntry
	generic []MemoryEntry
}

func (s *staticMemoryRetriever) Retrieve(_ context.Context, _, _, _ string, limit int) ([]MemoryEntry, error) {
	if len(s.generic) > limit {
		return s.generic[:limit], nil
	}
	return s.generic, nil
}

func (s *staticMemoryRetriever) RetrieveByType(_ context.Context, _, _, memType, _ string, limit int) ([]MemoryEntry, error) {
	mems := s.byType[memType]
	if len(mems) > limit {
		return mems[:limit], nil
	}
	return mems, nil
}

func (s *staticMemoryRetriever) BoostScoreBatch(_ context.Context, _ []memory.TouchRef, _ float64) error {
	return nil
}

// TestBuildDynamicMemory_AuditLogAndCitations is the AUDIT-P2-5 audit
// trail contract: every prompt-injection turn that actually returns
// memories must emit an Info-level structured log with
// `mem_ids=[...]`, so support engineers can answer "for this user
// reply, which past memory drove the answer?" by joining log streams.
func TestBuildDynamicMemory_AuditLogAndCitations(t *testing.T) {
	core, recorded := observer.New(zap.InfoLevel)
	logger := zap.New(core)

	retriever := &staticMemoryRetriever{
		byType: map[string][]MemoryEntry{
			"preference": {{ID: "mem-pref-1", Type: "preference", Content: "loves tabs over spaces", Score: 0.9}},
			"decision":   {{ID: "mem-dec-1", Type: "decision", Content: "always reply in Chinese", Score: 0.85}},
		},
		generic: []MemoryEntry{
			{ID: "mem-gen-1", Type: "knowledge", Content: "uses Go and gin", Score: 0.6},
		},
	}

	o := &Orchestrator{
		memoryStore: retriever,
		logger:      logger,
	}

	out := o.buildDynamicMemory(context.Background(), "user-7", "proj-1", "what coding style?")

	require.NotEmpty(t, out, "buildDynamicMemory must emit a block when retriever returns data")
	for _, want := range []string{"[mem:mem-pref-1]", "[mem:mem-dec-1]", "[mem:mem-gen-1]"} {
		assert.True(t, strings.Contains(out, want), "expected prompt to embed citation marker %s\n%s", want, out)
	}

	logs := recorded.FilterMessage("memories injected into prompt").All()
	require.Len(t, logs, 1, "expected exactly one audit log line")

	fields := logs[0].ContextMap()
	require.Contains(t, fields, "mem_ids", "audit log must include mem_ids field")
	ids, ok := fields["mem_ids"].([]interface{})
	require.True(t, ok, "mem_ids must be a slice")
	got := make([]string, 0, len(ids))
	for _, v := range ids {
		got = append(got, v.(string))
	}
	assert.ElementsMatch(t, []string{"mem-pref-1", "mem-dec-1", "mem-gen-1"}, got)
	assert.EqualValues(t, 3, fields["count"], "count field must match mem_ids length")
}
