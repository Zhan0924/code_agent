package memory

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/agent/code_agent/internal/metrics"
	"go.uber.org/zap"
)

// LLMClient is the minimum LLM surface the Distiller needs. Kept as a
// local interface (instead of importing internal/llm) so the memory
// package stays free of LLM-provider dependencies, and so tests can pass
// a trivial mock.
type LLMClient interface {
	GenerateContent(ctx context.Context, prompt string) (string, error)
}

// DistillerStore is the minimum HybridStore surface the Distiller needs.
// Defined as an interface so unit tests can inject an in-memory fake
// without standing up Redis + Postgres.
//
// MarkDistilled is the closing step of every successful distill cycle:
// without it, the next tick would pick up the same episodes and produce
// duplicate semantic memories. We pass IDs (not whole memories) because
// the update is set-based ("UPDATE ... WHERE id = ANY(...)") and a
// single round-trip beats a per-row Update call.
//
// ListActiveDistillTenants is the multi-tenant discovery hook used by
// the scheduler when AutoDiscover is enabled — without it, operators
// had to enumerate every (user, project) tuple in YAML, which doesn't
// scale past a handful of tenants. See cmd/agent/memory_adapter.go
// runMemoryDistillLoop for the consumer.
type DistillerStore interface {
	ListByType(ctx context.Context, userID, projectID string, memType MemoryType, limit int) ([]Memory, error)
	Store(ctx context.Context, m *Memory) error
	MarkDistilled(ctx context.Context, ids []string) error
	ListActiveDistillTenants(ctx context.Context, minEpisodic, limit int) ([]TenantRef, error)
	DeleteOldEpisodic(ctx context.Context, olderThan time.Duration) (int64, error)
}

// DistillerOptions tunes the consolidation pass without forcing callers to
// edit constants in this file.
type DistillerOptions struct {
	// MaxEpisodicPerRun caps how many episodic entries are sent to the LLM
	// in one call. Default 50. Distiller silently truncates beyond this
	// to avoid blowing the LLM context window or distillation latency.
	MaxEpisodicPerRun int
	// MinEpisodicToTrigger is the floor below which we skip the LLM call
	// entirely — distilling 1 or 2 episodes produces noise, not insight.
	MinEpisodicToTrigger int
	// SemanticScore is the initial score for distilled semantic memories.
	// Slightly above 1.0 so they survive the next decay pass and don't
	// get instantly demoted under fresh episodic memories.
	SemanticScore float64
}

func (o DistillerOptions) withDefaults() DistillerOptions {
	if o.MaxEpisodicPerRun <= 0 {
		o.MaxEpisodicPerRun = 50
	}
	if o.MinEpisodicToTrigger <= 0 {
		o.MinEpisodicToTrigger = 3
	}
	if o.SemanticScore <= 0 {
		o.SemanticScore = 1.2
	}
	return o
}

// Distiller periodically consolidates episodic memories into a smaller
// number of higher-signal semantic memories — the standard "hippocampus
// → cortex" pattern. It now operates over a full HybridStore (hot + cold)
// rather than the hot-only path of the previous implementation, so
// project-wide history is consolidated, not just the last 24h cache.
//
// Idempotency: the produced semantic memory is routed through
// HybridStore.Store, which invokes ConflictResolver — duplicate or
// reinforcing distillations are merged instead of stacking, so running
// Distill twice in a row does not double-up.
type Distiller struct {
	store      DistillerStore
	llm        LLMClient
	blackboard *Blackboard
	logger     *zap.Logger
	opts       DistillerOptions
}

// NewDistiller constructs a Distiller bound to a HybridStore (or any
// DistillerStore-compatible fake). The previous (store *RedisHot, ...)
// signature was intentionally tightened — distilling only the hot tier
// produced misleading semantic memories because the cold tier (where
// cross-session learning lives) was invisible to it.
func NewDistiller(store DistillerStore, llm LLMClient, bb *Blackboard, logger *zap.Logger, opts DistillerOptions) *Distiller {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Distiller{
		store:      store,
		llm:        llm,
		blackboard: bb,
		logger:     logger.With(zap.String("component", "memory.distiller")),
		opts:       opts.withDefaults(),
	}
}

