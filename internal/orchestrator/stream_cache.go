// Package orchestrator —— stream_cache.go
//
// StreamCache 把 ReactStreamEvent 镜像写到 Redis Stream，使刷新页面 / 重连后
// 仍能拉取历史事件并继续跟随直到任务完成。
//
// 为何引入：
//   ProcessMessageStreamFull 已用 background workCtx 让 SSE 断连不再撤销
//   业务（见 orchestrator.go 顶端长注释），但「事件本身」仍然只走内存
//   channel。客户端断开后再回来，看不到中间的 thinking / tool_call /
//   tool_result，只剩最后落 session 的 assistant 终态。任务感知体验断裂。
//
// 设计：
//  1. 每个 session 一条 Redis Stream（key: agent:stream:events:{sessionID}），
//     XADD 写入 ReactStreamEvent JSON。MaxLen ~ 2000 条（近似上限，足够覆盖
//     单次 ReAct 链路）。
//  2. 状态 key（agent:stream:status:{sessionID}）保存 task_id，TTL 6h。
//     MarkDone 后立即 DEL，调用方据此判断「是否还在跑」。
//  3. Replay 走 XRANGE 一次性返回历史；Follow 用 XREAD BLOCK 跟随增量，
//     直到 status 被清除（任务完成 / 失败）或 ctx 取消。
//  4. TTL：1h after MarkDone —— 完成后给前端几分钟容错重连，避免立即清掉。

package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/agent/code_agent/internal/models"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

const (
	streamEventsKeyPrefix = "agent:stream:events:"
	streamStatusKeyPrefix = "agent:stream:status:"

	// 一条 ReAct 链路通常几十~几百条事件；2000 给出安全余量。
	streamMaxLen int64 = 2000

	// MarkDone 后 stream 保留时长。短了：用户切回看不到结果；长了：浪费 Redis。
	streamDoneTTL = time.Hour

	// 任务跑起来时的 status TTL —— ReAct 30min 兜底 + 余量。
	streamRunningTTL = 6 * time.Hour

	// Follow 单次 XREAD 阻塞时长；超时后短暂检查 status 再继续。
	streamFollowBlock = 5 * time.Second
)

// StreamCache 是 ReactStreamEvent 的 Redis Stream 镜像。
// 不持有 sink 引用 —— 调用方负责在 Emit 时同步调用 Append。
type StreamCache struct {
	rdb    *redis.Client
	logger *zap.Logger
}

// NewStreamCache returns a StreamCache backed by the provided Redis client.
// rdb 为 nil 时返回 nil —— 调用方需 nil-check。
func NewStreamCache(rdb *redis.Client, logger *zap.Logger) *StreamCache {
	if rdb == nil {
		return nil
	}
	return &StreamCache{
		rdb:    rdb,
		logger: logger.With(zap.String("component", "stream_cache")),
	}
}

// MarkRunning 标记 session 当前在跑某个 taskID。同时给 stream key 设上 TTL
// 以防卡死遗忘 MarkDone 的极端情况下 key 永生。
func (c *StreamCache) MarkRunning(ctx context.Context, sessionID, taskID string) {
	if c == nil || sessionID == "" {
		return
	}
	statusKey := streamStatusKeyPrefix + sessionID
	if err := c.rdb.Set(ctx, statusKey, taskID, streamRunningTTL).Err(); err != nil {
		c.logger.Warn("mark running failed",
			zap.String("session_id", sessionID),
			zap.String("task_id", taskID),
			zap.Error(err))
		return
	}
	// 给 events stream 设上一个保守 TTL —— MarkDone 时会缩短到 streamDoneTTL。
	_ = c.rdb.Expire(ctx, streamEventsKeyPrefix+sessionID, streamRunningTTL).Err()
}

// MarkDone 清除 running 状态，并把 stream key 收紧到 1h TTL。
func (c *StreamCache) MarkDone(ctx context.Context, sessionID string) {
	if c == nil || sessionID == "" {
		return
	}
	statusKey := streamStatusKeyPrefix + sessionID
	if err := c.rdb.Del(ctx, statusKey).Err(); err != nil {
		c.logger.Warn("mark done (status del) failed",
			zap.String("session_id", sessionID), zap.Error(err))
	}
	if err := c.rdb.Expire(ctx, streamEventsKeyPrefix+sessionID, streamDoneTTL).Err(); err != nil {
		c.logger.Warn("mark done (events ttl) failed",
			zap.String("session_id", sessionID), zap.Error(err))
	}
}

