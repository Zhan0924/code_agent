package main

import (
	"context"
	"time"

	"github.com/agent/code_agent/internal/config"
	"github.com/agent/code_agent/internal/llm"
	"github.com/agent/code_agent/internal/memory"
	"github.com/agent/code_agent/internal/metrics"
	"github.com/agent/code_agent/internal/models"
	"github.com/agent/code_agent/internal/orchestrator"
	"github.com/agent/code_agent/internal/store"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// distillerLLMAdapter implements memory.LLMClient on top of llm.Client.
//
// memory.LLMClient is intentionally a minimal "give me one completion"
// surface so the Distiller stays trivially mockable; this adapter is the
// glue that lets us pass the real llm.Client without leaking llm types
// into the memory package.
type distillerLLMAdapter struct {
	c *llm.Client
}

func (a *distillerLLMAdapter) GenerateContent(ctx context.Context, prompt string) (string, error) {
	resp, err := a.c.ChatCompletion(ctx, &llm.ChatRequest{
		Messages: []models.Message{
			{Role: "user", Content: prompt},
		},
		// 256 tokens is plenty for "summarize episodes into one rule";
		// keeps the cost predictable across thousands of nightly distills.
		MaxTokens: 256,
	})
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

// memoryAdapter bridges memory.HybridStore to orchestrator.MemoryRetriever.
type memoryAdapter struct {
	store *memory.HybridStore
}

func (a *memoryAdapter) Retrieve(ctx context.Context, userID, projectID, query string, limit int) ([]orchestrator.MemoryEntry, error) {
	memories, err := a.store.Retrieve(ctx, userID, projectID, query, limit)
	if err != nil {
		return nil, err
	}
	return toEntries(memories), nil
}

func (a *memoryAdapter) BoostScoreBatch(ctx context.Context, refs []memory.TouchRef, boost float64) error {
	return a.store.BoostScoreBatch(ctx, refs, boost)
}

// RetrieveByType implements orchestrator.MemoryRetriever by delegating to
// HybridStore's type-filtered semantic search.
func (a *memoryAdapter) RetrieveByType(ctx context.Context, userID, projectID, memType, query string, limit int) ([]orchestrator.MemoryEntry, error) {
	memories, err := a.store.RetrieveByType(ctx, userID, projectID, memType, query, limit)
	if err != nil {
		return nil, err
	}
	return toEntries(memories), nil
}

func toEntries(memories []memory.Memory) []orchestrator.MemoryEntry {
	entries := make([]orchestrator.MemoryEntry, 0, len(memories))
	for _, m := range memories {
		entries = append(entries, orchestrator.MemoryEntry{
			ID:      m.ID,
			Type:    string(m.Type),
			Content: m.Content,
			Score:   m.Score,
		})
	}
	return entries
}

// HybridStore returns the underlying store for use by the memory extractor.
func (a *memoryAdapter) HybridStore() *memory.HybridStore {
	return a.store
}

// NewMemoryAdapter creates a memory adapter backed by Redis hot tier and PG cold tier.
//
// memCfg controls runtime thresholds (TTL, conflict threshold, embedding dim,
// decay parameters). All zero-valued fields fall back to the defaults baked
// into the constructor APIs — keep this in mind when changing defaults: a
// boot-time config that doesn't set ConflictThreshold will *not* become 0.0
// (which would treat *every* memory pair as a conflict), it stays 0.85.
func NewMemoryAdapter(rdb *redis.Client, pgStore *store.Store, embedder memory.Embedder, logger *zap.Logger, memCfg config.MemoryConfig) *memoryAdapter {
	if rdb == nil {
		return nil
	}

	// Hot tier with configurable TTL — zero falls back to the 24h default
	// inside NewRedisHotWithTTL itself, so callers don't need to defend.
	hot := memory.NewRedisHotWithTTL(rdb, logger, memCfg.HotTTL)
	// P1 #10: per-deployment SCAN ceiling. 0 → keep the constructor's
	// default (200). Clamp is enforced inside SetScanLimit.
	if memCfg.HotScanLimit != 0 {
		hot.SetScanLimit(memCfg.HotScanLimit)
	}

	var cold *memory.PGCold
	if pgStore != nil {
		// Use the dim-aware constructor so switching embedding models is a
		// single-config change. Default 0 → NewPGCold semantics (1536).
		if memCfg.EmbeddingDim > 0 {
			cold = memory.NewPGColdWithDim(pgStore.DB(), logger, memCfg.EmbeddingDim)
		} else {
			cold = memory.NewPGCold(pgStore.DB(), logger)
		}
		if err := cold.Migrate(); err != nil {
			logger.Warn("memory cold store migration failed", zap.Error(err))
			cold = nil
		}
	}

	hybrid := memory.NewHybridStore(hot, cold, logger)
	if embedder != nil {
		hybrid.SetEmbedder(embedder)
	}

	// Optional: replace the default conflict resolver if the operator
	// overrode any of the threshold / margin / preserve / dedup-cap
	// fields. MaxConflictsToDedup is the P1 #7 knob — see
	// ConflictResolverConfig comment.
	if memCfg.ConflictThreshold > 0 || memCfg.ConflictMargin > 0 ||
		memCfg.PreserveHighScore || memCfg.MaxConflictsToDedup > 0 {
		resolverCfg := memory.ConflictResolverConfig{
			Threshold:           memCfg.ConflictThreshold,
			MarginToOverride:    memCfg.ConflictMargin,
			PreserveHighScore:   memCfg.PreserveHighScore,
			MaxConflictsToDedup: memCfg.MaxConflictsToDedup,
		}
		hybrid.SetConflictResolver(memory.NewConflictResolverWithConfig(cold, resolverCfg))
	}

	bb := memory.NewBlackboard(rdb, logger)
	hybrid.SetBlackboard(bb)

	return &memoryAdapter{store: hybrid}
}

// runMemoryDecayLoop runs HybridStore.Decay on a fixed interval until ctx
// is cancelled. Each iteration logs the count of affected memories so an
// operator can verify decay is actually firing (and tune the
// interval/factor if it's churning too much state).
//
// Lives in main package (not internal/memory) because:
//  1. main.go already owns the lifecycle of background loops (Drain, signal handling);
//  2. Putting it inside internal/memory would require that package to import
//     a scheduler, polluting the unit-testable core with goroutine ergonomics.
func runMemoryDecayLoop(
	ctx context.Context,
	store *memory.HybridStore,
	interval, olderThan time.Duration,
	factor float64,
	logger *zap.Logger,
) {
	logger = logger.With(zap.String("component", "memory.decay.scheduler"))

	// First run after a short delay rather than immediately on startup —
	// gives the rest of the boot sequence room to settle and avoids a
	// thundering-herd write storm at t=0.
	timer := time.NewTimer(2 * time.Minute)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("memory decay scheduler stopping")
			return
		case <-timer.C:
		}

		start := time.Now()
		count, err := store.Decay(olderThan, factor)
		if err != nil {
			metrics.MemoryDecayRunsTotal.WithLabelValues("err").Inc()
			logger.Warn("memory decay iteration failed",
				zap.Error(err),
				zap.Duration("elapsed", time.Since(start)))
		} else {
			metrics.MemoryDecayRunsTotal.WithLabelValues("ok").Inc()
			metrics.MemoryDecayAffected.Observe(float64(count))
			logger.Info("memory decay iteration complete",
				zap.Int("memories_decayed", count),
				zap.Duration("elapsed", time.Since(start)))
		}

		// Reset the timer for the next iteration. Using Reset on a stopped
		// timer is safe because we just drained C above.
		timer.Reset(interval)
	}
}

