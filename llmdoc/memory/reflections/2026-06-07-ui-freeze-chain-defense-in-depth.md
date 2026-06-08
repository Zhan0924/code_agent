# 反思：UI 假死链 finalize 阶段防御性多层修复

日期：2026-06-07

## 背景

延续 2026-06-07 verifier retry 反思之后，用户在 Docker 实测中暴露了下一条体验缺口：LLM finalize 阶段（单次非流式 ChatCompletion）阻塞约 **20 分钟**，后端已把完整 818-token 的 final assistant 落 PG、`updated_at` 同步，但前端 UI 仍卡在 "Step 87/100" 不动，看不到最终消息。这是一条典型的 **UI 假死链**：

- 后端事件流静默 → 前端无心跳信号；
- 前端 watchdog 未触发 → 不知道该重连；
- 流终态信号丢失（never-emitted `done`）→ 即使重连也不会显示 final；
- 用户被迫怀疑「是否卡死」「是否要刷新」。

单一根因（finalize 慢）无法靠单点修复消除，必须分层兜底——这是与 verifier retry-once 反思「过程即产物」同源的延续。

## 经过

后端 Go + 前端 TS + 文档共改动 9 个核心文件，新增 5 个测试用例，本地全绿。改动按层次组织：

### 后端 Go

1. **T1 — Replay/Follow 合成 `done` 兜底**：`internal/api/handlers.go` 抽出可测的 `streamReplayFollow(ctx, cache, sessionID, sendEvent, writer, writeMu)` + 局部 `streamCacheReplayer` 接口。两处兜底：
   - Replay 后：history 末尾不是 `done`/`error` 且 `Status` not running → emit `{"reason":"synthesized_after_replay"}`；
   - Follow 退出后：整段未见终态 + ctx 仍存活 → emit `{"reason":"synthesized_after_follow"}`；
   - 守门变量 `hasTerminal` 保证幂等。

2. **T2 — LLM 进度三件套**：`internal/orchestrator/react_core.go` 新增 `callLLMWithProgress(ctx, call func(ctx)→(*ChatResponse,error), sink, ...)`。**纯函数 + 注入式 callback**（非方法），目的是单测可控。事件三件套：
   - `llm_call_started` Content `{attempt,messages,tools}`；
   - `llm_call_progress` 每 `llmProgressInterval`（包级 `var`，默认 3s，**仅测试可改**）emit `{attempt,elapsed_ms}`；
   - `llm_call_completed` Content `{attempt,elapsed_ms,err}`；
   - goroutine 用 `progressCtx` + `progressDone` 强同步，LLM 返回后立刻 `progressCancel()` + `<-progressDone`，无泄漏；
   - 经 `persistingSink` 自动入 Redis Stream，resume 路径同样可见。

### 前端 TS

3. **T3 — resume 链 90s watchdog 抽 helper**：`ChatPage.tsx` 新增 `runWithSilentWatchdog(body, sid, controller, state)`。state 用 `{timedOut: boolean}` 引用对象传出——因为 abort 会从 consumeReactStream 内抛错、helper 返回值不可达，**只能用 by-ref 模式**。两处调用：useEffect 重连 + runReactStream 内 fallback resume。

4. **T4 — 流终止后 `getSession` 兜底**：`reconcileFinalMessage(sid)` 用函数式 `setEntries(prev=>...)` 读最新 entries，逆序找最后一条 assistant message，比较 `new Date(timestamp).getTime() > entry.createdAt`，差值才推 step。`ChatEntry` 扩 `createdAt?: number`。

5. **T5 — 内联「LLM 处理中 (Ns)」渲染**：`types/index.ts::ReactStreamEventType` 加 `llm_call_started|progress|completed`。ReActTrace 组件从 traceSteps 排除三件套（不当作 step 卡片），逆序扫最近 progress event 算 `llmElapsedMs/llmAttempt`，渲染在 spinner 旁。

### 测试

- `internal/api/stream_replay_followup_test.go`（新）：用 miniredis-backed 真实 StreamCache + `captureSink`，2 例验证合成兜底与幂等；
- `internal/orchestrator/react_core_test.go`（新）：3 例，callback 注入慢/快/失败 LLM stub + 测试期缩短 `llmProgressInterval` 验证 progress 心跳真在跳。

### 文档同步

- `docs/architecture/09_orchestrator.md`：循环图加 progress 三件套，新增 §Q4.5「LLM 进度三件套（finalize 假死的根本对策）」；
- `docs/architecture/17_api.md`：SSE 事件列表加三件套，事件载荷表加 LLM 三件套行，新增 §5.3.1「Replay / Resume + 合成 done 兜底」。

## 关键发现

### 1. 「分层防御」思维

