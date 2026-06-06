package session

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/agent/code_agent/internal/config"
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
