package memory

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestHybridStore_FileSplit_REAUDIT_P1_1 locks the REAUDIT-P1-1 contract:
// hybrid.go is core-only (struct + ctor + setters) and domain methods live
// in dedicated hybrid_*.go files.
func TestHybridStore_FileSplit_REAUDIT_P1_1(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	dir := filepath.Dir(file)

	hybridCore := filepath.Join(dir, "hybrid.go")
	data, err := os.ReadFile(hybridCore)
	require.NoError(t, err)
	lines := strings.Count(string(data), "\n") + 1
	require.LessOrEqual(t, lines, 120, "hybrid.go should stay core-only (got %d lines)", lines)

	required := []string{
		"hybrid_embed.go",
		"hybrid_store.go",
		"hybrid_retrieve.go",
		"hybrid_list.go",
		"hybrid_admin.go",
		"hybrid_queues.go",
		"hybrid_decay.go",
		"hybrid_dedup.go",
		"hybrid_rrf.go",
	}
	for _, name := range required {
		path := filepath.Join(dir, name)
		_, err := os.Stat(path)
		require.NoError(t, err, "missing %s", name)
	}

	body := string(data)
	require.Contains(t, body, "type HybridStore struct")
	require.Contains(t, body, "func NewHybridStore")
	require.NotContains(t, body, "func (h *HybridStore) Retrieve(")
	require.NotContains(t, body, "func (h *HybridStore) Store(")
}