单一根因（LLM finalize 太慢）需要 5 层 fix 才能根治体验：

| 层 | 位置 | 角色 |
|----|------|------|
| L1 事件源头 | 后端 `callLLMWithProgress` | 让用户看到「正在等 LLM 第 N 秒」 |
| L2 终态兜底 | 后端 `streamReplayFollow` synthetic done | 防止客户端永远 pending |
| L3 断线检测 | 前端 `runWithSilentWatchdog` 90s | 静默太久主动 resume |
| L4 状态核对 | 前端 `reconcileFinalMessage` getSession | 流彻底失败时回退到 HTTP 读 final |
| L5 UI 反馈 | 前端 ReActTrace 内联渲染 | 把 progress event 翻译成可读 spinner 后缀 |

任何一层失效，下一层仍能让用户看到产物。这与 verifier retry-once 反思的「过程即产物」是同一脉络——区别在 verifier 反思是「让流程结果可见」，本次是「让阻塞流程的等待过程可见」。

### 2. 「编排器减重」模式再次复用

`callLLMWithProgress` 不做成 `(o *Orchestrator)` 方法，而是 **纯函数 + callback 注入**：测试可以注入慢/快/失败 stub，**完全不构造 Orchestrator**。和上一篇 verifier reflection 里 `decideVerificationFollowup` 抽纯函数的模式同源——这条已经积累两个证据点，应升级为 guide。

### 3. 「包级 var 而非 const」用于测试热点

`llmProgressInterval` 故意非 `const`，允许测试在 setup 阶段缩短到 50ms 验证 ticker 行为。const + 启发式估算只能间接验证，引入 flake。规则：**任何 ticker/timeout 常量，如果有测试想验证它的行为，必须是 `var`**。

### 4. 「合成 done 必须带 reason 字段」用于审计分辨

客户端对 `done` 幂等不区分真假，但运维/排查时需要区分「这是真终态」还是「兜底合成的」。`reason: "synthesized_after_replay" | "synthesized_after_follow"` 让日志可追溯，事故复盘时能立刻定位是哪一层兜底救场。

### 5. 「handler 抽接口仅为测试」是合法重构

`streamCacheReplayer` 接口只 3 个方法，只在 `handlers.go` 内使用，目的是让 `streamReplayFollow` 脱离 Orchestrator 构造测试。这是「**为测试服务的微接口**」，不是滥用抽象——判据是：接口边界紧贴单个函数、不跨包暴露、没有第二个实现的预设。

### 6. 「by-reference state 在 TS 抛错路径」

当 helper 内部调用会抛错（abort），helper 的返回值不可达，**只能用 `{flag: boolean}` 引用对象**让外层读 state。函数式风格（`return {timedOut}`）在抛错路径会失效，这是一类 JS/TS 常见反模式：「helper 在 throw 之前要交付的信息只能通过 mutable 引用传出」。

### 7. 「functional setState 读最新」

React `setState(prev=>...)` 是 closure-stale 的解药。`reconcileFinalMessage` 在 finally 中执行，直接读外层 `entries` 会拿到陈旧快照（流期间多次 setEntries 都没反映回闭包）。这是 React 18+ concurrent rendering 下必须的写法。

## 教训与模式

- **「分层防御」模式**：体验缺口由单一根因引发但需要多层兜底——按 `事件源头 → 终态兜底 → 断线检测 → 状态核对 → UI 反馈` 分层，每层独立可工作。
- **「编排器减重」模式（强化）**：编排函数（持有 channel/SSE writer/外部 client）天然难测；把决策点/同步点抽成接受值类型的纯函数 + callback，编排退化为 dispatcher。已积累 verifier 决策 + LLM progress 两个证据点。
- **「ticker 常量用 var」规则**：任何想被测试缩短的 timing 常量，包级 `var` 不 `const`。
- **「兜底事件带 reason 字段」契约**：所有「补偿性 emit」的事件必须带可识别字段，便于事故复盘区分真假终态。
- **「微接口仅为测试」合法性**：handler-private 接口、紧贴单函数、不跨包，是合法重构而非过度抽象。
- **「TS helper 抛错路径用 by-ref state」**：return value 不可达时，mutable 引用对象是唯一选择。
- **「React functional setState 读最新」**：finally / 异步回调 / try-catch 块内访问 state，必须用 setter 函数形式。

## 推广候选（从 memory → 稳定文档）

