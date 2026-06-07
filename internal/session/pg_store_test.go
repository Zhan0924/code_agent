package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/agent/code_agent/internal/models"
	"go.uber.org/zap"
)

// newPGStoreMock wires a PGSessionStore over a sqlmock DB. We use
// QueryMatcherRegexp so the long parameterized SQL in pg_store.go can be
// matched on the salient table+verb without re-pasting whitespace.
func newPGStoreMock(t *testing.T) (*PGSessionStore, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	store := NewPGSessionStore(db, zap.NewNop())
	return store, mock, func() { _ = db.Close() }
}

// Upsert must hit the INSERT ... ON CONFLICT (id) DO UPDATE branch with all
// nine columns. We don't pin column-by-column values — the JSON column is
// large and order-sensitive matchers are fragile — but we DO verify the
// canonical user_id ("" → anonymous) and message_count projection.
func TestPGSessionStore_Upsert_HappyPath(t *testing.T) {
	store, mock, done := newPGStoreMock(t)
	defer done()

	session := &models.Session{
		ID:        "sess-1",
		UserID:    "", // expect normalization to AnonymousUserID
		ProjectID: "",
		Messages: []models.Message{
			{Role: models.RoleUser, Content: "first"},
			{Role: models.RoleAssistant, Content: "second"},
		},
		CreatedAt: time.Now().Add(-time.Hour),
		UpdatedAt: time.Now(),
	}

	mock.ExpectExec(`INSERT INTO sessions`).
		WithArgs(
			"sess-1",
			AnonymousUserID,
			"default",
			sqlmock.AnyArg(), // data JSONB
			2,                // message_count
			"assistant",      // last_role from buildSummary heuristic
			"second",         // last_preview
			sqlmock.AnyArg(), // created_at
			sqlmock.AnyArg(), // updated_at
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.Upsert(context.Background(), session); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Get round-trips through JSON: marshal → mock row → Unmarshal must yield
// the same Messages slice. Catches regressions in the data column codec.
func TestPGSessionStore_Get_RoundTrip(t *testing.T) {
	store, mock, done := newPGStoreMock(t)
	defer done()

	original := &models.Session{
		ID:        "sess-2",
		UserID:    "alice",
		ProjectID: "proj",
		Messages: []models.Message{
			{Role: models.RoleUser, Content: "hello"},
		},
	}
	data, _ := json.Marshal(original)
	mock.ExpectQuery(`SELECT data FROM sessions WHERE id`).
		WithArgs("sess-2").
		WillReturnRows(sqlmock.NewRows([]string{"data"}).AddRow(data))

	got, err := store.Get(context.Background(), "sess-2")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != "sess-2" || got.UserID != "alice" {
		t.Fatalf("Get mismatch: %+v", got)
	}
	if len(got.Messages) != 1 || got.Messages[0].Content != "hello" {
		t.Fatalf("messages not round-tripped: %+v", got.Messages)
	}
}

// Get on a missing row must surface ErrSessionNotFound — Manager uses
// errors.Is on this sentinel to silently fall through.
func TestPGSessionStore_Get_NotFound(t *testing.T) {
	store, mock, done := newPGStoreMock(t)
	defer done()

	mock.ExpectQuery(`SELECT data FROM sessions WHERE id`).
		WithArgs("missing").
		WillReturnError(sql.ErrNoRows)

	_, err := store.Get(context.Background(), "missing")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound, got %v", err)
	}
}

// ListByUser must order by updated_at DESC with a LIMIT clause and project
// the lightweight summary columns. We assert at least the user_id and limit
// args, and that the result honors the order from the mock.
func TestPGSessionStore_ListByUser(t *testing.T) {
	store, mock, done := newPGStoreMock(t)
	defer done()

	now := time.Now()
	earlier := now.Add(-time.Hour)
	rows := sqlmock.NewRows([]string{
		"id", "user_id", "project_id", "message_count",
		"last_role", "last_preview", "created_at", "updated_at",
	}).
		AddRow("recent", "alice", "proj", 5, "assistant", "latest reply", earlier, now).
		AddRow("older", "alice", "proj", 2, "user", "first msg", earlier.Add(-time.Hour), earlier)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, user_id, project_id, message_count`)).
		WithArgs("alice", 50).
		WillReturnRows(rows)

	out, err := store.ListByUser(context.Background(), "alice", 0)
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d rows, want 2", len(out))
	}
	if out[0].ID != "recent" || out[1].ID != "older" {
		t.Fatalf("order not preserved: %+v", out)
	}
	if out[0].MessageCount != 5 || out[0].LastMessageRole != "assistant" {
		t.Fatalf("projection mismatch: %+v", out[0])
	}
}

// Empty user → anonymous normalization. Without this, anonymous sessions
// would never list (the manager normalizes "" → AnonymousUserID on Create).
func TestPGSessionStore_ListByUser_NormalizesEmptyUserID(t *testing.T) {
	store, mock, done := newPGStoreMock(t)
	defer done()

	rows := sqlmock.NewRows([]string{
		"id", "user_id", "project_id", "message_count",
		"last_role", "last_preview", "created_at", "updated_at",
	})

	mock.ExpectQuery(`SELECT id, user_id, project_id, message_count`).
		WithArgs(AnonymousUserID, 50).
		WillReturnRows(rows)

	if _, err := store.ListByUser(context.Background(), "", 0); err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
}

func TestPGSessionStore_Delete(t *testing.T) {
	store, mock, done := newPGStoreMock(t)
	defer done()

	mock.ExpectExec(`DELETE FROM sessions WHERE id`).
		WithArgs("sess-3").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.Delete(context.Background(), "sess-3"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}
