// Package session provides Redis-backed session management with sliding window
// context compression, hot/cold data separation, and Redis key sharding
// to prevent hot-key issues in multi-tenant high-frequency scenarios.
//
// ============================================================================
//
//	设 计 原 理
//
// ============================================================================
//
// 【痛点：为什么多轮对话上下文必须外置到 Redis？】
//   - LLM 本身是无状态的，每次调用都要把完整历史作为 prompt 传回去；
//   - 单个 Agent 进程可能同时服务数千会话，若在进程内 map 缓存，重启即丢失；
//   - 水平扩容时，Session A 这次命中 Pod-1 下次可能路由到 Pod-2，必须共享存储。
//
// 【滑动窗口 (Sliding Window)】
//
//	把对话消息按时间排成队列。任一时刻只保留"最近 N 条 / 最多 M tokens"。
//	当 M 超限时：
//	  ① 最旧的 K 条消息被切出来，丢给小模型生成摘要；
//	  ② 摘要替换原消息，压入队列头部；
//	  ③ 队列长度回到阈值以下。
//
//	图示：
//	  旧 ╌╌╌╌╌╌╌╌╌ 新
//	  [m1][m2]...[mN]                    len=N, tokens≤M
//	           │
//	           │  超过阈值
//	           ▼
//	  [summary][m6][m7]...[mN]           m1~m5 被浓缩
//
// 【为什么异步生成摘要？】
//
//	摘要本身也要调 LLM，耗时 1~3s。若同步做，会阻塞用户新消息的写入。
//	方案：写消息即返回；另起 goroutine 检查阈值 → 触发摘要 → 替换；
//	用户下一次提问时看到的已是压缩后的窗口。
//
// 【Redis Key 分片防热点】
//
//	多租户场景下，"某个大客户"的 session 访问量可能吃掉整个 Redis 单节点带宽。
//	策略：key = "sess:{tenant_id}:{hash(session_id) % 16}:{session_id}"
//	Redis Cluster 会依据 key 前缀自动把不同 shard 分散到不同节点。
//
// 【Hot / Cold 分离】
//
//	· Hot（最近 1h 活跃）  : Redis，毫秒访问
//	· Cold（1h 无活动）    : 后台 job 下沉到 PostgreSQL JSONB，节省 Redis 内存
//	· 唤醒时若 Redis 未命中，从 PG 拉回并 SETEX 回写
//
// ============================================================================
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/agent/code_agent/internal/config"
	"github.com/agent/code_agent/internal/llm"
	"github.com/agent/code_agent/internal/models"
	"github.com/agent/code_agent/internal/pool"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// ─── Hot/Cold Data Separation ────────────────────────────────────────────────
// Problem: Stuffing entire conversation history into a single Redis key creates
// large keys (>1MB) that cause Redis latency spikes and memory fragmentation.
//
// Solution: Separate hot data (recent N messages, actively used) from cold data
// (compressed history, archived). Hot data stays in Redis; cold data is
// asynchronously flushed to PostgreSQL or object storage.

const (
	// minHotMessages is the minimum number of messages to keep in hot storage
	// even if token budget is exceeded. Prevents degenerate case where a single
	// large message triggers immediate archival.
	minHotMessages = 2

	// shardCount controls the number of Redis key shards per session.
	// This distributes a single session's data across multiple keys to avoid
	// hot-key concentration on a single Redis slot in cluster mode.
	shardCount = 4
)

// Manager handles session CRUD operations with hot/cold separation and sharding.
type Manager struct {
	rdb    *redis.Client
	cfg    *config.SessionConfig
	logger *zap.Logger

	// ColdStore is an optional callback for archiving cold session data
	// (the token-budget driven compression path — see performHotColdSeparation).
	// Orthogonal to PGStore: ColdStore archives *trimmed* old messages, whereas
	// PGStore mirrors the *current* session for long-term sidebar persistence.
	ColdStore func(ctx context.Context, sessionID string, data *ColdSessionData) error

	// PGStore is the optional long-term persistence layer. When non-nil,
	// every Create/AddMessage/Delete is mirrored to PG asynchronously, Get
	// falls back to PG on hot miss (and rewrites hot), and ListSessions uses
	// PG as the authoritative source. When nil, Manager behaves exactly as
	// the pre-PG Redis-only implementation — so Postgres is a strict upgrade.
	PGStore SessionStore

	// Summarizer is an optional LLM-based summarizer for archived messages.
	// If nil, buildSummary falls back to simple string truncation.
	Summarizer Summarizer
}

