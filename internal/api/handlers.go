// handlers.go — REST / SSE / WebSocket handler 实现集合。
//
// ============================================================================
//
//	设 计 原 理
//
// ============================================================================
//
// 【Handler 的四个标准动作】
//
//	1. 反序列化 & 校验请求体
//	2. 从 gin.Context 提取 auth claims / request ID
//	3. 调用 orchestrator / sessionMgr / workspaceMgr 执行业务
//	4. 写响应（JSON / SSE event / WS frame）
//
//	handler 不持有状态；所有共享状态在 Server 结构体里。
//
// 【为什么 ReAct 聊天用"detached context" 模式】
//
//	handleChat 里做了一个反直觉的事：LLM 调用的 context **不是** c.Request.Context()，
//	而是 context.Background()。原因：
//	  · ReAct 多步调用可能耗时 5-10 分钟；
//	  · 前端（或反向代理）往往 60s 就断开连接；
//	  · 用请求 context → 客户端一超时，我们就把一半算完的任务杀掉，
//	    用户再发一次完全重跑。
//	  · 用 detached context → 后台跑完，结果存 session。用户下次 GET
//	    /sessions/:id 能拿到完整结果。**类似 Claude Code 的做法**。
//	  权衡：服务器必须独立承担 timeout 责任（write_timeout: 600s）。
//
// 【SSE 事件格式】
//
//	所有 SSE 端点遵循 Gin 的 c.SSEvent("<event>", <data>) 约定。
//	·  chat/stream      — 单一"content"事件流（token 级别）
//	·  chat/react-stream — 多 event 类型：intent / thinking / tool_call /
//	                      tool_result / final / approval_request / done
//	客户端必须监听 `done` 事件来关闭连接，否则 buffer 一满就阻塞 worker。
//
// 【WS 升级策略】
//
//	handleWS 使用 gorilla/websocket 做升级。CheckOrigin 策略见 corsMiddleware
//	用的 allowedOrigins。消息格式 JSON: {type, content, session_id}。
//	心跳：ping/pong 60s；客户端需在 60s 内响应 pong，否则断开。
//
// 【Webhook handler 的假设】
//
//	handleWebhookMCPCallback / handleWebhookCICallback 仅在 HMAC 中间件
//	通过后执行。handler 本身不再校验身份，但会记录 request_id + payload 摘要
//	用于事后审计。
//
// ============================================================================
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/agent/code_agent/internal/models"
	"github.com/agent/code_agent/internal/orchestrator"
	"github.com/agent/code_agent/internal/session"
	"github.com/agent/code_agent/internal/workspace"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// ─── Health Check Handlers ───────────────────────────────────────────────────

func (s *Server) handleHealthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"service": "code-agent",
	})
}

func (s *Server) handleReadyz(c *gin.Context) {
	checks := make(map[string]string)
	allReady := true

	// Check Redis connectivity via session manager
	if err := s.sessionMgr.Ping(c.Request.Context()); err != nil {
		checks["redis"] = "unhealthy: " + err.Error()
		allReady = false
	} else {
		checks["redis"] = "ok"
	}

	// Check PostgreSQL connectivity (if configured)
	if s.pgHealthPing != nil {
		if err := s.pgHealthPing(c.Request.Context()); err != nil {
			checks["postgres"] = "unhealthy: " + err.Error()
			allReady = false
		} else {
			checks["postgres"] = "ok"
		}
	}

	if allReady {
		c.JSON(http.StatusOK, gin.H{"status": "ready", "checks": checks})
	} else {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready", "checks": checks})
	}
}

// ─── Chat Handlers ───────────────────────────────────────────────────────────