// Distill pulls recent episodic memories for (userID, projectID), asks the
// LLM to extract a single semantic rule, and writes that rule back as a
// MemoryTypeSemantic entry. Returns the number of episodic memories that
// fed into the distillation (0 means: nothing to do, not an error).
//
// All emitted metrics share the path label so dashboards can split
// "skipped vs distilled vs errored" without grepping log lines.
func (d *Distiller) Distill(ctx context.Context, userID, projectID string) (int, error) {
	start := time.Now()
	path := "ok"
	defer func() {
		metrics.MemoryDistillRunsTotal.WithLabelValues(path).Inc()
		metrics.MemoryDistillDuration.Observe(time.Since(start).Seconds())
	}()

	episodic, err := d.store.ListByType(ctx, userID, projectID, MemoryTypeEpisodic, d.opts.MaxEpisodicPerRun)
	if err != nil {
		path = "error_list"
		return 0, fmt.Errorf("distiller: list episodic: %w", err)
	}

	if len(episodic) < d.opts.MinEpisodicToTrigger {
		path = "skipped"
		d.logger.Debug("distill skipped: below threshold",
			zap.Int("count", len(episodic)),
			zap.Int("min", d.opts.MinEpisodicToTrigger),
			zap.String("user_id", userID),
			zap.String("project_id", projectID))
		return len(episodic), nil
	}

	var sb strings.Builder
	for i, m := range episodic {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, strings.TrimSpace(m.Content))
	}
	prompt := fmt.Sprintf(
		"You consolidate %d recent episodic memories into ONE concise semantic rule (max 2 sentences) "+
			"capturing the most actionable pattern. Output only the rule.\n\nEpisodes:\n%s",
		len(episodic), sb.String())

	distilled, err := d.llm.GenerateContent(ctx, prompt)
	if err != nil {
		path = "error_llm"
		return len(episodic), fmt.Errorf("distiller: llm: %w", err)
	}
	distilled = strings.TrimSpace(distilled)
	if distilled == "" {
		path = "empty_llm"
		return len(episodic), nil
	}

	semanticMem := &Memory{
		ID:             fmt.Sprintf("sem-%s-%s-%d", userID, projectID, time.Now().UnixNano()),
		UserID:         userID,
		ProjectID:      projectID,
		Type:           MemoryTypeSemantic,
		Content:        distilled,
		Score:          d.opts.SemanticScore,
		AccessCount:    0,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
		LastAccessedAt: time.Now(),
	}

	if err := d.store.Store(ctx, semanticMem); err != nil {
		path = "error_store"
		return len(episodic), fmt.Errorf("distiller: store: %w", err)
	}

	// Critical: mark the source episodes as consumed BEFORE returning so
	// the next tick doesn't re-distill them. We do this *after* Store —
	// if Store fails the episodes stay unmarked and the next tick gets
	// another shot at producing the semantic memory (idempotent failure
	// recovery).
	ids := make([]string, 0, len(episodic))
	for _, m := range episodic {
		ids = append(ids, m.ID)
	}
	if err := d.store.MarkDistilled(ctx, ids); err != nil {
		// Non-fatal: we already produced the semantic memory; the worst
		// case if MarkDistilled fails is a duplicate semantic memory
		// next tick (the ConflictResolver in HybridStore.Store will then
		// merge / preserve based on similarity, so even the failure mode
		// degrades gracefully).
		path = "warn_mark_failed"
		d.logger.Warn("distill: mark-distilled failed (next tick may re-consume)",
			zap.Int("episode_count", len(ids)),
			zap.Error(err))
	} else {
		metrics.MemoryDistillProduced.WithLabelValues("marked").Add(float64(len(ids)))
	}

	metrics.MemoryDistillProduced.WithLabelValues("semantic").Inc()
	d.logger.Info("distilled episodic into semantic",
		zap.Int("episodic_count", len(episodic)),
		zap.String("user_id", userID),
		zap.String("project_id", projectID))

	if d.blackboard != nil {
		_ = d.blackboard.Publish(ctx, "distilled", semanticMem)
	}
	return len(episodic), nil
}
