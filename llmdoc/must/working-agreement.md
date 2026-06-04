# 工作约定

修改本代码库前，必须了解以下约定和陷阱。

## Setter-based DI 模式

`cmd/agent/main.go` 使用 setter 打破循环依赖（例如 Orchestrator 需要 SkillRegistry，而 SkillRegistry 构造需要 Orchestrator 的工具定义）。关键 setter 调用：

- `apiServer.SetIndexer(idx)`
- `orch.SetSkillRegistry(skillReg)` + `apiServer.SetSkillRegistry(skillReg)`
- `apiServer.SetMCPGateway(mcpGateway)`
- `orch.SetWorkspaceManager(wsMgr)` + `apiServer.SetWorkspaceManager(wsMgr)`
- `apiServer.SetGenerator(gen)`

**不变量**：Handler 和 Orchestrator 必须对通过 setter 注入的依赖做 nil 检查，因为这些依赖可能在降级模式下为 nil。

## KV-Cache 友好的 Prompt 结构

`internal/context/prompt_builder.go` (`PromptBuilder`) 按 5 个区域组装 prompt，顺序不可更改：

1. **不可变系统提示** — 静态，永不变，最大化 KV cache 前缀命中
2. **半稳定长期记忆** — 会话摘要，仅在摘要更新时变化
3. **裁剪后代码上下文** — RAG 检索 chunk，经 TokenPruner 多信号评分裁剪
4. **近期对话** — 历史消息，从最近向前填充 token 预算
5. **当前用户消息** — 新输入

`prefixHash`（sha256 of 区域1+2）用于监控 cache 命中率。维护 `prompt_cache_prefix_hash` 指标。

**不变量**：`tools.Registry.Definitions()` 返回排序后的切片以保证确定性。不要在 Definitions 路径中引入 map 遍历。

## 工具分发拆分

工具系统存在两套并行机制：

| 机制 | 位置 | 用途 |
|------|------|------|
| Orchestrator `executeTool()` switch | `internal/orchestrator/orchestrator.go` | ReAct 循环内置工具分发 |
| `tools.Registry` | `internal/tools/registry.go` | API 层暴露的动态工具（MCP、Skill） |

Orchestrator 的 `executeTool()` 优先级：(1) MCP gateway `FindServerForTool()` → (2) 内置 switch（execute_code, read_file, write_file 等）→ (3) skill registry 回退。

**不变量**：核心 ReAct 循环不走 `tools.Registry.Execute()`，而是走 orchestrator 自己的 switch。修改工具分发时需同时考虑两套机制。

**工具名常量化（2026-06-04 PR 8）**：orchestrator 包内的硬编码工具名字符串（原散落 18 个文件）已收敛到 `internal/orchestrator/tool_names.go::Tool*` 常量，新增内置工具应在该文件登记。跨包硬编码（multiagent / agentloop / planner）仍待迁移，见 `llmdoc/memory/doc-gaps.md::TOOL-NAMES`。

### 工具元数据中心化（2026-06 重构）

新增内置工具时，**在工具定义上声明行为 metadata bit 即可**，无需手动同步多处白名单。`models.ToolDefinition` 携带四个行为位：

| Bit | 含义 | 消费点 |
|---|---|---|
| `IsFileWrite` | 写文件类工具 | `Orchestrator.captureForTransaction` / `multiagent.Supervisor.fileWriteClassifier` |
| `IsIdempotentRead` | 幂等纯读 | `SpeculativeToolCache.isIdempotent`（推测缓存白名单） |
| `TriggersAutoTest` | 写完触发 auto-test | `react_core.go` 编辑追踪 |
| `InvalidatesCache` | 写后清缓存 | `SpeculativeToolCache.shouldInvalidate` |

**源头**：
- `internal/orchestrator/file_tools.go::fileToolDefinitions()` — read/write/patch/edit/apply_diff
- `internal/orchestrator/git_tools.go::gitToolDefinitions()` — git_status/diff/commit/log/branch
- `internal/orchestrator/lsp_tools.go::RegisterLSPTools` — goto_definition/find_references/hover_info/rename_symbol

**消费点**（不再硬编码工具名）：
- `Orchestrator.toolMetadata/IsFileWriteTool/IsBuiltinTool/triggersAutoTest` (`tool_metadata.go`)
- `SpeculativeToolCache.SetMetadataLookup`（运行时由 orchestrator 注入注册表查询）
- `multiagent.WithFileWriteClassifier`（main.go 注入 `orch.IsFileWriteTool`）
- `api/dynamic_tool_handlers.go` 用 `s.orchestrator.IsBuiltinTool(name)`
- `api/mcp_skill_handlers.go::handleListTools` 直接遍历 `orchestrator.GetAvailableTools()`

**CI 守卫**：`internal/orchestrator/tool_metadata_test.go::TestBuiltinToolsHaveMetadata` 遍历所有 builtin 定义，要求每个工具至少声明一个 metadata bit（少数纯执行工具如 `run_tests/execute_code` 在 exempt 白名单中）。`TestFileWriteToolsTriggerAutoTest` 校验 `IsFileWrite=true → TriggersAutoTest && InvalidatesCache`。