// handleChat processes a synchronous chat request and returns the complete response.
// [FIX] ReAct loop uses a detached context so client disconnects don't kill in-flight LLM calls.
func (s *Server) handleChat(c *gin.Context) {
	var req models.ChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
		return
	}

	// Validate session_id is present
	sessionID := req.SessionID
	if sessionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session_id is required"})
		return
	}

	// Verify session exists (use request context for quick check)
	if _, err := s.sessionMgr.Get(c.Request.Context(), sessionID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found: " + sessionID})
		return
	}

	// [FIX] Create a detached context for the ReAct loop.
	// Problem: When curl/client disconnects, Gin cancels c.Request.Context(),
	// which kills all in-flight LLM calls mid-ReAct-loop.
	// Solution: Use a background context with its own generous timeout (10 min).
	// The HTTP response will wait, but if client disconnects, the agent continues
	// working and the result is stored in the session for later retrieval.
	agentCtx, agentCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer agentCancel()

	// Use a channel to receive the result so we can also detect client disconnect
	type chatResult struct {
		resp *models.ChatResponse
		err  error
	}
	resultCh := make(chan chatResult, 1)

	// trackInflight registers this goroutine with the Server's shutdown
	// barrier. Without this, a SIGTERM mid-chat would tear the process down
	// while the goroutine was still talking to the LLM, abandoning the
	// session write.
	s.trackInflight(func() {
		resp, err := s.orchestrator.ProcessMessage(agentCtx, sessionID, req.Message, orchestrator.ProcessOptions{OutputFormat: req.OutputFormat})
		resultCh <- chatResult{resp: resp, err: err}
	})

	// Wait for either: agent completes, or client disconnects
	select {
	case result := <-resultCh:
		// Agent finished — return response to client
		if result.err != nil {
			s.logger.Error("chat processing failed", zap.Error(result.err))
			c.JSON(http.StatusInternalServerError, gin.H{"error": "processing failed: " + result.err.Error()})
			return
		}
		result.resp.SessionID = sessionID
		c.JSON(http.StatusOK, result.resp)

	case <-c.Request.Context().Done():
		// Client disconnected — agent continues in background, result stored in session
		s.logger.Info("client disconnected, agent continues in background",
			zap.String("session_id", sessionID))
		// Don't cancel agentCtx — let the goroutine finish and store result in session
		// The result will be available via GET /sessions/:id or next chat message
	}
}

// handleChatStream processes a chat request with real Server-Sent Events (SSE) streaming.
// [OPT-1] Now uses ProcessMessageStream for true token-by-token streaming instead of
// buffering the full response.
func (s *Server) handleChatStream(c *gin.Context) {
	var req models.ChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
		return
	}

	sessionID := req.SessionID
	if sessionID == "" {
		sess, err := s.sessionMgr.Create(c.Request.Context(), session.AnonymousUserID, "")
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create session"})
			return
		}
		sessionID = sess.ID
	}

	// Set SSE headers
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("Transfer-Encoding", "chunked")

	// Send session ID event
	s.sendSSEEvent(c, models.StreamEvent{
		Type: "session",
		Data: json.RawMessage(fmt.Sprintf(`{"session_id":"%s"}`, sessionID)),
	})

	// [OPT-1] Use real streaming via ProcessMessageStream
	streamCh, err := s.orchestrator.ProcessMessageStream(c.Request.Context(), sessionID, req.Message)
	if err != nil {
		s.sendSSEEvent(c, models.StreamEvent{
			Type: "error",
			Data: json.RawMessage(fmt.Sprintf(`{"error":"%s"}`, err.Error())),
		})
		return
	}

	// Stream chunks as SSE events in real-time
	for chunk := range streamCh {
		if chunk.Err != nil {
			errData, _ := json.Marshal(map[string]string{"error": chunk.Err.Error()})
			s.sendSSEEvent(c, models.StreamEvent{Type: "error", Data: errData})
			return
		}
		if chunk.Content != "" {
			msgData, _ := json.Marshal(map[string]string{"content": chunk.Content})
			s.sendSSEEvent(c, models.StreamEvent{Type: "message", Data: msgData})
		}
		if chunk.Done {
			break
		}
	}

	// Send done event
	s.sendSSEEvent(c, models.StreamEvent{
		Type: "done",
		Data: json.RawMessage(`{}`),
	})
}

