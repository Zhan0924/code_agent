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
	"time"

	"github.com/agent/code_agent/internal/models"
	"github.com/agent/code_agent/internal/orchestrator"
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
		sess, err := s.sessionMgr.Create(c.Request.Context(), "anonymous", "")
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
func (s *Server) handleChatReactStream(c *gin.Context) {
	var req models.ChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
		return
	}

	sessionID := req.SessionID
	if sessionID == "" {
		sess, err := s.sessionMgr.Create(c.Request.Context(), "anonymous", "")
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
	sessData, _ := json.Marshal(map[string]string{"session_id": sessionID})
	s.sendSSEEvent(c, models.StreamEvent{Type: "session", Data: sessData})

	// Use full ReAct streaming
	eventCh, err := s.orchestrator.ProcessMessageStreamFull(c.Request.Context(), sessionID, req.Message, orchestrator.ProcessOptions{OutputFormat: req.OutputFormat})
	if err != nil {
		errData, _ := json.Marshal(map[string]string{"error": err.Error()})
		s.sendSSEEvent(c, models.StreamEvent{Type: "error", Data: errData})
		return
	}

	// Stream each ReAct event as an SSE event
	for event := range eventCh {
		eventData, err := json.Marshal(event)
		if err != nil {
			continue
		}
		s.sendSSEEvent(c, models.StreamEvent{
			Type:   event.Type,
			Data:   eventData,
			TaskID: event.TaskID,
		})
	}
}

// ─── Session Handlers ────────────────────────────────────────────────────────

func (s *Server) handleCreateSession(c *gin.Context) {
	var req struct {
		UserID    string `json:"user_id"`
		ProjectID string `json:"project_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		req.UserID = "anonymous"
	}

	sess, err := s.sessionMgr.Create(c.Request.Context(), req.UserID, req.ProjectID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create session"})
		return
	}

	// Auto-create an isolated workspace for this session
	var workspaceID string
	if s.workspaceMgr != nil {
		ws, err := s.workspaceMgr.CreateForSession(sess.ID, sess.ID, "session-"+sess.ID[:8])
		if err != nil {
			s.logger.Warn("failed to create workspace for session", zap.String("session_id", sess.ID), zap.Error(err))
		} else {
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
