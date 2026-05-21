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

## 死代码清单

以下功能已完整实现但未接线——它们编译但从不被调用：

| 组件 | 文件 | 说明 |
|------|------|------|
| `llm.Router` | `internal/llm/router.go` | 按 intent/复杂度/消息数路由到 Heavy/Medium/Light 模型层级。未在 main.go 或 Orchestrator 中使用 |
| `SpeculativeToolCache` | `internal/orchestrator/speculative_cache.go` | 幂等只读工具结果的 TTL 缓存。Orchestrator 无此字段，无调用 |
| Git 工具 | `internal/orchestrator/git_tools.go` | 5 个 git 工具已定义（`gitToolDefinitions()`），但未纳入 `getAvailableTools()` — LLM 无法调用 |
| `LLMSummarizer` | `internal/session/summarizer.go` | 定义了 Summarizer 接口和 LLM 实现，但 `Manager.buildSummary()` 使用内联截断逻辑 |

**影响**：修改这些文件的代码不会影响运行时行为。若要启用它们，需要在 main.go 或 Orchestrator 中补充接线。

## 双重 Token 估算器

存在两个不同精度的 token 估算器：

- `internal/llm/client.go` (`EstimateTokens`) — rune 分类法，CJK 1 token/rune，ASCII 4 chars/token
- `internal/session/manager.go` (`estimateTokens`) — 朴素 `len(text)/4 + 1`，CJK 低估约 3 倍

Session 层使用精度较低的估算器，可能导致 token 预算计算偏差。

## 测试惯例

- 始终启用 race detector：`go test -race`
- 集成测试使用 `httptest.Server` + `miniredis` + `zap/observer`，见 `internal/api/integration_test.go`
- `make test-short` 的 `-short` 标志当前无实际效果（零个测试文件检查 `testing.Short()`）
- `internal/temporal/` 在 lint 中排除 unused 检查（Temporal SDK 类型在注册前看起来未使用）
- `.golangci.yml` 全局禁用 G104（unchecked errors）和 G304（file inclusion）
