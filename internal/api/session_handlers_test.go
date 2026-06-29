package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/agent/code_agent/internal/config"
	"github.com/agent/code_agent/internal/memory"
	"github.com/agent/code_agent/internal/models"
	"github.com/agent/code_agent/internal/session"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

func newFeedbackHarness(t *testing.T) (*Server, *httptest.Server, *session.Manager, *memory.HybridStore, func()) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	logger := zap.NewNop()

	sessCfg := &config.SessionConfig{TTL: time.Hour}
	sessionMgr := session.NewManager(rdb, sessCfg, logger)
	hot := memory.NewRedisHot(rdb, logger)
	store := memory.NewHybridStore(hot, nil, logger)

	s := NewServer(nil, sessionMgr, logger)
	s.SetMemoryStore(store)
	srv := httptest.NewServer(s.Handler())

	cleanup := func() {
		srv.Close()
		mr.Close()
	}
	return s, srv, sessionMgr, store, cleanup
}

func postFeedback(t *testing.T, url string, score float64) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]float64{"score": score})
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestHandleMessageFeedback_NegativeBoost(t *testing.T) {
	_, srv, sessionMgr, store, cleanup := newFeedbackHarness(t)
	defer cleanup()

	ctx := context.Background()
	sess, err := sessionMgr.Create(ctx, "alice", "p1")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	now := time.Now()
	if err := store.Store(ctx, &memory.Memory{
		ID: "mem-1", UserID: "alice", ProjectID: "p1",
		Type: memory.MemoryPreference, Content: "pref", Score: 0.5,
		CreatedAt: now, LastAccessedAt: now,
	}); err != nil {
		t.Fatalf("store memory: %v", err)
	}

	if err := sessionMgr.AddMessage(ctx, sess.ID, models.Message{
		Role:    models.RoleAssistant,
		Content: "Answer citing [mem:mem-1] twice [mem:mem-1]",
	}); err != nil {
		t.Fatalf("add message: %v", err)
	}

	updated, err := sessionMgr.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	msgID := updated.Messages[len(updated.Messages)-1].ID

	url := srv.URL + "/api/v1/sessions/" + sess.ID + "/messages/" + msgID + "/feedback"
	code, body := postFeedback(t, url, -1)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%v", code, body)
	}
	if int(body["memories_affected"].(float64)) != 1 {
		t.Fatalf("expected 1 memory affected, got %v", body)
	}

	mems, _, _, err := store.List(ctx, "alice", "p1", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(mems) != 1 {
		t.Fatalf("expected 1 memory, got %d", len(mems))
	}
	if mems[0].Score != 0.3 {
		t.Fatalf("expected score 0.3 after -0.2 boost, got %f", mems[0].Score)
	}
}

func TestHandleMessageFeedback_NoMemoryStore(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	sessCfg := &config.SessionConfig{TTL: time.Hour}
	sessionMgr := session.NewManager(rdb, sessCfg, zap.NewNop())
	s := NewServer(nil, sessionMgr, zap.NewNop())
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	ctx := context.Background()
	sess, err := sessionMgr.Create(ctx, "alice", "p1")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := sessionMgr.AddMessage(ctx, sess.ID, models.Message{
		Role: models.RoleAssistant, Content: "hi [mem:x]",
	}); err != nil {
		t.Fatalf("add message: %v", err)
	}
	updated, _ := sessionMgr.Get(ctx, sess.ID)
	msgID := updated.Messages[0].ID

	url := srv.URL + "/api/v1/sessions/" + sess.ID + "/messages/" + msgID + "/feedback"
	code, _ := postFeedback(t, url, 1)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", code)
	}
}
