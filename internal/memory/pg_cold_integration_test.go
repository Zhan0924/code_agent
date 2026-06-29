package memory

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/zap"
)

// setupPGColdIntegration boots a pgvector container and returns a migrated PGCold store.
func setupPGColdIntegration(t *testing.T) (context.Context, *PGCold, *sql.DB, func()) {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	postgresContainer, err := postgres.Run(ctx,
		"docker.io/pgvector/pgvector:pg16",
		postgres.WithDatabase("testdb"),
		postgres.WithUsername("testuser"),
		postgres.WithPassword("testpass"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(15*time.Second)),
	)
	require.NoError(t, err)

	cleanup := func() {
		if err := postgresContainer.Terminate(ctx); err != nil {
			t.Logf("failed to terminate container: %s", err)
		}
	}

	connStr, err := postgresContainer.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	db, err := sql.Open("postgres", connStr)
	require.NoError(t, err)

	logger := zap.NewNop()
	pgStore := NewPGColdWithDim(db, logger, 3)
	require.NoError(t, pgStore.Migrate())

	return ctx, pgStore, db, func() {
		_ = db.Close()
		cleanup()
	}
}

// TestPGCold_Integration_BoostScoreBatch covers §35 citation score bumps on real PG.
func TestPGCold_Integration_BoostScoreBatch(t *testing.T) {
	ctx, pg, db, cleanup := setupPGColdIntegration(t)
	defer cleanup()

	memID := "00000000-0000-0000-0000-000000000010"
	_, err := db.Exec(`INSERT INTO memories (id, user_id, project_id, type, content, score, created_at, updated_at)
		VALUES ($1, 'u_boost', 'p1', 'knowledge', 'boost me', 0.5, NOW(), NOW())`, memID)
	require.NoError(t, err)

	require.NoError(t, pg.BoostScoreBatch(ctx, []string{memID}, 0.2))

	var score float64
	require.NoError(t, db.QueryRow(`SELECT score FROM memories WHERE id = $1`, memID).Scan(&score))
	assert.InDelta(t, 0.7, score, 0.001)
}

// TestPGCold_Integration_MarkDistilled covers §36 UPDATE semantics (REAUDIT-P0-1).
func TestPGCold_Integration_MarkDistilled(t *testing.T) {
	ctx, pg, db, cleanup := setupPGColdIntegration(t)
	defer cleanup()

	epID := "00000000-0000-0000-0000-000000000011"
	_, err := db.Exec(`INSERT INTO memories (id, user_id, project_id, type, content, score, created_at, updated_at, last_accessed_at)
		VALUES ($1, 'u_distill', 'p1', 'episodic', 'episode', 1.0, NOW(), NOW(), NOW())`, epID)
	require.NoError(t, err)

	require.NoError(t, pg.MarkDistilled(ctx, []string{epID}))

	var distilledAt sql.NullTime
	require.NoError(t, db.QueryRow(`SELECT distilled_at FROM memories WHERE id = $1`, epID).Scan(&distilledAt))
	assert.True(t, distilledAt.Valid)

	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM memories WHERE id = $1`, epID).Scan(&count))
	assert.Equal(t, 1, count, "MarkDistilled must UPDATE not DELETE")
}

// TestPGCold_Integration_DeleteByUser covers §26 GDPR delete path.
func TestPGCold_Integration_DeleteByUser(t *testing.T) {
	ctx, pg, db, cleanup := setupPGColdIntegration(t)
	defer cleanup()

	_, err := db.Exec(`INSERT INTO memories (id, user_id, project_id, type, content, score, created_at, updated_at) VALUES
		('00000000-0000-0000-0000-000000000012', 'gdpr_user', 'p1', 'knowledge', 'a', 0.5, NOW(), NOW()),
		('00000000-0000-0000-0000-000000000013', 'gdpr_user', 'p2', 'preference', 'b', 0.5, NOW(), NOW()),
		('00000000-0000-0000-0000-000000000014', 'other_user', 'p1', 'knowledge', 'c', 0.5, NOW(), NOW())`)
	require.NoError(t, err)

	deleted, err := pg.DeleteByUser(ctx, "gdpr_user")
	require.NoError(t, err)
	assert.EqualValues(t, 2, deleted)

	var remaining int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM memories WHERE user_id = 'gdpr_user'`).Scan(&remaining))
	assert.Equal(t, 0, remaining)

	require.NoError(t, db.QueryRow(`SELECT count(*) FROM memories WHERE user_id = 'other_user'`).Scan(&remaining))
	assert.Equal(t, 1, remaining)
}

// TestPGCold_Integration_CrossTypeRetrieve covers §29 type-filtered vector retrieval.
func TestPGCold_Integration_CrossTypeRetrieve(t *testing.T) {
	_, pg, db, cleanup := setupPGColdIntegration(t)
	defer cleanup()

	_, err := db.Exec(`INSERT INTO memories (id, user_id, project_id, type, content, score, embedding_3, created_at, updated_at) VALUES
		('00000000-0000-0000-0000-000000000015', 'u_type', 'p1', 'knowledge', 'k1', 0.8, '[1,0,0]', NOW(), NOW()),
		('00000000-0000-0000-0000-000000000016', 'u_type', 'p1', 'knowledge', 'k2', 0.8, '[0,1,0]', NOW(), NOW()),
		('00000000-0000-0000-0000-000000000017', 'u_type', 'p1', 'preference', 'pref', 0.8, '[1,0,0]', NOW(), NOW())`)
	require.NoError(t, err)

	results, err := pg.RetrieveByVectorAndType([]float32{1, 0, 0}, "u_type", "p1", string(MemoryKnowledge), 5)
	require.NoError(t, err)
	assert.Len(t, results, 2)
	assert.Equal(t, string(MemoryKnowledge), string(results[0].Type))
}
