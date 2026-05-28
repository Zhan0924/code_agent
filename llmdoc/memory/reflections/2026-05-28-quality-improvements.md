# 反思：P0-P2 全量质量修复（8 commits）

日期：2026-05-28

## 触发事件

对代码库执行系统性质量审计后，按 P0（安全）→ P1（功能）→ P2（质量）优先级修复了 8 项问题，涵盖路径遍历误报、Egress 注入、Planner 硬拒绝、LLM Provider 测试、apply_diff 工具、tool_progress 流式回调、包级测试补全。

## 做得好的地方

- 优先级分层执行有效：先修安全问题（路径遍历、Egress 注入），再加功能（apply_diff、tool_progress），最后补测试。每步 Docker 验证 + 独立提交，回滚粒度清晰。
- macOS 路径遍历误报的修复方式正确：用 `filepath.EvalSymlinks` 规范化 `/tmp` → `/private/tmp` 的 symlink 差异，而非放宽校验逻辑。
- MCP Gateway Egress 注入是预防性修复——在漏洞被利用前堵住了 HTTP client 未受控的问题。
- 新增约 2400 行测试代码覆盖了 metrics、models、temporal、tracing 四个此前零测试的包。

## 做错的地方 / 返工

1. **apply_diff 工具注册遗漏 9 处**：工具定义写好后，只在 orchestrator switch 中注册了一处。审计阶段才发现还需要在 multiagent allowed tools、dynamic tool conflict list、mcp_skill_handlers builtin list 等共 9 个位置补注册。这导致了额外的返工提交。
2. **workspace 包 macOS 路径 bug 长期存在**：该 bug 仅在 macOS 集成测试中暴露（`/tmp` 是 `/private/tmp` 的 symlink），单元测试未覆盖此路径。说明核心安全校验缺少跨平台测试用例。
3. **LLM provider 测试此前为零**：anthropic_provider 和 openai_provider 是关键路径代码，却没有任何单元测试。这意味着之前的 provider 修改（如 Prompt Caching header 注入）全靠手动验证。

## 根因分析

- **工具注册分散**：当前工具白名单分布在 5 个包的 9 个位置，没有单一注册表或编译期检查。新增工具时必须人工搜索所有注册点，极易遗漏。`working-agreement.md` 记录了"两套并行机制"，但实际是 N 套（orchestrator switch + multiagent allowedTools + dynamic conflict list + mcp_skill_handlers builtins + getAvailableTools 返回值）。
- **跨平台测试缺失**：CI 在 Linux（Docker alpine）中运行，macOS 特有的 symlink 行为（`/tmp` → `/private/tmp`）从未被 CI 覆盖。
- **Provider 测试债务**：LLM provider 代码依赖外部 API，开发者倾向于跳过测试。正确做法是 mock HTTP transport 测试请求构造和响应解析。

## 缺失的文档或信号

1. **工具注册清单不完整**：`working-agreement.md` 的"工具分发拆分"节只描述了 orchestrator switch 和 tools.Registry 两套机制，未提及 multiagent allowedTools、dynamic conflict list、mcp_skill_handlers builtins。新增工具时无法从文档获得完整注册点列表。
2. **跨平台测试要求未记录**：对于涉及文件路径安全校验的代码，应明确要求 symlink 场景的测试用例。
3. **Provider 测试模式未记录**：如何用 mock HTTP RoundTripper 测试 LLM provider 的模式应作为测试指南存在。

## 晋升候选（从 memory → 稳定文档）

| 内容 | 目标位置 | 理由 |
|------|----------|------|
| 完整工具注册点清单（9 处） | `must/working-agreement.md` "工具分发拆分"节 | 每次新增工具都会踩坑，属于必读不变量 |
| 路径安全校验必须 EvalSymlinks | `must/working-agreement.md` 新增"文件路径安全"节 | workspace 包的核心安全约束 |
| LLM Provider mock 测试模式 | `guides/testing-patterns.md`（新建） | 可复用的测试模式，降低未来 provider 测试债务 |

## 后续行动

1. **更新 `must/working-agreement.md`**：将"工具分发拆分"节扩展为完整的 N 处注册点清单，包含 grep 命令示例供快速验证。
2. **考虑编译期工具注册检查**：探索用 `go generate` 或 init() 断言确保所有工具在所有注册点一致。
3. **CI 增加 macOS matrix**：至少对 workspace 包的路径校验测试在 macOS runner 上执行。
4. **创建 `guides/testing-patterns.md`**：记录 mock HTTP transport、miniredis、httptest.Server 等可复用测试模式。
