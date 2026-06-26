package agentloop

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
)

// IntentEmbedder turns an intent string into a vector. Defined locally
// (instead of importing internal/memory.Embedder) so the agentloop
// package stays free of cross-package dependencies on memory — the two
// concerns are independent and we don't want them to ride together.
//
// Any embedder that satisfies memory.Embedder also satisfies this
// interface, so cmd/agent can pass the same instance to both.
type IntentEmbedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// PGTrajectoryStore persists execution trajectories in PostgreSQL and
// supports KNN recall by intent embedding (pgvector). When no embedder
// is configured, recall degrades to exact intent string match — which
// matches the legacy TrajectoryMemory behaviour, so swapping the store
// in does not regress unembedded deployments.
//
// Two-tier read strategy (see Retrieve): KNN first, then a string-equality
// fallback if the KNN result set is empty. The fallback exists because:
//   - early in a deployment the embedding index is empty;
//   - some intents are not natural-language strings (e.g. enum tokens like
//     "code_fix") and a degenerate embedding can rank dissimilar but
//     identically-named intents above the exact match.
type PGTrajectoryStore struct {
	db       *sql.DB
	logger   *zap.Logger
	embedder IntentEmbedder
	embedDim int // 0 means "skip CREATE TABLE dim enforcement and don't embed"
}