**遗留**：
- `planner.defaultActions`（`internal/planner/planner.go`）和 `multiagent/sub_agent.allowedTools`、`role_selector.actionScore`——这些是**策略/ACL**，不是工具行为，保持为常量数组。
- `agentloop/runner.go`、`agentloop/adaptive_feedback.go` 内尚有少量硬编码工具名（独立包内的轻量启发式），与本次改动正交。

## 死代码清单（2026-06 大幅更新）

之前文档列出的"死代码"经核实，**大部分已接线**：

| 组件 | 状态 | 接线位置 |
|------|------|---------|
| `llm.Router` | ✅ 已接 | `cmd/agent/main.go::SetRouter`（`cfg.LLM.Router.Enabled()` 触发；默认 nil 跳过） |
| `mcp.ConnPool` | ✅ 已接 | `Gateway.servers` 改为 `map[string]*ConnPool`，`PoolSize<=1` 等价单连接 |
| `toollearn.PGStore` | ✅ 已接 | `main.go::orch.SetToolLearnStore(toollearn.NewPGStore(pgStore.DB()))`，自动 Migrate `tool_feedback` 表 |
| Git 工具 | ✅ 已接 | 通过 `tools.Registry` 走通用分发，LLM 可调用 |
| `LLMSummarizer` | ✅ 已接 | `main.go::sessionMgr.Summarizer = session.NewLLMSummarizer(...)` |
| `SpeculativeToolCache` | ✅ 已接 | Orchestrator 在 `NewOrchestrator` 构造并 `SetMetadataLookup` 给注册表 |
| `RedisRateLimiter` | ✅ 已接 | `api/router.go:190-191` |

**仍然死的**（本表只列"曾在此处声明但已修复"的项；其它跨子系统死代码以 `llmdoc/memory/doc-gaps.md` 为准）：

> ✅ MCP SSE 传输（2026-06）：`internal/mcp/transport_sse.go` 已实现 HTTP+SSE 传输，`dialTransport` 按 `cfg.Transport` 分发；`NewGateway` 不再 skip SSE 配置。详见 `llmdoc/architecture/infrastructure-subsystems.md::传输`。其它未接线项（如 `internal/audit`、`internal/pool` 单一 importer 等）见 `llmdoc/memory/doc-gaps.md::死代码`。

## 双重 Token 估算器

存在两个不同精度的 token 估算器：

- `internal/llm/client.go` (`EstimateTokens`) — rune 分类法，CJK 1 token/rune，ASCII 4 chars/token
- `internal/session/manager.go` (`estimateTokens`) — 朴素 `len(text)/4 + 1`，CJK 低估约 3 倍

Session 层使用精度较低的估算器，可能导致 token 预算计算偏差。

## Docker 部署陷阱

### `docker build` ≠ `docker compose build`

镜像标签不同，两者**不会复用**对方的产物：

| 命令 | 镜像名 | 被 compose 使用？ |
|------|--------|-------------------|
| `docker build -t code-agent:latest .` | `code-agent:latest` | ❌ 否（compose 找不到） |
| `docker compose build agent` | `code_agent-agent:latest`（项目名_服务名） | ✅ 是 |

`docker-compose.yml` 中 `build: .` 字段不会复用任意标签的镜像。要让 `docker build` 的产物被 compose 使用，需在 compose 中显式：

```yaml
services:
  agent:
    image: code-agent:latest  # 显式标签
    build: .
```

否则 `make docker-build` 的产物会被 `make docker-up` 完全忽略，导致看似"重新构建"但实际跑的是旧镜像。

### 镜像新鲜度三招验证

排查"代码改了但行为没变"时，按顺序验证：

1. `docker compose images agent` — 看镜像 ID 和 CREATED 时间，是否真是新的
2. 日志 caller 行号 — 在源码新加几行后，编译产物的 `caller: "agent/main.go:NNN"` 必然变化；如果跑的还是旧行号，说明镜像没换
3. `docker exec <ctr> strings /usr/local/bin/code-agent | rg <known_string>` — 二进制必须含新加的日志字符串

### 配置文件部署

镜像构建采用**双保险**：

1. `.dockerignore` 显式排除 `configs/config.yaml` 与 `configs/config.allinone.yaml`——即使 Dockerfile 写错也挡住；
2. `Dockerfile` / `Dockerfile.local` 只 `COPY configs/config.example.yaml`，不复制整个 `configs/` 目录。

运行时通过 docker-compose volume bind mount 注入真实配置：

```yaml
volumes:
  - ./configs/config.yaml:/etc/code-agent/configs/config.yaml:ro
```

bind mount 的作用：(1) 运行时挂载真实配置（镜像内不含此文件）；(2) 让本地修改立即生效，`docker compose restart agent` 无需 rebuild。

**`Dockerfile.allinone` 是例外**：单文件 `COPY config.allinone.yaml`，设计上就是内嵌配置的镜像。allinone 镜像不可公开发布。

## 测试惯例

- 始终启用 race detector：`go test -race`
- 集成测试使用 `httptest.Server` + `miniredis` + `zap/observer`，见 `internal/api/integration_test.go`
- `make test-short` 的 `-short` 标志当前无实际效果（零个测试文件检查 `testing.Short()`）
- `internal/temporal/` 在 lint 中排除 unused 检查（Temporal SDK 类型在注册前看起来未使用）
- `.golangci.yml` 全局禁用 G104（unchecked errors）和 G304（file inclusion）
