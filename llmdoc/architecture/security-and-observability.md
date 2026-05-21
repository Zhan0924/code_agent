# 安全与可观测性

覆盖认证、安全防御、限流、可观测性（指标/追踪/审计）的完整架构。

## 认证

### JWT

`internal/auth/jwt.go` (`JWTManager`)：

- 算法：HS256
- Claims：`sub`, `user_id`, `role`, `email`, `iss`, `iat`, `exp`, `jti`（格式 `userID-unixNano`）
- 角色：`admin`, `dev`, `readonly`, `service`
- 默认过期：Token 24h, Refresh 7d
- 撤销：内存 map + 可选 Redis（`RedisRevocationStore`）

`JWTManagerWithRedis` (`internal/auth/redis_revocation.go`)：包装 `JWTManager`，覆盖 `ValidateToken` 同时检查 Redis。`RevokeToken` 同时写入内存和 Redis。Redis key `jwt:revoked:{jti}`，TTL = token 剩余生命周期。

### API Key

`internal/auth/jwt.go` (`APIKeyStore`)：

- 存储 SHA-256 哈希（不保存明文）
- `Validate()` 遍历所有条目，`subtle.ConstantTimeCompare`（timing-safe，位置无关）
- `APIKeyEntry`：Key（仅注册时）, UserID, Role, Label, Created

### Auth 中间件

`AuthMiddleware(jwtMgr, apiKeys, logger)`：优先检查 `X-API-Key` header，其次 `Authorization: Bearer`。注入 `Claims` 到 gin context（key `"auth_claims"`）。

`RequireRole(roles ...Role)`：admin 始终通过，否则检查 claim 角色是否在允许集合中。

### Token 端点

`POST /api/v1/auth/token`：需要 `X-Admin-Secret` header 或 admin JWT claims。验证角色枚举。注册在独立路由组（绕过 v1 auth）。

## HMAC Webhook 验证

`internal/security/hmac.go` (`HMACVerifier`)：

- HMAC-SHA256 + `hmac.Equal`（constant-time 比较）
- 签名 header：`X-Signature-256`（可配置），前缀 `sha256=`
- 时间戳防重放：`X-Timestamp` header，±5 分钟窗口（同时拒绝过旧和过未来）
- Body 大小限制：1MB
- `GinMiddleware()`：4 步验证（提取签名 → 验证时间戳 → 读取 body → 验证 HMAC），body 重放给下游 handler

出站签名：`SignPayload(payload)` 生成签名。`SigningTransport` 作为 `http.RoundTripper` 自动签名请求 body + 注入时间戳 header。

## Egress ACL（出站访问控制）

`internal/security/egress.go` (`EgressValidator`)：

### 策略配置

- `DefaultEgressPolicy()`：deny-all，阻止云元数据（169.254.169.254, 100.100.100.200）+ 所有 RFC1918
- `InternalServiceEgressPolicy()`：允许 10.0.0.0/8，仍阻止元数据
- 规则：blocked CIDRs 优先于 allowed

### 双层 HTTP 强制执行

**为什么需要两层**：单纯的 URL 级检查无法防御 DNS rebinding 攻击（域名在 DNS 检查后解析到内部 IP）。

| 层 | 机制 | 防御目标 |
|---|------|----------|
| L1 — `EgressTransport` | `http.RoundTripper`，URL 级 host 检查（DNS 前） | 已知恶意域名 |
| L2 — `NewEgressHTTPClient` | `net.Dialer.Control` 回调，检查解析后 IP（DNS 后、connect(2) 前） | DNS rebinding |

### 容器网络

- `GenerateIptablesRules()`：生成容器 network namespace 的 iptables 规则
- `DockerNetworkMode()`：全隔离→"none"，部分→"code-agent-sandbox"，禁用→"bridge"

## 限流

两种实现，当前使用进程内版本：

### 进程内 Token Bucket（活跃）

`internal/api/middleware.go` (`rateLimiterMiddleware`)：

- 按客户端 IP 限流
- 默认：10 rps，burst 20
- 清理：每 5 分钟移除过期条目

### Redis 固定窗口（可用未接线）

`internal/auth/redis_ratelimit.go` (`RedisRateLimiter`)：

- Lua 脚本原子 INCR + EXPIRE
- Key 格式：`<prefix>:<bucket>:<windowStart>`
- Bucket 优先级：user_id > API-key-hash > IP
- Redis 故障时 fail-open
- `GinMiddleware()` 可直接替换 `rateLimiterMiddleware`，但需手动修改 `setupMiddleware`

