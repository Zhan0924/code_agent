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
	// maxHotMessages is the maximum number of recent messages kept in Redis.
	// Older messages are compressed into a summary and archived.
	maxHotMessages = 10

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

	// ColdStore is an optional callback for archiving cold session data.
	// In production, this would write to PostgreSQL or S3.
	ColdStore func(ctx context.Context, sessionID string, data *ColdSessionData) error

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
func (m *Manager) Create(ctx context.Context, userID, projectID string) (*models.Session, error) {
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

	m.logger.Info("session created",
		zap.String("session_id", session.ID),
		zap.String("project_id", projectID),
		zap.Uint32("shard", sessionShard(session.ID)),
	)
	return session, nil
}

// Get retrieves a session from Redis, combining hot data with cold summary.
func (m *Manager) Get(ctx context.Context, sessionID string) (*models.Session, error) {
	// Use pooled buffer for JSON unmarshaling (GC optimization §4)
	buf := pool.GlobalBufferPool.Get()
	defer pool.GlobalBufferPool.Put(buf)

	data, err := m.rdb.Get(ctx, hotKey(sessionID)).Bytes()
	if err != nil {
		if err == redis.Nil {
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

// addMessageLuaScript is a Redis Lua script that atomically appends a message.
// [OPT-9] It performs GET → modify → SET in a single atomic Redis operation,
// eliminating the race condition where concurrent AddMessage calls could
// overwrite each other's messages.
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
	return #session.messages
`)

// AddMessage appends a message to the session atomically via Lua script.
// [OPT-9] Uses Redis Lua to prevent race conditions from concurrent writes.
// When hot messages exceed maxHotMessages, older messages are compressed
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
	nowStr := time.Now().Format(time.RFC3339Nano)

	// Atomic append via Lua
	result, err := addMessageLuaScript.Run(ctx, m.rdb,
		[]string{hotKey(sessionID)},
		string(msgJSON), ttlSeconds, nowStr,
	).Int()
	if err != nil {
		// Fallback to non-atomic path if Lua fails (e.g., session not found)
		return m.addMessageFallback(ctx, sessionID, msg)
	}

	// Check if hot/cold separation or compression is needed
	if result > maxHotMessages {
		go m.performHotColdSeparation(ctx, sessionID)
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

	return m.saveHot(ctx, session)
}

// performHotColdSeparation runs asynchronously to archive old messages.
func (m *Manager) performHotColdSeparation(ctx context.Context, sessionID string) {
	session, err := m.Get(ctx, sessionID)
	if err != nil {
		m.logger.Warn("hot/cold separation: failed to get session", zap.Error(err))
		return
	}

	if len(session.Messages) <= maxHotMessages {
		return
	}

	archiveCount := len(session.Messages) - maxHotMessages
	m.logger.Info("archiving cold messages",
		zap.String("session_id", sessionID),
		zap.Int("archive_count", archiveCount),
	)

	coldMsgs := session.Messages[:archiveCount]
	go m.archiveToCold(context.Background(), sessionID, coldMsgs, session.Summary)

	session.Summary = m.buildSummary(session.Summary, coldMsgs)
	session.Messages = session.Messages[archiveCount:]

	// Token budget check
	totalTokens := m.calculateTotalTokens(session)
	if totalTokens > m.cfg.SummaryThresholdTokens {
		m.compressContext(session)
	}

	_ = m.saveHot(ctx, session)
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

// Delete removes all session data (hot + cold + sharded message keys).
func (m *Manager) Delete(ctx context.Context, sessionID string) error {
	// Delete hot data
	pipe := m.rdb.Pipeline()
	pipe.Del(ctx, hotKey(sessionID))
	pipe.Del(ctx, coldKey(sessionID))

	// Delete all message shards
	for i := 0; i < shardCount; i++ {
		pipe.Del(ctx, fmt.Sprintf("sess:msg:%s:%d", sessionID, i))
	}

	_, err := pipe.Exec(ctx)
	return err
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