// sendSSEEvent writes a single SSE event to the response writer.
func (s *Server) sendSSEEvent(c *gin.Context, event models.StreamEvent) {
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	c.SSEvent("message", string(data))
	c.Writer.Flush()
}

// handleChatReactStream processes a chat request using the full ReAct loop with SSE streaming.
// It emits structured events for each step: intent parsing, thinking, tool calls, tool results, and final answer.
// This gives the frontend complete visibility into the agent's reasoning process.
//
// 心跳设计（2026-06-04）：
//
//	server.write_timeout=600s 是兜底硬上限，任何 ≥600s 无写出的 SSE 连接都会被 net/http 撕掉。
//	长 ReAct 任务（大型 run_tests / sandbox 编译）可能合法地 60+s 无业务事件，此前会被
//	write_timeout 误杀。本处加入 25s 心跳 ping —— 用 SSE 注释行 `: ping\n\n`（标准格式，
//	EventSource 与 fetch reader 都不会把它当 data 投递给上层），让 idle 期间也保证每 25s
//	至少一次写入；如此 write_timeout 在合法长任务中永远不可能触发。
//	与业务事件共享 writeMu 串行写入，避免与 c.SSEvent 的内部多次 Write 交错。
//	参考前例：internal/mcp/transport_sse.go:257-259 用 90s 心跳节奏。
func (s *Server) handleChatReactStream(c *gin.Context) {
	var req models.ChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
		return
	}

	sessionID := req.SessionID
	if sessionID == "" {
		sess, err := s.sessionMgr.Create(c.Request.Context(), session.AnonymousUserID, "")
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create session"})
			return
		}
		sessionID = sess.ID
	}

	// Set SSE headers
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("Transfer-Encoding", "chunked")

	var writeMu sync.Mutex
	sendEvent := func(event models.StreamEvent) {
		writeMu.Lock()
		defer writeMu.Unlock()
		s.sendSSEEvent(c, event)
	}

	// Send session ID event
	sessData, _ := json.Marshal(map[string]string{"session_id": sessionID})
	sendEvent(models.StreamEvent{Type: "session", Data: sessData})

	// Use full ReAct streaming
	eventCh, err := s.orchestrator.ProcessMessageStreamFull(c.Request.Context(), sessionID, req.Message, orchestrator.ProcessOptions{OutputFormat: req.OutputFormat})
	if err != nil {
		errData, _ := json.Marshal(map[string]string{"error": err.Error()})
		sendEvent(models.StreamEvent{Type: "error", Data: errData})
		return
	}

	// Heartbeat goroutine: writes SSE comment line every sseHeartbeatInterval while the stream is alive.
	pingCtx, pingCancel := context.WithCancel(c.Request.Context())
	defer pingCancel()
	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		runSSEHeartbeat(pingCtx, c.Writer, &writeMu, sseHeartbeatInterval)
	}()

	// Stream each ReAct event as an SSE event
	for event := range eventCh {
		eventData, err := json.Marshal(event)
		if err != nil {
			continue
		}
		sendEvent(models.StreamEvent{
			Type:   event.Type,
			Data:   eventData,
			TaskID: event.TaskID,
		})
	}

	// 关闭心跳并等待其退出，避免在 handler 返回后 ticker goroutine 还在写已被 gin 释放的 writer。
	pingCancel()
	<-pingDone
}

