# 动态 MCP & Skill 管理技术方案

## 1. 目标与边界

### 核心目标
让 Code Agent 支持**运行时动态添加/删除/启用/禁用** MCP Server 和 Skill（自定义工具），无需重启服务。新增的 MCP/Skill 立即对 LLM 的 `function_call` 可用。

### 术语定义
| 概念 | 说明 |
|------|------|
| **MCP Server** | 符合 MCP 协议的外部进程，通过 stdio/SSE 通信，提供 `tools/list` 和 `tools/call` |
| **Skill** | 不依赖 MCP 协议的自定义工具，通过 HTTP webhook 或内置 Go 函数实现 |

### 非目标
- 不实现 MCP Server 的编写框架（用户自行编写）
- 不实现 Skill 的可视化编辑器

## 2. 架构设计

```
┌─────────────────────────────────────────────────────┐
│                    REST API Layer                      │
│  POST /api/v1/mcp/servers     (添加 MCP Server)      │
│  DELETE /api/v1/mcp/servers/:name                     │
│  GET /api/v1/mcp/servers      (列出所有 + 状态)       │
│  POST /api/v1/skills          (添加 Skill)            │
│  DELETE /api/v1/skills/:name                          │
│  GET /api/v1/skills           (列出所有)              │
│  GET /api/v1/tools            (列出所有生效的工具)     │
├─────────────────────────────────────────────────────┤
│                MCP Gateway (已有, 需扩展)              │
│  + AddServer(cfg) → connect + init + discover tools   │
│  + RemoveServer(name) → close + cleanup               │
│  + ListServers() → name + status + tool count         │
├─────────────────────────────────────────────────────┤
│                Skill Registry (新增)                   │
│  + RegisterSkill(def) → 存入内存 Map                  │
│  + UnregisterSkill(name)                              │
│  + GetSkills() → []ToolDefinition                     │
│  + ExecuteSkill(name, args) → webhook/function call   │
├─────────────────────────────────────────────────────┤
│            Orchestrator.getAvailableTools()            │
│  = builtin tools + MCP tools + Skill tools            │
│  (每次 LLM 调用时动态组装)                             │
└─────────────────────────────────────────────────────┘
```

## 3. 接口契约

### 3.1 MCP Server CRUD API

```
POST /api/v1/mcp/servers
{
  "name": "github-mcp",
  "transport": "stdio",
  "command": "npx",
  "args": ["-y", "@modelcontextprotocol/server-github"],
  "env": {"GITHUB_TOKEN": "ghp_xxx"}
}
→ 200 {"name":"github-mcp","status":"connected","tools":["create_issue","list_prs",...]}

DELETE /api/v1/mcp/servers/github-mcp
→ 200 {"message":"server disconnected"}

GET /api/v1/mcp/servers
→ 200 [{"name":"github-mcp","status":"connected","tools_count":5,"tools":["..."]}]
```

### 3.2 Skill CRUD API

```
POST /api/v1/skills
{
  "name": "query_prod_db",
  "description": "Execute a read-only SQL query against the production database",
  "parameters": {"type":"object","properties":{"sql":{"type":"string"}},"required":["sql"]},
  "executor": {
    "type": "webhook",
    "url": "https://internal-api.company.com/sql-proxy",
    "method": "POST",
    "headers": {"Authorization": "Bearer xxx"}
  }
}
→ 200 {"name":"query_prod_db","status":"active"}

DELETE /api/v1/skills/query_prod_db
→ 200 {"message":"skill removed"}

GET /api/v1/skills
→ 200 [{"name":"query_prod_db","description":"...","status":"active"}]
```

### 3.3 统一工具列表

```
GET /api/v1/tools
→ 200 [
  {"name":"execute_code","source":"builtin"},
  {"name":"read_file","source":"builtin"},
  {"name":"create_issue","source":"mcp:github-mcp"},
  {"name":"query_prod_db","source":"skill:query_prod_db"}
]
```

## 4. 核心实现要点

### 4.1 MCP Gateway 扩展 (mcp/client.go)
- 新增 `AddServer(cfg *config.MCPServerConfig) error` — 运行时连接新 MCP Server
- 新增 `RemoveServer(name string) error` — 断开连接并清理
- 新增 `ListServers() []ServerStatus` — 返回所有 server 的状态

### 4.2 Skill Registry (新文件: skill/registry.go)
- `SkillDefinition` 包含 name, description, parameters, executor config
- `SkillExecutor` 接口: `Execute(ctx, args) → ToolResult`
- 内置两种 executor:
  - `WebhookExecutor` — 调用外部 HTTP 端点
  - `FunctionExecutor` — 调用 Go 注册的函数（用于内置 skill）

### 4.3 Orchestrator 集成
- `getAvailableTools()` 已经合并 builtin + MCP，只需再加 skill
- `executeTool()` 已有 MCP fallback，只需再加 skill fallback

### 4.4 工具生效机制
工具列表是**每次 LLM 调用时动态组装**的（非缓存），所以：
- 添加 MCP Server → Gateway 发现 tools → 下次 LLM 调用自动包含
- 添加 Skill → Registry 注册 → 下次 LLM 调用自动包含
- 删除同理，立即生效

## 5. 执行清单

- [ ] Step 1: 扩展 Gateway — 添加 AddServer/RemoveServer/ListServers
- [ ] Step 2: 创建 Skill Registry — SkillDefinition + WebhookExecutor
- [ ] Step 3: 集成到 Orchestrator — getAvailableTools + executeTool
- [ ] Step 4: 创建 REST API handlers — MCP CRUD + Skill CRUD + Tools list
- [ ] Step 5: 注册路由到 router.go
- [ ] Step 6: 单元测试
- [ ] Step 7: 构建 + E2E 验证
