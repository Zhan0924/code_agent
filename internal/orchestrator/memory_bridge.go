package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/agent/code_agent/internal/memory"
	"go.uber.org/zap"
)

// MemoryRetriever is the interface the orchestrator needs from a memory store.
// Implement this in the wiring layer (main.go) to adapt memory.HybridStore.
type MemoryRetriever interface {
	Retrieve(ctx context.Context, userID, projectID, query string, limit int) ([]MemoryEntry, error)
}

// MemoryEntry is a minimal representation of a memory item for prompt injection.
type MemoryEntry struct {
	Type    string
	Content string
	Score   float64
}

// SetMemoryStore injects an optional long-term memory store.
func (o *Orchestrator) SetMemoryStore(ms MemoryRetriever) {
	o.memoryStore = ms
}

// SetMemoryExtractor injects an optional memory extractor for learning from interactions.
func (o *Orchestrator) SetMemoryExtractor(ext *memory.Extractor) {
	o.memoryExtractor = ext
}

// extractMemoriesAsync runs memory extraction in a background goroutine.
func (o *Orchestrator) extractMemoriesAsync(sessionID, userMsg, assistantMsg string) {
	if o.memoryExtractor == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), memoryExtractionTimeout)
		defer cancel()
		o.memoryExtractor.ExtractFromInteraction(ctx, sessionID, "", userMsg, assistantMsg)
	}()
}

const memoryExtractionTimeout = 30 * time.Second

// buildLongTermMemory combines session summary with relevant memories
// for injection into the prompt's semi-stable region.
func (o *Orchestrator) buildLongTermMemory(ctx context.Context, sessionSummary, userID, projectID, query string) string {
	var parts []string

	if sessionSummary != "" {
		parts = append(parts, sessionSummary)
	}

	if o.memoryStore != nil && query != "" {
		memories, err := o.memoryStore.Retrieve(ctx, userID, projectID, query, 5)
		if err != nil {
			o.logger.Debug("memory retrieval failed", zap.Error(err))
		} else if len(memories) > 0 {
			var memParts []string
			for _, m := range memories {
				memParts = append(memParts, fmt.Sprintf("[%s] %s", m.Type, m.Content))
			}
			parts = append(parts, "[Long-Term Memory]\n"+strings.Join(memParts, "\n"))
		}
	}

	return strings.Join(parts, "\n\n")
}
