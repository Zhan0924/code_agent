// Package api provides the HTTP API layer using Gin framework,
// including REST endpoints, WebSocket, and Server-Sent Events (SSE).
//
// ============================================================================
//
//	设 计 原 理
//
// ============================================================================
//
// 【这层的职责边界】
//
//	api 包**只做**：请求解析、中间件装配、业务转发、响应序列化。
//	**不做**：业务逻辑（都在 orchestrator）、鉴权算法（auth 包）、
//	熔断（llm 包）、限流算法本身（auth/ratelimit*）。
//
// 【中间件顺序（栈，外到内）】
//
//	recovery → requestID → tracing → metrics → rateLimit → logging → CORS
//	  → (认证组) authMW → (RBAC 组) RequireRole → handler
//
//	顺序很关键：
//	  · recovery 必须最外层，所有 panic 都能兜底；
//	  · requestID 在最外层打，整个请求生命周期共用同一个 ID；
//	  · metrics 要看到"最终"的 status code，所以必须在认证之后；
//	  · rateLimit 比认证更外是故意的：rl 失败不应消耗 auth 的 Redis 查询预算；
//	  · logging 最内，handler 已处理完后精确记录 status。
//
// 【分组策略】
//
//	/healthz /readyz /metrics — 完全匿名、裸路由，方便 K8s probe 和 LB；
//	/api/v1                   — AuthMiddleware 包住（authEnabled=true 时）；
//	  /auth/token             — AuthMW 豁免（鸡生蛋）；
//	  /chat* /sessions        — 需鉴权，但任意 role 可用；
//	  /tasks/:id/approve      — RequireRole(Admin, Dev)；
//	  /mcp/servers* /skills*  — RequireRole(Admin, Dev)；
//	  /webhooks/*             — HMAC 验签代替 JWT；
//	/api/v1/debug             — 运维口，生产应网关屏蔽。
//
// 【晚绑定 Setter 们】
//
//	NewServer 只需要最少的必备依赖；MCP/Skill/Indexer/Generator/Workspace
//	在构造之后通过 Set 注入。理由见 cmd/agent/main.go 的注释。
//
// ============================================================================
package api

