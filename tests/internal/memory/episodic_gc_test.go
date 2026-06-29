package memory_test

import (
	"context"
	"testing"
	"time"

	"github.com/agent/code_agent/internal/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDeleteOldEpisodic_OnlyTouchesDistilled is the AUDIT-P0-2 safety
// contract: GC must skip any episodic row whose DistilledAt is nil,
// even if its created_at is older than the retention window. Otherwise
// the "Distiller half-dead" failure mode loses raw user history before
// it can ever be consolidated into semantic memory.
func TestDeleteOldEpisodic_OnlyTouchesDistilled(t *testing.T) {
	now := time.Now()
	past := now.Add(-48 * time.Hour)
	recent := now.Add(-1 * time.Hour)
	stalePast := past // already-distilled, far in the past — should die

	store := &fakeDistillerStore{
		mems: []memory.Memory{
			{
				ID:          "old-distilled",
				Type:        memory.MemoryTypeEpisodic,
				UserID:      "u",
				ProjectID:   "p",
				CreatedAt:   past,
				DistilledAt: &stalePast,
			},
			{
				ID:        "old-undistilled",
				Type:      memory.MemoryTypeEpisodic,
				UserID:    "u",
				ProjectID: "p",
				CreatedAt: past,
			},
			{
				ID:          "recent-distilled",
				Type:        memory.MemoryTypeEpisodic,
				UserID:      "u",
				ProjectID:   "p",
				CreatedAt:   recent,
				DistilledAt: &recent,
			},
			{
				ID:        "semantic-noise",
				Type:      memory.MemoryTypeSemantic,
				UserID:    "u",
				ProjectID: "p",
				CreatedAt: past,
			},
		},
	}

	deleted, err := store.DeleteOldEpisodic(context.Background(), 24*time.Hour)
	require.NoError(t, err)
	assert.EqualValues(t, 1, deleted, "only the old + distilled episodic row should be deleted")

	got := map[string]bool{}
	for _, m := range store.mems {
		got[m.ID] = true
	}
	assert.False(t, got["old-distilled"], "old + distilled must be removed")
	assert.True(t, got["old-undistilled"], "old but undistilled must be preserved")
	assert.True(t, got["recent-distilled"], "recent rows must be preserved")
	assert.True(t, got["semantic-noise"], "semantic rows must never be touched")
}
