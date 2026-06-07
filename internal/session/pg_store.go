package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agent/code_agent/internal/models"
	"go.uber.org/zap"
)

// ─── SessionStore Interface ──────────────────────────────────────────────────
//
// SessionStore is the cold-but-durable layer that backs Redis hot/cold (which
// expire after 24h/48h). It exists so the sidebar listing and rehydrate path
// survive Redis TTL — without it, any session older than the cold TTL falls
// off the sidebar permanently (the previous ListSessions implementation even
// ZREM'd these as "stale", making the disappearance irreversible).
//
// All methods MUST tolerate nil interface usage at the Manager site: the
// Manager treats `m.PGStore == nil` as "Redis-only legacy mode" and shorts
// out before calling.

// ErrSessionNotFound is returned by SessionStore.Get when no row matches.
// Manager treats this as "fall through to the next layer" (today: surface to
// caller; future: try external cold archive).
var ErrSessionNotFound = errors.New("session not found in pg store")

// SessionStore is the persistence contract for long-term session storage.
type SessionStore interface {
	// Upsert writes (or replaces) the full Session row.
	Upsert(ctx context.Context, s *models.Session) error

	// Get hydrates a Session by ID. Returns ErrSessionNotFound when missing.
	Get(ctx context.Context, sessionID string) (*models.Session, error)

	// ListByUser returns the most-recently-updated sessions for a user.
	// Manager uses this as the authoritative sidebar feed when PGStore is wired.
	ListByUser(ctx context.Context, userID string, limit int) ([]*SessionSummary, error)

	// Delete removes the row for sessionID. Missing rows are NOT an error.
	Delete(ctx context.Context, sessionID string) error
}

// ─── PGSessionStore ──────────────────────────────────────────────────────────

// PGSessionStore persists sessions into the `sessions` table defined in
// internal/store/postgres.go::Migrate. It stores the full models.Session as
// JSONB so a single row read fully rehydrates a session — the existing
// performHotColdSeparation keeps the JSON within bounds.
type PGSessionStore struct {
	db     *sql.DB
	logger *zap.Logger
}

// NewPGSessionStore wires a SessionStore over an *sql.DB. The DB is expected
// to have been migrated already (store.Store.Migrate runs at startup).
func NewPGSessionStore(db *sql.DB, logger *zap.Logger) *PGSessionStore {
	return &PGSessionStore{
		db:     db,
		logger: logger.With(zap.String("component", "session.pg_store")),
	}
}

// Upsert writes the full session JSON, refreshing the projection columns
// (message_count / last_role / last_preview / updated_at) that the sidebar
// listing reads without unmarshalling `data`.
func (p *PGSessionStore) Upsert(ctx context.Context, s *models.Session) error {
	if s == nil {
		return fmt.Errorf("session is nil")
	}
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("marshal session: %w", err)
	}

	lastRole, lastPreview := lastNonEmptyMessage(s)

	userID := s.UserID
	if userID == "" {
		userID = AnonymousUserID
	}
	projectID := s.ProjectID
	if projectID == "" {
		projectID = "default"
	}

	created := s.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	updated := s.UpdatedAt
	if updated.IsZero() {
		updated = time.Now()
	}

	_, err = p.db.ExecContext(ctx, `
		INSERT INTO sessions (
			id, user_id, project_id, data,
			message_count, last_role, last_preview,
			created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (id) DO UPDATE SET
			user_id       = EXCLUDED.user_id,
			project_id    = EXCLUDED.project_id,
			data          = EXCLUDED.data,
			message_count = EXCLUDED.message_count,
			last_role     = EXCLUDED.last_role,
			last_preview  = EXCLUDED.last_preview,
			updated_at    = EXCLUDED.updated_at
	`,
		s.ID, userID, projectID, data,
		len(s.Messages), lastRole, lastPreview,
		created, updated,
	)
	if err != nil {
		return fmt.Errorf("upsert session: %w", err)
	}
	return nil
}

// Get fetches the JSONB row and unmarshals it back into a Session.
func (p *PGSessionStore) Get(ctx context.Context, sessionID string) (*models.Session, error) {
	var data []byte
	err := p.db.QueryRowContext(ctx,
		`SELECT data FROM sessions WHERE id = $1`, sessionID,
	).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("select session: %w", err)
	}

	var s models.Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("unmarshal session: %w", err)
	}
	return &s, nil
}

// ListByUser returns SessionSummary rows ordered by recency. The projection
// columns avoid unmarshalling the full data blob for every entry.
func (p *PGSessionStore) ListByUser(ctx context.Context, userID string, limit int) ([]*SessionSummary, error) {
	if userID == "" {
		userID = AnonymousUserID
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	rows, err := p.db.QueryContext(ctx, `
		SELECT id, user_id, project_id, message_count, last_role, last_preview, created_at, updated_at
		FROM sessions
		WHERE user_id = $1
		ORDER BY updated_at DESC
		LIMIT $2
	`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	out := make([]*SessionSummary, 0, limit)
	for rows.Next() {
		var s SessionSummary
		if err := rows.Scan(
			&s.ID, &s.UserID, &s.ProjectID,
			&s.MessageCount, &s.LastMessageRole, &s.LastMessagePreview,
			&s.CreatedAt, &s.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan session row: %w", err)
		}
		out = append(out, &s)
	}
	return out, rows.Err()
}

// Delete removes the row. Missing row is not an error — Manager's Delete is
// the only caller and treats this idempotently.
func (p *PGSessionStore) Delete(ctx context.Context, sessionID string) error {
	_, err := p.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = $1`, sessionID)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// lastNonEmptyMessage scans backwards for a preview line, matching the
// buildSummary heuristic so the PG-only listing matches the Redis-backed one.
func lastNonEmptyMessage(s *models.Session) (role, preview string) {
	for i := len(s.Messages) - 1; i >= 0; i-- {
		if s.Messages[i].Content == "" {
			continue
		}
		return string(s.Messages[i].Role), truncate(s.Messages[i].Content, 120)
	}
	return "", ""
}
