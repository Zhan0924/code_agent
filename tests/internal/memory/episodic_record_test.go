package memory_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/agent/code_agent/internal/memory"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

// fakeEpisodeStore implements memory.MemoryStorer (the Extractor's
// minimum surface). Captures everything stored so the test can assert
// on type, content, PII masking, and that RecordTaskEpisode bypasses
// the importance < 0.3 cutoff that ExtractFromInteraction applies.
type fakeEpisodeStore struct {
	mu      sync.Mutex
	stored  []memory.Memory
	recalls []memory.Memory
}

func (f *fakeEpisodeStore) Store(_ context.Context, m *memory.Memory) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stored = append(f.stored, *m)
	return nil
}

func (f *fakeEpisodeStore) Retrieve(_ context.Context, _, _, _ string, _ int) ([]memory.Memory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]memory.Memory, len(f.recalls))
	copy(out, f.recalls)
	return out, nil
}

// RetrieveCandidates satisfies the P1 #9 MemoryStorer surface. This
// test focuses on RecordTaskEpisode (the episodic write path), which
// doesn't trigger isDuplicate, so returning the recall fixture is
// sufficient to keep the interface stable.
func (f *fakeEpisodeStore) RetrieveCandidates(_ context.Context, _, _ string, _ []float32, _ int) ([]memory.Memory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]memory.Memory, len(f.recalls))
	copy(out, f.recalls)
	return out, nil
}

func TestRecordTaskEpisode_StoresEpisodicType(t *testing.T) {
	store := &fakeEpisodeStore{}
	ext := memory.NewExtractor(store, nil /* no LLM */, zap.NewNop())

	err := ext.RecordTaskEpisode(
		context.Background(),
		"user1", "proj1",
		"how do I run the tests?",
		"use go test ./internal/...",
		[]string{"shell_exec", "read_file"},
	)
	assert.NoError(t, err)

	assert.Len(t, store.stored, 1, "exactly one episodic memory per task")
	m := store.stored[0]
	assert.Equal(t, memory.MemoryTypeEpisodic, m.Type)
	assert.Equal(t, "user1", m.UserID)
	assert.Equal(t, "proj1", m.ProjectID)
	assert.Greater(t, m.Score, 0.0, "episodic memory should carry a non-zero default score")
	assert.Contains(t, m.Content, "USER:")
	assert.Contains(t, m.Content, "ASSISTANT:")
	assert.Contains(t, m.Content, "TOOLS: shell_exec -> read_file")
}

// Episodic recording must skip the importance < 0.3 cutoff so the
// Distiller has a complete timeline. Verify by recording a trivial
// task that would clearly fail the cutoff applied to typed memories.
func TestRecordTaskEpisode_SkipsImportanceCutoff(t *testing.T) {
	store := &fakeEpisodeStore{}
	ext := memory.NewExtractor(store, nil, zap.NewNop())

	err := ext.RecordTaskEpisode(
		context.Background(),
		"user1", "proj1",
		"ping",
		"pong",
		nil,
	)
	assert.NoError(t, err)
	assert.Len(t, store.stored, 1, "even trivial tasks produce an episode")
}

// PII masking must apply to episodic content before storage — episodes
// are later fed to the LLM during distillation, so unmasked secrets
// would leak twice (storage + distillation prompt).
func TestRecordTaskEpisode_AppliesPIIMasking(t *testing.T) {
	store := &fakeEpisodeStore{}
	ext := memory.NewExtractor(store, nil, zap.NewNop())

	err := ext.RecordTaskEpisode(
		context.Background(),
		"user1", "proj1",
		"my key is sk-abc1234567890abcdef1234567890",
		"I won't store it",
		nil,
	)
	assert.NoError(t, err)
	assert.Len(t, store.stored, 1)
	assert.NotContains(t, store.stored[0].Content, "sk-abc1234567890abcdef1234567890",
		"raw secret must not be persisted")
	assert.Contains(t, store.stored[0].Content, "[REDACTED:",
		"masking marker must appear in stored episode")
}

// Empty input → no memory stored. Defensive: if the caller mis-wires
// recordTaskEpisodeAsync (e.g. passing only whitespace), we must not
// pollute the store with empty episodes.
func TestRecordTaskEpisode_SkipsEmptyInput(t *testing.T) {
	store := &fakeEpisodeStore{}
	ext := memory.NewExtractor(store, nil, zap.NewNop())

	err := ext.RecordTaskEpisode(
		context.Background(),
		"user1", "proj1",
		"   ", "\n\t",
		nil,
	)
	assert.NoError(t, err)
	assert.Len(t, store.stored, 0, "empty input should not create an episode")
}

func TestRecordTaskEpisode_RequiresUserID(t *testing.T) {
	store := &fakeEpisodeStore{}
	ext := memory.NewExtractor(store, nil, zap.NewNop())

	err := ext.RecordTaskEpisode(
		context.Background(),
		"" /* no user */, "proj1",
		"user msg", "assistant msg", nil,
	)
	assert.Error(t, err, "userID is required")
	assert.Len(t, store.stored, 0)
}

// Content is truncated to a fixed byte budget so a single rambling task
// can't blow up the distiller context. The exact size matters for token
// economics; this test pins it so a regression is caught.
func TestRecordTaskEpisode_TruncatesLongContent(t *testing.T) {
	store := &fakeEpisodeStore{}
	ext := memory.NewExtractor(store, nil, zap.NewNop())

	longUser := strings.Repeat("a", 10_000)
	longAssist := strings.Repeat("b", 10_000)

	err := ext.RecordTaskEpisode(
		context.Background(),
		"user1", "proj1",
		longUser, longAssist, nil,
	)
	assert.NoError(t, err)
	assert.Len(t, store.stored, 1)
	// Stored content should be well under the raw 20k input. The exact
	// boundary depends on episodicMaxContentBytes (1500) — we assert a
	// soft upper bound rather than the exact value so the test is robust
	// to small tuning changes.
	assert.Less(t, len(store.stored[0].Content), 2200,
		"episode content should be bounded to ~1.5k bytes plus overhead")
}