// ClearAllRunning 启动时调用：清扫所有 agent:stream:status:* 孤儿 key。
//
// 背景：MarkDone 仅在任务正常 return 路径上跑；进程 SIGTERM / panic / OOM
// 会留下 6h TTL 的 status key。下次进程起来，前端 GET /react-stream/status
// 仍然看到 running:true → 自动 /resume → Follow 死等永远不会到来的事件 →
// 90s 前端 watchdog 触发 "Connection error: Failed to fetch"。
//
// 单进程部署下，「进程启动」即等价于「没有任何 in-flight 任务」，
// 整把 status 名空间清空是安全的。stream events 自身不动 —— 它们配的是
// 1h done-TTL，刷新仍可看历史；只是不会再触发 resume Follow。
//
// 多实例部署时若引入此方法需要改成「按本实例 host id 限定」清扫范围。
func (c *StreamCache) ClearAllRunning(ctx context.Context) (int, error) {
	if c == nil {
		return 0, nil
	}
	var (
		cursor uint64
		total  int
	)
	for {
		keys, next, err := c.rdb.Scan(ctx, cursor, streamStatusKeyPrefix+"*", 256).Result()
		if err != nil {
			return total, fmt.Errorf("scan %s*: %w", streamStatusKeyPrefix, err)
		}
		if len(keys) > 0 {
			n, err := c.rdb.Del(ctx, keys...).Result()
			if err != nil {
				return total, fmt.Errorf("del status keys: %w", err)
			}
			total += int(n)
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	return total, nil
}

// Status 返回当前 running task_id；running=false 表示没有进行中任务。
func (c *StreamCache) Status(ctx context.Context, sessionID string) (taskID string, running bool) {
	if c == nil || sessionID == "" {
		return "", false
	}
	v, err := c.rdb.Get(ctx, streamStatusKeyPrefix+sessionID).Result()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			c.logger.Warn("status get failed",
				zap.String("session_id", sessionID), zap.Error(err))
		}
		return "", false
	}
	return v, true
}

// EventCount 返回 session 的 events stream 当前长度。0 表示流不存在或为空。
// 用途：前端可借此判断「即使 !running，cache 里也仍有未消费历史」——
// 这种情况发生在原 SSE 已断、任务在 workCtx 里继续跑完并 MarkDone 之后，
// 客户端刷新或重连时仅靠 status.running 看不出该 Replay 拉回来的 tail。
func (c *StreamCache) EventCount(ctx context.Context, sessionID string) int64 {
	if c == nil || sessionID == "" {
		return 0
	}
	n, err := c.rdb.XLen(ctx, streamEventsKeyPrefix+sessionID).Result()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			c.logger.Debug("event count failed",
				zap.String("session_id", sessionID), zap.Error(err))
		}
		return 0
	}
	return n
}

// LastEventAt 返回 session 最后一条事件的写入毫秒戳；流为空或不存在返回 0。
//
// 用途:前端 GET /chat/react-stream/status 据此判断"后端是否真活着"——
// 即使 running=true、event_count>0，若 last_event_at 已停滞数分钟,前端可据此
// 提前提示用户"后端可能卡死",避免无止境地依赖 watchdog 90s 兜底。
//
// 实现:Redis Stream ID 形如 "1717843200000-0",前段就是 XADD 时的服务器毫秒戳;
// 直接 split 解析即可,无需服务端 TIME 调用。
func (c *StreamCache) LastEventAt(ctx context.Context, sessionID string) int64 {
	if c == nil || sessionID == "" {
		return 0
	}
	key := streamEventsKeyPrefix + sessionID
	msgs, err := c.rdb.XRevRangeN(ctx, key, "+", "-", 1).Result()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			c.logger.Debug("last event at xrevrange failed",
				zap.String("session_id", sessionID), zap.Error(err))
		}
		return 0
	}
	if len(msgs) == 0 {
		return 0
	}
	id := msgs[0].ID
	if idx := strings.IndexByte(id, '-'); idx > 0 {
		id = id[:idx]
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// Append 把一个 ReactStreamEvent 序列化并 XADD 到 session 的 stream。
// 失败仅记日志 —— 主路径 channelSink 仍然成功的话用户体验不受影响。
func (c *StreamCache) Append(ctx context.Context, sessionID string, ev models.ReactStreamEvent) {
	if c == nil || sessionID == "" {
		return
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		c.logger.Warn("event marshal failed",
			zap.String("session_id", sessionID),
			zap.String("type", ev.Type), zap.Error(err))
		return
	}
	args := &redis.XAddArgs{
		Stream: streamEventsKeyPrefix + sessionID,
		Values: map[string]interface{}{"event": string(payload)},
		// MAXLEN ~ N：近似裁剪，O(1) 摊销。
		MaxLen: streamMaxLen,
		Approx: true,
	}
	if err := c.rdb.XAdd(ctx, args).Err(); err != nil {
		c.logger.Warn("event xadd failed",
			zap.String("session_id", sessionID),
			zap.String("type", ev.Type), zap.Error(err))
	}
}

// Replay 一次性返回 sessionID 当前 stream 中所有事件 + 最后一条的 Redis Stream ID。
// 若 stream 不存在或为空，返回 (nil, "0", nil)。lastID 可以传给后续 Follow。
func (c *StreamCache) Replay(ctx context.Context, sessionID string) ([]models.ReactStreamEvent, string, error) {
	if c == nil || sessionID == "" {
		return nil, "0", nil
	}
	key := streamEventsKeyPrefix + sessionID
	msgs, err := c.rdb.XRange(ctx, key, "-", "+").Result()
	if err != nil {
		return nil, "0", fmt.Errorf("xrange %s: %w", key, err)
	}
	events := make([]models.ReactStreamEvent, 0, len(msgs))
	lastID := "0"
	for _, m := range msgs {
		raw, ok := m.Values["event"].(string)
		if !ok {
			continue
		}
		var ev models.ReactStreamEvent
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			c.logger.Warn("event unmarshal failed during replay",
				zap.String("session_id", sessionID), zap.Error(err))
			continue
		}
		events = append(events, ev)
		lastID = m.ID
	}
	return events, lastID, nil
}