import (
	"context"
	"net/http"

	"github.com/agent/code_agent/internal/auth"
	"github.com/agent/code_agent/internal/mcp"
	"github.com/agent/code_agent/internal/models"
	"github.com/agent/code_agent/internal/orchestrator"
	"github.com/agent/code_agent/internal/security"
	"github.com/agent/code_agent/internal/session"
	"github.com/agent/code_agent/internal/skill"
	"github.com/agent/code_agent/internal/tracing"
	"github.com/agent/code_agent/internal/workspace"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

// Server 聚合所有 handler 会用到的依赖。把业务依赖（orchestrator / sessionMgr）
// 与横切组件（metrics / auth / hmac）放到一起，handler 通过方法接收者访问。
//
// 并发性：Server 本身一旦构造完就是只读的，所有字段都是 goroutine-safe 组件。
// Gin 的 engine 本身允许并发处理，每个请求一个 goroutine。
type Server struct {
	router        *gin.Engine
	orchestrator  *orchestrator.Orchestrator
	sessionMgr    *session.Manager
	workspaceMgr  *workspace.Manager // 工作区文件管理（浏览器/编辑器）
	indexer       Indexer            // [OPT-21] RAG 仓库索引触发器（可选）
	generator     ProjectGenerator   // 多阶段项目生成
	mcpGateway    *mcp.Gateway
	skillRegistry *skill.Registry
	hmac          *security.HMACVerifier
	jwtMgr        *auth.JWTManager
	apiKeys       *auth.APIKeyStore
	authEnabled   bool
	pgHealthPing  func(ctx context.Context) error // 可选 PG 健康检查
	p0            *P0Probes                       // [P0-debug] 可观测探针（可选）
	logger        *zap.Logger
}

// Indexer 是仓库索引接口，独立定义以避免 api 包反向依赖 indexer 包。
// 具体实现是 indexer.Indexer.IndexRepositoryAny。用 interface{} 返回值
// 是因为 IndexResult 类型定义在 indexer 包，这里避免把它暴露上来。
type Indexer interface {
	IndexRepositoryAny(ctx context.Context, repoPath, projectName string) (interface{}, error)
}

// ServerOptions holds optional dependencies for the API server.
type ServerOptions struct {
	JWTManager   *auth.JWTManager
	APIKeyStore  *auth.APIKeyStore
	AuthEnabled  bool
	PGHealthPing func(ctx context.Context) error // PostgreSQL health check (store.Ping)
}

// NewServer creates a new API server with all routes registered.
func NewServer(
	orch *orchestrator.Orchestrator,
	sessionMgr *session.Manager,
	logger *zap.Logger,
	opts ...ServerOptions,
) *Server {
	gin.SetMode(gin.ReleaseMode)

	// Initialize HMAC verifier for webhook endpoints (§3 security)
	hmacVerifier := security.NewHMACVerifier(security.DefaultHMACConfig(), logger)

	s := &Server{
		router:       gin.New(),
		orchestrator: orch,
		sessionMgr:   sessionMgr,
		hmac:         hmacVerifier,
		logger:       logger.With(zap.String("component", "api")),
	}

	// Apply optional configuration
	if len(opts) > 0 {
		if opts[0].AuthEnabled {
			s.jwtMgr = opts[0].JWTManager
			s.apiKeys = opts[0].APIKeyStore
			s.authEnabled = opts[0].AuthEnabled
		}
		s.pgHealthPing = opts[0].PGHealthPing
	}

	s.setupMiddleware(logger)
	s.setupRoutes()

	return s
}

// SetMCPGateway injects the MCP gateway for dynamic server management.
func (s *Server) SetMCPGateway(gw *mcp.Gateway) {
	s.mcpGateway = gw
}

// SetSkillRegistry injects the skill registry for dynamic skill management.
func (s *Server) SetSkillRegistry(sr *skill.Registry) {
	s.skillRegistry = sr
}

// GetAvailableTools returns all tools from MCP + Skills (used by handlers).
func (s *Server) GetAvailableTools() []models.ToolDefinition {
	var tools []models.ToolDefinition
	if s.mcpGateway != nil {
		tools = append(tools, s.mcpGateway.GetAvailableTools()...)
	}
	if s.skillRegistry != nil {
		tools = append(tools, s.skillRegistry.GetToolDefinitions()...)
	}
	return tools
}

// SetIndexer injects an optional Indexer into the server for repository indexing.
// [OPT-21] This allows the API to actually trigger indexing rather than returning a stub.
func (s *Server) SetIndexer(idx Indexer) {
	s.indexer = idx
}

// Handler returns the underlying http.Handler for use with custom servers.
func (s *Server) Handler() http.Handler {
	return s.router
}

// setupMiddleware configures global middleware with observability and security.
func (s *Server) setupMiddleware(logger *zap.Logger) {
	rl := newRateLimiter(DefaultRateLimiterConfig(), logger)

	s.router.Use(recoveryMiddleware(logger))
	s.router.Use(requestIDMiddleware())
	s.router.Use(tracing.GinMiddleware("code-agent"))
	s.router.Use(metricsMiddleware())
	s.router.Use(rateLimiterMiddleware(rl))
	s.router.Use(s.loggingMiddleware())
	s.router.Use(s.corsMiddleware())
}

// setupRoutes registers all API routes.
func (s *Server) setupRoutes() {
	// Health check & observability
	s.router.GET("/healthz", s.handleHealthz)
	s.router.GET("/readyz", s.handleReadyz)
	s.router.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// API v1 group
	v1 := s.router.Group("/api/v1")

	// Apply auth middleware if enabled
	if s.authEnabled && s.jwtMgr != nil {
		v1.Use(auth.AuthMiddleware(s.jwtMgr, s.apiKeys, s.logger))

		// Auth management endpoints (public - no auth required)
		authGroup := s.router.Group("/api/v1/auth")
		authGroup.POST("/token", s.handleGenerateToken)
	}

	{
		// Chat endpoints
		v1.POST("/chat", s.handleChat)
		v1.POST("/chat/stream", s.handleChatStream)
		v1.POST("/chat/react-stream", s.handleChatReactStream)
		v1.POST("/chat/:session_id/interrupt", s.handleInterrupt)

		// Session management
		v1.POST("/sessions", s.handleCreateSession)
		v1.GET("/sessions/:id", s.handleGetSession)
		v1.GET("/sessions/:id/workspace", s.handleGetSessionWorkspace)
		v1.DELETE("/sessions/:id", s.handleDeleteSession)

		// HITL approval (requires admin or dev role)
		approvalGroup := v1.Group("")
		if s.authEnabled {
			approvalGroup.Use(auth.RequireRole(auth.RoleAdmin, auth.RoleDev))
		}
		approvalGroup.POST("/tasks/:id/approve", s.handleApproval)

		// [OPT-8] Repository indexing (requires admin or dev role)
		indexGroup := v1.Group("")
		if s.authEnabled {
			indexGroup.Use(auth.RequireRole(auth.RoleAdmin, auth.RoleDev))
		}
		indexGroup.POST("/index", s.handleIndexRepository)

		// Project generation endpoints
		projectGroup := v1.Group("/projects")
		if s.authEnabled {
			projectGroup.Use(auth.RequireRole(auth.RoleAdmin, auth.RoleDev))
		}
		projectGroup.POST("/generate", s.handleGenerateProject)
		projectGroup.POST("/generate/stream", s.handleGenerateProjectSSE)
		projectGroup.GET("/:id/status", s.handleGetProjectStatus)

		// ─── MCP Server Management ───────────────────────────────────
		mcpGroup := v1.Group("/mcp")
		if s.authEnabled {
			mcpGroup.Use(auth.RequireRole(auth.RoleAdmin, auth.RoleDev))
		}
		mcpGroup.POST("/servers", s.handleAddMCPServer)
		mcpGroup.DELETE("/servers/:name", s.handleRemoveMCPServer)
		mcpGroup.GET("/servers", s.handleListMCPServers)

		// ─── Skill Management ────────────────────────────────────────
		skillGroup := v1.Group("/skills")
		if s.authEnabled {
			skillGroup.Use(auth.RequireRole(auth.RoleAdmin, auth.RoleDev))
		}
		skillGroup.POST("", s.handleAddSkill)
		skillGroup.DELETE("/:name", s.handleRemoveSkill)
		skillGroup.GET("", s.handleListSkills)

		// ─── Workspace File Browser/Editor ───────────────────────────
		wsGroup := v1.Group("/workspaces")
		wsGroup.GET("", s.handleListWorkspaces)
		wsGroup.GET("/:id/tree", s.handleGetWorkspaceTree)
		wsGroup.GET("/:id/files", s.handleReadWorkspaceFile)
		wsGroup.PUT("/:id/files", s.handleWriteWorkspaceFile)
		wsGroup.DELETE("/:id/files", s.handleDeleteWorkspaceFile)
		wsGroup.POST("/:id/directories", s.handleCreateWorkspaceDir)
		wsGroup.GET("/:id/download", s.handleDownloadWorkspace)

		// ─── Unified Tools List ──────────────────────────────────────
		v1.GET("/tools", s.handleListTools)

		// ─── P0 优化可观测 / 调试端点 ─────────────────────────────────
		// 用于集成测试（端口级）验证 P0-1/2/3/4 在 HTTP 服务运行时生效。
		debugGroup := v1.Group("/debug/p0")
		debugGroup.GET("", s.handleP0Aggregate)
		debugGroup.GET("/schema", s.handleP0SchemaSnapshot)
		debugGroup.GET("/spec-cache", s.handleP0SpecCacheGet)
		debugGroup.POST("/spec-cache", s.handleP0SpecCachePut)
		debugGroup.GET("/spec-cache/query", s.handleP0SpecCacheQuery)

		// WebSocket endpoint for real-time communication
		v1.GET("/ws", s.handleWebSocket)

		// HMAC-protected webhook endpoints (§3 security hardening)
		webhooks := v1.Group("/webhooks")
		webhooks.Use(s.hmac.GinMiddleware())
		{
			webhooks.POST("/mcp-callback", s.handleWebhookMCPCallback)
			webhooks.POST("/ci-callback", s.handleWebhookCICallback)
		}
	}
}

// loggingMiddleware logs each request with structured fields.
func (s *Server) loggingMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()

		s.logger.Info("request",
			zap.Int("status", c.Writer.Status()),
			zap.String("method", c.Request.Method),
			zap.String("path", c.Request.URL.Path),
			zap.String("ip", c.ClientIP()),
		)
	}
}

// corsMiddleware adds CORS headers for cross-origin requests.
// [OPT-29] No longer uses wildcard "*" — reuses the allowedOrigins map from WebSocket
// validation, plus same-origin detection, to prevent credential leakage.
func (s *Server) corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" {
			host := c.Request.Host
			if allowedOrigins[origin] || origin == "http://"+host || origin == "https://"+host {
				c.Header("Access-Control-Allow-Origin", origin)
				c.Header("Access-Control-Allow-Credentials", "true")
			}
			// If origin not allowed, omit CORS headers → browser blocks the request.
		}
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}