// handleChatReactStreamStatus 返回 sessionID 当前是否有 ReAct 任务在跑。
// 前端在刷新页面恢复 session 后调用：若 running=true → 立刻打开 /resume SSE
// 拉历史 + 跟随增量；若 running=false 但 event_count>0 → 任务在断流期间跑完了，
// 仍需 /resume 把 cache 里的 tail Replay 给客户端。
//
//	GET /api/v1/chat/react-stream/status?session_id=<id>
//	200 {"running": false, "event_count": 0,                  "last_event_at_ms": 0}
//	200 {"running": false, "event_count": 924, "last_event_at_ms": 17178...}  —— 已完成,有 cache 可回放
//	200 {"running": true,  "task_id": "<uuid>", "event_count": N, "last_event_at_ms": 17178...}
//
// last_event_at_ms 是 Redis Stream 最末一条事件的写入毫秒戳。前端在 watchdog
// 触发前可据此判断"后端是否真活着":若 running=true 但 last_event_at_ms 已
// 停滞数分钟,后端八成卡在 finalize LLM 调用上,UI 应给出更友好提示。
func (s *Server) handleChatReactStreamStatus(c *gin.Context) {
	sessionID := c.Query("session_id")
	if sessionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session_id query parameter required"})
		return
	}
	cache := s.orchestrator.StreamCache()
	if cache == nil {
		c.JSON(http.StatusOK, gin.H{"running": false, "event_count": 0, "last_event_at_ms": 0})
		return
	}
	ctx := c.Request.Context()
	taskID, running := cache.Status(ctx, sessionID)
	eventCount := cache.EventCount(ctx, sessionID)
	lastEventAtMs := cache.LastEventAt(ctx, sessionID)
	resp := gin.H{
		"running":          running,
		"event_count":      eventCount,
		"last_event_at_ms": lastEventAtMs,
	}
	if running {
		resp["task_id"] = taskID
	}
	c.JSON(http.StatusOK, resp)
}

// handleChatReactStreamResume 重新建立 SSE 流：先 Replay 历史 Stream 事件，再
// 用 Follow 跟随增量直到任务完成或客户端断开。
//
//	GET /api/v1/chat/react-stream/resume?session_id=<id>
//	→ text/event-stream，事件格式与 /chat/react-stream 完全一致：session / step_start /
//	  thinking / tool_call / tool_result / message / done / error
//
// 即使 session 没有在跑任务（已经 MarkDone），resume 仍然会把已有事件 Replay
// 一遍——这是有意的：让客户端在「我以为还在跑、其实刚好完成」的赛跑窗口里
// 仍然能补到尾巴。Replay 完后 Follow 立刻发现 status 不存在，drain tail 后退出。
func (s *Server) handleChatReactStreamResume(c *gin.Context) {
	sessionID := c.Query("session_id")
	if sessionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session_id query parameter required"})
		return
	}
	cache := s.orchestrator.StreamCache()
	if cache == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "stream cache disabled"})
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("Transfer-Encoding", "chunked")

	var writeMu sync.Mutex
	sendEvent := func(event models.StreamEvent) {
		writeMu.Lock()
		defer writeMu.Unlock()
		s.sendSSEEvent(c, event)
	}

	sessData, _ := json.Marshal(map[string]string{"session_id": sessionID})
	sendEvent(models.StreamEvent{Type: "session", Data: sessData})

	streamReplayFollow(c.Request.Context(), cache, sessionID, sendEvent, c.Writer, &writeMu)
}

