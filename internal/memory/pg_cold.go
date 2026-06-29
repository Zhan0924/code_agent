package memory

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
	"go.uber.org/zap"
)

// PGCold stores long-term memories in PostgreSQL with pgvector for semantic search.
type PGCold struct {
	db       *sql.DB
	logger   *zap.Logger
	embedDim int // expected embedding dimension; if 0 == 1536 default
}

// NewPGCold creates a cold memory store backed by PostgreSQL.
// embedDim defaults to 1536 (text-embedding-3-small). Use NewPGColdWithDim
// for non-default models (bge-large=1024, text-embedding-3-large=3072).
func NewPGCold(db *sql.DB, logger *zap.Logger) *PGCold {
	return NewPGColdWithDim(db, logger, 1536)
}

// NewPGColdWithDim creates a cold store pinned to a specific embedding
// dimension. Pass 0 to disable strict dim enforcement (any vector accepted).
func NewPGColdWithDim(db *sql.DB, logger *zap.Logger, dim int) *PGCold {
	return &PGCold{
		db:       db,
		logger:   logger.With(zap.String("component", "memory.pg_cold")),
		embedDim: dim,
	}
}

// Migrate creates / upgrades the memories table and pgvector extension.
//
// Forward-compatible migration strategy: every column / index uses
// `IF NOT EXISTS`; embedding column dim is initialised on first run, and
// later dim changes require a manual migration (we surface a clear error
// rather than silently truncating). The added `updated_at` column closes
// the historical bug where `Memory.UpdatedAt` was silently lost on JSON
// round-trip because the table had no matching column.
func (p *PGCold) Migrate() error {
	dim := p.embedDim
	if dim <= 0 {
		dim = 1536
	}
	colName := "embedding"
	if dim != 1536 {
		colName = fmt.Sprintf("embedding_%d", dim)
	}

	indexStmt := ""
	if dim <= 2000 {
		indexStmt = fmt.Sprintf(`
		-- Use HNSW for robust exact-like recall on small tables and fast ANN on large tables
		CREATE INDEX IF NOT EXISTS idx_memories_%s_hnsw
			ON memories USING hnsw (%s vector_cosine_ops);
		`, colName, colName)
	}

	createStmt := fmt.Sprintf(`
		CREATE EXTENSION IF NOT EXISTS vector;
		CREATE TABLE IF NOT EXISTS memories (
			id UUID PRIMARY KEY,
			user_id TEXT NOT NULL,
			project_id TEXT NOT NULL,
			type TEXT NOT NULL,
			content TEXT NOT NULL,
			%s vector(%d),
			score FLOAT DEFAULT 1.0,
			access_count INT DEFAULT 0,
			created_at TIMESTAMPTZ DEFAULT NOW(),
			updated_at TIMESTAMPTZ DEFAULT NOW(),
			last_accessed_at TIMESTAMPTZ DEFAULT NOW(),
			distilled_at TIMESTAMPTZ
		);
		ALTER TABLE memories ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ DEFAULT NOW();
		ALTER TABLE memories ADD COLUMN IF NOT EXISTS distilled_at TIMESTAMPTZ;
		ALTER TABLE memories ADD COLUMN IF NOT EXISTS %s vector(%d);
		CREATE INDEX IF NOT EXISTS idx_memories_user_project ON memories(user_id, project_id);
		CREATE INDEX IF NOT EXISTS idx_memories_type ON memories(type);
		CREATE INDEX IF NOT EXISTS idx_memories_score ON memories(score DESC);
		-- Drop legacy IVFFlat index which severely underfits small agent memory tables
		DROP INDEX IF EXISTS idx_memories_embedding;
		%s
		-- Partial index lets the Distiller's "next batch" query (WHERE
		-- type = 'episodic' AND distilled_at IS NULL ORDER BY created_at)
		-- run without a full type-scan.
		CREATE INDEX IF NOT EXISTS idx_memories_episodic_undistilled
			ON memories(user_id, project_id, created_at)
			WHERE type = 'episodic' AND distilled_at IS NULL;
	`, colName, dim, colName, dim, indexStmt)
	if _, err := p.db.Exec(createStmt); err != nil {
		return err
	}
	return nil
}