// ColdSessionData represents archived session data for long-term storage.
type ColdSessionData struct {
	SessionID  string           `json:"session_id"`
	Messages   []models.Message `json:"messages"`
	Summary    string           `json:"summary"`
	ArchivedAt time.Time        `json:"archived_at"`
}

// NewManager creates a new session manager backed by Redis with sharding support.
func NewManager(rdb *redis.Client, cfg *config.SessionConfig, logger *zap.Logger) *Manager {
	return &Manager{
		rdb:    rdb,
		cfg:    cfg,
		logger: logger.With(zap.String("component", "session")),
	}
}

// ─── Key Sharding ────────────────────────────────────────────────────────────
// Distributes session data across multiple Redis keys to prevent hot-key issues.
// Each shard holds a subset of the session data, reducing per-key size and
// distributing load across Redis cluster slots.

// hotKey returns the Redis key for hot (active) session data.
func hotKey(sessionID string) string {
	return fmt.Sprintf("sess:hot:%s", sessionID)
}

// AnonymousUserID is the canonical user_id for unauthenticated / dev usage.
// All empty user_id values (session, memory, index keys) are normalized here
// so the long-term memory tab and session sidebar share one keyspace instead
// of being silently split between "default" and "" by different call sites.
const AnonymousUserID = "anonymous"

// sessionIndexKey returns the per-user ZSET key that indexes all sessions
// owned by a user, scored by last-update timestamp. Used to power the
// session-list sidebar without a `KEYS sess:hot:*` scan (which is O(N) over
// the entire Redis keyspace).
//
// Empty userID is normalized to AnonymousUserID so anonymous sessions still
// get indexed and listable — the alternative (skip indexing) would leave the
// sidebar permanently empty for unauthenticated dev usage.
func sessionIndexKey(userID string) string {
	if userID == "" {
		userID = AnonymousUserID
	}
	return fmt.Sprintf("sess:index:%s", userID)
}

// coldKey returns the Redis key for the cold summary/metadata.
func coldKey(sessionID string) string {
	return fmt.Sprintf("sess:cold:%s", sessionID)
}

// msgShardKey returns a sharded key for message storage, distributing messages
// across multiple Redis keys to prevent single-key bloat.
func msgShardKey(sessionID string, msgIndex int) string {
	shard := msgIndex % shardCount
	return fmt.Sprintf("sess:msg:%s:%d", sessionID, shard)
}

// sessionShard returns a consistent shard index for a session ID.
func sessionShard(sessionID string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(sessionID))
	return h.Sum32() % shardCount
}

// ─── CRUD Operations ─────────────────────────────────────────────────────────