// streamReplayFollow 是 handleChatReactStreamResume 的核心逻辑，独立出来纯粹
// 为可测性：它只依赖 StreamCacheReplayer 接口（足以 stub）+ sendEvent 闭包，
// 不绑 *gin.Context，可以在测试里直接驱动而无需构造完整 Orchestrator。
//
// 终态兜底：Replay 历史与 Follow 增量任一段缺 done/error 都会让前端 reader
// 永远不收到结束信号，UI spinner 卡到 90s watchdog 才降级。本函数对两段都
// 主动检查 hasTerminal，必要时 emit 一条合成 done。
func streamReplayFollow(
	ctx context.Context,
	cache streamCacheReplayer,
	sessionID string,
	sendEvent func(models.StreamEvent),
	writer sseFlushWriter,
	writeMu *sync.Mutex,
) {
	// 1) Replay 历史
	historyCtx, historyCancel := context.WithTimeout(ctx, 10*time.Second)
	history, lastID, err := cache.Replay(historyCtx, sessionID)
	historyCancel()
	if err != nil {
		errData, _ := json.Marshal(map[string]string{"error": err.Error()})
		sendEvent(models.StreamEvent{Type: "error", Data: errData})
		return
	}
	hasTerminal := false
	for _, ev := range history {
		data, mErr := json.Marshal(ev)
		if mErr != nil {
			continue
		}
		sendEvent(models.StreamEvent{Type: ev.Type, Data: data, TaskID: ev.TaskID})
		if ev.Type == "done" || ev.Type == "error" {
			hasTerminal = true
		}
	}

	// 如果 status 已经 NOT running，直接结束 —— history 应已含 done。
	if _, running := cache.Status(ctx, sessionID); !running {
		if !hasTerminal {
			emitSyntheticDone(sendEvent, "synthesized_after_replay")
		}
		return
	}

	// 2) 心跳 + Follow 增量（heartbeat 仅在 writer 非 nil 时启动 —— 测试可跳过）
	var pingDone chan struct{}
	pingCtx, pingCancel := context.WithCancel(ctx)
	defer pingCancel()
	if writer != nil && writeMu != nil {
		pingDone = make(chan struct{})
		go func() {
			defer close(pingDone)
			runSSEHeartbeat(pingCtx, writer, writeMu, sseHeartbeatInterval)
		}()
	}

	for ev := range cache.Follow(ctx, sessionID, lastID) {
		data, mErr := json.Marshal(ev)
		if mErr != nil {
			continue
		}
		sendEvent(models.StreamEvent{Type: ev.Type, Data: data, TaskID: ev.TaskID})
		if ev.Type == "done" || ev.Type == "error" {
			hasTerminal = true
		}
	}
	pingCancel()
	if pingDone != nil {
		<-pingDone
	}

	// Follow 退出后再兜底一次：Follow 已 drainTail 但 stream 末尾仍可能因 cache
	// 截断 / 写入顺序竞态而缺终态。若客户端 ctx 还活着，补一条 done 让 UI 收尾。
	if ctx.Err() == nil && !hasTerminal {
		emitSyntheticDone(sendEvent, "synthesized_after_follow")
	}
}

// streamCacheReplayer 抽象 StreamCache 的三个方法，仅供 streamReplayFollow 使用 ——
// 解耦测试 stub 与真 cache。生产路径用 *orchestrator.StreamCache 满足该接口。
type streamCacheReplayer interface {
	Replay(ctx context.Context, sessionID string) ([]models.ReactStreamEvent, string, error)
	Status(ctx context.Context, sessionID string) (string, bool)
	Follow(ctx context.Context, sessionID, lastID string) <-chan models.ReactStreamEvent
}

// emitSyntheticDone 在 Replay/Follow 都没碰到 done/error 时主动合成一条 done。
// reason 出现在 SSE data 字段里，方便前端 console 与后端日志关联排查。
// 不增加日志噪音 —— 走「兜底成功」路径时不需要 warn。
func emitSyntheticDone(send func(models.StreamEvent), reason string) {
	payload, _ := json.Marshal(map[string]string{"reason": reason})
	send(models.StreamEvent{Type: "done", Data: payload})
}

// sseHeartbeatInterval 控制 /chat/react-stream 心跳周期。write_timeout=600s 提供 24x 余量。
// 与 internal/mcp/transport_sse.go 的 90s 心跳节奏对齐风格但更短，因为业务 SSE 直连前端，
// 我们对网络空闲的容忍要远低于 MCP 内部传输。
const sseHeartbeatInterval = 25 * time.Second

// sseFlushWriter 是 runSSEHeartbeat 需要的最小写入接口：能写字节并能 flush。
// gin.ResponseWriter 天然满足；测试里可以传入任意实现该接口的 ResponseRecorder 包装。
type sseFlushWriter interface {
	Write(p []byte) (int, error)
	Flush()
}

