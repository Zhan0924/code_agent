# 反思：verifier retry-once + process-as-artifact 落地

日期：2026-06-07

## 背景

参考 Claude Code / Codex / Cursor 等顶级 agent 的两条核心做法：

1. **过程即产物**：每次工具调用（Edit/Bash）实时把结构化产物（diff、命令输出）推到 UI，不依赖 final assistant message。
2. **独立审查可见**：critique/verifier 失败时显示给用户，并允许主 agent 修一次。

code_agent 此前 `verifyOutput` 已存在但是死分支：失败只 `logger.Warn`（注释明写 "never user-visible"）。实测在 Docker 中跑 spider 生成任务，agent 完整写了文件、跑通了测试，最终却只口头总结一段、verifier 给 0.2 分、用户拿不到产物且 UI 无任何提示。两条体验缺口同时收敛到本次任务。

## 经过

后端 + 前端 + 文档共改 11 个文件，测试 `go test -race -count=1 ./internal/orchestrator/...` 全绿。核心改动结构：

- **数据通道**：`ToolResult.Metadata json.RawMessage` 串到 SSE `ReactStreamEvent.Metadata`，工具侧把结构化产物（如 `EditResult.DiffPreview`）序列化进 metadata。
- **决策门控**：抽出纯函数 `decideVerificationFollowup(retried, globalStep, absoluteMaxSteps, vResult)`，返回 `(payload, canRetry)`，是唯一决定是否 retry 的地方。
- **流式编排**：stream 路径在 verify 失败时 emit `verification_warning` 事件 → 满足 retry 条件时把 `formatVerificationFeedback(vResult)` 以 `RoleUser` 推回 messages，置 `Task.VerificationRetried=true`，`continue` 主循环触发自然的下一轮 ReAct。
- **传输路径分化**：sync 路径不做 retry（保留旧行为）。
- **前端**：tool_result step 渲染 DiffBlock；`verification_warning` 渲染评分条 + issues bullet。

## 关键发现

### 1. 「难单测的大函数」→ 抽门控为纯函数

`ProcessMessageStreamFull` 与 `*llm.Client`、sessionMgr、promptBuilder、SSE writer 深耦合，无法直接单测 retry 决策。把决策剥成 `decideVerificationFollowup` 后，编排器只剩 sink/loop/state-mutation，决策本身用 5 个子用例覆盖边界（197/198/200/already-retried/low-score）。这是一个可复用的「编排器减重」模式：**当大函数难单测时，把决策抽成纯函数，编排只负责调用与状态变更**。

### 2. Metadata 用 `json.RawMessage` 而不是 `map[string]any`

中间层用 `map[string]any` 转结构化数据会让 typed struct 形状失真（数字类型、null 处理、字段顺序）。改用 `json.RawMessage` 直接透传 → 前端拿到的 JSON 形状由后端 struct 单点决定。这是一个跨语言边界的通用做法。

### 3. 同一逻辑在不同传输路径采取差异化策略

sync API 有严格延迟预算，二次 LLM 调用会让调用方意外超时；stream 路径用户已经在看事件流，retry 对 UX 是透明加分。**判据是延迟容忍度**，不是「逻辑应该一致」。这条破除了「重构必须统一行为」的惯性。

### 4. Retry guard 必须给后续 ReAct 留预算

`globalStep < absoluteMaxSteps - 3` 留 3 步给二次 ReAct 落实 feedback；否则 retry 触发后立刻 step-exhausted，等于白做。任何「触发额外子任务」的门控都要预留下游步数。

### 5. 死代码复活需要更新清单

`formatVerificationFeedback` 此前在 `must/working-agreement.md` 死代码清单里。本次 retry 路径把它消费了，但清单尚未同步——这是 stable doc 与代码现实漂移的典型场景。

## 教训与模式

- **「编排器减重」模式**：编排函数（持有 channel/SSE writer/外部 client 引用）天然难测；把每个决策点抽成接受值类型的纯函数，编排退化为 dispatcher。
- **「结构化产物通过元数据带回」契约**：工具结果不要只返回字符串 + 让 UI 解析；用 `Metadata json.RawMessage` 携带按工具类型变化的结构化负载，前端按 metadata 字段渲染产物块。
- **「retry-once 一次性哨兵」模式**：在 Task 上挂 `VerificationRetried bool json:"-"`，配合纯函数门控保证「最多一次」语义；`json:"-"` 避免该哨兵被持久化影响后续会话。
- **「死代码清单是有时效的」**：标记死代码时记下「为何死」，复活时回头同步清单。

