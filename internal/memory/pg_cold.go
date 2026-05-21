package memory

import (
	"database/sql"
	"time"

	"go.uber.org/zap"
)

// PGCold stores long-term memories in PostgreSQL with pgvector for semantic search.
type PGCold struct {
	db     *sql.DB
	logger *zap.Logger
}

// NewPGCold creates a cold memory store backed by PostgreSQL.
func NewPGCold(db *sql.DB, logger *zap.Logger) *PGCold {
	return &PGCold{
		db:     db,
		logger: logger.With(zap.String("component", "memory.pg_cold")),
	}
}

// Migrate creates the memories table and pgvector extension.
func (p *PGCold) Migrate() error {
	_, err := p.db.Exec(`
		CREATE EXTENSION IF NOT EXISTS vector;
		CREATE TABLE IF NOT EXISTS memories (
			id UUID PRIMARY KEY,
			user_id TEXT NOT NULL,
			project_id TEXT NOT NULL,
			type TEXT NOT NULL,
			content TEXT NOT NULL,
			embedding vector(1536),
			score FLOAT DEFAULT 1.0,
			access_count INT DEFAULT 0,
			created_at TIMESTAMPTZ DEFAULT NOW(),
			last_accessed_at TIMESTAMPTZ DEFAULT NOW()
		);
		CREATE INDEX IF NOT EXISTS idx_memories_user_project ON memories(user_id, project_id);
		CREATE INDEX IF NOT EXISTS idx_memories_type ON memories(type);
		CREATE INDEX IF NOT EXISTS idx_memories_score ON memories(score DESC);
	`)
	return err
}

// Store persists a memory to PostgreSQL.
func (p *PGCold) Store(m *Memory) error {
	_, err := p.db.Exec(`
		INSERT INTO memories (id, user_id, project_id, type, content, score, access_count, created_at, last_accessed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (id) DO UPDATE SET
			content = EXCLUDED.content,
			score = EXCLUDED.score,
			last_accessed_at = NOW()
	`, m.ID, m.UserID, m.ProjectID, m.Type, m.Content, m.Score, m.AccessCount, m.CreatedAt, m.LastAccessedAt)
	return err
}

// Retrieve searches memories by content similarity (text match fallback when no embedding).
func (p *PGCold) Retrieve(userID, projectID string, query string, limit int) ([]Memory, error) {
	rows, err := p.db.Query(`
		SELECT id, user_id, project_id, type, content, score, access_count, created_at, last_accessed_at
		FROM memories
		WHERE user_id = $1 AND project_id = $2
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
		if err := rows.Scan(&m.ID, &m.UserID, &m.ProjectID, &m.Type, &m.Content,
			&m.Score, &m.AccessCount, &m.CreatedAt, &m.LastAccessedAt); err != nil {
			continue
		}
		memories = append(memories, m)
	}
	return memories, rows.Err()
}

// Touch increments access count and updates last_accessed_at.
func (p *PGCold) Touch(id string) error {
	_, err := p.db.Exec(`
		UPDATE memories SET access_count = access_count + 1, last_accessed_at = NOW() WHERE id = $1
	`, id)
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