## 敏感内容检测

`internal/orchestrator/orchestrator.go` (`containsSensitiveContent`, line 1447)：

- 遍历 `o.sensitiveRules`（`[]*regexp.Regexp`），首次匹配返回 true
- 规则在 `NewOrchestrator()` 从 `securityCfg.SensitivePatterns` 编译，统一添加 `(?i)` 标志

默认模式（`configs/config.yaml:107-114`）：

```
DROP\s+DATABASE, DROP\s+TABLE, DELETE\s+FROM\s+\w+\s*$,
kubectl\s+delete, kubectl\s+apply, rm\s+-rf\s+/, TRUNCATE
```

匹配时触发 HITL 审批流程（`suspendForApproval()`）。

## Prometheus 指标

`internal/metrics/` 使用 `promauto` 自动注册。命名空间：`code_agent`。

### 核心指标

| 指标 | 类型 | 标签 | 子系统 |
|------|------|------|--------|
| `api_request_total` | CounterVec | method, path, status | api |
| `api_request_duration_seconds` | HistogramVec | method, path | api |
| `api_websocket_connections` | Gauge | — | api |
| `llm_request_total` | CounterVec | provider, model, status | llm |
| `llm_request_duration_seconds` | HistogramVec | provider, model | llm |
| `llm_tokens_used_total` | CounterVec | provider, type | llm |
| `llm_circuit_breaker_state` | GaugeVec | provider | llm |
| `llm_fallback_total` | Counter | — | llm |
| `rag_retrieval_duration_seconds` | Histogram | — | rag |
| `rag_chunks_returned` | Histogram | — | rag |
| `pruner_tokens_saved_total` | Counter | — | pruner |
| `pruner_chunks_pruned_total` | Counter | — | pruner |
| `session_active_count` | Gauge | — | session |
| `session_cold_archive_total` | Counter | — | session |
| `session_context_compression_total` | Counter | — | session |
| `sandbox_execution_total` | CounterVec | language, status | sandbox |
| `sandbox_execution_duration_seconds` | HistogramVec | language | sandbox |
| `mcp_call_total` | CounterVec | server, tool, status | mcp |
| `mcp_call_duration_seconds` | HistogramVec | server | mcp |
| `hitl_approval_total` | CounterVec | decision | hitl |
| `hitl_pending_count` | Gauge | — | hitl |
| `prompt_cache_prefix_hash` | GaugeVec | hash | prompt |

### 成本追踪

| 指标 | 类型 | 标签 |
|------|------|------|
| `LLMCostUSD` | CounterVec | model, tier, session_id, user_id, task_id |
| `ToolExecutionDuration` | HistogramVec | tool, source, status |
| `ToolExecutionTotal` | CounterVec | tool, source, status |
| `PlannerStepsTotal` / `PlannerRevisionTotal` / `PlannerPlansCreated` | Counter | — |

`LLMCostUSD` 有意高基数（按 session/user/task 维度），用于企业成本归因。

`EstimateCostUSD(model, inputTokens, outputTokens)`：从硬编码 `PricePerModel` map 查询（gpt-4o, claude-3-5-sonnet, deepseek-coder 等）。

## OTel 追踪

`internal/tracing/otel.go` (`Provider`)：

- 导出器：OTLP gRPC（默认 `localhost:4317`）
- 采样：`SampleRate` 配置（默认 0.1），支持 AlwaysSample / NeverSample / TraceIDRatioBased
- 传播：W3C TraceContext + Baggage
- `GinMiddleware(serviceName)`：提取父 span，创建 server span `"METHOD /route"`，记录 `http.request.method`, `url.path`, `client.address`, `http.response.status_code`, `http.response.body.size`。5xx 设置 error status

## 审计日志

`internal/audit/logger.go` (`Logger`)：

- 结构化 zap 日志，`log_type=audit` 字段标记
- 事件类型：`approval_requested`, `approval_granted`, `approval_denied`, `approval_timeout`, `sandbox_execution`, `mcp_tool_call`, `sensitive_blocked`, `session_created`, `session_deleted`, `indexing_started`
- `Event` 结构：Timestamp, Type, SessionID, TaskID, UserID, Action, Details(map), IP, Success, Error
- 便捷方法：`LogApproval`, `LogSandboxExec`, `LogMCPCall`

**限制**：审计日志是 zap 包装器，无独立持久化层。`store` 包有 `audit_logs` 表，但两者未关联——审计事件未写入 PostgreSQL。输出目标由 zap 输出配置决定（文件/Elasticsearch/SIEM）。