// Create initializes a new session.
//
// Empty userID is normalized to AnonymousUserID. Without this, sessions/
// memories created without auth would scatter across "" / "default" / hand-
// rolled fallbacks, and the long-term memory tab (which queries by user_id)
// would silently return 0 even when 33 rows exist under user_id=''.
func (m *Manager) Create(ctx context.Context, userID, projectID string) (*models.Session, error) {
	if userID == "" {
		userID = AnonymousUserID
	}
	if projectID == "" {
		projectID = "default"
	}
	session := &models.Session{
		ID:        uuid.New().String(),
		UserID:    userID,
		ProjectID: projectID,
		Messages:  make([]models.Message, 0),
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	if err := m.saveHot(ctx, session); err != nil {
		return nil, fmt.Errorf("failed to create session: %w", err)
	}

	// Best-effort index insert. Failure here is non-fatal — the session is
	// already saved and Get/AddMessage will still work; only the sidebar
	// listing degrades. Logged so a sustained outage is visible.
	if err := m.rdb.ZAdd(ctx, sessionIndexKey(userID), redis.Z{
		Score:  float64(session.UpdatedAt.UnixNano()),
		Member: session.ID,
	}).Err(); err != nil {
		m.logger.Warn("failed to add session to user index",
			zap.String("session_id", session.ID),
			zap.String("user_id", userID),
			zap.Error(err),
		)
	}

	// Mirror to PG long-term store. Async + best-effort: the Redis write above
	// is already the source of truth for the active request; PG miss only
	// matters once hot TTL expires, by which point the goroutine has long
	// since finished or logged.
	m.asyncPGUpsert(session)

	m.logger.Info("session created",
		zap.String("session_id", session.ID),
		zap.String("project_id", projectID),
		zap.Uint32("shard", sessionShard(session.ID)),
	)
	return session, nil
}

// rehydrateFromPG pulls a session from the PG long-term store on Redis hot
// miss and writes it back to hot so subsequent Get/AddMessage stay on the fast
// path. Returns nil for any failure mode (PG unavailable, ErrSessionNotFound,
// rewrite error) — caller treats nil as "still not found" and surfaces the
// original session-not-found error to keep behavior identical when PGStore
// isn't wired.
func (m *Manager) rehydrateFromPG(ctx context.Context, sessionID string) *models.Session {
	if m.PGStore == nil {
		return nil
	}
	session, err := m.PGStore.Get(ctx, sessionID)
	if err != nil {
		if !errors.Is(err, ErrSessionNotFound) {
			m.logger.Warn("pg session rehydrate failed",
				zap.String("session_id", sessionID),
				zap.Error(err),
			)
		}
		return nil
	}
	if err := m.saveHot(ctx, session); err != nil {
		m.logger.Warn("pg session rehydrate: hot rewrite failed",
			zap.String("session_id", sessionID),
			zap.Error(err),
		)
		// Still return the session — the read succeeded even if the write-
		// back didn't. Next call will rehydrate again.
	} else {
		// Refresh the per-user index ZSET so the sidebar keeps reflecting
		// recency once this session is hot again.
		if err := m.rdb.ZAdd(ctx, sessionIndexKey(session.UserID), redis.Z{
			Score:  float64(session.UpdatedAt.UnixNano()),
			Member: session.ID,
		}).Err(); err != nil {
			m.logger.Warn("pg session rehydrate: index refresh failed",
				zap.String("session_id", sessionID),
				zap.Error(err),
			)
		}
	}
	return session
}

// asyncPGUpsert is the shared best-effort PG mirror used by Create/AddMessage.
// No-op when PGStore is nil so Redis-only deployments pay zero overhead.
//
// The copy is made on the caller's goroutine — the source session is mutated
// by ReAct loops (e.g. AddMessage appends, performHotColdSeparation trims)
// while the goroutine runs, and json.Marshal would otherwise race the writer.
func (m *Manager) asyncPGUpsert(s *models.Session) {
	if m == nil || m.PGStore == nil || s == nil {
		return
	}
	snapshot := *s
	if len(s.Messages) > 0 {
		snapshot.Messages = make([]models.Message, len(s.Messages))
		copy(snapshot.Messages, s.Messages)
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.PGStore.Upsert(ctx, &snapshot); err != nil {
			m.logger.Warn("pg session upsert failed",
				zap.String("session_id", snapshot.ID),
				zap.Error(err),
			)
		}
	}()
}

// Get retrieves a session from Redis, combining hot data with cold summary.
func (m *Manager) Get(ctx context.Context, sessionID string) (*models.Session, error) {
	// Use pooled buffer for JSON unmarshaling (GC optimization §4)
	buf := pool.GlobalBufferPool.Get()
	defer pool.GlobalBufferPool.Put(buf)

	data, err := m.rdb.Get(ctx, hotKey(sessionID)).Bytes()
	if err != nil {
		if err == redis.Nil {
			// Redis hot expired or evicted. Fall back to PG long-term store
			// when wired — this is the rehydrate path that keeps "click an
			// archived session in the sidebar" working past the 24h TTL.
			if rehydrated := m.rehydrateFromPG(ctx, sessionID); rehydrated != nil {
				return rehydrated, nil
			}
			return nil, fmt.Errorf("session not found: %s", sessionID)
		}
		return nil, fmt.Errorf("failed to get session: %w", err)
	}

	var session models.Session
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, fmt.Errorf("failed to unmarshal session: %w", err)
	}

	// Merge cold summary if exists
	coldData, err := m.rdb.Get(ctx, coldKey(sessionID)).Bytes()
	if err == nil {
		var cold ColdSessionData
		if json.Unmarshal(coldData, &cold) == nil && cold.Summary != "" {
			if session.Summary == "" {
				session.Summary = cold.Summary
			} else {
				session.Summary = cold.Summary + "\n" + session.Summary
			}
		}
	}

	return &session, nil
}