// runMemoryDistillLoop periodically asks the Distiller to consolidate
// episodic memories into a semantic rule for each active tenant.
//
// Tenant selection per tick:
//  1. AutoDiscover (default true when Enabled): query PG for tenants
//     with >= MinEpisodicForDiscovery undistilled episodes.
//  2. Static Targets are added as *forced inclusion* — they always
//     get a distill attempt even if their episodic count is below
//     MinEpisodicForDiscovery (Distiller still skips at the
//     MinEpisodicToTrigger floor, so this just gives operators a
//     "always retry this tenant" knob).
//  3. The merged list is de-duped and capped at MaxTenantsPerTick.
//
// LLM failures are *not* fatal: a 5xx from the LLM provider should not
// take the agent down. We log+metric and continue on the next tick.
func runMemoryDistillLoop(
	ctx context.Context,
	store memory.DistillerStore,
	llm memory.LLMClient,
	bb *memory.Blackboard,
	cfg config.MemoryDistillConfig,
	logger *zap.Logger,
) {
	logger = logger.With(zap.String("component", "memory.distiller.scheduler"))

	if !cfg.Enabled {
		logger.Debug("distiller disabled; scheduler exiting")
		return
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = 6 * time.Hour
	}
	maxTenants := cfg.MaxTenantsPerTick
	if maxTenants <= 0 {
		maxTenants = 32
	}
	minTrigger := cfg.MinEpisodicToTrigger
	if minTrigger <= 0 {
		minTrigger = 3
	}
	minDiscover := cfg.MinEpisodicForDiscovery
	if minDiscover <= 0 {
		minDiscover = minTrigger
	}
	// Default AutoDiscover ON when Enabled — without it, multi-tenant
	// deployments effectively rely on operators editing YAML for every
	// new (user, project). Operators who want the strict "static only"
	// behaviour explicitly set auto_discover: false in YAML.
	if !cfg.AutoDiscover && len(cfg.Targets) == 0 {
		logger.Warn("distiller enabled but auto_discover=false and no targets configured; ticks will be no-op",
			zap.Duration("interval", interval))
	}

	distiller := memory.NewDistiller(store, llm, bb, logger, memory.DistillerOptions{
		MaxEpisodicPerRun:    cfg.MaxEpisodicPerRun,
		MinEpisodicToTrigger: minTrigger,
	})

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("memory distill scheduler stopping")
			return
		case <-timer.C:
		}

		tenants := buildDistillTenants(ctx, store, cfg, maxTenants, minDiscover, logger)
		for _, t := range tenants {
			n, err := distiller.Distill(ctx, t.UserID, t.ProjectID)
			if err != nil {
				logger.Warn("distill iteration failed",
					zap.String("user_id", t.UserID),
					zap.String("project_id", t.ProjectID),
					zap.Error(err))
				continue
			}
			logger.Debug("distill iteration complete",
				zap.String("user_id", t.UserID),
				zap.String("project_id", t.ProjectID),
				zap.Int("episodic_seen", n))
		}

		timer.Reset(interval)
	}
}