// runSSEHeartbeat 周期性地向 w 写出业务 SSE 事件 `data: {"type":"ping","ts":N}\n\n`,
// 直至 ctx 取消。每次写入通过 mu 与业务事件串行化,避免与 c.SSEvent 的多次 Write 交错。
//
// P1 改造:从 SSE 注释行 `: ping\n\n` 改为业务事件 — 注释行虽然能保 net/http write_timeout
// 计时器复位,但前端 `consumeReactStream` 的 `data:` 行解析才触发 onByte 回调,
// 注释行被 fetch reader 透传却不调用业务回调,导致前端 90s 静默 watchdog 误判超时。
// 业务级 ping 事件强制走 onByte → 重置 lastEventAt,前端类型 union 已扩 "ping",
// traceSteps switch 默认 drop 该类型不入 UI。
func runSSEHeartbeat(ctx context.Context, w sseFlushWriter, mu *sync.Mutex, interval time.Duration) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			payload := fmt.Sprintf("data: {\"type\":\"ping\",\"ts\":%d}\n\n", time.Now().UnixMilli())
			mu.Lock()
			_, _ = w.Write([]byte(payload))
			w.Flush()
			mu.Unlock()
		}
	}
}

// ─── Session Handlers ────────────────────────────────────────────────────────

func (s *Server) handleCreateSession(c *gin.Context) {
	var req struct {
		UserID    string `json:"user_id"`
		ProjectID string `json:"project_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		req.UserID = session.AnonymousUserID
	}

	sess, err := s.sessionMgr.Create(c.Request.Context(), req.UserID, req.ProjectID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create session"})
		return
	}

	// Auto-create a workspace for this session
	var workspaceID string
	if s.workspaceMgr != nil {
		var ws *workspace.Workspace
		
		// If project_id is a valid directory path, use it as the workspace root
		if req.ProjectID != "" {
			if _, err := os.Stat(req.ProjectID); err == nil {
				// Project path exists, create workspace from project directory
				ws, err = s.workspaceMgr.CreateFromProject(sess.ID, sess.ID, req.ProjectID)
				if err != nil {
					s.logger.Warn("failed to create workspace from project", 
						zap.String("session_id", sess.ID), 
						zap.String("project_path", req.ProjectID), 
						zap.Error(err))
				}
			}
		}
		
		// Fall back to isolated workspace if no valid project path
		if ws == nil {
			ws, err = s.workspaceMgr.CreateForSession(sess.ID, sess.ID, "session-"+sess.ID[:8])
			if err != nil {
				s.logger.Warn("failed to create workspace for session", zap.String("session_id", sess.ID), zap.Error(err))
			}
		}
		
		if ws != nil {
			workspaceID = ws.ID
		}
	}

	c.JSON(http.StatusCreated, gin.H{
		"session_id":   sess.ID,
		"workspace_id": workspaceID,
		"created_at":   sess.CreatedAt,
	})
}

// handleGetSessionWorkspace returns the workspace associated with a session.
func (s *Server) handleGetSessionWorkspace(c *gin.Context) {
	sessionID := c.Param("id")

	if s.workspaceMgr == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "workspace manager not available"})
		return
	}

	// Look up workspace by session ID
	ws, ok := s.workspaceMgr.GetBySession(sessionID)
	if !ok {
		// Auto-create one if missing (for sessions created before this feature)
		var err error
		ws, err = s.workspaceMgr.CreateForSession(sessionID, sessionID, "session-"+sessionID[:8])
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create workspace: " + err.Error()})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"workspace_id": ws.ID,
		"session_id":   ws.SessionID,
		"project":      ws.Project,
		"created_at":   ws.CreatedAt,
	})
}

func (s *Server) handleGetSession(c *gin.Context) {
	sessionID := c.Param("id")

	sess, err := s.sessionMgr.Get(c.Request.Context(), sessionID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}

	c.JSON(http.StatusOK, sess)
}

// handleListSessions returns the lightweight session list for the sidebar,
// ordered by most-recent activity. Reads `user_id` from query string and
// defaults to AnonymousUserID (matches session.sessionIndexKey's normalization,
// so anonymous sessions created without auth are still listable).
func (s *Server) handleListSessions(c *gin.Context) {
	userID := c.Query("user_id")
	if userID == "" {
		userID = session.AnonymousUserID
	}

	limit := 50
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}

	summaries, err := s.sessionMgr.ListSessions(c.Request.Context(), userID, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list sessions: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"user_id":  userID,
		"sessions": summaries,
	})
}

func (s *Server) handleDeleteSession(c *gin.Context) {
	sessionID := c.Param("id")

	if err := s.sessionMgr.Delete(c.Request.Context(), sessionID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete session"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}

// ─── HITL Approval Handler ───────────────────────────────────────────────────

func (s *Server) handleApproval(c *gin.Context) {
	taskID := c.Param("id")

	var req models.ApprovalResponse
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	req.TaskID = taskID

	resp, err := s.orchestrator.HandleApproval(c.Request.Context(), req)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, resp)
}

// handleInterrupt sends an interrupt signal to a running session's ReAct loop.
func (s *Server) handleInterrupt(c *gin.Context) {
	sessionID := c.Param("session_id")
	if sessionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session_id is required"})
		return
	}

	var req struct {
		Type       string `json:"type" binding:"required"`
		NewMessage string `json:"new_message,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
		return
	}

	signal := orchestrator.InterruptSignal{
		Type:       orchestrator.InterruptType(req.Type),
		NewMessage: req.NewMessage,
	}

	if ok := s.orchestrator.InterruptSession(sessionID, signal); !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "no active task for session"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "interrupt_ack", "session_id": sessionID})
}