// NewPGTrajectoryStore constructs a Postgres-backed store. embedder may
// be nil — in that case Record skips the embedding column and Retrieve
// falls back to exact string matching. embedDim must match the embedder
// output (defaults to 1024 to match DashScope text-embedding-v3 which the
// rest of the system standardises on).
func NewPGTrajectoryStore(db *sql.DB, logger *zap.Logger, embedder IntentEmbedder, embedDim int) *PGTrajectoryStore {
	if embedDim <= 0 {
		embedDim = 1024
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &PGTrajectoryStore{
		db:       db,
		logger:   logger.With(zap.String("component", "agentloop.pg_trajectory_store")),
		embedder: embedder,
		embedDim: embedDim,
	}
}

// Migrate creates the trajectories table and indexes idempotently.
// Forward-compatible: every column uses IF NOT EXISTS so re-running on
// an existing deployment is a no-op.
func (s *PGTrajectoryStore) Migrate() error {
	createStmt := fmt.Sprintf(`
		CREATE EXTENSION IF NOT EXISTS vector;
		CREATE TABLE IF NOT EXISTS trajectories (
			id BIGSERIAL PRIMARY KEY,
			intent TEXT NOT NULL,
			tools JSONB NOT NULL,
			step_count INT NOT NULL,
			success BOOLEAN NOT NULL,
			intent_embedding vector(%d),
			created_at TIMESTAMPTZ DEFAULT NOW()
		);
		CREATE INDEX IF NOT EXISTS idx_trajectories_intent ON trajectories(intent);
		CREATE INDEX IF NOT EXISTS idx_trajectories_success ON trajectories(success);
		CREATE INDEX IF NOT EXISTS idx_trajectories_intent_embedding
			ON trajectories USING ivfflat (intent_embedding vector_cosine_ops) WITH (lists = 50);
	`, s.embedDim)
	if _, err := s.db.Exec(createStmt); err != nil {
		return fmt.Errorf("trajectory store migrate: %w", err)
	}
	return nil
}

// Record persists a new trajectory entry. If embedder is configured,
// intent is embedded and stored for later KNN recall; embedder failure
// is non-fatal — we keep the row, just without the vector (it will only
// be hit via the string-equality fallback path).
func (s *PGTrajectoryStore) Record(ctx context.Context, intent string, tools []string, success bool) error {
	if intent == "" {
		return nil
	}
	if len(tools) == 0 {
		return nil
	}
	if len(tools) > maxToolsPerEpisode {
		tools = tools[:maxToolsPerEpisode]
	}

	toolsJSON, err := json.Marshal(tools)
	if err != nil {
		return fmt.Errorf("trajectory record: marshal tools: %w", err)
	}

	var embeddingStr *string
	if s.embedder != nil {
		vecs, embErr := s.embedder.Embed(ctx, []string{intent})
		if embErr != nil {
			s.logger.Debug("intent embedding failed; storing without vector",
				zap.String("intent", intent), zap.Error(embErr))
		} else if len(vecs) > 0 && len(vecs[0]) == s.embedDim {
			vs := formatVectorAgent(vecs[0])
			embeddingStr = &vs
		} else if len(vecs) > 0 {
			s.logger.Warn("intent embedding dim mismatch; dropping vector",
				zap.Int("got", len(vecs[0])), zap.Int("want", s.embedDim))
		}
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO trajectories (intent, tools, step_count, success, intent_embedding)
		VALUES ($1, $2, $3, $4, $5)
	`, intent, toolsJSON, len(tools), success, embeddingStr)
	if err != nil {
		return fmt.Errorf("trajectory record: insert: %w", err)
	}
	return nil
}

// Retrieve returns up to `limit` successful trajectories matching the
// intent. KNN path is tried first (when an embedder is configured AND
// produces a vector AND at least one row in the table has an
// intent_embedding); failure or empty result falls back to exact-string
// match ordered by recency. This is the inverse of "string-equality
// always wins" so semantically similar intents recall the best-matching
// historical sequence, not just identically-named ones.
//
// Only success=true rows are returned by design — recalling failed
// trajectories is anti-pattern (the hint would teach the LLM to repeat
// the failure).
func (s *PGTrajectoryStore) Retrieve(ctx context.Context, intent string, limit int) ([]TrajectoryEntry, error) {
	if limit <= 0 {
		limit = trajectoryTopK
	}
	if intent == "" {
		return nil, nil
	}

	// 1) KNN path.
	if s.embedder != nil {
		vecs, err := s.embedder.Embed(ctx, []string{intent})
		if err == nil && len(vecs) > 0 && len(vecs[0]) == s.embedDim {
			vecStr := formatVectorAgent(vecs[0])
			entries, kerr := s.queryKNN(ctx, vecStr, limit)
			if kerr != nil {
				s.logger.Debug("trajectory KNN query failed; falling back to exact match",
					zap.Error(kerr))
			} else if len(entries) > 0 {
				return entries, nil
			}
		}
	}

	// 2) Exact-match fallback (covers cold-start and embedder-less deployments).
	rows, err := s.db.QueryContext(ctx, `
		SELECT intent, tools, step_count, success
		FROM trajectories
		WHERE intent = $1 AND success = true
		ORDER BY created_at DESC
		LIMIT $2
	`, intent, limit)
	if err != nil {
		return nil, fmt.Errorf("trajectory exact-match query: %w", err)
	}
	defer rows.Close()
	return scanTrajectoryRows(rows)
}

func (s *PGTrajectoryStore) queryKNN(ctx context.Context, vecStr string, limit int) ([]TrajectoryEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT intent, tools, step_count, success
		FROM trajectories
		WHERE success = true AND intent_embedding IS NOT NULL
		ORDER BY intent_embedding <=> $1
		LIMIT $2
	`, vecStr, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTrajectoryRows(rows)
}

func scanTrajectoryRows(rows *sql.Rows) ([]TrajectoryEntry, error) {
	var out []TrajectoryEntry
	for rows.Next() {
		var (
			intent    string
			toolsJSON []byte
			steps     int
			success   bool
		)
		if err := rows.Scan(&intent, &toolsJSON, &steps, &success); err != nil {
			return nil, err
		}
		var tools []string
		if err := json.Unmarshal(toolsJSON, &tools); err != nil {
			continue // skip malformed row rather than failing the whole recall
		}
		out = append(out, TrajectoryEntry{
			Intent:    intent,
			Tools:     tools,
			StepCount: steps,
			Success:   success,
		})
	}
	return out, rows.Err()
}

// Cleanup deletes trajectories older than the given duration, used by
// operators wanting to bound table growth. Currently unused by the
// scheduler — exposed for ad-hoc cron / migrations.
func (s *PGTrajectoryStore) Cleanup(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	res, err := s.db.ExecContext(ctx, `DELETE FROM trajectories WHERE created_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// formatVectorAgent mirrors memory.formatVector. Duplicated locally
// instead of moved to a shared internal package because (a) it's 12
// lines, (b) the two callers have no other shared types, and (c)
// introducing a `vectorpg` helper package solely for this would cost
// more than the duplication.
func formatVectorAgent(v []float32) string {
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
