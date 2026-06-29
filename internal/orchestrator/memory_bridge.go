package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/agent/code_agent/internal/memory"
	"github.com/agent/code_agent/internal/models"
	"github.com/agent/code_agent/internal/session"
	"go.uber.org/zap"
)

// MemoryRetriever is the interface the orchestrator needs from a memory store.
// Implement this in the wiring layer (main.go) to adapt memory.HybridStore.
//
// Retrieve is generic semantic search. RetrieveByType is the type-filtered
// variant used by importance bucketing in buildLongTermMemory — without it,
// a fixed top-K could return 5 "knowledge" entries and zero "preference",
// silently dropping the highest-signal personalization the user expressed.
type MemoryRetriever interface {
	Retrieve(ctx context.Context, userID, projectID, query string, limit int) ([]MemoryEntry, error)
	RetrieveByType(ctx context.Context, userID, projectID, memType, query string, limit int) ([]MemoryEntry, error)
	BoostScoreBatch(ctx context.Context, refs []memory.TouchRef, boost float64) error
}

// MemoryEntry is a minimal representation of a memory item for prompt injection.
type MemoryEntry struct {
	ID      string
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

// SetCoreMemory injects the MemGPT-style CoreMemoryManager. Without this,
// the core_memory_append / core_memory_replace tools are a write-only
// blackhole — the agent could write but never read its persona / human /
// project sections back. buildLongTermMemory wires the read side.
func (o *Orchestrator) SetCoreMemory(cm memory.CoreMemoryManager) {
	o.coreMemory = cm
}

// resolveTenantIDs unifies API + orchestrator tenant fallback (REAUDIT-P1-2).
// Order: context values → session record → session.NormalizeTenantIDs.
func (o *Orchestrator) resolveTenantIDs(ctx context.Context, sessionID string) (userID, projectID string) {
	userID = models.UserIDFromContext(ctx)
	projectID = models.ProjectIDFromContext(ctx)
	beforeUser, beforeProj := userID, projectID

	if (userID == "" || projectID == "") && o.sessionMgr != nil && sessionID != "" {
		if sess, err := o.sessionMgr.Get(ctx, sessionID); err == nil && sess != nil {
			if userID == "" {
				userID = sess.UserID
			}
			if projectID == "" {
				projectID = sess.ProjectID
			}
		}
	}

	userID, projectID = session.NormalizeTenantIDs(userID, projectID)
	if beforeUser != userID || beforeProj != projectID {
		o.logger.Info("tenant ids normalized for memory pipeline",
			zap.String("audit_id", "REAUDIT-P1-2"),
			zap.String("op", "tenant_normalize"),
			zap.String("before_user_id", beforeUser),
			zap.String("before_project_id", beforeProj),
			zap.String("user_id", userID),
			zap.String("project_id", projectID),
			zap.String("result", "ok"))
	}
	return userID, projectID
}

// ResolveTenantIDsForTest exposes resolveTenantIDs for dev-only HTTP smoke tests.
func (o *Orchestrator) ResolveTenantIDsForTest(ctx context.Context, sessionID string) (userID, projectID string) {
	return o.resolveTenantIDs(ctx, sessionID)
}

// extractMemoriesAsync runs memory extraction in a background goroutine.
//
// Trace-context propagation: we MUST NOT use a bare context.Background()
// here — that severs OTel span / Datadog trace_id / structured logger
// fields the caller already populated (userID, request_id, etc.).
// Instead we derive a detached-cancel context: keeps Values (traces,
// loggers, identity) but ignores the parent's Cancel/Deadline, so the
// extractor survives the HTTP request returning while still showing up
// under the originating trace.
func (o *Orchestrator) extractMemoriesAsync(ctx context.Context, sessionID, userMsg, assistantMsg string) {
	if o.memoryExtractor == nil {
		return
	}
	// Snapshot identity once on the request goroutine. Doing the
	// sessionMgr.Get *outside* the bgCtx avoids the race where the
	// session is evicted before the extractor goroutine wakes up.
	userID, projectID := o.resolveTenantIDs(ctx, sessionID)

	go func(parent context.Context) {
		bgCtx, cancel := context.WithTimeout(detachCancel(parent), memoryExtractionTimeout)
		defer cancel()
		o.memoryExtractor.ExtractFromInteraction(bgCtx, userID, projectID, userMsg, assistantMsg)
	}(ctx)
}

// recordTaskEpisodeAsync persists ONE episodic memory per completed task
// — the raw timeline (user msg + assistant final + tool sequence) that
// the Distiller later consolidates into semantic rules.
//
// Lives parallel to extractMemoriesAsync rather than inside it because:
//   - Different failure model: extractor produces typed memories via LLM
//     and can legitimately produce zero entries; episode recording is
//     deterministic and produces exactly one entry per task.
//   - Different lifecycle: the episodic entry survives the typed
//     extraction failing (LLM down / parse error) — we still want a
//     trail for the Distiller next tick.
//   - Different cost profile: extractor calls the LLM (expensive);
//     episode recording is store-only (cheap). Splitting lets us run
//     episode recording even on hosts without LLM access.
//
// Like extractMemoriesAsync we use detachCancel so the request returning
// before the goroutine finishes doesn't abort the write, while keeping
// trace_id / userID / structured logger fields attached.
func (o *Orchestrator) recordTaskEpisodeAsync(ctx context.Context, sessionID, userMsg, assistantMsg string, tools []string) {
	if o.memoryExtractor == nil {
		return
	}
	userID, projectID := o.resolveTenantIDs(ctx, sessionID)

	go func(parent context.Context, tools []string) {
		bgCtx, cancel := context.WithTimeout(detachCancel(parent), memoryExtractionTimeout)
		defer cancel()
		if err := o.memoryExtractor.RecordTaskEpisode(bgCtx, userID, projectID, userMsg, assistantMsg, tools); err != nil {
			o.logger.Debug("episode recording failed", zap.Error(err))
		}
	}(ctx, tools)
}

// detachCancel returns a context that inherits Values (logger, trace, identity)
// but ignores the parent's Cancel / Deadline. Use only when the work must
// outlive the originating request (e.g. fire-and-forget extraction).
type detachedCtx struct{ context.Context }

func (detachedCtx) Deadline() (deadline time.Time, ok bool) { return }
func (detachedCtx) Done() <-chan struct{}                   { return nil }
func (detachedCtx) Err() error                              { return nil }

func detachCancel(parent context.Context) context.Context {
	if parent == nil {
		return context.Background()
	}
	return detachedCtx{Context: parent}
}

const memoryExtractionTimeout = 30 * time.Second

// buildLongTermMemory combines session summary, MemGPT-style core memory,
// and semantically-retrieved long-term memories for injection into the
// prompt's semi-stable region.
//
// Layout (top → bottom = stable → query-specific) is intentional: KV-cache
// hits more often when the *stable* prefix (core memory, summary) doesn't
// shift between turns; only the [Long-Term Memory] block changes with
// `query`. See `internal/context/prompt_builder.go` for the surrounding
// cache-friendly slot architecture.
func (o *Orchestrator) buildStableMemory(ctx context.Context, sessionSummary, userID, projectID string) string {
	var parts []string

	// 1. Core memory (always-on, no query needed). Highest stability slot.
	if o.coreMemory != nil && userID != "" {
		if formatted := o.formatCoreMemory(ctx, userID, projectID); formatted != "" {
			parts = append(parts, formatted)
		}
	}

	// 2. Session summary (rolling). Mid stability — flushes per session.
	if sessionSummary != "" {
		parts = append(parts, sessionSummary)
	}

	return strings.Join(parts, "\n\n")
}

func (o *Orchestrator) buildDynamicMemory(ctx context.Context, userID, projectID, query string) (string, []string) {
	var parts []string
	var injectedIDs []string

	// 3. Query-specific long-term memories (lowest stability, changes per turn).
	if o.memoryStore != nil && query != "" {
		o.logger.Debug("retrieving long-term memories",
			zap.String("user_id", userID),
			zap.String("project_id", projectID),
			zap.String("query", query))
		memories := o.retrieveBucketedMemories(ctx, userID, projectID, query, 5)
		if len(memories) > 0 {
			// AUDIT-P2-5 audit trail: emit a structured log line that
			// records *which* memory IDs were injected into the prompt
			// this turn. Dashboards (Datadog/ES) can join this with the
			// downstream `[mem:id]` citations to answer "for this user
			// reply, which past memory drove the answer?".
			ids := make([]string, 0, len(memories))
			for _, m := range memories {
				ids = append(ids, m.ID)
			}
			injectedIDs = ids
			o.logger.Info("memories injected into prompt",
				zap.String("user_id", userID),
				zap.String("project_id", projectID),
				zap.Int("count", len(memories)),
				zap.Strings("mem_ids", ids))
			parts = append(parts, "### Long-Term Memory (Context from past interactions)")
			parts = append(parts, "Use these memories to inform your answer. If you use a memory, explicitly cite its ID like `[mem:<id>]` in your response to boost its relevance.")

			var memParts []string
			for _, m := range memories {
				memParts = append(memParts, fmt.Sprintf("[mem:%s] [%s] %s", m.ID, m.Type, m.Content))
			}
			parts = append(parts, strings.Join(memParts, "\n\n"))
			parts = append(parts, "")
		} else {
			o.logger.Debug("no memories found for query")
		}
	}

	return strings.Join(parts, "\n\n"), injectedIDs
}

// retrieveBucketedMemories returns up to `limit` memories with importance
// bucketing: we reserve at least 1 slot each for `preference` and
// `decision` types (the highest-signal personalization), then fill the
// remainder with a generic semantic top-K. This prevents the failure mode
// where a topical query buries the user's explicit preferences under 5
// "knowledge" entries and the LLM "forgets" what the user told it.
//
// Per-type slots are advisory — if no preference matches the query, we
// don't waste the slot; the generic top-K fills it.
//
// Dedupe across buckets happens by content (no IDs at this layer).
func (o *Orchestrator) retrieveBucketedMemories(ctx context.Context, userID, projectID, query string, limit int) []MemoryEntry {
	if limit <= 0 {
		limit = 5
	}
	seen := make(map[string]struct{}, limit*2)
	out := make([]MemoryEntry, 0, limit)
	push := func(m MemoryEntry) {
		if _, dup := seen[m.Content]; dup {
			return
		}
		seen[m.Content] = struct{}{}
		out = append(out, m)
	}

	// 1) Reserve 1 slot each for the two highest-signal types.
	for _, t := range []string{"preference", "decision"} {
		if len(out) >= limit {
			break
		}
		mems, err := o.memoryStore.RetrieveByType(ctx, userID, projectID, t, query, 1)
		if err != nil {
			o.logger.Debug("type-bucketed retrieve failed", zap.String("type", t), zap.Error(err))
			continue
		}
		for _, m := range mems {
			push(m)
		}
	}

	// 2) Fill remaining slots with generic top-K. Over-fetch so dedup
	//    doesn't leave us short of `limit`.
	remaining := limit - len(out)
	if remaining > 0 {
		general, err := o.memoryStore.Retrieve(ctx, userID, projectID, query, remaining*2)
		if err != nil {
			o.logger.Debug("generic memory retrieve failed", zap.Error(err))
		} else {
			for _, m := range general {
				if len(out) >= limit {
					break
				}
				push(m)
			}
		}
	}

	return out
}

// formatCoreMemory renders persona / human_context / project_context into a
// prompt-friendly block. Empty sections are skipped so a fresh session
// doesn't surface 3 empty headers.
//
// Uses GetMerged so project scope overlays user scope: persona written at
// user level ("I prefer Chinese responses") survives across projects;
// project_context written at project level overrides any user default.
// Scope origin is appended as a comment-style tag for transparency without
// bloating tokens.
//
// Errors are *not* propagated — core memory is "best effort" enrichment.
// A Redis hiccup must not block the user's main request.
func (o *Orchestrator) formatCoreMemory(ctx context.Context, userID, projectID string) string {
	cm, err := o.coreMemory.GetMerged(ctx, userID, projectID)
	if err != nil || cm == nil {
		if err != nil {
			o.logger.Debug("core memory fetch failed", zap.Error(err))
		}
		return ""
	}

	// Render in a stable order so KV-cache benefits from prefix matching.
	// Map iteration is random in Go, hence the explicit sequence.
	order := []string{"persona", "human_context", "project_context"}
	var lines []string
	render := func(name string, sec *memory.CoreMemorySection) {
		if sec == nil || strings.TrimSpace(sec.Content) == "" {
			return
		}
		header := fmt.Sprintf("### %s", name)
		if sec.Scope == memory.CoreScopeUser {
			// Mark user-scope entries so the LLM knows this preference
			// applies cross-project, not just to the current task.
			header += " (user)"
		}
		lines = append(lines, fmt.Sprintf("%s\n%s", header, strings.TrimSpace(sec.Content)))
	}
	for _, name := range order {
		render(name, cm.Sections[name])
	}
	// Render any *extra* sections (user-defined) after the canonical 3.
	for name, sec := range cm.Sections {
		if name == "persona" || name == "human_context" || name == "project_context" {
			continue
		}
		render(name, sec)
	}

	if len(lines) == 0 {
		return ""
	}
	return "[Core Memory]\n" + strings.Join(lines, "\n\n")
}
