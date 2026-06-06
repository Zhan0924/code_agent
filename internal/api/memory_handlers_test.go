package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agent/code_agent/internal/config"
	"github.com/agent/code_agent/internal/memory"
	"github.com/agent/code_agent/internal/session"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

func newMemoryHarness(t *testing.T) (*Server, *httptest.Server, *memory.HybridStore, func()) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	logger := zap.NewNop()

	sessCfg := &config.SessionConfig{
		TTL:                    1 * time.Hour,
		MaxHistoryTokens:       8000,
		SummaryThresholdTokens: 4000,
	}
	sessionMgr := session.NewManager(rdb, sessCfg, logger)

	// HybridStore with hot only (cold=nil — UI listing path tolerates it)
	hot := memory.NewRedisHot(rdb, logger)
	store := memory.NewHybridStore(hot, nil, logger)

	s := NewServer(nil, sessionMgr, logger)
	s.SetMemoryStore(store)

	srv := httptest.NewServer(s.Handler())

	cleanup := func() {
		srv.Close()
		mr.Close()
	}
	return s, srv, store, cleanup
}

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("http get: %v", err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestMemoryHandlers_NoStore(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	sessCfg := &config.SessionConfig{TTL: time.Hour}
	sessionMgr := session.NewManager(rdb, sessCfg, zap.NewNop())
	s := NewServer(nil, sessionMgr, zap.NewNop())
	// NOTE: SetMemoryStore intentionally NOT called.

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	code, _ := getJSON(t, srv.URL+"/api/v1/memory?user_id=alice")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", code)
	}
	code, _ = getJSON(t, srv.URL+"/api/v1/memory/stats?user_id=alice")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("stats expected 503, got %d", code)
	}
}

// 缺省 user_id 不再 400 —— 归一化为 AnonymousUserID 并按 (anonymous, default)
// 维度返回空 list,跟 session.Manager.Create 的入口对齐。这样未登录的
// 默认视图(前端不带任何参数)也能拿到该空间的长期 memory。
func TestMemoryHandlers_MissingUserID_FallsBackToAnonymous(t *testing.T) {
	_, srv, _, cleanup := newMemoryHarness(t)
	defer cleanup()

	code, body := getJSON(t, srv.URL+"/api/v1/memory")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%v", code, body)
	}
	if total, ok := body["total"].(float64); !ok || total != 0 {
		t.Fatalf("expected empty list under anonymous/default, got body=%v", body)
	}
}

func TestMemoryHandlers_ListAndStats(t *testing.T) {
	_, srv, store, cleanup := newMemoryHarness(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now()
	items := []*memory.Memory{
		{ID: "m1", UserID: "alice", ProjectID: "p1", Type: memory.MemoryPreference, Content: "prefers tabs", Score: 0.9, CreatedAt: now, LastAccessedAt: now.Add(-3 * time.Second)},
		{ID: "m2", UserID: "alice", ProjectID: "p1", Type: memory.MemoryDecision, Content: "use Postgres", Score: 0.8, CreatedAt: now, LastAccessedAt: now.Add(-1 * time.Second)},
		{ID: "m3", UserID: "alice", ProjectID: "p1", Type: memory.MemoryKnowledge, Content: "x is y", Score: 0.7, CreatedAt: now, LastAccessedAt: now.Add(-2 * time.Second)},
	}
	for _, m := range items {
		if err := store.Store(ctx, m); err != nil {
			t.Fatalf("store: %v", err)
		}
	}

	code, body := getJSON(t, srv.URL+"/api/v1/memory?user_id=alice&project_id=p1")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%v", code, body)
	}
	total, _ := body["total"].(float64)
	if int(total) != 3 {
		t.Fatalf("expected total=3, got %v body=%v", total, body)
	}
	mems, _ := body["memories"].([]any)
	if len(mems) != 3 {
		t.Fatalf("expected 3 memories, got %d", len(mems))
	}

	// stats grouped by type
	code, statsBody := getJSON(t, srv.URL+"/api/v1/memory/stats?user_id=alice&project_id=p1")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%v", code, statsBody)
	}
	byType, _ := statsBody["by_type"].(map[string]any)
	if byType == nil {
		t.Fatalf("by_type missing: %v", statsBody)
	}
	for _, key := range []string{"preference", "decision", "knowledge", "pattern"} {
		if _, ok := byType[key]; !ok {
			t.Fatalf("by_type missing key %q: %v", key, byType)
		}
	}
	if v, _ := byType["preference"].(float64); int(v) != 1 {
		t.Fatalf("preference count != 1: %v", byType)
	}
	if v, _ := byType["pattern"].(float64); int(v) != 0 {
		t.Fatalf("pattern count != 0: %v", byType)
	}

	// type filter
	code, fBody := getJSON(t, srv.URL+"/api/v1/memory?user_id=alice&project_id=p1&type=preference")
	if code != http.StatusOK {
		t.Fatalf("filtered 200, got %d", code)
	}
	if int(fBody["total"].(float64)) != 1 {
		t.Fatalf("filtered total != 1: %v", fBody)
	}
}

func TestMemoryHandlers_DefaultLimit(t *testing.T) {
	_, srv, _, cleanup := newMemoryHarness(t)
	defer cleanup()

	resp, err := http.Get(srv.URL + "/api/v1/memory?user_id=bob")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("empty list should be 200, got %d", resp.StatusCode)
	}
	body := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	// strings.Contains as a defensive sanity check on json keys
	if !strings.Contains(asJSON(body), "memories") {
		t.Fatalf("response missing memories key: %v", body)
	}
}

func asJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
