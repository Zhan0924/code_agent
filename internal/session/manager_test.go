package session

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/agent/code_agent/internal/config"
	"github.com/agent/code_agent/internal/models"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// newTestManager 用 miniredis 启动一个隔离的 Manager,返回 mr 以便测试断言
// 底层 keyspace。
func newTestManager(t *testing.T) (*Manager, *miniredis.Miniredis, func()) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	cfg := &config.SessionConfig{
		MaxHistoryTokens:       16000,
		SummaryThresholdTokens: 12000,
	}
	mgr := NewManager(rdb, cfg, zap.NewNop())
	return mgr, mr, func() { _ = rdb.Close(); mr.Close() }
}

// 核心不变量:Create 接到空 userID 后,session.UserID 必须落在 AnonymousUserID,
// 且 sess:index:<AnonymousUserID> 里必须 ZADD 进去 —— 否则未登录用户的 session
// 在侧栏永远拉不出来(根因:历史上 "" 走 default,新代码若不 normalize 会再次出现
// 类似 split keyspace 的 bug)。
func TestManager_Create_NormalizesEmptyUserID(t *testing.T) {
	mgr, mr, cleanup := newTestManager(t)
	defer cleanup()
	ctx := context.Background()

	sess, err := mgr.Create(ctx, "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess.UserID != AnonymousUserID {
		t.Fatalf("session.UserID = %q, want %q", sess.UserID, AnonymousUserID)
	}

	// hot JSON 持久化的 user_id 也必须是 anonymous
	raw, err := mr.Get("sess:hot:" + sess.ID)
	if err != nil {
		t.Fatalf("hot key missing: %v", err)
	}
	var stored map[string]any
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if stored["user_id"] != AnonymousUserID {
		t.Fatalf("stored user_id = %v, want %q", stored["user_id"], AnonymousUserID)
	}

	// 索引必须落在 sess:index:anonymous,而不是历史的 sess:index:default
	if score, err := mr.ZScore("sess:index:"+AnonymousUserID, sess.ID); err != nil || score == 0 {
		t.Fatalf("session not indexed under sess:index:anonymous: score=%v err=%v", score, err)
	}
	if mr.Exists("sess:index:default") {
		t.Fatal("sess:index:default should not be created for empty user_id")
	}
}

// 显式给定 userID 不应被改写。
func TestManager_Create_PreservesExplicitUserID(t *testing.T) {
	mgr, mr, cleanup := newTestManager(t)
	defer cleanup()
	ctx := context.Background()

	sess, err := mgr.Create(ctx, "alice", "proj")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess.UserID != "alice" {
		t.Fatalf("UserID = %q, want alice", sess.UserID)
	}
	if score, err := mr.ZScore("sess:index:alice", sess.ID); err != nil || score == 0 {
		t.Fatalf("alice session not indexed: score=%v err=%v", score, err)
	}
}

// ─── PG-mode integration ────────────────────────────────────────────────────
//
// fakePGStore is a thread-safe in-memory SessionStore. We avoid sqlmock here
// because the manager's PG calls are mostly fired in goroutines (asyncPGUpsert
// is a defining property of the path) and sqlmock's strict expectation order
// would race with them. A map keyed by session ID models PG with the precision
// the Manager tests care about: rows persist, listing is by recency, delete
// removes — that's the whole behavioral surface.
type fakePGStore struct {
	mu       sync.Mutex
	rows     map[string]*models.Session
	updated  map[string]time.Time // separate so we can sort even if Session.UpdatedAt is zero
	listErr  error
	getErr   error
	upsertCh chan string // optional notifier for tests waiting on async writes
}

func newFakePGStore() *fakePGStore {
	return &fakePGStore{
		rows:    make(map[string]*models.Session),
		updated: make(map[string]time.Time),
	}
}

func (f *fakePGStore) Upsert(ctx context.Context, s *models.Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	clone := *s
	if len(s.Messages) > 0 {
		clone.Messages = make([]models.Message, len(s.Messages))
		copy(clone.Messages, s.Messages)
	}
	f.rows[s.ID] = &clone
	if s.UpdatedAt.IsZero() {
		f.updated[s.ID] = time.Now()
	} else {
		f.updated[s.ID] = s.UpdatedAt
	}
	if f.upsertCh != nil {
		select {
		case f.upsertCh <- s.ID:
		default:
		}
	}
	return nil
}

func (f *fakePGStore) Get(ctx context.Context, sessionID string) (*models.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	s, ok := f.rows[sessionID]
	if !ok {
		return nil, ErrSessionNotFound
	}
	clone := *s
	if len(s.Messages) > 0 {
		clone.Messages = make([]models.Message, len(s.Messages))
		copy(clone.Messages, s.Messages)
	}
	return &clone, nil
}

