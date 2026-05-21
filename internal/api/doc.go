// Package api 基于 Gin 框架实现 Code Agent 的 HTTP/WebSocket/SSE 对外接口。
//
// # 路由分组
//
//	/healthz              —— K8s liveness
//	/readyz               —— K8s readiness (检查 redis/pg/qdrant/llm)
//	/metrics              —— Prometheus 指标
//
//	/api/v1/chat          —— 对话入口（JSON/SSE/WebSocket 三模式自动协商）
//	/api/v1/tasks         —— 任务管理（GET 列表，GET 单任务，POST /approve）
//	/api/v1/workspaces    —— 工作区 CRUD（隔离工作目录）
//	/api/v1/projects      —— 项目级操作（git clone / index / rebuild）
//	/api/v1/mcp           —— MCP server 管理（list / connect / disconnect）
//	/api/v1/skills        —— Skill 聚合视图（builtin + mcp + script）
//	/api/v1/auth          —— 登录/令牌刷新/注销
//
// # 中间件链
//
// 按注册顺序执行（请求方向 ↓，响应方向 ↑）：
//
//  1. Recovery      —— panic 捕获 + 栈堆栈 → 500 JSON
//  2. RequestID     —— 为每个请求分配 uuid，注入 ctx 与响应头
//  3. Tracing       —— OpenTelemetry Start Span（HTTP semantic conventions）
//  4. Metrics       —— Prom Histogram 计时 + Counter 计数
//  5. Logger        —— Zap 结构化访问日志
//  6. CORS          —— 允许前端跨域 (可配置白名单)
//  7. RateLimit     —— Redis 令牌桶（按 IP/User/APIKey 维度）
//  8. Auth          —— JWT / APIKey / HMAC 三选一
//  9. RBAC          —— 基于 role-permission 的资源级检查
//  10. HandlerFunc   —— 业务逻辑
//
// # 三种对话模式
//
//	POST /api/v1/chat
//	  Accept: application/json        → 同步返回完整 JSON
//	  Accept: text/event-stream       → SSE 流式（逐 token/事件推送）
//	  Upgrade: websocket              → WebSocket（双向，支持中断）
//
// # 关键文件
//
//	router.go               —— 路由注册 + 中间件装配 + 优雅关停
//	middleware.go           —— 7 个横切中间件实现
//	handlers.go             —— /chat + /healthz + /readyz 主入口
//	workspace_handlers.go   —— 工作区管理 handler
//	project_handlers.go     —— 项目管理 handler
//	mcp_skill_handlers.go   —— MCP + Skill handler
//	auth_handlers.go        —— 登录/注销/令牌刷新
//	integration_test.go     —— 端到端 HTTP 测试
//
// # 依赖注入
//
// Router 构造时注入所有下游服务（LLM/RAG/Sandbox/Orchestrator/Store/Auth/...），
// 避免全局变量，便于测试与替换实现。
//
// # 错误处理
//
// 所有 handler 返回 errors.AgentError（带 code/status/message/cause），
// 中间件统一序列化为 JSON：
//
//	{
//	  "error": {
//	    "code": "LLM_FAILURE",
//	    "message": "primary provider unreachable, fallback exhausted",
//	    "request_id": "abc-123"
//	  }
//	}
//
// 详见 docs/architecture/17_api.md。
package api
