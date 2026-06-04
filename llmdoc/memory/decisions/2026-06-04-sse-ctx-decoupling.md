# 2026-06-04 — SSE/ctx 解耦 + DeepSeek primary 切换

## Context

线上一次 ReAct 任务在 `Processing... (step 40/200)` 永久卡死。docker 日志显示 t+619s 时 SSE 连接被 `server.write_timeout: 600s` 撕掉，HTTP 请求 ctx 被取消并一路传播到 orchestrator / LLM 客户端 / 工具执行；orchestrator 静默 `return` 未发任何终态事件；UI 永远旋转、HITL 通道孤立。

## 根因（已 Read 验证）

| 位置 | 病征 |
|---|---|
| `internal/api/handlers.go::handleChatReactStream` | 直接把 `c.Request.Context()` 作为业务 ctx 传给 `ProcessMessageStreamFull` |
| `configs/config.example.yaml::server.write_timeout: 600s` | 作为唯一硬天花板对每个 SSE 连接生效 |
| `internal/api/handlers.go` SSE 主循环 | 无心跳/ping，60+s 静默 IO 真实穿过 write_timeout |
| `internal/orchestrator/orchestrator.go::ProcessMessageStreamFull` | 业务 goroutine 用 inbound ctx 跑完整链路，ctx 一炸全链路炸 |
| `internal/orchestrator/react_core.go` 步循环 `case <-ctx.Done()` | 仅 `return`，**未** Emit 终态事件 |
| `code_agent_ui/src/pages/ChatPage.tsx::handleSend` | 无 stuck 检测；server 沉默时 fetch reader 无限期挂着 |

## 决策

四条改动按风险序、独立可 rollback：

### PR-A · DeepSeek 切 primary（配置 only）

`configs/config.yaml` 改 `llm.primary.{provider,model,base_url,api_key}` 指向 `https://api.deepseek.com/anthropic` + `deepseek-v4-pro`。`config.yaml` 为 gitignored，秘密不入库。零代码改动：`internal/llm/anthropic_provider.go::NewAnthropicProvider` 已通过 `option.WithBaseURL(cfg.BaseURL)` 注入，DeepSeek `/anthropic` 端点完全 Anthropic Messages API 兼容（pre-flight 三项探针通过：普通响应 / tool_use / SSE 流）。fallback 保留 ollama，故障域隔离。

### PR-B · SSE 服务端心跳 ping（25s 间隔）

`internal/api/handlers.go`：写完 SSE headers 后起独立 goroutine 每 25s 发 `: ping\n\n`（SSE 注释行，前端 `data:` parser 不消费）；`sendSSEEvent` 与心跳共用 `writeMu` 串行化。新增 `runSSEHeartbeat` 抽出可测；`internal/api/sse_ping_test.go` 三例覆盖：周期 emit、ctx cancel 立停、与业务写入并发不交错。25s 给 600s write_timeout 24x 缓冲，沿用 MCP transport 90s 心跳的项目前例。

### PR-C · 业务 ctx 与 HTTP ctx 解耦 + 终态事件契约

`internal/orchestrator/orchestrator.go::ProcessMessageStreamFull`：

1. 重命名形参 `ctx → reqCtx`；
2. 函数级构造 `workCtx, _ := context.WithTimeout(context.Background(), 30*time.Minute)`；
3. 起桥接 goroutine：`reqCtx.Done()` 时仅 `dropCancel()`，**不** 取消 `workCtx`；
4. `channelSink{ch, droppedCtx}` 替换所有 `eventCh <- ...` 直接 send，新增 `droppedCtx` 字段使 `Emit` 在客户端断后非阻塞丢弃事件（防止 cap=64 buffer 满阻塞业务）；
5. 全部业务调用（`parseIntent` / `sessionMgr.*` / `ragEngine.Retrieve` / `reactLoopCore` / `verifyOutput` / Temporal HITL / 持久化）切换到 `workCtx`；
6. `react_core.go` 步循环 `case <-ctx.Done()` 与 LLM retry `case <-ctx.Done()` 补 `sink.Emit(error)` 再 return，终结"静默挂死"。

`tool_approval.go::waitToolApproval` 无需改：它通过 `reactLoopCore` 传入 ctx，新拓扑下天然就是 `workCtx`，30min 边界与 `toolApprovalTimeout` 双重保护。

测试 `internal/orchestrator/stream_decouple_test.go` 四例：

- `channelSink` cap 满后 droppedCtx 取消使 50 次连续 Emit 不阻塞；
- droppedCtx 未取消时事件有序到达；
- droppedCtx==nil 退化为阻塞 send；
- 复刻 ProcessMessageStreamFull 的桥接拓扑，验证 reqCtx 取消后 workCtx 仍 alive。

### PR-D · 前端静默超时降级

`code_agent_ui/src/pages/ChatPage.tsx::handleSend`：90s 无字节看门狗。每次 `reader.read()` 返回任何字节（含 ping）刷新 `lastEventAt`；`setInterval` 每 5s 比对，超 90s（>3 个 25s ping 间隔）追加 error step、`controller.abort()`、清看门狗。`finally` 永远清除 interval。

## 验证

- `go test -race -count=1 -short ./...` 全绿
- `cd code_agent_ui && pnpm run build` 通过
- `internal/orchestrator/stream_decouple_test.go::TestChannelSink_EmitDoesNotBlockAfterDroppedCtxCancel` 在 2s 内完成 50 次 Emit
- `internal/api/sse_ping_test.go` 三例通过；250ms 内见到 ≥3 次 `: ping\n\n`，flush 计数与 write 计数严格一致

## 不做（明确边界）

- 不做 Redis pub/sub / Postgres 持久化任务事件流（"真断线续看"是独立议题，规模远大于本批）
- 不动 `toolApprovalCh` 的内存 map 结构
- 不收紧 `write_timeout`（PR-B 后已无必要触发，保留 600s 作兜底）
- 不把 fallback 也改 DeepSeek（保留 ollama 本地兜底）
- 不动 `internal/llm/anthropic_provider.go`（已支持 BaseURL 配置）

## 影响

- `docs/architecture/09_orchestrator.md` 需补 "ProcessMessageStreamFull 的双 ctx 拓扑" 章节
- `docs/architecture/14_workspace.md` 无须改（之前 plan 14_session.md 是误写——实际架构文档中 session 散落在 09/15，没有专门的 14_session.md）
- 长期：可观测性面板需添加 `sse_heartbeat_pings_total` 与 `stream_work_ctx_expired_total` 指标（留待下一次度量补丁）
