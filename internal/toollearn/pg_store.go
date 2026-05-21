package toollearn

import (
	"database/sql"
	"time"
)

// PGStore implements Store using PostgreSQL.
type PGStore struct {
	db *sql.DB
}

// NewPGStore creates a PostgreSQL-backed feedback store.
func NewPGStore(db *sql.DB) *PGStore {
	return &PGStore{db: db}
}

// Migrate creates the tool_feedback table if it doesn't exist.
func (s *PGStore) Migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS tool_feedback (
			id BIGSERIAL PRIMARY KEY,
			tool_name TEXT NOT NULL,
			args_hash TEXT NOT NULL,
			success BOOLEAN NOT NULL,
			duration_ms INT NOT NULL,
			error_msg TEXT DEFAULT '',
			session_id TEXT NOT NULL,
			created_at TIMESTAMPTZ DEFAULT NOW()
		);
		CREATE INDEX IF NOT EXISTS idx_tool_feedback_tool ON tool_feedback(tool_name);
		CREATE INDEX IF NOT EXISTS idx_tool_feedback_created ON tool_feedback(created_at);
	`)
	return err
}

func (s *PGStore) RecordFeedback(fb *Feedback) error {
	_, err := s.db.Exec(
		`INSERT INTO tool_feedback (tool_name, args_hash, success, duration_ms, error_msg, session_id, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		fb.ToolName, fb.ArgsHash, fb.Success, fb.Duration, fb.ErrorMsg, fb.SessionID, fb.CreatedAt,
	)
	return err
}

func (s *PGStore) GetPatterns(toolName string, limit int) ([]ToolPattern, error) {
	rows, err := s.db.Query(`
		SELECT tool_name,
			   COUNT(*) as total,
			   SUM(CASE WHEN NOT success THEN 1 ELSE 0 END) as failures,
			   AVG(duration_ms) as avg_duration
		FROM tool_feedback
		WHERE ($1 = '' OR tool_name = $1)
		  AND created_at > NOW() - INTERVAL '7 days'
		GROUP BY tool_name
		ORDER BY total DESC
		LIMIT $2
	`, toolName, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var patterns []ToolPattern
	for rows.Next() {
		var p ToolPattern
		var total, failures int
		var avgDur float64
		if err := rows.Scan(&p.ToolName, &total, &failures, &avgDur); err != nil {
			continue
		}
		p.SampleSize = total
		if total > 0 {
			p.FailureRate = float64(failures) / float64(total)
		}
		p.AvgDuration = int(avgDur)
		patterns = append(patterns, p)
	}
	return patterns, rows.Err()
}

func (s *PGStore) GetRecentFailures(toolName string, window time.Duration) ([]Feedback, error) {
	rows, err := s.db.Query(`
		SELECT id, tool_name, args_hash, success, duration_ms, error_msg, session_id, created_at
		FROM tool_feedback
		WHERE tool_name = $1 AND NOT success AND created_at > NOW() - $2::interval
		ORDER BY created_at DESC
		LIMIT 50
	`, toolName, window.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []Feedback
	for rows.Next() {
		var fb Feedback
		if err := rows.Scan(&fb.ID, &fb.ToolName, &fb.ArgsHash, &fb.Success, &fb.Duration, &fb.ErrorMsg, &fb.SessionID, &fb.CreatedAt); err != nil {
			continue
		}
		results = append(results, fb)
	}
	return results, rows.Err()
}