// ─── WebSocket Handler ───────────────────────────────────────────────────────

// [OPT-12] WebSocket origin validation. In production, configure AllowedOrigins
// via environment or config to prevent cross-site WebSocket hijacking.
var allowedOrigins = map[string]bool{
	"http://localhost:3000":  true,
	"http://localhost:5173":  true,
	"http://localhost:8080":  true,
	"https://localhost:3000": true,
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true // Allow non-browser clients (curl, etc.)
		}
		// Allow configured origins
		if allowedOrigins[origin] {
			return true
		}
		// Allow same-origin requests
		if origin == "http://"+r.Host || origin == "https://"+r.Host {
			return true
		}
		return false
	},
}

// handleWebSocket upgrades HTTP to WebSocket for bidirectional real-time communication.
func (s *Server) handleWebSocket(c *gin.Context) {
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		s.logger.Error("websocket upgrade failed", zap.Error(err))
		return
	}
	defer conn.Close()

	s.logger.Info("websocket connection established", zap.String("remote", conn.RemoteAddr().String()))

	// Create a session for this WebSocket connection
	sess, err := s.sessionMgr.Create(c.Request.Context(), "ws-user", "")
	if err != nil {
		s.logger.Error("failed to create session for websocket", zap.Error(err))
		return
	}

	// Send session info
	_ = conn.WriteJSON(gin.H{
		"type":       "connected",
		"session_id": sess.ID,
	})

	// Message processing loop
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				s.logger.Warn("websocket closed unexpectedly", zap.Error(err))
			}
			break
		}

		// Parse incoming message
		var wsMsg struct {
			Type    string          `json:"type"`
			Message string          `json:"message,omitempty"`
			Data    json.RawMessage `json:"data,omitempty"`
		}
		if err := json.Unmarshal(message, &wsMsg); err != nil {
			_ = conn.WriteJSON(gin.H{"type": "error", "error": "invalid message format"})
			continue
		}

		switch wsMsg.Type {
		case "chat":
			// Process chat message
			resp, err := s.orchestrator.ProcessMessage(c.Request.Context(), sess.ID, wsMsg.Message)
			if err != nil {
				_ = conn.WriteJSON(gin.H{"type": "error", "error": err.Error()})
				continue
			}
			_ = conn.WriteJSON(gin.H{
				"type":    "response",
				"task_id": resp.TaskID,
				"message": resp.Message,
				"state":   resp.State,
			})

			// Send approval request if needed
			if resp.Approval != nil {
				_ = conn.WriteJSON(gin.H{
					"type":     "approval_required",
					"approval": resp.Approval,
				})
			}

		case "approve":
			var approval models.ApprovalResponse
			if err := json.Unmarshal(wsMsg.Data, &approval); err != nil {
				_ = conn.WriteJSON(gin.H{"type": "error", "error": "invalid approval data"})
				continue
			}
			resp, err := s.orchestrator.HandleApproval(c.Request.Context(), approval)
			if err != nil {
				_ = conn.WriteJSON(gin.H{"type": "error", "error": err.Error()})
				continue
			}
			_ = conn.WriteJSON(gin.H{
				"type":    "approval_response",
				"task_id": resp.TaskID,
				"message": resp.Message,
				"state":   resp.State,
			})

		default:
			_ = conn.WriteJSON(gin.H{"type": "error", "error": "unknown message type"})
		}
	}
}