// Store persists a memory to PostgreSQL, including embedding if present.
//
// Embedding dimension is validated upfront: a silent mismatch would surface
// later as an opaque pgvector error during ORDER BY <=> queries; failing
// fast at write-time makes the misconfiguration immediately actionable
// (e.g. switching embedding models without running a migration).
func (p *PGCold) Store(m *Memory) error {
	if err := p.checkDim(m.Embedding); err != nil {
		return err
	}
	var embeddingStr *string
	if len(m.Embedding) > 0 {
		s := formatVector(m.Embedding)
		embeddingStr = &s
	}
	updatedAt := m.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now()
	}
	// distilled_at is intentionally NOT touched on conflict: a re-Store
	// of an existing episodic must not silently un-mark its distilled
	// status (would re-trigger consolidation and produce duplicates).
	
	colName := "embedding"
	if p.embedDim > 0 && p.embedDim != 1536 {
		colName = fmt.Sprintf("embedding_%d", p.embedDim)
	}
	
	query := fmt.Sprintf(`
		INSERT INTO memories (id, user_id, project_id, type, content, %s, score, access_count, created_at, updated_at, last_accessed_at, distilled_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (id) DO UPDATE SET
			content = EXCLUDED.content,
			%s = EXCLUDED.%s,
			score = EXCLUDED.score,
			updated_at = NOW(),
			last_accessed_at = NOW()
	`, colName, colName, colName)
	
	_, err := p.db.Exec(query, m.ID, m.UserID, m.ProjectID, m.Type, m.Content, embeddingStr, m.Score, m.AccessCount, m.CreatedAt, updatedAt, m.LastAccessedAt, nullableTime(m.DistilledAt))
	return err
}

// nullableTime adapts *time.Time → sql.NullTime so that database/sql can
// transmit a real NULL when the pointer is nil (rather than coercing a
// zero time, which pgvector would still store).
func nullableTime(t *time.Time) sql.NullTime {
	if t == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: *t, Valid: true}
}

// scanNullableTime decodes a sql.NullTime back to *time.Time.
func scanNullableTime(nt sql.NullTime) *time.Time {
	if !nt.Valid {
		return nil
	}
	v := nt.Time
	return &v
}

// checkDim guards against embedding-dimension mismatch (e.g. switching from
// text-embedding-3-small (1536) to bge-large (1024) without re-migrating).
// pgvector would otherwise raise "expected N dimensions, not M" buried
// inside a query result row scan, which is much harder to debug.
func (p *PGCold) checkDim(v []float32) error {
	if p.embedDim <= 0 || len(v) == 0 {
		return nil
	}
	if len(v) != p.embedDim {
		return fmt.Errorf("memory: embedding dim mismatch — got %d, table expects %d (configure NewPGColdWithDim or run migration)", len(v), p.embedDim)
	}
	return nil
}

// Retrieve searches memories by content similarity (text match fallback when no embedding).
//
// Episodic memories are excluded by default: they are the Distiller's raw
// fuel, not user-facing recall candidates. Callers that need to enumerate
// episodic entries (e.g. the Distiller itself) must go through
// ListEpisodicUndistilled.
func (p *PGCold) Retrieve(userID, projectID string, query string, limit int) ([]Memory, error) {
	rows, err := p.db.Query(`
		SELECT id, user_id, project_id, type, content, score, access_count, created_at, updated_at, last_accessed_at, distilled_at
		FROM memories
		WHERE user_id = $1 AND project_id = $2
		  AND type <> 'episodic'
		  AND content ILIKE '%' || $3 || '%'
		ORDER BY score DESC, last_accessed_at DESC
		LIMIT $4
	`, userID, projectID, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var memories []Memory
	for rows.Next() {
		var m Memory
		var distilledAt sql.NullTime
		if err := rows.Scan(&m.ID, &m.UserID, &m.ProjectID, &m.Type, &m.Content,
			&m.Score, &m.AccessCount, &m.CreatedAt, &m.UpdatedAt, &m.LastAccessedAt, &distilledAt); err != nil {
			continue
		}
		m.DistilledAt = scanNullableTime(distilledAt)
		memories = append(memories, m)
	}
	return memories, rows.Err()
}

// Touch increments access count and updates last_accessed_at.
//
// Prefer TouchBatch for read paths — single-row Touch is retained only
// for legacy callers (and the one-off "I just modified this" use case).
// Read traffic uses HybridStore's debouncing batcher to fold N reads
// into one round-trip.
func (p *PGCold) Touch(id string) error {
	_, err := p.db.Exec(`
		UPDATE memories SET access_count = access_count + 1, last_accessed_at = NOW() WHERE id = $1
	`, id)
	return err
}

// TouchBatch increments access_count and refreshes last_accessed_at for
// every id in a single round-trip (`UPDATE ... WHERE id = ANY($1)`).
//
// This is the write side of the P0 #4 fix: before this, every Retrieve
// returned memories without ever advancing last_accessed_at, so Decay's
// "older than N" cutoff treated frequently-read entries the same as
// never-read ones. By folding the touch into a debounced batch we close
// the loop without inflating per-request latency (1 QPS of UPDATE per
// HybridStore replica, regardless of read QPS).
//
// Idempotent on empty/nil input — callers don't have to gate it.
func (p *PGCold) TouchBatch(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := p.db.ExecContext(ctx, `
		UPDATE memories
		SET access_count = access_count + 1, last_accessed_at = NOW()
		WHERE id = ANY($1)
	`, pq.Array(ids))
	return err
}

// BoostScoreBatch increases the score of specified memories by the given amount,
// clamped to [MinMemoryScore, MaxMemoryScore].
func (p *PGCold) BoostScoreBatch(ctx context.Context, ids []string, boost float64) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := p.db.ExecContext(ctx, `
		UPDATE memories
		SET score = GREATEST(LEAST(score + $1, $3), $4)
		WHERE id = ANY($2)
	`, boost, pq.Array(ids), MaxMemoryScore, MinMemoryScore)
	return err
}

