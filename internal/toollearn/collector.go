package toollearn

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Collector records tool execution feedback in-memory with optional persistence.
type Collector struct {
	mu       sync.Mutex
	buffer   []Feedback
	store    Store
	maxBuf   int
	logger   *zap.Logger
}

// NewCollector creates a feedback collector. Pass nil store for in-memory only.
func NewCollector(store Store, logger *zap.Logger) *Collector {
	return &Collector{
		buffer: make([]Feedback, 0, 256),
		store:  store,
		maxBuf: 1024,
		logger: logger.With(zap.String("component", "toollearn.collector")),
	}
}

// Record captures the outcome of a tool call.
func (c *Collector) Record(toolName string, args []byte, success bool, duration time.Duration, errMsg string, sessionID string) {
	fb := Feedback{
		ToolName:  toolName,
		ArgsHash:  hashArgs(args),
		Success:   success,
		Duration:  int(duration.Milliseconds()),
		ErrorMsg:  errMsg,
		SessionID: sessionID,
		CreatedAt: time.Now(),
	}

	c.mu.Lock()
	if len(c.buffer) < c.maxBuf {
		c.buffer = append(c.buffer, fb)
	}
	store := c.store
	c.mu.Unlock()

	if store != nil {
		if err := store.RecordFeedback(&fb); err != nil {
			c.logger.Debug("failed to persist feedback", zap.Error(err))
		}
	}
}

// SetStore wires a persistent backing store into the collector at runtime.
// In-memory buffer entries already collected are NOT replayed — they remain
// the recency window for in-process queries. Subsequent Record calls fan out
// to both buffer and store.
func (c *Collector) SetStore(s Store) {
	c.mu.Lock()
	c.store = s
	c.mu.Unlock()
}

// RecentFeedback returns the last N feedback entries for a tool.
func (c *Collector) RecentFeedback(toolName string, n int) []Feedback {
	c.mu.Lock()
	defer c.mu.Unlock()

	var results []Feedback
	for i := len(c.buffer) - 1; i >= 0 && len(results) < n; i-- {
		if c.buffer[i].ToolName == toolName {
			results = append(results, c.buffer[i])
		}
	}
	return results
}

// Stats returns aggregate stats for a tool from the in-memory buffer.
func (c *Collector) Stats(toolName string) (total, failures int, avgDurationMs int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var sumDuration int
	for _, fb := range c.buffer {
		if fb.ToolName == toolName {
			total++
			sumDuration += fb.Duration
			if !fb.Success {
				failures++
			}
		}
	}
	if total > 0 {
		avgDurationMs = sumDuration / total
	}
	return
}

func hashArgs(args []byte) string {
	h := sha256.Sum256(args)
	return hex.EncodeToString(h[:10])
}