// ─── HMAC-Protected Webhook Handlers ─────────────────────────────────────────
// These endpoints receive callbacks from external systems (MCP servers, CI/CD
// pipelines) and are protected by HMAC-SHA256 signature verification (§3).

// handleWebhookMCPCallback processes authenticated callbacks from MCP servers.
func (s *Server) handleWebhookMCPCallback(c *gin.Context) {
	var payload struct {
		TaskID   string          `json:"task_id"`
		ServerID string          `json:"server_id"`
		Result   json.RawMessage `json:"result"`
		Status   string          `json:"status"`
	}
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payload"})
		return
	}

	s.logger.Info("MCP callback received (HMAC verified)",
		zap.String("task_id", payload.TaskID),
		zap.String("server_id", payload.ServerID),
		zap.String("status", payload.Status),
	)

	// In production, route the result back to the corresponding Temporal workflow
	c.JSON(http.StatusOK, gin.H{"status": "accepted"})
}

// handleWebhookCICallback processes authenticated callbacks from CI/CD pipelines.
func (s *Server) handleWebhookCICallback(c *gin.Context) {
	var payload struct {
		PipelineID string `json:"pipeline_id"`
		Status     string `json:"status"`
		CommitSHA  string `json:"commit_sha"`
		LogURL     string `json:"log_url,omitempty"`
	}
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payload"})
		return
	}

	s.logger.Info("CI/CD callback received (HMAC verified)",
		zap.String("pipeline_id", payload.PipelineID),
		zap.String("status", payload.Status),
		zap.String("commit", payload.CommitSHA),
	)

	c.JSON(http.StatusOK, gin.H{"status": "accepted"})
}

// ─── Indexer Handler ─────────────────────────────────────────────────────────
// [OPT-8] Expose repository indexing as an API endpoint.

func (s *Server) handleIndexRepository(c *gin.Context) {
	var req struct {
		RepoPath    string `json:"repo_path" binding:"required"`
		ProjectName string `json:"project_name" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
		return
	}

	s.logger.Info("repository indexing requested",
		zap.String("repo_path", req.RepoPath),
		zap.String("project", req.ProjectName),
	)

	// [OPT-21] If indexer is wired, trigger real async indexing via goroutine.
	if s.indexer != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			result, err := s.indexer.IndexRepositoryAny(ctx, req.RepoPath, req.ProjectName)
			if err != nil {
				s.logger.Error("background indexing failed",
					zap.String("repo_path", req.RepoPath), zap.Error(err))
				return
			}
			s.logger.Info("background indexing complete",
				zap.String("repo_path", req.RepoPath), zap.Any("stats", result))
		}()
	}

	c.JSON(http.StatusAccepted, gin.H{
		"status":  "indexing_started",
		"path":    req.RepoPath,
		"project": req.ProjectName,
	})
}