func (f *fakePGStore) ListByUser(ctx context.Context, userID string, limit int) ([]*SessionSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	if userID == "" {
		userID = AnonymousUserID
	}
	if limit <= 0 {
		limit = 50
	}

	type rec struct {
		s *models.Session
		t time.Time
	}
	matches := make([]rec, 0, len(f.rows))
	for id, s := range f.rows {
		if s.UserID == userID {
			matches = append(matches, rec{s: s, t: f.updated[id]})
		}
	}
	// Sort updated_at desc.
	for i := 1; i < len(matches); i++ {
		for j := i; j > 0 && matches[j].t.After(matches[j-1].t); j-- {
			matches[j-1], matches[j] = matches[j], matches[j-1]
		}
	}
	if len(matches) > limit {
		matches = matches[:limit]
	}
	out := make([]*SessionSummary, 0, len(matches))
	for _, r := range matches {
		out = append(out, buildSummary(r.s))
	}
	return out, nil
}

func (f *fakePGStore) Delete(ctx context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, sessionID)
	delete(f.updated, sessionID)
	return nil
}

// newTestManagerWithPG layers a fakePGStore on top of miniredis. Returns mr
// so tests can poke Redis state (e.g. DEL hot to simulate TTL expiry).
func newTestManagerWithPG(t *testing.T) (*Manager, *miniredis.Miniredis, *fakePGStore, func()) {
	t.Helper()
	mgr, mr, cleanup := newTestManager(t)
	pg := newFakePGStore()
	mgr.PGStore = pg
	mgr.cfg.TTL = time.Hour // explicit TTL so saveHot doesn't no-op on miniredis
	return mgr, mr, pg, cleanup
}

// waitForUpsert polls the fake store for sessionID up to 1s. Async asyncPGUpsert
// is unobservable through Manager directly, so we synchronize via this poll.
func waitForUpsert(t *testing.T, pg *fakePGStore, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		pg.mu.Lock()
		_, ok := pg.rows[sessionID]
		pg.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("PG upsert for %s did not happen within 1s", sessionID)
}

// Create must mirror to PG (async). The whole reason for the PG path.
func TestManager_Create_DoubleWrite(t *testing.T) {
	mgr, _, pg, cleanup := newTestManagerWithPG(t)
	defer cleanup()

	sess, err := mgr.Create(context.Background(), "alice", "proj")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	waitForUpsert(t, pg, sess.ID)

	pg.mu.Lock()
	got, ok := pg.rows[sess.ID]
	pg.mu.Unlock()
	if !ok {
		t.Fatal("session not mirrored to PG")
	}
	if got.UserID != "alice" || got.ProjectID != "proj" {
		t.Fatalf("PG row: %+v", got)
	}
}

// Get must rehydrate from PG when hot key is gone (the bug we're fixing).
// After rehydrate, hot should also be repopulated so the next Get hits Redis.
func TestManager_Get_HotMissFallsBackToPG(t *testing.T) {
	mgr, mr, pg, cleanup := newTestManagerWithPG(t)
	defer cleanup()
	ctx := context.Background()

	sess, err := mgr.Create(ctx, "alice", "proj")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	waitForUpsert(t, pg, sess.ID)

	// Simulate Redis hot TTL expiry — the exact failure mode that motivated PG.
	mr.Del(hotKey(sess.ID))
	if mr.Exists(hotKey(sess.ID)) {
		t.Fatal("setup: hot key should be gone")
	}

	rehydrated, err := mgr.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get after hot miss: %v", err)
	}
	if rehydrated.ID != sess.ID || rehydrated.UserID != "alice" {
		t.Fatalf("rehydrated mismatch: %+v", rehydrated)
	}
	// Hot must be re-warmed so subsequent reads stay on the fast path.
	if !mr.Exists(hotKey(sess.ID)) {
		t.Fatal("hot key should be rewritten after rehydrate")
	}
}

// When PG is wired, ListSessions must return rows that exist ONLY in PG —
// this is the core regression test for "sessions vanish after 48h".
func TestManager_ListSessions_PGAuthoritative(t *testing.T) {
	mgr, mr, pg, cleanup := newTestManagerWithPG(t)
	defer cleanup()
	ctx := context.Background()

	sess, err := mgr.Create(ctx, "alice", "proj")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	waitForUpsert(t, pg, sess.ID)

	// Wipe ALL Redis state: hot, cold, and the per-user index ZSET. This
	// emulates the post-TTL state where Redis has nothing useful but PG
	// still owns the row.
	mr.Del(hotKey(sess.ID))
	mr.Del(coldKey(sess.ID))
	mr.Del(sessionIndexKey("alice"))

	summaries, err := mgr.ListSessions(ctx, "alice", 50)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("got %d summaries, want 1", len(summaries))
	}
	if summaries[0].ID != sess.ID {
		t.Fatalf("listed wrong session: %+v", summaries[0])
	}
}

