package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/agent/code_agent/internal/config"
	"github.com/agent/code_agent/internal/session"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestResolveTenantIDs_NormalizesEmptyToAnonymousDefault(t *testing.T) {
	core, recorded := observer.New(zap.InfoLevel)
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	sessMgr := session.NewManager(rdb, &config.SessionConfig{TTL: time.Hour}, zap.NewNop())
	o := &Orchestrator{
		sessionMgr: sessMgr,
		logger:     zap.New(core),
	}

	userID, projectID := o.resolveTenantIDs(context.Background(), "")
	assert.Equal(t, session.AnonymousUserID, userID)
	assert.Equal(t, session.DefaultProjectID, projectID)

	logs := recorded.FilterMessage("tenant ids normalized for memory pipeline").All()
	require.Len(t, logs, 1)
	fields := logs[0].ContextMap()
	assert.Equal(t, "REAUDIT-P1-2", fields["audit_id"])
}

func TestResolveTenantIDs_UsesSessionBeforeNormalize(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	sessMgr := session.NewManager(rdb, &config.SessionConfig{TTL: time.Hour}, zap.NewNop())
	o := &Orchestrator{
		sessionMgr: sessMgr,
		logger:     zap.NewNop(),
	}

	ctx := context.Background()
	sess, err := sessMgr.Create(ctx, "alice", "p1")
	require.NoError(t, err)

	userID, projectID := o.resolveTenantIDs(ctx, sess.ID)
	assert.Equal(t, "alice", userID)
	assert.Equal(t, "p1", projectID)
}
