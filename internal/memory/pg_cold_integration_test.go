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

// TestPGCold_Integration runs integration tests against a real pgvector container.
func TestPGCold_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()

	// Start pgvector container
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
	defer func() {
		if err := postgresContainer.Terminate(ctx); err != nil {
			t.Logf("failed to terminate container: %s", err)
		}
	}()

	connStr, err := postgresContainer.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	db, err := sql.Open("postgres", connStr)
	require.NoError(t, err)
	defer db.Close()

	// Initialize PGCold store
	logger, _ := zap.NewDevelopment()
	dim := 3
	pgStore := NewPGColdWithDim(db, logger, dim)

	// Run migrations
	err = pgStore.Migrate()
	require.NoError(t, err, "Migration should succeed")

	// 1. Test DedupTx
	t.Run("DedupTx", func(t *testing.T) {
		anchorID := "00000000-0000-0000-0000-000000000001"
		dup1ID := "00000000-0000-0000-0000-000000000002"
		dup2ID := "00000000-0000-0000-0000-000000000003"
		
		memories := []Memory{
			{ID: anchorID, UserID: "u1", ProjectID: "p1", Type: MemoryKnowledge, Content: "Anchor content", Score: 0.8},
			{ID: dup1ID, UserID: "u1", ProjectID: "p1", Type: MemoryKnowledge, Content: "Dup 1", Score: 0.5},
			{ID: dup2ID, UserID: "u1", ProjectID: "p1", Type: MemoryKnowledge, Content: "Dup 2", Score: 0.6},
		}
		
		for _, m := range memories {
			_, err := db.Exec(`
				INSERT INTO memories (id, user_id, project_id, type, content, score, created_at, updated_at) 
				VALUES ($1, $2, $3, $4, $5, $6, $7, $7)`,
				m.ID, m.UserID, m.ProjectID, m.Type, m.Content, m.Score, time.Now())
			require.NoError(t, err)
		}

		anchorToUpdate := &Memory{
			ID:          anchorID,
			Score:       0.95,
			AccessCount: 2,
			Embedding:   []float32{0.1, 0.2, 0.3},
		}
		err = pgStore.DedupTx(ctx, anchorToUpdate, []string{dup1ID, dup2ID})
		require.NoError(t, err)

		// Verify duplicates are deleted
		var count int
		err = db.QueryRow(`SELECT count(*) FROM memories WHERE id IN ($1, $2)`, dup1ID, dup2ID).Scan(&count)
		require.NoError(t, err)
		assert.Equal(t, 0, count, "Duplicates should be deleted")

		// Verify anchor is updated
		var newScore float64
		var accessCount int
		err = db.QueryRow(`SELECT score, access_count FROM memories WHERE id = $1`, anchorID).Scan(&newScore, &accessCount)
		require.NoError(t, err)
		assert.InDelta(t, 0.95, newScore, 0.001, "Score should be updated")
		assert.Equal(t, 2, accessCount, "Access count should be updated")
	})

	// 2. Test RetrieveByVectorAndType
	t.Run("RetrieveByVectorAndType", func(t *testing.T) {
		_, err := db.Exec(`DELETE FROM memories`)
		require.NoError(t, err)

		// Insert 3 records, 2 knowledge and 1 preference
		_, err = db.Exec(`INSERT INTO memories (id, user_id, project_id, type, content, score, embedding, created_at, updated_at) VALUES 
			('00000000-0000-0000-0000-000000000004', 'u2', 'p2', 'knowledge', 'content1', 0.8, '[1,0,0]', NOW(), NOW()),
			('00000000-0000-0000-0000-000000000005', 'u2', 'p2', 'knowledge', 'content2', 0.8, '[0,1,0]', NOW(), NOW()),
			('00000000-0000-0000-0000-000000000006', 'u2', 'p2', 'preference', 'content3', 0.8, '[1,0,0]', NOW(), NOW())`)
		require.NoError(t, err)

		vec := []float32{1, 0, 0}
		results, err := pgStore.RetrieveByVectorAndType(vec, "u2", "p2", string(MemoryKnowledge), 5)
		require.NoError(t, err)
		
		assert.Len(t, results, 2, "Should only retrieve knowledge type memories")
		assert.Equal(t, "00000000-0000-0000-0000-000000000004", results[0].ID, "m1 should be first as it is closer to [1,0,0]")
	})

	// 3. Test Decay
	t.Run("Decay", func(t *testing.T) {
		_, err := db.Exec(`DELETE FROM memories`)
		require.NoError(t, err)

		// Insert older memory
		_, err = db.Exec(`INSERT INTO memories (id, user_id, project_id, type, content, score, created_at, updated_at, last_accessed_at) VALUES 
			('00000000-0000-0000-0000-000000000007', 'u3', 'p3', 'knowledge', 'content', 1.0, $1, $1, $1)`, time.Now().Add(-48*time.Hour))
		require.NoError(t, err)

		// Run decay for everything older than 24h
		decayed, err := pgStore.Decay(24*time.Hour, 0.9)
		require.NoError(t, err)
		assert.Greater(t, decayed, 0)

		var score float64
		err = db.QueryRow(`SELECT score FROM memories WHERE id = '00000000-0000-0000-0000-000000000007'`).Scan(&score)
		require.NoError(t, err)

		assert.Less(t, score, 1.0, "Score should have decayed")
	})

	// 4. Test MarkDistilled + DeleteOldEpisodic (REAUDIT-P0-1 contract)
	t.Run("MarkDistilledAndDeleteOldEpisodic", func(t *testing.T) {
		_, err := db.Exec(`DELETE FROM memories`)
		require.NoError(t, err)

		epID := "00000000-0000-0000-0000-000000000008"
		_, err = db.Exec(`INSERT INTO memories (id, user_id, project_id, type, content, score, created_at, updated_at, last_accessed_at)
			VALUES ($1, 'u4', 'p4', 'episodic', 'episode to distill', 1.0, NOW(), NOW(), NOW())`, epID)
		require.NoError(t, err)

		err = pgStore.MarkDistilled(ctx, []string{epID})
		require.NoError(t, err)

		var distilledAt sql.NullTime
		err = db.QueryRow(`SELECT distilled_at FROM memories WHERE id = $1`, epID).Scan(&distilledAt)
		require.NoError(t, err)
		assert.True(t, distilledAt.Valid, "distilled_at should be set after MarkDistilled")

		// Force distilled_at into the past so DeleteOldEpisodic can pick it up.
		_, err = db.Exec(`UPDATE memories SET distilled_at = NOW() - INTERVAL '48 hours' WHERE id = $1`, epID)
		require.NoError(t, err)

		deleted, err := pgStore.DeleteOldEpisodic(ctx, 24*time.Hour)
		require.NoError(t, err)
		assert.EqualValues(t, 1, deleted, "old distilled episodic should be deleted by GC")

		var count int
		err = db.QueryRow(`SELECT count(*) FROM memories WHERE id = $1`, epID).Scan(&count)
		require.NoError(t, err)
		assert.Equal(t, 0, count, "row should be gone after DeleteOldEpisodic")
	})
}
