# Code Agent API Test Document

> 完整的 HTTP API 测试指南 — 覆盖每个端点的请求格式、响应格式、验证通过的测试用例、以及已修复的安全边界。
>
> 测试栈：`docker-compose.test.yml` 下的 `code-agent:p0-fixes` 镜像，监听 `http://localhost:28080`。
> 鉴权：默认 `auth.enabled=false`，全部 `/api/v1/*` 可匿名访问；开启鉴权后走 `Authorization: Bearer <jwt>` 或 `X-API-Key: <key>`。
>
> 文档约定：所有 `curl` 例子都可直接粘贴运行。`HTTP XXX` 行是实测响应状态码。

---

## 目录

1. [环境准备](#1-环境准备)
2. [健康检查与可观测性](#2-健康检查与可观测性)
3. [会话管理](#3-会话管理)
4. [聊天（同步 / SSE 流 / ReAct 流）](#4-聊天)
5. [工作区文件操作](#5-工作区文件操作)
6. [RAG 索引](#6-rag-索引)
7. [项目生成](#7-项目生成)
8. [MCP 服务器管理](#8-mcp-服务器管理)
9. [技能（Skill）管理](#9-技能skill管理)
10. [HITL 任务审批](#10-hitl-任务审批)
11. [HMAC Webhook（含 P0 #5 回归测试）](#11-hmac-webhook)
12. [鉴权 Token](#12-鉴权-token)
13. [Debug / 诊断端点](#13-debug--诊断端点)
14. [端到端测试剧本](#14-端到端测试剧本)
15. [压力与回归测试](#15-压力与回归测试)

---

## 1. 环境准备

### 1.1 启动测试栈

```bash
cd code_agent
docker compose -f docker-compose.test.yml up -d
# 等待 healthy
docker ps --filter "name=-p0" --format "{{.Names}} {{.Status}}"
# 期望输出：
# agent-p0   Up 10 seconds (healthy)
# redis-p0   Up 15 seconds (healthy)
# qdrant-p0  Up 15 seconds
```

### 1.2 环境变量

```bash
export BASE=http://localhost:28080
export CONTENT="Content-Type: application/json"
```

### 1.3 快速连通性自检

```bash
curl -sS $BASE/healthz
# 期望：(空响应体，HTTP 200)
```

---

## 2. 健康检查与可观测性

### 2.1 `GET /healthz` — 存活探针

| 字段 | 值 |
|---|---|
| 用途 | K8s liveness probe、LB 心跳 |
| 鉴权 | 不需要 |
| 依赖 | 无 |
| 成功码 | 200 |

**请求**
```bash
curl -sS -w "HTTP %{http_code}\n" $BASE/healthz
```

**响应**
```
HTTP 200
```

### 2.2 `GET /readyz` — 就绪探针

检查下游依赖是否可用（当前仅检查 Redis，未来扩展 Qdrant / Postgres）。

**请求**
```bash
curl -sS -w "\nHTTP %{http_code}\n" $BASE/readyz
```

**成功响应**
```json
{"checks":{"redis":"ok"},"status":"ready"}
HTTP 200
```

**失败响应（Redis 断开时）**
```json
{"checks":{"redis":"error: connection refused"},"status":"not_ready"}
HTTP 503
```

### 2.3 `GET /metrics` — Prometheus 指标

返回标准 Prometheus 文本格式。

**请求**
```bash
curl -sS $BASE/metrics | head -20
```

**核心指标**
| 指标 | 类型 | 标签 | 用途 |
|---|---|---|---|
| `code_agent_api_request_total` | Counter | `method,path,status` | API 调用量 |
| `code_agent_api_request_duration_seconds` | Histogram | `method,path` | 延迟分布 |
| `code_agent_llm_request_total` | Counter | `provider,model,status` | LLM 调用 |
| `code_agent_llm_tokens_used_total` | Counter | `provider,kind` | Token 消耗 |
| `code_agent_llm_circuit_breaker_state` | Gauge | `provider` | 熔断器状态 0/1/2 |
| `code_agent_session_active_count` | Gauge | — | 活跃会话数 |

---

## 3. 会话管理

### 3.1 `POST /api/v1/sessions` — 创建会话

每次调用生成新的 UUID 会话 + 对应独立 workspace（**P0 #15 修复后**：绝不会共享其他租户的 workspace）。

**请求**
```bash
curl -sS -X POST $BASE/api/v1/sessions -H "$CONTENT" -d '{}'
```

**可选字段**
```json
{ "user_id": "alice" }
```

**响应 (201)**
```json
{
  "session_id": "a4563d6a-fe06-430f-940a-5b5846d059a9",
  "workspace_id": "a4563d6a-fe06-430f-940a-5b5846d059a9",
  "created_at": "2026-05-01T13:17:59.600561171Z"
}
```

### 3.2 `GET /api/v1/sessions/:id` — 获取会话

**请求**
```bash
SID=a4563d6a-fe06-430f-940a-5b5846d059a9
curl -sS -w "\nHTTP %{http_code}\n" $BASE/api/v1/sessions/$SID
```

**响应 (200)**
```json
{
  "id": "a4563d6a-fe06-430f-940a-5b5846d059a9",
  "user_id": "",
  "messages": [
    {"role":"user","content":"..."},
    {"role":"assistant","content":"..."}
  ],
  "created_at": "2026-05-01T13:17:59.600561171Z",
  "updated_at": "2026-05-01T13:18:03.298Z"
}
```

**不存在 (404)**
```bash
curl -sS -w "\nHTTP %{http_code}\n" $BASE/api/v1/sessions/nonexistent
# {"error":"session not found"}
# HTTP 404
```

### 3.3 `GET /api/v1/sessions/:id/workspace` — 会话关联的 workspace

**请求 / 响应 (200)**
```json
{
  "workspace_id": "a4563d6a-...",
  "root_dir": "/tmp/agent-workspaces/a4563d6a-...",
  "project_name": "session-a4563d6a"
}
```

### 3.4 `DELETE /api/v1/sessions/:id` — 删除会话

**请求**
```bash
curl -sS -w "\nHTTP %{http_code}\n" -X DELETE $BASE/api/v1/sessions/$SID
```

**响应 (200)**
```json
{"status":"deleted"}
```

---

## 4. 聊天

### 4.1 `POST /api/v1/chat` — 同步聊天

阻塞调用，完整响应一次性返回。**LLM 调用通过熔断器**（P0 #21 可选接入跨副本熔断器）。

**请求 Schema**
```json
{
  "session_id": "<existing session_id>",
  "message": "user message text",
  "stream": false,
  "context": {}
}
```

**请求**
```bash
curl --max-time 30 -sS -X POST $BASE/api/v1/chat -H "$CONTENT" -d "{
  \"session_id\":\"$SID\",
  \"message\":\"what is 2+2? just answer with a number.\"
}"
```

**响应 (200)**
```json
{
  "session_id": "a4563d6a-...",
  "task_id": "5a72abe5-...",
  "message": "4",
  "state": "completed"
}
```

**需要人工审批 (200, state=pending)**
```json
{
  "session_id": "...",
  "task_id": "...",
  "message": "",
  "state": "pending_approval",
  "approval_required": true,
  "approval_reason": "Deployment operation requires approval"
}
```

**触发条件**：消息命中 `security.sensitive_patterns`（如 `DROP DATABASE`、`kubectl apply`、`rm -rf /`）或 intent 分类为 `deploy`。
**解除**：`POST /api/v1/tasks/:task_id/approve`。

### 4.2 `POST /api/v1/chat/stream` — 简单 SSE 流

返回 token-level SSE 流。

**请求**
```bash
curl -N -sS -X POST $BASE/api/v1/chat/stream -H "$CONTENT" -d "{
  \"session_id\":\"$SID\",
  \"message\":\"count from 1 to 3\"
}"
```

**响应 (text/event-stream)**
```
data: {"content":"1","done":false}

data: {"content":", 2","done":false}

data: {"content":", 3","done":false}

data: {"done":true}
```

### 4.3 `POST /api/v1/chat/react-stream` — ReAct 完整流

同上，但包含 intent、thinking、tool_call、tool_result 所有中间步骤。前端主要用这个。

**响应 event 类型**
```
event: intent
data: {"intent":"code_query"}

event: thinking
data: {"content":"Let me search the repo for..."}

event: tool_call
data: {"name":"search_code","arguments":{"query":"authentication"}}

event: tool_result
data: {"name":"search_code","output":"Found 3 matches in ..."}

event: final
data: {"content":"The authentication flow is..."}

event: done
data: {}
```

---

## 5. 工作区文件操作

### 5.1 `GET /api/v1/workspaces` — 列出所有 workspace

```bash
curl -sS $BASE/api/v1/workspaces
```

**响应 (200)**
```json
[
  {
    "id": "a4563d6a-...",
    "session_id": "a4563d6a-...",
    "root_dir": "/tmp/agent-workspaces/a4563d6a-...",
    "project_name": "session-a4563d6a",
    "created_at": "2026-05-01T13:17:59Z"
  }
]
```

### 5.2 `GET /api/v1/workspaces/:id/tree` — 目录树

```bash
curl -sS $BASE/api/v1/workspaces/$SID/tree
```

**响应 (200)**
```json
{
  "workspace_id": "a4563d6a-...",
  "project": "session-a4563d6a",
  "tree": [
    {"name":"hello.py","path":"hello.py","type":"file","size":22},
    {"name":"src","path":"src","type":"dir","size":0,
      "children":[{"name":"main.go","path":"src/main.go","type":"file","size":128}]}
  ]
}
```

### 5.3 `PUT /api/v1/workspaces/:id/files` — 写文件

```bash
curl -sS -X PUT "$BASE/api/v1/workspaces/$SID/files" -H "$CONTENT" -d '{
  "path":"hello.py",
  "content":"print(\"hi from test\")\n"
}'
```

**响应 (200)**
```json
{"message":"file saved","path":"hello.py","size":22}
```

**路径穿越拒绝 (400)**
```bash
curl -sS -X PUT "$BASE/api/v1/workspaces/$SID/files" -H "$CONTENT" -d '{
  "path":"../etc/passwd",
  "content":"x"
}'
# {"error":"path traversal not allowed"}
```

### 5.4 `GET /api/v1/workspaces/:id/files?path=...` — 读文件

```bash
curl -sS "$BASE/api/v1/workspaces/$SID/files?path=hello.py"
```

**响应 (200)**
```json
{
  "path": "hello.py",
  "content": "print(\"hi from test\")\n",
  "language": "python",
  "size": 22
}
```

### 5.5 `DELETE /api/v1/workspaces/:id/files?path=...`

```bash
curl -sS -X DELETE "$BASE/api/v1/workspaces/$SID/files?path=hello.py"
# {"status":"deleted","path":"hello.py"}
```

### 5.6 `POST /api/v1/workspaces/:id/directories` — 创建目录

```bash
curl -sS -X POST "$BASE/api/v1/workspaces/$SID/directories" -H "$CONTENT" -d '{"path":"src/internal"}'
# {"status":"created","path":"src/internal"}
```

### 5.7 `GET /api/v1/workspaces/:id/download` — 打包下载

返回 tar.gz 流，直接写到文件。

```bash
curl -sS -o workspace.tar.gz "$BASE/api/v1/workspaces/$SID/download"
tar tzf workspace.tar.gz | head
```

---

## 6. RAG 索引

### 6.1 `POST /api/v1/index` — 异步索引仓库

索引一个本地目录；后台跑 AST 切块、embedding、写入 Qdrant，以及**（P0 #18 修复）构建 BM25 稀疏索引**。

**请求**
```bash
curl -sS -w "\nHTTP %{http_code}\n" -X POST $BASE/api/v1/index -H "$CONTENT" -d "{
  \"repo_path\":\"/tmp/agent-workspaces/$SID\",
  \"project_name\":\"smoketest\"
}"
```

**响应 (202 Accepted)**
```json
{
  "status": "indexing_started",
  "path": "/tmp/agent-workspaces/a4563d6a-...",
  "project": "smoketest"
}
```

**参数校验失败 (400)**
```json
{"error":"invalid request: Key: 'RepoPath' Error:Field validation for 'RepoPath' failed on the 'required' tag"}
```

**索引完成日志**（tail 容器日志可见）
```
"msg":"repository indexing complete","indexed":5,"skipped":0,"errors":0,"duration":2.3
```

> 📝 当 `cfg.RAG.EmbeddingBaseURL` 未配置时，**P1 #20 修复后**会正确 fallback 到 LLM primary 的 BaseURL/APIKey。修复前会把字面量 `${...}` 当 URL 导致 `unsupported protocol scheme`。

---

## 7. 项目生成

### 7.1 `POST /api/v1/projects/generate` — 同步多阶段生成

阶段：Blueprint → Scaffold → Implementation → Validation → Polish。大任务可能要几分钟。

**请求**
```bash
curl --max-time 300 -sS -X POST $BASE/api/v1/projects/generate -H "$CONTENT" -d '{
  "prompt": "build a simple TODO REST API in Go with in-memory storage",
  "language": "go",
  "options": { "with_tests": true }
}'
```

**响应 (200)**
```json
{
  "project_id": "proj-abc123",
  "status": "completed",
  "workspace_id": "proj-abc123",
  "summary": "Generated 7 files (3 tests), all validation passed",
  "files_generated": 7,
  "duration_seconds": 92.4
}
```

### 7.2 `POST /api/v1/projects/generate/stream` — SSE 流式生成

SSE 事件类型：`blueprint`, `scaffold_file`, `impl_file`, `validation`, `polish`, `done`.

### 7.3 `GET /api/v1/projects/:id/status` — 查询进度

```bash
curl -sS $BASE/api/v1/projects/proj-abc123/status
```

```json
{
  "project_id": "proj-abc123",
  "phase": "implementation",
  "progress": 0.62,
  "files_so_far": 4,
  "started_at": "..."
}
```

---

## 8. MCP 服务器管理

### 8.1 `POST /api/v1/mcp/servers` — 注册 MCP 服务器

```bash
curl -sS -X POST $BASE/api/v1/mcp/servers -H "$CONTENT" -d '{
  "name": "github",
  "transport": "stdio",
  "command": "npx",
  "args": ["-y", "@modelcontextprotocol/server-github"],
  "env": { "GITHUB_TOKEN": "ghp_xxx" }
}'
```

**响应 (201)**
```json
{"status":"registered","name":"github","tools":["create_issue","list_repos",...]}
```

### 8.2 `GET /api/v1/mcp/servers` — 列表

```bash
curl -sS $BASE/api/v1/mcp/servers
```

**响应 (200)**
```json
[
  {"name":"github","transport":"stdio","status":"healthy","tool_count":12},
  {"name":"jira","transport":"sse","status":"degraded"}
]
```

### 8.3 `DELETE /api/v1/mcp/servers/:name`

```bash
curl -sS -X DELETE $BASE/api/v1/mcp/servers/github
# {"status":"removed","name":"github"}
```

---

## 9. 技能（Skill）管理

### 9.1 `POST /api/v1/skills` — 添加技能

```bash
curl -sS -X POST $BASE/api/v1/skills -H "$CONTENT" -d '{
  "name": "security_review",
  "description": "Review code for security vulnerabilities",
  "instructions": "Focus on OWASP top 10...",
  "parameters": { "type":"object","properties":{"code":{"type":"string"}} }
}'
```

### 9.2 `GET /api/v1/skills` / `DELETE /api/v1/skills/:name`

同样的 CRUD 模式。

---

## 10. HITL 任务审批

### 10.1 `POST /api/v1/tasks/:id/approve` — 批准/拒绝

**请求**
```bash
TASK_ID=5a72abe5-...
curl -sS -X POST $BASE/api/v1/tasks/$TASK_ID/approve -H "$CONTENT" -d '{
  "approved": true,
  "reason": "reviewed by oncall",
  "approver": "alice"
}'
```

**响应 (200)**
```json
{"status":"approved","task_id":"5a72abe5-...","resumed":true}
```

**拒绝**
```json
{"approved": false, "reason": "this is production, not allowed"}
```

---

## 11. HMAC Webhook

⚠️ **本节每个用例对应 P0 #5 的回归测试 — 修复前的"缺 timestamp 即放行"漏洞必须复发为失败。**

### 11.1 测试用例矩阵

| 用例 | 签名头 | Timestamp 头 | 期望状态 | 期望错误 |
|---|---|---|---|---|
| A 完全无头 | ❌ | ❌ | **401** | `missing signature header: X-Signature-256` |
| B 有签名无 ts | ✅ 任意 | ❌ | **401** | `missing timestamp header: X-Timestamp` ⚠️ P0 #5 |
| C 签名+过期 ts | ✅ deadbeef | 2020-01-01 | **401** | `request timestamp expired or skewed` |
| D 签名+未来 ts | ✅ deadbeef | 未来 1h | **401** | `request timestamp expired or skewed` |
| E 签名+ts 格式错 | ✅ deadbeef | `not-a-ts` | **400** | `invalid timestamp format` |
| F 有效 ts + 错签名 | ✅ deadbeef | now | **403** | `invalid HMAC signature` |
| G 有效 ts + 正确签名 | ✅ 计算 | now | **200** | 业务响应 |

### 11.2 用例 A — 无任何头（401）

```bash
curl -sSo /tmp/r -w "HTTP %{http_code}\n" \
  -X POST $BASE/api/v1/webhooks/mcp-callback \
  -H "$CONTENT" -d '{"event":"ping"}'
cat /tmp/r
```
**验证通过**：
```
HTTP 401
{"error":"missing signature header: X-Signature-256"}
```

### 11.3 用例 B — 有签名无 timestamp（P0 #5 关键回归）

```bash
curl -sSo /tmp/r -w "HTTP %{http_code}\n" \
  -X POST $BASE/api/v1/webhooks/mcp-callback \
  -H "$CONTENT" \
  -H 'X-Signature-256: sha256=deadbeef' \
  -d '{"event":"ping"}'
cat /tmp/r
```
**验证通过**：
```
HTTP 401
{"error":"missing timestamp header: X-Timestamp"}
```

> 修复前的行为：`if tsHeader != ""` 跳过整个 timestamp 校验，只要签名校验失败（必然失败，因为是假签名）才 403。等于缺 timestamp 就把重放保护绕过了一半。

### 11.4 用例 C — 过期 timestamp（401）

```bash
curl -sSo /tmp/r -w "HTTP %{http_code}\n" \
  -X POST $BASE/api/v1/webhooks/mcp-callback \
  -H "$CONTENT" \
  -H 'X-Signature-256: sha256=deadbeef' \
  -H 'X-Timestamp: 2020-01-01T00:00:00Z' \
  -d '{"event":"ping"}'
```
**验证通过**：
```
HTTP 401
{"error":"request timestamp expired or skewed"}
```

### 11.5 用例 D — 未来时间戳（防时钟欺骗）

```bash
FUTURE=$(date -u -v+1H +"%Y-%m-%dT%H:%M:%SZ")   # macOS
# FUTURE=$(date -u -d "+1 hour" +"%Y-%m-%dT%H:%M:%SZ") # Linux
curl -sSo /tmp/r -w "HTTP %{http_code}\n" \
  -X POST $BASE/api/v1/webhooks/mcp-callback \
  -H "$CONTENT" \
  -H 'X-Signature-256: sha256=deadbeef' \
  -H "X-Timestamp: $FUTURE" \
  -d '{"event":"ping"}'
```
**验证通过**：`HTTP 401 request timestamp expired or skewed`

### 11.6 用例 F — 正确 ts + 错签名（403）

```bash
TS=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
curl -sSo /tmp/r -w "HTTP %{http_code}\n" \
  -X POST $BASE/api/v1/webhooks/mcp-callback \
  -H "$CONTENT" \
  -H 'X-Signature-256: sha256=deadbeef' \
  -H "X-Timestamp: $TS" \
  -d '{"event":"ping"}'
```
**验证通过**：`HTTP 403 invalid HMAC signature`

### 11.7 用例 G — 端到端合法请求（200）

```bash
SECRET="your-webhook-secret"   # configured in server
BODY='{"event":"ping","task_id":"abc"}'
TS=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
SIG="sha256=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$SECRET" -hex | awk '{print $2}')"

curl -sS -w "\nHTTP %{http_code}\n" \
  -X POST $BASE/api/v1/webhooks/mcp-callback \
  -H "$CONTENT" \
  -H "X-Signature-256: $SIG" \
  -H "X-Timestamp: $TS" \
  -d "$BODY"
```
**期望**：`HTTP 200 {"status":"accepted"}`.

### 11.8 CI Webhook（同规则）

所有 7 个用例对 `POST /api/v1/webhooks/ci-callback` 都应得到对称的结果，换一个 body schema 即可：
```json
{
  "pipeline_id": "ci-123",
  "status": "success",
  "commit_sha": "abc123",
  "log_url": "https://ci.example.com/log/123"
}
```

---

## 12. 鉴权 Token

### 12.1 `POST /api/v1/auth/token` — 颁发 JWT

```bash
curl -sS -X POST $BASE/api/v1/auth/token -H "$CONTENT" -d '{
  "user_id": "alice",
  "role": "dev"
}'
```

**响应 (200)**
```json
{
  "token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIi...",
  "expires_in": 86400
}
```

### 12.2 使用 Token

```bash
TOKEN=$(curl -sS -X POST $BASE/api/v1/auth/token -H "$CONTENT" \
  -d '{"user_id":"alice","role":"dev"}' | jq -r .token)

curl -sS -H "Authorization: Bearer $TOKEN" $BASE/api/v1/workspaces
```

### 12.3 API Key（服务账号）

```bash
curl -sS -H "X-API-Key: my-service-key" $BASE/api/v1/workspaces
```

> ℹ️ **P0 #4 修复后**：API Key 以 SHA-256 哈希存储，Validate 用 `subtle.ConstantTimeCompare` 做常量时间比较，避免侧信道。仓库不再保留 plaintext。

### 12.4 鉴权失败

```bash
# 禁用时端点全开（auth.enabled=false）
# 启用后：
curl -sS -w "\nHTTP %{http_code}\n" $BASE/api/v1/workspaces
# HTTP 401  {"error":"authorization token missing"}

curl -sS -w "\nHTTP %{http_code}\n" -H "Authorization: Bearer bad" $BASE/api/v1/workspaces
# HTTP 401  {"error":"token is invalid"}
```

---

## 13. Debug / 诊断端点

仅用于内部运维，生产环境建议通过网关屏蔽。

### 13.1 `GET /api/v1/debug/p0` — P0 状态总览

```bash
curl -sS $BASE/api/v1/debug/p0
```

**响应（摘要）**
```json
{
  "schema": {"enabled":true,"generation":3,"etag":"abc","tool_count":12},
  "spec_cache": {"enabled":true,"hits":45,"misses":12,"bypass":8,"hit_rate":0.79}
}
```

### 13.2 `GET /api/v1/debug/p0/spec-cache` — 缓存统计

```bash
curl -sS $BASE/api/v1/debug/p0/spec-cache
# {"hits":45,"misses":12,"bypass":8,"hit_rate":0.79}
```

### 13.3 `POST /api/v1/debug/p0/spec-cache` — 手动注入 / `GET .../query` — 查询

仅白名单工具（`read_file`、`grep`、`git_status` 等幂等工具）可缓存。

---

## 14. 端到端测试剧本

### 14.1 完整用户流程

```bash
set -e
BASE=http://localhost:28080

# 1. 建会话
SID=$(curl -sS -X POST $BASE/api/v1/sessions -H 'Content-Type: application/json' \
      -d '{}' | jq -r .session_id)
echo "session: $SID"

# 2. 写文件
curl -sS -X PUT "$BASE/api/v1/workspaces/$SID/files" \
  -H 'Content-Type: application/json' \
  -d '{"path":"hello.go","content":"package main\n\nfunc main() { println(\"hi\") }\n"}' | jq

# 3. 聊天：让 agent 解释 workspace 里的代码
curl --max-time 60 -sS -X POST $BASE/api/v1/chat \
  -H 'Content-Type: application/json' \
  -d "{\"session_id\":\"$SID\",\"message\":\"in one sentence, what does hello.go print?\"}" | jq

# 4. 查看 session messages
curl -sS "$BASE/api/v1/sessions/$SID" | jq '.messages | length'

# 5. 打包下载
curl -sS -o /tmp/ws.tar.gz "$BASE/api/v1/workspaces/$SID/download"
tar tzf /tmp/ws.tar.gz

# 6. 清理
curl -sS -X DELETE "$BASE/api/v1/sessions/$SID" | jq
```

### 14.2 HITL 审批完整链路

```bash
# 1. 建会话
SID=$(curl -sS -X POST $BASE/api/v1/sessions -H 'Content-Type: application/json' -d '{}' | jq -r .session_id)

# 2. 发一个敏感指令（触发 HITL）
RESP=$(curl --max-time 60 -sS -X POST $BASE/api/v1/chat -H 'Content-Type: application/json' \
  -d "{\"session_id\":\"$SID\",\"message\":\"run kubectl apply -f manifests/prod.yaml\"}")
echo "$RESP" | jq
TASK=$(echo "$RESP" | jq -r .task_id)
STATE=$(echo "$RESP" | jq -r .state)
echo "state=$STATE task=$TASK"
# 期望：state=pending_approval

# 3. 批准
curl -sS -X POST "$BASE/api/v1/tasks/$TASK/approve" -H 'Content-Type: application/json' \
  -d '{"approved":true,"approver":"alice","reason":"scheduled maintenance"}' | jq

# 4. 重新拉取会话（应看到 agent 已执行后的消息）
curl -sS "$BASE/api/v1/sessions/$SID" | jq '.messages[-1]'
```

---

## 15. 压力与回归测试

### 15.1 简单并发

```bash
SID=$(curl -sS -X POST $BASE/api/v1/sessions -d '{}' -H 'Content-Type: application/json' | jq -r .session_id)

# 并发写 20 个文件 — 验证 EditEngine per-path 锁 + workspace 线程安全 (P0 #13)
for i in $(seq 1 20); do
  curl -sS -X PUT "$BASE/api/v1/workspaces/$SID/files" \
    -H 'Content-Type: application/json' \
    -d "{\"path\":\"f_$i.txt\",\"content\":\"content $i\"}" &
done
wait

# 应看到 20 个文件
curl -sS "$BASE/api/v1/workspaces/$SID/tree" | jq '.tree | length'
```

### 15.2 速率限制（配合 P0 #22 修复）

```bash
# 快速发 150 个 /healthz（若启用了 Redis rate limiter，默认 100/min 应有部分 429）
for i in $(seq 1 150); do
  curl -o /dev/null -sS -w "%{http_code} " $BASE/healthz
done
echo
```

**期望**（若配置了 RedisRateLimiter 中间件）：
- 前 100 个 → `200`
- 剩下 50 个 → `429 rate limit exceeded`

### 15.3 P0 修复的实测回归清单

| 修复 | 如何通过 API 验证 | 命令 |
|---|---|---|
| P0 #3 `.gitignore` | — | `ls -la` 检查 repo 根目录 |
| P0 #4 API Key 哈希 | 启动时无 plaintext 泄漏；单测覆盖 | `go test ./internal/auth/ -run NoPlaintextStorage` |
| **P0 #5 HMAC ts 必填** | 第 11 章用例 B | ✅ |
| P0 #6 Egress ACL | 单测覆盖；集成需接线 | `go test ./internal/security/ -run Egress` |
| P0 #8-11 沙箱加固 | 单测 + 容器内沙箱 run | `go test ./internal/sandbox/ -run Hardening` |
| **P0 #12 Intent cache HITL** | 同一 session 先发普通消息再发敏感 → 应走 pending_approval | § 14.2 变体 |
| P0 #13 EditEngine 并发 | § 15.1 | ✅ |
| **P0 #14 spec_cache workspace 作用域** | 多 session 共享 workspace 时 write 能让对方读 miss | 单测 |
| **P0 #15 workspace fallback 泄漏** | 新 session 总建新 workspace，不复用 | § 3.1 |
| P0 #16 diff hunk 算错 | 通过 edit_file 工具 + diff preview 检查 hunk 对齐 | 看 `/metrics` 里 `edit_file_*` |
| **P0 #17 Temporal 占位符** | 启动日志有 `temporal dial failed` 或 `temporal worker started` | `docker logs agent-p0 \| grep temporal` |
| P0 #18 BM25 | 索引后稀疏检索有合理排名 | § 6.1 + RAG 查询 |
| P0 #19 AST parser Go 分派 | 单测 | `go test ./internal/rag/ -run ExtractGo` |
| **P0 #20 Token estimator** | CJK 不再被低估 | 单测 |
| P0 #21 Shared breaker | 连续 LLM 失败后其他副本被跳过 | 注入故障后观察 `llm_request_total{status=error}` |
| P0 #22 Redis rate limiter | § 15.2 |

---

## 附录 A：错误响应统一格式

所有 4xx/5xx 响应遵循：
```json
{
  "error": "<human-readable message>",
  "code": "<machine-readable>",       // 可选，来自 internal/errors
  "request_id": "<uuid>",             // X-Request-ID 头也有
  "details": { ... }                  // 可选
}
```

| HTTP | code | 含义 |
|---|---|---|
| 400 | `INVALID_INPUT` | 参数错误 / JSON 不合法 |
| 401 | `UNAUTHORIZED` / `MISSING_TOKEN` | 未鉴权 |
| 403 | `FORBIDDEN` | 鉴权通过但权限不足（RBAC） |
| 404 | `NOT_FOUND` | 资源不存在 |
| 409 | `CONFLICT` | 重复创建等 |
| 429 | `RATE_LIMITED` | 限流 |
| 500 | `INTERNAL` | 内部错误 |
| 503 | `DEPENDENCY_UNAVAILABLE` | 下游不可用（LLM 熔断、Redis 断） |

## 附录 B：常用调试命令

```bash
# 查看 agent 容器日志
docker logs agent-p0 --tail 100

# 实时 follow
docker logs -f agent-p0

# 只看请求日志
docker logs agent-p0 2>&1 | grep 'msg":"request"'

# 只看 LLM 调用
docker logs agent-p0 2>&1 | grep -iE "llm|openai|anthropic"

# 查看 Prometheus 关键指标
curl -sS $BASE/metrics | grep -E "^code_agent_(llm|api)_"

# Redis 内部状态
docker exec redis-p0 redis-cli info stats
docker exec redis-p0 redis-cli keys 'session:*' | head

# 强制清掉所有会话
docker exec redis-p0 redis-cli flushall
```