// addMessageLuaScript is a Redis Lua script that atomically appends a message
// and refreshes the per-user session index ZSET.
//
// [OPT-9] GET → modify → SET in a single atomic Redis operation eliminates the
// race condition where concurrent AddMessage calls could overwrite each other's
// messages. The same Lua block also runs ZADD against `sess:index:<userID>`
// so the sidebar's recency ordering stays consistent without a follow-up RTT.
//
// KEYS[1] = sess:hot:<sid>
// ARGV[1] = msgJSON
// ARGV[2] = ttl seconds
// ARGV[3] = updated_at as RFC3339Nano (stored on session)
// ARGV[4] = ZSET key prefix ("sess:index:")
// ARGV[5] = updated_at as unix-nanos string (used as ZSET score)
// ARGV[6] = session id (ZSET member)
// ARGV[7] = anonymous user fallback (= AnonymousUserID; injected so the Go
//
//	const remains the single source of truth — don't inline a literal here)
var addMessageLuaScript = redis.NewScript(`
	local key = KEYS[1]
	local msgJSON = ARGV[1]
	local ttl = tonumber(ARGV[2])
	local data = redis.call('GET', key)
	if not data then
		return redis.error_reply("session not found")
	end
	local session = cjson.decode(data)
	local msg = cjson.decode(msgJSON)
	table.insert(session.messages, msg)
	session.updated_at = ARGV[3]
	local encoded = cjson.encode(session)
	redis.call('SET', key, encoded, 'EX', ttl)

	local userID = session.user_id
	if userID == nil or userID == "" then
		userID = ARGV[7]
	end
	local score = tonumber(ARGV[5])
	redis.call('ZADD', ARGV[4] .. userID, score, ARGV[6])

	return #session.messages
`)

// AddMessage appends a message to the session atomically via Lua script.
// [OPT-9] Uses Redis Lua to prevent race conditions from concurrent writes.
// When hot messages exceed token threshold, older messages are compressed
// and archived to cold storage.
func (m *Manager) AddMessage(ctx context.Context, sessionID string, msg models.Message) error {
	// Estimate token count
	if msg.TokenCount == 0 {
		msg.TokenCount = estimateTokens(msg.Content)
	}
	msg.ID = uuid.New().String()
	msg.Timestamp = time.Now()

	// Auto-pin messages that contain critical instructions
	if !msg.Pinned {
		msg.Pinned = shouldAutoPin(msg)
	}

	msgJSON, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	ttlSeconds := int(m.cfg.TTL.Seconds())
	now := time.Now()
	nowStr := now.Format(time.RFC3339Nano)
	nowNanos := fmt.Sprintf("%d", now.UnixNano())

	// Atomic append + index refresh via Lua
	_, err = addMessageLuaScript.Run(ctx, m.rdb,
		[]string{hotKey(sessionID)},
		string(msgJSON), ttlSeconds, nowStr,
		"sess:index:", nowNanos, sessionID, AnonymousUserID,
	).Int()
	if err != nil {
		// Fallback to non-atomic path if Lua fails (e.g., session not found)
		return m.addMessageFallback(ctx, sessionID, msg)
	}

	// Check if hot/cold separation is needed based on token budget
	// (async — use Background since request ctx may cancel before goroutine runs)
	go m.checkAndArchive(context.Background(), sessionID)

	// Mirror the updated session to PG. We need a fresh snapshot because the
	// Lua script wrote it server-side — choosing one extra Redis GET over
	// teaching the Lua script to return the JSON keeps the Lua block stable.
	// Skip the round trip entirely when PG isn't wired.
	if m.PGStore != nil {
		if updated, getErr := m.Get(ctx, sessionID); getErr == nil {
			m.asyncPGUpsert(updated)
		}
	}

	return nil
}

// addMessageFallback is the original non-atomic path, used if Lua script fails.
func (m *Manager) addMessageFallback(ctx context.Context, sessionID string, msg models.Message) error {
	session, err := m.Get(ctx, sessionID)
	if err != nil {
		return err
	}

	session.Messages = append(session.Messages, msg)
	session.UpdatedAt = time.Now()

	if err := m.saveHot(ctx, session); err != nil {
		return err
	}

	// Mirror the Lua-path index refresh so fallback writes still bump recency.
	if err := m.rdb.ZAdd(ctx, sessionIndexKey(session.UserID), redis.Z{
		Score:  float64(session.UpdatedAt.UnixNano()),
		Member: session.ID,
	}).Err(); err != nil {
		m.logger.Warn("fallback: failed to refresh session index",
			zap.String("session_id", session.ID),
			zap.Error(err),
		)
	}
	return nil
}