| 内容 | 目标位置 | 理由 |
|------|----------|------|
| LLM 进度三件套契约（`llm_call_started/progress/completed` + Content JSON 形状） | `must/working-agreement.md` 新增「SSE 长任务心跳契约」段 + `llmdoc/architecture/request-flow.md` ReAct 循环段更新 | 跨前后端契约，每个新接入 SSE 的页面都要知道这三个事件不是 step 卡片 |
| Replay/Follow synthetic `done` 兜底（reason 字段约定：`synthesized_after_replay` / `synthesized_after_follow`） | `llmdoc/architecture/request-flow.md` 新增「断线恢复」段 | 长期生效的恢复契约，运维诊断必须 |
| 「分层防御」模式（5 层划分） | 可写入 `guides/sse-long-task-defenses.md`（新建）或并入未来 `guides/testing-patterns.md` | 新增长任务接口时的检查清单 |
| 「编排器减重」抽纯函数 + callback 注入模式 | `guides/testing-patterns.md`（与 2026-06-07 verifier 反思候选合并） | 已两次证据，应该形成 guide |
| `streamCacheReplayer` 局部接口模式 | 不推广（handler-private，无复用价值） | 单点重构，不上推 |
| 「ticker 常量用 var」规则 | `must/working-agreement.md` 测试惯例段补一行 | 规则简短，不需独立 guide |

## 已发现的潜在 doc gap

1. **SSE 心跳契约缺失**：`must/working-agreement.md` 暂无关于「SSE 长任务心跳契约」的章节——既有协议级 `: ping\n\n` 25s heartbeat 又有业务级 progress 三件套，新人会困惑两者差异。应加一段「协议级 keepalive vs 业务级进度事件」的对照说明。
2. **ReAct 循环描述滞后**：`llmdoc/architecture/request-flow.md` 的 ReAct 循环段没提到 progress 三件套，也没提 retry-once 是 stream-only 性质（在 verifier 反思中也未落地）——这两条都该补。
3. **前端 SSE 鲁棒性条目过时**：`llmdoc/memory/doc-gaps.md::功能缺口 — 前端::无 SSE 重连` 已部分过时——本次落地 90s watchdog + `reconcileFinalMessage` 兜底，鲁棒性大幅提升。该条目应改为「watchdog + session-messages reconcile 已接，但 EventSource 自动重试仍未做」。
4. **兜底事件 reason 字段未在 API 文档定型**：`docs/architecture/17_api.md` 虽已加 §5.3.1，但 `reason` 枚举值表暂时只有 2 条，未来若增加新兜底路径（比如 timeout/cancel）会缺乏维护规范。建议加一条「新增 reason 必须同步本表」的规则。

## 后续行动

1. 下次 `/llmdoc:update` 时把推广候选中 **SSE 心跳契约 + synthetic done reason 字段** 落入 `must/working-agreement.md` 和 `llmdoc/architecture/request-flow.md`。
2. 评估是否值得合并 verifier 反思（2026-06-07 第一篇）与本篇的「编排器减重」证据，独立创建 `guides/testing-patterns.md`——已两个证据点，门槛达到。
3. 更新 `llmdoc/memory/doc-gaps.md` 的「前端 SSE 重连」条目为新现状（watchdog + reconcile 已接、EventSource 自动重试未做）。
4. 后续若 finalize 阶段仍然慢到 90s 以上，watchdog 会反复 resume 导致 stream 抖动；下一步考虑给 LLM 调用本身加 client-side timeout + 显式取消（目前只靠 progress 事件让用户知道、并未限制时长）。
5. 若 sync 路径也想做 progress，需评估 HTTP response 一次性返回模型与流式心跳的本质矛盾——目前结论是 sync 不做，与 verifier retry-once 路径分化判据一致。

## 关联文件

- `/Users/qiankun/code/agent/code_agent/internal/api/handlers.go` — `streamReplayFollow` + `streamCacheReplayer` 接口
- `/Users/qiankun/code/agent/code_agent/internal/api/stream_replay_followup_test.go` — 新增测试
- `/Users/qiankun/code/agent/code_agent/internal/orchestrator/react_core.go` — `callLLMWithProgress` + 包级 `llmProgressInterval`
- `/Users/qiankun/code/agent/code_agent/internal/orchestrator/react_core_test.go` — 新增三例 progress 测试
- `/Users/qiankun/code/agent/code_agent_ui/src/pages/ChatPage.tsx` — `runWithSilentWatchdog` + `reconcileFinalMessage` + ReActTrace LLM 内联渲染
- `/Users/qiankun/code/agent/code_agent_ui/src/types/index.ts` — `ReactStreamEventType` 扩展（三件套）
- `/Users/qiankun/code/agent/code_agent/docs/architecture/09_orchestrator.md` — §Q4.5 LLM 进度三件套
- `/Users/qiankun/code/agent/code_agent/docs/architecture/17_api.md` — §5.3.1 Replay/Resume 合成 done 兜底 + SSE 事件表更新