// buildDistillTenants assembles the per-tick distill list: static Targets
// (forced inclusion) merged with PG-discovered active tenants, de-duped
// and capped. Pulled out so runMemoryDistillLoop stays linear.
func buildDistillTenants(
	ctx context.Context,
	store memory.DistillerStore,
	cfg config.MemoryDistillConfig,
	maxTenants, minDiscover int,
	logger *zap.Logger,
) []memory.TenantRef {
	seen := make(map[string]struct{}, maxTenants)
	out := make([]memory.TenantRef, 0, maxTenants)

	keyFn := func(u, p string) string { return u + "\x00" + p }

	// Forced inclusion first so they survive the cap even when PG returns
	// many high-count tenants. Operators use Targets as an "always retry"
	// allow-list (e.g. shared knowledge bases).
	for _, t := range cfg.Targets {
		if t.UserID == "" || t.ProjectID == "" {
			continue
		}
		k := keyFn(t.UserID, t.ProjectID)
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, memory.TenantRef{UserID: t.UserID, ProjectID: t.ProjectID})
		if len(out) >= maxTenants {
			break
		}
	}
	metrics.MemoryDistillTargetsTotal.WithLabelValues("static").Add(float64(len(out)))

	if cfg.AutoDiscover && len(out) < maxTenants {
		// Ask PG for at most (maxTenants - already-included) so the
		// final list stays bounded. Avoids the dedup-then-truncate
		// trap where a partial pre-fill from Targets would otherwise
		// silently drop high-priority discovered tenants.
		want := maxTenants - len(out)
		start := time.Now()
		discovered, err := store.ListActiveDistillTenants(ctx, minDiscover, want)
		metrics.MemoryDistillDiscoverDuration.Observe(time.Since(start).Seconds())
		if err != nil {
			logger.Warn("distill auto-discover failed (falling back to static targets only)",
				zap.Error(err))
		}
		metrics.MemoryDistillTargetsTotal.WithLabelValues("discovered").Add(float64(len(discovered)))
		for _, t := range discovered {
			if t.UserID == "" || t.ProjectID == "" {
				continue
			}
			k := keyFn(t.UserID, t.ProjectID)
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			out = append(out, t)
			if len(out) >= maxTenants {
				break
			}
		}
	}

	metrics.MemoryDistillTargetsTotal.WithLabelValues("merged").Add(float64(len(out)))
	return out
}