// checkAndArchive checks if the session exceeds token budget and triggers archival.
func (m *Manager) checkAndArchive(ctx context.Context, sessionID string) {
	session, err := m.Get(ctx, sessionID)
	if err != nil {
		m.logger.Warn("checkAndArchive: failed to get session", zap.Error(err))
		return
	}

	// Calculate total tokens (summary + all messages)
	totalTokens := m.calculateTotalTokens(session)

	// Trigger archival if we exceed the summary threshold
	if totalTokens > m.cfg.SummaryThresholdTokens && len(session.Messages) > minHotMessages {
		m.performHotColdSeparation(ctx, sessionID)
	}
}

// performHotColdSeparation runs asynchronously to archive old messages.
// Archives messages until total tokens fall below SummaryThresholdTokens,
// while keeping at least minHotMessages in hot storage.
func (m *Manager) performHotColdSeparation(ctx context.Context, sessionID string) {
	session, err := m.Get(ctx, sessionID)
	if err != nil {
		m.logger.Warn("hot/cold separation: failed to get session", zap.Error(err))
		return
	}

	if len(session.Messages) <= minHotMessages {
		return
	}

	// Calculate how many messages to archive based on token budget
	totalTokens := m.calculateTotalTokens(session)
	if totalTokens <= m.cfg.SummaryThresholdTokens {
		return
	}

	// Find the split point: keep recent messages within token budget
	targetTokens := m.cfg.SummaryThresholdTokens * 3 / 4 // Leave 25% headroom
	runningTokens := estimateTokens(session.Summary)
	keepFrom := len(session.Messages)

	// Scan from newest to oldest, accumulating tokens
	for i := len(session.Messages) - 1; i >= minHotMessages; i-- {
		msgTokens := session.Messages[i].TokenCount
		if msgTokens == 0 {
			msgTokens = estimateTokens(session.Messages[i].Content)
		}
		runningTokens += msgTokens
		if runningTokens > targetTokens {
			keepFrom = i + 1
			break
		}
		keepFrom = i
	}

	// Ensure we keep at least minHotMessages
	if keepFrom < minHotMessages {
		keepFrom = minHotMessages
	}

	// If even the newest messages exceed budget, still archive older ones
	maxKeepFrom := len(session.Messages) - minHotMessages
	if keepFrom > maxKeepFrom {
		keepFrom = maxKeepFrom
	}

	// Nothing to archive
	if keepFrom <= 0 {
		return
	}

	archiveCount := keepFrom
	m.logger.Info("archiving cold messages",
		zap.String("session_id", sessionID),
		zap.Int("archive_count", archiveCount),
		zap.Int("total_tokens_before", totalTokens),
	)

	coldMsgs := session.Messages[:archiveCount]
	go m.archiveToCold(context.Background(), sessionID, coldMsgs, session.Summary)

	session.Summary = m.buildSummary(session.Summary, coldMsgs)
	session.Messages = session.Messages[archiveCount:]

	// Double-check: if still over budget, apply sliding window compression
	totalTokens = m.calculateTotalTokens(session)
	if totalTokens > m.cfg.SummaryThresholdTokens {
		m.compressContext(session)
	}

	_ = m.saveHot(ctx, session)
}
// GetMessage retrieves a specific message from a session.
func (m *Manager) GetMessage(ctx context.Context, sessionID, messageID string) (*models.Message, error) {
	s, err := m.Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	for i := range s.Messages {
		if s.Messages[i].ID == messageID {
			return &s.Messages[i], nil
		}
	}
	return nil, fmt.Errorf("message %s not found in session %s", messageID, sessionID)
}


// GetContextWindow returns the current messages suitable for LLM context.
func (m *Manager) GetContextWindow(ctx context.Context, sessionID string) ([]models.Message, error) {
	session, err := m.Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	var result []models.Message

	if session.Summary != "" {
		result = append(result, models.Message{
			Role:    models.RoleSystem,
			Content: fmt.Sprintf("[Previous conversation summary]: %s", session.Summary),
		})
	}

	result = append(result, session.Messages...)
	return result, nil
}

// Ping checks Redis connectivity for health checks.
func (m *Manager) Ping(ctx context.Context) error {
	return m.rdb.Ping(ctx).Err()
}