// In PG mode, ListSessions must NOT prune the ZSET — pruning was what made
// sessions vanish permanently when PG didn't yet exist. We verify that the
// ZSET membership is unchanged after a list that hits PG-only rows.
func TestManager_ListSessions_NoZREM_WhenPGEnabled(t *testing.T) {
	mgr, mr, pg, cleanup := newTestManagerWithPG(t)
	defer cleanup()
	ctx := context.Background()

	sess, err := mgr.Create(ctx, "alice", "proj")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	waitForUpsert(t, pg, sess.ID)

	// ZSET starts populated by Create.
	if score, _ := mr.ZScore(sessionIndexKey("alice"), sess.ID); score == 0 {
		t.Fatal("setup: ZSET should contain the session")
	}

	// Hot expires (TTL fires), but the ZSET entry persists — this is the
	// exact pre-bug shape that ListSessions would ZREM and lose forever.
	mr.Del(hotKey(sess.ID))

	if _, err := mgr.ListSessions(ctx, "alice", 50); err != nil {
		t.Fatalf("ListSessions: %v", err)
	}

	// In legacy Redis-only mode the listing would now ZREM. PG mode must not.
	if score, _ := mr.ZScore(sessionIndexKey("alice"), sess.ID); score == 0 {
		t.Fatal("ListSessions in PG mode must NOT ZREM stale members")
	}
}

// PG outage must not blank the sidebar — Manager falls back to the Redis-only
// list path. We simulate PG failure and verify the legacy path still answers.
func TestManager_ListSessions_PGOutageFallsBackToRedis(t *testing.T) {
	mgr, _, pg, cleanup := newTestManagerWithPG(t)
	defer cleanup()
	ctx := context.Background()

	sess, err := mgr.Create(ctx, "alice", "proj")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	waitForUpsert(t, pg, sess.ID)

	// Force PG ListByUser to error — hot path is still intact via Redis.
	pg.listErr = errors.New("simulated PG outage")

	summaries, err := mgr.ListSessions(ctx, "alice", 50)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(summaries) != 1 || summaries[0].ID != sess.ID {
		t.Fatalf("fallback path did not surface session: %+v", summaries)
	}
}

// Delete must propagate to PG so the listing reflects user intent after the
// 24h Redis TTL elapses (otherwise deleted sessions reappear from PG).
func TestManager_Delete_RemovesPGRow(t *testing.T) {
	mgr, _, pg, cleanup := newTestManagerWithPG(t)
	defer cleanup()
	ctx := context.Background()

	sess, err := mgr.Create(ctx, "alice", "proj")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	waitForUpsert(t, pg, sess.ID)

	if err := mgr.Delete(ctx, sess.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// PG delete is async; poll up to 1s.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		pg.mu.Lock()
		_, exists := pg.rows[sess.ID]
		pg.mu.Unlock()
		if !exists {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("PG row not removed within 1s")
}

// AddMessage must mirror the updated session to PG so the projection columns
// stay in sync (message_count grows, last_role flips, preview updates).
func TestManager_AddMessage_MirrorsToPG(t *testing.T) {
	mgr, _, pg, cleanup := newTestManagerWithPG(t)
	defer cleanup()
	ctx := context.Background()

	sess, err := mgr.Create(ctx, "alice", "proj")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	waitForUpsert(t, pg, sess.ID)

	if err := mgr.AddMessage(ctx, sess.ID, models.Message{
		Role:    models.RoleUser,
		Content: "hello world",
	}); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	// Poll until PG sees the message.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		pg.mu.Lock()
		row := pg.rows[sess.ID]
		pg.mu.Unlock()
		if row != nil && len(row.Messages) >= 1 {
			if row.Messages[0].Content != "hello world" {
				t.Fatalf("message content not mirrored: %+v", row.Messages[0])
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("AddMessage did not mirror to PG within 1s")
}

// sessionIndexKey 是 fan-out 调用点最多的辅助函数,直接打表。
func TestSessionIndexKey_Normalize(t *testing.T) {
	cases := map[string]string{
		"":          "sess:index:anonymous",
		"anonymous": "sess:index:anonymous",
		"alice":     "sess:index:alice",
	}
	for in, want := range cases {
		if got := sessionIndexKey(in); got != want {
			t.Errorf("sessionIndexKey(%q) = %q, want %q", in, got, want)
		}
	}
}