## 推广候选（从 memory → 稳定文档）

| 内容 | 目标位置 | 理由 |
|------|----------|------|
| process-as-artifact 契约：`ToolResult.Metadata json.RawMessage` 透传到 SSE | `must/working-agreement.md` 新增「工具产物 Metadata 契约」 | 每次新增工具都会面临「如何让前端看到产物」问题，需要约定 |
| verifier retry-once 门控：唯一决策函数 + stream-only + step budget guard | `docs/architecture/09_orchestrator.md` §9.2（已更新） + `llmdoc/architecture/request-flow.md`（已更新） | 已落地，需在 `must/working-agreement.md` 死代码清单移除 `formatVerificationFeedback` |
| 「编排器减重」抽纯门控模式 | `guides/testing-patterns.md`（新建，与 2026-05-28 反思中的 mock HTTP 模式合并） | 可复用，未来 orchestrator 长出新决策点都适用 |
| `Task.VerificationRetried` 运行时哨兵 | `docs/architecture/09_orchestrator.md` 或 `16_store.md` Task 字段表 | 防止后续维护者误删该字段 |

## 已发现的潜在 doc gap

- `must/working-agreement.md` 死代码清单需移除 `formatVerificationFeedback`（已被消费）。
- `must/working-agreement.md` ToolResult 章节未提 `Metadata` 字段约定，需加一条「用 `json.RawMessage` 不要 `map`」。
- `docs/architecture/09_orchestrator.md` 或 `16_session.md` 中 Task 字段表需补 `VerificationRetried` 运行时哨兵（json 标签 `-`，不参与持久化）。

## 后续行动

1. 下次 `/llmdoc:update` 时同步死代码清单：把 `formatVerificationFeedback` 从「死代码」迁到「retry 路径消费」。
2. 在 `must/working-agreement.md` 增加「工具产物 Metadata 契约」段落，明确 `json.RawMessage` 选择理由。
3. 评估是否值得创建 `guides/testing-patterns.md`（与 2026-05-28 反思中的 mock HTTP 模式合并），收录「编排器减重」模式。
4. 后续若 sync 路径也想做 retry，需先评估延迟预算（当前 600s write_timeout 是否能容纳二次 LLM 调用 + retry 后 ReAct 的最坏情况）。

## 关联文件

- `/Users/qiankun/code/agent/code_agent/internal/orchestrator/verification.go` — 新增 `decideVerificationFollowup` 纯函数
- `/Users/qiankun/code/agent/code_agent/internal/orchestrator/orchestrator.go` — stream 路径 retry 编排
- `/Users/qiankun/code/agent/code_agent/internal/orchestrator/file_tools.go` — `editResultToMetadata` 助手
- `/Users/qiankun/code/agent/code_agent/internal/orchestrator/react_core.go` — Metadata 透传到 SSE
- `/Users/qiankun/code/agent/code_agent/internal/models/models.go` — `ToolResult.Metadata` + `Task.VerificationRetried`
- `/Users/qiankun/code/agent/code_agent/internal/orchestrator/verification_test.go` — 决策函数 5 子用例
- `/Users/qiankun/code/agent/code_agent/internal/orchestrator/edit_engine_test.go` — Metadata 序列化测试
- `/Users/qiankun/code/agent/code_agent_ui/src/types/index.ts` — 前端 Metadata 类型
- `/Users/qiankun/code/agent/code_agent_ui/src/pages/ChatPage.tsx` — DiffBlock + verification_warning 渲染
- `/Users/qiankun/code/agent/code_agent/docs/architecture/09_orchestrator.md` §9.2 — 架构文档同步
- `/Users/qiankun/code/agent/code_agent/docs/architecture/17_api.md` — SSE 事件表与 Metadata 表
- `/Users/qiankun/code/agent/code_agent/llmdoc/architecture/request-flow.md` — llmdoc 汇总版同步