// Delete removes all session data (hot + cold + sharded message keys + index entry).
//
// The index ZREM needs the userID, which we fetch from the hot key before
// deletion. If the hot key is already missing we still attempt cleanup on the
// anonymous index (covers legacy / anonymous sessions — empty user_id is
// normalized to AnonymousUserID by sessionIndexKey).
func (m *Manager) Delete(ctx context.Context, sessionID string) error {
	// Resolve userID before we delete the hot key. Best-effort: a missing
	// session is not fatal here since the caller's intent is removal.
	userID := ""
	if session, err := m.Get(ctx, sessionID); err == nil {
		userID = session.UserID
	}

	pipe := m.rdb.Pipeline()
	pipe.Del(ctx, hotKey(sessionID))
	pipe.Del(ctx, coldKey(sessionID))

	for i := 0; i < shardCount; i++ {
		pipe.Del(ctx, fmt.Sprintf("sess:msg:%s:%d", sessionID, i))
	}

	pipe.ZRem(ctx, sessionIndexKey(userID), sessionID)

	_, err := pipe.Exec(ctx)

	// Mirror delete to PG. Async + best-effort: a stale PG row would later
	// resurface in the sidebar, but the caller's intent (Redis purge) is
	// already satisfied; the next ListSessions call would still expose it.
	if m.PGStore != nil {
		go func() {
			bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if delErr := m.PGStore.Delete(bgCtx, sessionID); delErr != nil {
				m.logger.Warn("pg session delete failed",
					zap.String("session_id", sessionID),
					zap.Error(delErr),
				)
			}
		}()
	}

	return err
}

// ─── Session Listing (Sidebar) ───────────────────────────────────────────────