// Follow 阻塞读取 lastID 之后的新事件，循环把它们推到返回的 channel；
// 当 status key 被清除（MarkDone）或 ctx 取消时，关闭 channel 退出。
//
// 调用方可对返回的 channel 做 range 消费；底层 goroutine 在退出前会 close 它。
// 调用方应该负责传入超时 / 可取消 ctx 防止永远阻塞。
func (c *StreamCache) Follow(ctx context.Context, sessionID, lastID string) <-chan models.ReactStreamEvent {
	out := make(chan models.ReactStreamEvent, 16)
	if c == nil || sessionID == "" {
		close(out)
		return out
	}
	if lastID == "" {
		lastID = "0"
	}
	go func() {
		defer close(out)
		key := streamEventsKeyPrefix + sessionID
		cur := lastID
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			res, err := c.rdb.XRead(ctx, &redis.XReadArgs{
				Streams: []string{key, cur},
				Block:   streamFollowBlock,
				Count:   64,
			}).Result()
			if err != nil {
				if errors.Is(err, redis.Nil) || errors.Is(err, context.Canceled) ||
					errors.Is(err, context.DeadlineExceeded) {
					// XREAD timeout —— 检查是否已 MarkDone，决定继续还是退出。
					if _, running := c.Status(ctx, sessionID); !running {
						// 给最后一波延迟落 stream 的事件一次抓尾巴的机会。
						c.drainTail(ctx, key, cur, out)
						return
					}
					continue
				}
				c.logger.Warn("xread failed",
					zap.String("session_id", sessionID), zap.Error(err))
				time.Sleep(500 * time.Millisecond)
				continue
			}
			for _, stream := range res {
				for _, m := range stream.Messages {
					raw, ok := m.Values["event"].(string)
					if !ok {
						continue
					}
					var ev models.ReactStreamEvent
					if err := json.Unmarshal([]byte(raw), &ev); err != nil {
						continue
					}
					select {
					case <-ctx.Done():
						return
					case out <- ev:
					}
					cur = m.ID
					if ev.Type == "done" || ev.Type == "error" {
						// 收到终态事件后再 drain 一次确保不漏，然后退出。
						c.drainTail(ctx, key, cur, out)
						return
					}
				}
			}
		}
	}()
	return out
}

// persistingSink wraps another sink and Appends every event to the StreamCache
// before forwarding. This is the integration point that lets a refreshed
// client replay history via /chat/react-stream/resume.
//
// 故意先 Append 后 Emit：在极端情况下（channelSink 被 droppedCtx 静默丢弃）
// 我们仍然希望事件留在 Redis 里给重连客户端读到。
type persistingSink struct {
	inner     reactEventSink
	cache     *StreamCache
	sessionID string
	ctx       context.Context
}

func (s *persistingSink) Emit(ev models.ReactStreamEvent) {
	s.cache.Append(s.ctx, s.sessionID, ev)
	s.inner.Emit(ev)
}

// drainTail XRANGE 把 lastID 之后剩余的（如果有）一次性发出来，避免 Follow
// 退出时丢掉最后一两条事件（典型：MarkDone 与最后一次 XREAD 之间的窗口）。
func (c *StreamCache) drainTail(ctx context.Context, key, lastID string, out chan<- models.ReactStreamEvent) {
	exclusive := "(" + lastID
	msgs, err := c.rdb.XRange(ctx, key, exclusive, "+").Result()
	if err != nil {
		return
	}
	for _, m := range msgs {
		raw, ok := m.Values["event"].(string)
		if !ok {
			continue
		}
		var ev models.ReactStreamEvent
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case out <- ev:
		}
	}
}