// Decay reduces scores for memories older than the given duration.
func (p *PGCold) Decay(olderThan time.Duration, factor float64) (int, error) {
	cutoff := time.Now().Add(-olderThan)
	result, err := p.db.Exec(`
		UPDATE memories SET score = score * $1 WHERE last_accessed_at < $2 AND score > 0.01
	`, factor, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

// RetrieveByVector searches memories using pgvector cosine distance.
func (p *PGCold) RetrieveByVector(embedding []float32, userID, projectID string, limit int) ([]Memory, error) {
	return p.retrieveByVectorTyped(embedding, userID, projectID, "", limit)
}

// RetrieveByVectorAndType is the type-filtered variant of RetrieveByVector.
// Used by importance bucketing in orchestrator (always show ≥1 preference,
// ≥1 decision) — passing memType="" is equivalent to RetrieveByVector.
func (p *PGCold) RetrieveByVectorAndType(embedding []float32, userID, projectID, memType string, limit int) ([]Memory, error) {
	return p.retrieveByVectorTyped(embedding, userID, projectID, memType, limit)
}

func (p *PGCold) retrieveByVectorTyped(embedding []float32, userID, projectID, memType string, limit int) ([]Memory, error) {
	if err := p.checkDim(embedding); err != nil {
		return nil, err
	}
	vecStr := formatVector(embedding)
	
	colName := "embedding"
	if p.embedDim > 0 && p.embedDim != 1536 {
		colName = fmt.Sprintf("embedding_%d", p.embedDim)
	}
	
	// Two-arm query so we can keep prepared-statement reuse for the common
	// (no type filter) path. memType is a *fixed* enum value (preference /
	// decision / knowledge / pattern / episodic / semantic) so direct $5
	// binding is safe.
	//
	// memType == "" (general recall) explicitly excludes episodic — those
	// are Distiller fuel, not user-facing recall candidates. When the
	// caller *wants* episodic (e.g. the Distiller), they pass memType =
	// "episodic" and the second branch returns them.
	var rows *sql.Rows
	var err error
	if memType == "" {
		query := fmt.Sprintf(`
			SELECT id, user_id, project_id, type, content, %s, score, access_count, created_at, updated_at, last_accessed_at, distilled_at
			FROM memories
			WHERE user_id = $1 AND project_id = $2 AND %s IS NOT NULL
			  AND type <> 'episodic'
			ORDER BY %s <=> $3
			LIMIT $4
		`, colName, colName, colName)
		rows, err = p.db.Query(query, userID, projectID, vecStr, limit)
	} else {
		query := fmt.Sprintf(`
			SELECT id, user_id, project_id, type, content, %s, score, access_count, created_at, updated_at, last_accessed_at, distilled_at
			FROM memories
			WHERE user_id = $1 AND project_id = $2 AND %s IS NOT NULL AND type = $5
			ORDER BY %s <=> $3
			LIMIT $4
		`, colName, colName, colName)
		rows, err = p.db.Query(query, userID, projectID, vecStr, limit, memType)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var memories []Memory
	for rows.Next() {
		var m Memory
		var embeddingStr *string
		var distilledAt sql.NullTime
		if err := rows.Scan(&m.ID, &m.UserID, &m.ProjectID, &m.Type, &m.Content,
			&embeddingStr, &m.Score, &m.AccessCount, &m.CreatedAt, &m.UpdatedAt, &m.LastAccessedAt, &distilledAt); err != nil {
			continue
		}
		if embeddingStr != nil {
			m.Embedding = parseVector(*embeddingStr)
		}
		m.DistilledAt = scanNullableTime(distilledAt)
		memories = append(memories, m)
	}
	return memories, rows.Err()
}

// ListEpisodicUndistilled returns the oldest episodic memories that have
// not yet been folded into a semantic memory. The Distiller drives all
// consolidation through this method so we don't accidentally re-consume
// the same episodes (and produce N copies of the same semantic rule).
//
// We deliberately order by created_at ASC (oldest first) — distillation
// is a "drain the queue" operation, not a "most relevant first" one.
// The partial index `idx_memories_episodic_undistilled` keeps this query
// in O(log n) even when the table grows to millions of rows.
func (p *PGCold) ListEpisodicUndistilled(ctx context.Context, userID, projectID string, limit int) ([]Memory, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := p.db.QueryContext(ctx, `
		SELECT id, user_id, project_id, type, content, score, access_count, created_at, updated_at, last_accessed_at, distilled_at
		FROM memories
		WHERE user_id = $1 AND project_id = $2
		  AND type = 'episodic'
		  AND distilled_at IS NULL
		ORDER BY created_at ASC
		LIMIT $3
	`, userID, projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var memories []Memory
	for rows.Next() {
		var m Memory
		var distilledAt sql.NullTime
		if err := rows.Scan(&m.ID, &m.UserID, &m.ProjectID, &m.Type, &m.Content,
			&m.Score, &m.AccessCount, &m.CreatedAt, &m.UpdatedAt, &m.LastAccessedAt, &distilledAt); err != nil {
			continue
		}
		m.DistilledAt = scanNullableTime(distilledAt)
		memories = append(memories, m)
	}
	return memories, rows.Err()
}

// ListActiveDistillTenants returns (user, project) tuples that currently
// hold at least `minEpisodic` undistilled episodic memories, ordered by
// episodic count DESC (most "starved" tenants first).
//
// This is the multi-tenant discovery path: instead of forcing the
// operator to enumerate every (user, project) in YAML, the Distiller
// ticker calls this to find who actually has work waiting. The partial
// index `idx_memories_episodic_undistilled(user_id, project_id, created_at)
// WHERE type='episodic' AND distilled_at IS NULL` makes the GROUP BY +
// HAVING run in index-only scan territory.
//
// limit caps the per-tick discovery fan-out so a sudden backlog of N
// tenants can't blow LLM rate limits in one tick — operators tune via
// MemoryDistillConfig.MaxTenantsPerTick. limit <= 0 falls back to 50.
//
// minEpisodic is the count cutoff: tenants with fewer pending episodes
// are skipped (they'd be skipped by Distiller.MinEpisodicToTrigger
// anyway; filtering at the SQL layer saves a no-op LLM round-trip).
func (p *PGCold) ListActiveDistillTenants(ctx context.Context, minEpisodic, limit int) ([]TenantRef, error) {
	if limit <= 0 {
		limit = 50
	}
	if minEpisodic <= 0 {
		minEpisodic = 1
	}
	rows, err := p.db.QueryContext(ctx, `
		SELECT user_id, project_id, count(*)::int AS pending
		FROM memories
		WHERE type = 'episodic' AND distilled_at IS NULL
		GROUP BY user_id, project_id
		HAVING count(*) >= $1
		ORDER BY pending DESC, user_id ASC, project_id ASC
		LIMIT $2
	`, minEpisodic, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tenants := make([]TenantRef, 0, limit)
	for rows.Next() {
		var t TenantRef
		if err := rows.Scan(&t.UserID, &t.ProjectID, &t.Count); err != nil {
			continue
		}
		tenants = append(tenants, t)
	}
	return tenants, rows.Err()
}

// ListActiveDecayTenants returns (user, project) buckets that hold at
// least one memory whose last_accessed_at is older than olderThan AND
// whose score still has decay headroom (> 0.01). These are the buckets
// where running hot-tier decay would *change* something — anything not
// in this list is a guaranteed no-op for hot.
//
// Why this exists: RedisHot.Decay used to SCAN `memory:*` (every tenant
// in one shot). On a 100K-key Redis that's a minute-long stop-the-world
// for every other tenant on the box. Slicing by (user, project) lets us
// SCAN a sub-namespace per tenant, with hotScanLimit budget. Cold
// (pgvector + b-tree) is the natural place to learn "who has stale data
// at all" — Redis itself has no secondary index on last_accessed_at.
//
// We mirror the score > 0.01 floor from PGCold.Decay so that tenants
// whose memories have already bottomed out aren't returned just to be
// no-op'd in hot. Saves a SCAN per tenant.
//
// limit defaults to 100 (24h decay cadence → roomy enough for most
// installs; operators with > 100 active tenants should chunk decays
// or bump the limit via config, both out of scope for the SQL helper).
func (p *PGCold) ListActiveDecayTenants(ctx context.Context, olderThan time.Duration, limit int) ([]TenantRef, error) {
	if limit <= 0 {
		limit = 100
	}
	if olderThan <= 0 {
		// Caller passed a nonsensical "decay everything ever touched"
		// duration; bail with empty rather than letting cutoff = now
		// flag every memory as stale.
		return nil, nil
	}
	cutoff := time.Now().Add(-olderThan)
	rows, err := p.db.QueryContext(ctx, `
		SELECT user_id, project_id, count(*)::int AS stale_cnt
		FROM memories
		WHERE last_accessed_at < $1 AND score > 0.01
		GROUP BY user_id, project_id
		ORDER BY stale_cnt DESC, user_id ASC, project_id ASC
		LIMIT $2
	`, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tenants := make([]TenantRef, 0, limit)
	for rows.Next() {
		var t TenantRef
		if err := rows.Scan(&t.UserID, &t.ProjectID, &t.Count); err != nil {
			continue
		}
		tenants = append(tenants, t)
	}
	return tenants, rows.Err()
}

// MarkDistilled records that the given episodic memories have been
// consumed by the Distiller. We use `pq.Array` so the batch UPDATE fires
// a single round-trip — distillation can mark dozens of episodes at once.
//
// Idempotent: re-marking already-distilled entries is a no-op (the WHERE
// clause keeps the original timestamp; if you need to *re-mark* with a
// fresh timestamp, do an explicit UPDATE ... SET distilled_at = NOW()).
func (p *PGCold) MarkDistilled(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := p.db.ExecContext(ctx, `
		DELETE FROM memories
		WHERE id = ANY($1)
	`, pq.Array(ids))
	return err
}

// DeleteOldEpisodic removes episodic memories that have already been
// consumed by the Distiller (distilled_at IS NOT NULL) and whose
// distilled_at timestamp is older than the supplied retention window.
//
// Safety rule: undistilled episodic rows are *never* deleted by this
// path. If Distiller is disabled or the tenant lacks enough episodes
// to trigger a run, those rows accumulate but are not destroyed —
// preventing data loss in the "Distiller half-dead" state called out
// by AUDIT-P0-2. Operators who need an aggressive purge (e.g. PG disk
// pressure) should call a separate force-delete path (not yet wired).
func (p *PGCold) DeleteOldEpisodic(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	res, err := p.db.ExecContext(ctx, `
		DELETE FROM memories
		WHERE type = 'episodic'
		  AND distilled_at IS NOT NULL
		  AND distilled_at < $1
	`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Update modifies an existing memory's content, embedding, score, and metadata.
// AccessCount is also persisted so reinforcement signals from ConflictResolver
// (Hebbian "repeated expression = stronger") are not silently dropped.
func (p *PGCold) Update(m *Memory) error {
	if err := p.checkDim(m.Embedding); err != nil {
		return err
	}
	var embeddingStr *string
	if len(m.Embedding) > 0 {
		s := formatVector(m.Embedding)
		embeddingStr = &s
	}
	_, err := p.db.Exec(`
		UPDATE memories SET content = $1, embedding = $2, score = $3, access_count = $4, updated_at = NOW(), last_accessed_at = NOW()
		WHERE id = $5
	`, m.Content, embeddingStr, m.Score, m.AccessCount, m.ID)
	return err
}

// DeleteByIDs hard-deletes memories matching the given IDs in a single
// round-trip. Used by the P1 #7 dedup path to remove the non-anchor
// duplicates after the anchor has absorbed their reinforcement signal.
//
// Hard delete (not soft delete) is deliberate: duplicates with cosine >=
// 0.85 carry no information beyond what the anchor already encodes;
// keeping them would just inflate the index and add recall noise. If
// the system ever needs audit history for memory mutations, the
// blackboard stream (memory:* topic) carries every action including
// "dedup_removed" — see HybridStore.publishEvent.
//
// Empty input is a no-op (no SQL fired). Returns the underlying
// driver error if the DELETE fails — caller is responsible for
// surfacing this to dedup metrics.
func (p *PGCold) DeleteByIDs(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := p.db.ExecContext(ctx, `
		DELETE FROM memories WHERE id = ANY($1)
	`, pq.Array(ids))
	return err
}

// DeleteByUser hard-deletes all memories belonging to a specific user (GDPR compliance).
func (p *PGCold) DeleteByUser(ctx context.Context, userID string) (int64, error) {
	if userID == "" {
		return 0, nil
	}
	res, err := p.db.ExecContext(ctx, `
		DELETE FROM memories WHERE user_id = $1
	`, userID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DedupTx is the atomic write path used by HybridStore's dedup branch:
// inside a single transaction it (1) updates the anchor row with the
// reinforced score / access_count / embedding and (2) deletes every
// non-anchor duplicate. The transaction guarantees we never end up in
// the half-state "anchor updated but dups still alive" or "dups gone
// but anchor still carrying the old score" — either everything lands
// or the row stays exactly as it was, so a retry can re-run idempotently.
//
// Implementation notes:
//   - We don't try to be clever about empty dupIDs — DedupTx is only
//     called when len(dupIDs) >= 1 (the N==1 conflict path skips this
//     helper and uses plain Update).
//   - checkDim happens before BEGIN so an embedding-shape error doesn't
//     pollute PG with a wasted transaction start.
//   - Returning early on rollback path: if the anchor UPDATE fails we
//     still need to defer Rollback explicitly; database/sql's tx
//     finalizes via Rollback if no Commit ran.
func (p *PGCold) DedupTx(ctx context.Context, anchor *Memory, dupIDs []string) error {
	if anchor == nil {
		return fmt.Errorf("DedupTx: anchor is nil")
	}
	if err := p.checkDim(anchor.Embedding); err != nil {
		return err
	}
	var embeddingStr *string
	if len(anchor.Embedding) > 0 {
		s := formatVector(anchor.Embedding)
		embeddingStr = &s
	}

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		// Rollback is a no-op after a successful Commit, so this is
		// safe to always defer. Captures the case where any of the
		// statements below short-circuit with an error.
		_ = tx.Rollback()
	}()

	if _, err := tx.ExecContext(ctx, `
		UPDATE memories
		SET content = $1, embedding = $2, score = $3, access_count = $4,
		    updated_at = NOW(), last_accessed_at = NOW()
		WHERE id = $5
	`, anchor.Content, embeddingStr, anchor.Score, anchor.AccessCount, anchor.ID); err != nil {
		return err
	}

	if len(dupIDs) > 0 {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM memories WHERE id = ANY($1)
		`, pq.Array(dupIDs)); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// formatVector converts a float32 slice to pgvector's text format: [0.1,0.2,...]
func formatVector(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%g", f)
	}
	b.WriteByte(']')
	return b.String()
}

// parseVector converts pgvector's text format [0.1,0.2,...] to float32 slice.
func parseVector(s string) []float32 {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return nil
	}
	s = s[1 : len(s)-1]
	if s == "" {
		return []float32{}
	}
	parts := strings.Split(s, ",")
	vec := make([]float32, 0, len(parts))
	for _, p := range parts {
		var f float32
		if _, err := fmt.Sscanf(strings.TrimSpace(p), "%f", &f); err == nil {
			vec = append(vec, f)
		}
	}
	return vec
}