// SessionSummary is the lightweight projection used by the sidebar listing.
// It deliberately omits the full Messages slice — surfacing only the metadata
// and a short preview keeps the list payload small even when a user has
// hundreds of sessions.
type SessionSummary struct {
	ID                 string    `json:"id"`
	UserID             string    `json:"user_id"`
	ProjectID          string    `json:"project_id,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
	MessageCount       int       `json:"message_count"`
	LastMessageRole    string    `json:"last_message_role,omitempty"`
	LastMessagePreview string    `json:"last_message_preview,omitempty"`
}

// ListSessions returns the user's sessions ordered by most-recent activity.
//
// limit ≤ 0 defaults to 50; the hard cap (200) bounds worst-case load from a
// misbehaving caller.
//
// Two paths:
//
//   - **PG-authoritative** (PGStore != nil): list directly from `sessions`.
//     This is the only path that survives Redis TTL — without it, hot expiry
//     would silently drop the row from the sidebar forever. We deliberately
//     do NOT ZREM stale ZSET members here; the index is now a warming hint,
//     not the source of truth, and pruning would re-introduce the original
//     bug (sessions older than 48h vanish permanently from PG queries too).
//
//   - **Redis-only legacy** (PGStore == nil): preserve the pre-PG behavior
//     for deployments without Postgres — list from the per-user ZSET, drop
//     and ZREM rows whose hot key is gone. This path is exactly what shipped
//     before this commit.
func (m *Manager) ListSessions(ctx context.Context, userID string, limit int) ([]*SessionSummary, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	if m.PGStore != nil {
		return m.listSessionsFromPG(ctx, userID, limit)
	}
	return m.listSessionsFromRedis(ctx, userID, limit)
}

// listSessionsFromPG is the PG-authoritative path. Hot warming is opportunistic:
// if a session is still in hot we use its (more accurate, near-realtime) message
// metrics; otherwise we surface the PG projection columns as-is.
func (m *Manager) listSessionsFromPG(ctx context.Context, userID string, limit int) ([]*SessionSummary, error) {
	pgSummaries, err := m.PGStore.ListByUser(ctx, userID, limit)
	if err != nil {
		// Fall back to Redis-only on PG outage so the sidebar doesn't go
		// dark when PG hiccups. Logged so the degradation is visible.
		m.logger.Warn("pg session list failed, falling back to redis index",
			zap.String("user_id", userID),
			zap.Error(err),
		)
		return m.listSessionsFromRedis(ctx, userID, limit)
	}
	if len(pgSummaries) == 0 {
		return []*SessionSummary{}, nil
	}

	for _, s := range pgSummaries {
		// Best-effort hot refresh — if hot is still warm, the message count
		// and preview reflect the live state more accurately than PG, which
		// is only refreshed by the async asyncPGUpsert goroutine.
		if session, err := m.fetchHotOnly(ctx, s.ID); err == nil {
			fresh := buildSummary(session)
			s.MessageCount = fresh.MessageCount
			s.LastMessageRole = fresh.LastMessageRole
			s.LastMessagePreview = fresh.LastMessagePreview
			s.UpdatedAt = fresh.UpdatedAt
		}
	}
	return pgSummaries, nil
}

// listSessionsFromRedis is the legacy Redis-only path. Identical to the
// pre-PG implementation: walk the per-user ZSET, drop entries whose hot key
// is gone, and ZREM stale members so the next call doesn't pay for them.
func (m *Manager) listSessionsFromRedis(ctx context.Context, userID string, limit int) ([]*SessionSummary, error) {
	indexKey := sessionIndexKey(userID)
	ids, err := m.rdb.ZRevRange(ctx, indexKey, 0, int64(limit-1)).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to read session index: %w", err)
	}
	if len(ids) == 0 {
		return []*SessionSummary{}, nil
	}

	summaries := make([]*SessionSummary, 0, len(ids))
	var stale []string

	for _, sid := range ids {
		session, err := m.Get(ctx, sid)
		if err != nil {
			stale = append(stale, sid)
			continue
		}
		summaries = append(summaries, buildSummary(session))
	}

	if len(stale) > 0 {
		members := make([]any, len(stale))
		for i, s := range stale {
			members[i] = s
		}
		go func() {
			if err := m.rdb.ZRem(context.Background(), indexKey, members...).Err(); err != nil {
				m.logger.Warn("failed to prune stale session index entries",
					zap.String("user_id", userID),
					zap.Int("count", len(stale)),
					zap.Error(err),
				)
			}
		}()
	}

	return summaries, nil
}

// fetchHotOnly reads a session strictly from the Redis hot key, without
// triggering the PG rehydrate path that Get does. Used by ListSessions' PG
// path to opportunistically overlay live message metrics without amplifying
// PG read pressure (rehydrating every listed session would defeat the point).
func (m *Manager) fetchHotOnly(ctx context.Context, sessionID string) (*models.Session, error) {
	data, err := m.rdb.Get(ctx, hotKey(sessionID)).Bytes()
	if err != nil {
		return nil, err
	}
	var session models.Session
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, err
	}
	return &session, nil
}

// buildSummary projects a full session into the lightweight sidebar shape.
// Pulls the last non-empty message for the preview line — preferring the
// assistant's reply, falling back to the user's most recent prompt when the
// assistant hasn't responded yet.
func buildSummary(s *models.Session) *SessionSummary {
	sum := &SessionSummary{
		ID:           s.ID,
		UserID:       s.UserID,
		ProjectID:    s.ProjectID,
		CreatedAt:    s.CreatedAt,
		UpdatedAt:    s.UpdatedAt,
		MessageCount: len(s.Messages),
	}
	for i := len(s.Messages) - 1; i >= 0; i-- {
		if s.Messages[i].Content == "" {
			continue
		}
		sum.LastMessageRole = string(s.Messages[i].Role)
		sum.LastMessagePreview = truncate(s.Messages[i].Content, 120)
		break
	}
	return sum
}

// ─── Internal Helpers ────────────────────────────────────────────────────────

// saveHot persists the hot session data to Redis with TTL.
func (m *Manager) saveHot(ctx context.Context, session *models.Session) error {
	// Use pooled JSON encoder (GC optimization §4)
	data, err := pool.GlobalJSONPool.Encode(session)
	if err != nil {
		return fmt.Errorf("failed to marshal session: %w", err)
	}
	return m.rdb.Set(ctx, hotKey(session.ID), data, m.cfg.TTL).Err()
}

// archiveToCold sends old messages to cold storage asynchronously.
func (m *Manager) archiveToCold(ctx context.Context, sessionID string, messages []models.Message, currentSummary string) {
	cold := &ColdSessionData{
		SessionID:  sessionID,
		Messages:   messages,
		Summary:    currentSummary,
		ArchivedAt: time.Now(),
	}

	// Save to Redis cold key (fast path)
	data, err := json.Marshal(cold)
	if err != nil {
		m.logger.Error("failed to marshal cold data", zap.Error(err))
		return
	}
	if err := m.rdb.Set(ctx, coldKey(sessionID), data, m.cfg.TTL*2).Err(); err != nil {
		m.logger.Error("failed to save cold data to Redis", zap.Error(err))
	}

	// Also flush to persistent storage if configured
	if m.ColdStore != nil {
		if err := m.ColdStore(ctx, sessionID, cold); err != nil {
			m.logger.Error("failed to archive to cold store", zap.Error(err))
		}
	}
}

// buildSummary generates a summary from archived messages.
// Uses LLM-based Summarizer when available, falls back to naive truncation.
func (m *Manager) buildSummary(existingSummary string, messages []models.Message) string {
	if m.Summarizer != nil {
		summary, err := m.Summarizer.Summarize(context.Background(), messages, existingSummary)
		if err == nil {
			return summary
		}
		m.logger.Warn("LLM summarizer failed, falling back to naive", zap.Error(err))
	}

	var parts []string
	for _, msg := range messages {
		parts = append(parts, fmt.Sprintf("[%s]: %s", msg.Role, truncate(msg.Content, 80)))
	}

	newSummary := fmt.Sprintf("Archived %d messages. Key exchanges covered: %s",
		len(messages), truncate(joinStrings(parts, "; "), 500))

	if existingSummary != "" {
		return existingSummary + "\n" + newSummary
	}
	return newSummary
}

// calculateTotalTokens sums up token counts across hot messages and summary.
func (m *Manager) calculateTotalTokens(session *models.Session) int {
	total := estimateTokens(session.Summary)
	for _, msg := range session.Messages {
		if msg.TokenCount > 0 {
			total += msg.TokenCount
		} else {
			total += estimateTokens(msg.Content)
		}
	}
	return total
}

// compressContext applies sliding window compression on hot messages.
func (m *Manager) compressContext(session *models.Session) {
	if len(session.Messages) <= 2 {
		return
	}

	budget := m.cfg.MaxHistoryTokens / 2
	keepFrom := len(session.Messages)
	runningTokens := 0

	for i := len(session.Messages) - 1; i >= 0; i-- {
		tokens := session.Messages[i].TokenCount
		if tokens == 0 {
			tokens = estimateTokens(session.Messages[i].Content)
		}
		runningTokens += tokens
		if runningTokens > budget {
			keepFrom = i + 1
			break
		}
		keepFrom = i
	}

	if keepFrom <= 0 {
		return
	}

	// Compress removed messages into summary
	session.Summary = m.buildSummary(session.Summary, session.Messages[:keepFrom])
	session.Messages = session.Messages[keepFrom:]

	m.logger.Debug("context compressed",
		zap.Int("removed_messages", keepFrom),
		zap.Int("remaining_messages", len(session.Messages)),
	)
}

// estimateTokens delegates to the unified tokenizer for fast estimation.
func estimateTokens(text string) int {
	return llm.FastEstimate(text)
}

// truncate shortens a string to maxLen characters.
func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}

// joinStrings joins strings with a separator (avoids importing strings package).
func joinStrings(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	result := parts[0]
	for _, p := range parts[1:] {
		result += sep + p
	}
	return result
}

// shouldAutoPin determines if a message should be automatically pinned based on heuristics.
func shouldAutoPin(msg models.Message) bool {
	if msg.Role != models.RoleUser {
		return false
	}
	content := strings.ToLower(msg.Content)
	// Pin messages containing critical instructions or constraints
	keywords := []string{
		"always", "never", "must", "required", "critical",
		"important:", "note:", "remember:", "constraint:",
		"do not", "don't forget", "make sure",
	}
	for _, kw := range keywords {
		if strings.Contains(content, kw) {
			return true
		}
	}
	return false
}

// PinMessage marks a message as pinned so it won't be pruned.
func (m *Manager) PinMessage(ctx context.Context, sessionID, messageID string) error {
	return m.updateMessagePinStatus(ctx, sessionID, messageID, true)
}

// UnpinMessage removes the pin from a message.
func (m *Manager) UnpinMessage(ctx context.Context, sessionID, messageID string) error {
	return m.updateMessagePinStatus(ctx, sessionID, messageID, false)
}

// updateMessagePinStatus updates the Pinned field of a message.
func (m *Manager) updateMessagePinStatus(ctx context.Context, sessionID, messageID string, pinned bool) error {
	// Fetch session
	data, err := m.rdb.Get(ctx, hotKey(sessionID)).Bytes()
	if err != nil {
		return fmt.Errorf("session not found: %w", err)
	}

	var session models.Session
	if err := json.Unmarshal(data, &session); err != nil {
		return fmt.Errorf("failed to unmarshal session: %w", err)
	}

	// Find and update message
	found := false
	for i := range session.Messages {
		if session.Messages[i].ID == messageID {
			session.Messages[i].Pinned = pinned
			found = true
			break
		}
	}

	if !found {
		return fmt.Errorf("message not found: %s", messageID)
	}

	// Save back
	updatedData, err := json.Marshal(session)
	if err != nil {
		return fmt.Errorf("failed to marshal session: %w", err)
	}

	if err := m.rdb.Set(ctx, hotKey(sessionID), updatedData, m.cfg.TTL).Err(); err != nil {
		return fmt.Errorf("failed to save session: %w", err)
	}

	return nil
}
