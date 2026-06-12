# 19. Observability 可观测性栈

> **范围**：`internal/metrics/` + `internal/tracing/` + `internal/audit/` + `internal/errors/` + `internal/pool/`
> **物理路径与行数**：
> - `internal/metrics/metrics.go` (234) — Prometheus 全量指标定义（25 个 metric，9 个子系统）
> - `internal/metrics/cost.go` (133) — LLM 成本归因 + 工具执行 + Planner 指标
> - `internal/tracing/otel.go` (214) — OpenTelemetry Provider + Gin Middleware
> - `internal/audit/logger.go` (132) — 审计事件 logger（**未被生产代码引用**，见 §5.4）
> - `internal/errors/errors.go` (126) — `AgentError` + 15 种 Code + HTTP 映射（**未被生产代码引用**，见 §5.5）
> - `internal/pool/pool.go` (185) — sync.Pool 封装（**仅 session/manager.go 一处引用**，见 §5.6）
> - 装配点：`cmd/agent/main.go:106` zap、L366-375 tracing、L488+ metrics 中间件经 17_api

---

## 1. 模块定位

**"让系统透明可诊断"**——把五个横切关注合并讲：

| 包 | 职责 | 实际使用度 |
|---|---|---|
| `metrics` | Prometheus 暴露 | ✅ 全系统调用 |
| `tracing` | OTel 分布式追踪 → Jaeger/Tempo | ✅ Gin middleware + LLM/MCP span |
| `audit` | 敏感操作结构化审计日志 | ❌ **代码存在，但无任何 production 调用方** |
| `errors` | 领域错误类型 + HTTP 映射 | ❌ **代码存在，但无任何 production 调用方** |
| `pool` | sync.Pool 复用（GC 降压） | ⚠️ **仅 session/manager.go 调用** |

四件套互补（理想状态）：

```
Metrics  → "系统健康不健康"        聚合 / 低基数 / 报警
Tracing  → "单次请求为什么慢"      单次请求 / span 级
Logs     → "当时发生了什么"        结构化 / 高基数 / 可搜
Audit    → "谁做了什么"（合规）    结构化 / 只增不删 / 归档
```

> **诚实标注**：当前实现只把 Metrics + Tracing + Logs 三件套接成生产路径。Audit 是设计完整但未接线的占位包（19-1）；Errors / Pool 都是孤儿设施（19-2 / 19-3）。

---

## 2. 设计哲学

### 2.1 三段独立暴露（Metrics / Tracing / Logs）

每条路径独立配置、独立失败：
- Metrics 启动失败 → 仍跑（promauto 写入 DefaultRegisterer，没 backend 抓取也无所谓）。
- Tracing 启动失败 → log Warn 后继续（`main.go:375`）。
- Logs 启动失败 → fatal（`zap.NewProduction()` 失败 = panic，因为 Logger 是所有其他启动步骤的前提）。

这是"可观测性不能成为可用性的负担"的体现。**反例**：Datadog Agent 故障导致主进程拒服务——本系统刻意避免。

### 2.2 Label Cardinality 紧 vs 松的明确分层

`metrics.go` 全量保持低基数（label 取值有界几十）：

```go
LLMRequestTotal  []string{"provider", "model", "status"}      // 3 label，各 < 10 值
APIRequestTotal  []string{"method", "path", "status"}          // path 用 route template
```

`cost.go` 故意打破这个原则：

```go
LLMCostUSD  []string{"model", "tier", "session_id", "user_id", "task_id"}  // 高基数！
```

**理由**（cost.go:9-15）：cost attribution 是企业部署的硬需求，业务接受高基数代价。**解法**是在 remote_write 层用 Prometheus relabel_configs drop 这几个 label，或者直接禁用 cost 指标抓取，改用 `agent_cost_ledger` PG 表（16_store）。

### 2.3 Histogram Bucket 按数据形态定制

- LLM 调用：`0.1 / 0.5 / 1 / 2 / 5 / 10 / 30 / 60` 秒（一次流式调用动辄分钟级）。
- RAG 检索：`0.01 / 0.05 / 0.1 / 0.25 / 0.5 / 1` 秒（vector + BM25 应在 100ms 内）。
- 沙箱执行：`0.1 / 0.5 / 1 / 5 / 10 / 30 / 60 / 120` 秒（长跑测试是常态）。
- 工具执行：`prometheus.ExponentialBuckets(0.01, 2, 14)` ≈ 10ms…80s（覆盖 builtin、MCP、Skill 三种来源）。

默认 `prometheus.DefBuckets` 只覆盖 5ms–10s，对 LLM 来说几乎全部落到 +Inf bucket，P99 完全不可见。**Bucket 选择 = SLO 形状**。

### 2.4 Tracing：head-based 采样 + W3C propagation

`SampleRate: 0.1`（10% 抽样）。新 trace 进来时一次性决定，整棵 span 树要么全部记录、要么全部丢弃。
不用 tail-based（按 trace 完成后看慢/失败决定保留）的原因：tail-based 必须配 OpenTelemetry Collector 中间节点持有完整 trace 才能决定，运维负担高。生产先用 head-based 跑，慢请求另开 100% 采样的 debug header。

W3C `traceparent` header 在 inbound 自动解析为父 span ID（`tracing.GinMiddleware:174`），outbound HTTP 由 otelhttp 自动注入。

### 2.5 Logger 单例：zap.NewProduction()

`main.go:106` 全局唯一 logger。所有子模块通过参数注入或 `logger.Named("xxx")` 衍生子 logger，**不允许**子包自建 logger。
原因：单一输出（stdout/stderr）+ 单一 sampling 策略；多 logger 会出现日志级别不一致。

### 2.6 Audit ≠ Log：分开通道

`audit.Logger` 也是 zap，但它 `.Named("audit")` 并加 `log_type=audit` 字段，方便 fluentbit / vector 把 audit 单独 route 到 SIEM / 长期归档存储（合规要求审计日志保留 7 年）。普通 zap 日志只保留 30 天。

**当前问题**：audit logger 类型实现完整，但 orchestrator/api 都未调用。**审计日志通道是空的**。

---

## 3. 依赖架构

```
                  ┌── cmd/agent/main.go ───────────────┐
                  │  L106  zap.NewProduction()         │
                  │  L366-375 tracing.NewProvider      │
                  │  L488+ apiServer (注入 tracing/metrics middleware via router.go) │
                  └─────────┬──────────────────────────┘
                            │ logger
                            │
              ┌─────────────┼──────────────┬─────────────┐
              ▼             ▼              ▼             ▼
       ┌──────────┐  ┌───────────┐  ┌───────────┐  ┌──────────┐
       │ metrics  │  │ tracing   │  │ audit     │  │ pool     │
       │ (active) │  │ (active)  │  │ (orphan)  │  │ (mostly  │
       │          │  │           │  │           │  │  orphan) │
       └──────────┘  └───────────┘  └───────────┘  └──────────┘
              ▲             ▲              ▲             ▲
              │             │              │             │
              ├── orch      ├── api        ├── (none)    ├── session
              ├── llm       ├── llm        │             │
              ├── rag       ├── rag        │             │
              ├── sandbox   ├── sandbox    │             │
              ├── mcp       ├── mcp        │             │
              ├── session   └── store      │             │
              ├── planner                  │             │
              └── api                      │             │
                                           │             │
                              errors (zero importer)  ❌
```

**外部依赖**：
- `github.com/prometheus/client_golang/prometheus` + `promauto` — 包级注册到 DefaultRegisterer
- `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc` — OTLP gRPC exporter
- `go.opentelemetry.io/otel/sdk/trace` — TracerProvider / Sampler
- `go.uber.org/zap` — 结构化日志
- 标准库 `sync.Pool`

---

## 4. 数据流总览

### 4.1 一次 HTTP 请求的可观测性事件

```
HTTP Request
    │
    ▼
[1] tracingMiddleware (otel.go:172)
    │   - 解析 W3C traceparent 头作为 parent span
    │   - 新建 server span：method + route_template
    │   - 写入 ctx，注入 c.Request
    │   - defer span.End()
    ▼
[2] metricsMiddleware (api/middleware.go via router.go:198)
    │   - record start time
    │   - call c.Next()
    │   - 完成后：APIRequestTotal.Inc(method, path, status)
    │            APIRequestDuration.Observe(method, path)
    ▼
[3] loggingMiddleware
    │   - 请求开始 + 完成两条 INFO 日志
    │   - request_id / method / path / status / latency
    ▼
[4] Handler (orchestrator, llm, sandbox...)
    │   - 子 span: tracer.Start(ctx, "llm.chat_completion")
    │   - 子 metric: LLMRequestDuration.Observe(provider, model)
    ▼
[5] span.End() + metric flushed
    │
    ▼
HTTP Response (附 traceparent 头)
```

### 4.2 LLM 调用的指标 + Cost 归因

```
orchestrator.callLLM(ctx, sessionID, userID, taskID, msgs)
    │
    ▼ ctx, span := tracer.Start(ctx, "llm.chat_completion")
    │   span.SetAttributes(provider, model, prompt_tokens)
    │
    ▼ resp, err := llmClient.ChatCompletion(...)
    │
    ▼ metrics.LLMRequestTotal.Inc(provider, model, status)
    ▼ metrics.LLMRequestDuration.Observe(provider, model, latency)
    ▼ metrics.LLMTokensUsed.Add(provider, "prompt", n_prompt)
    ▼ metrics.LLMTokensUsed.Add(provider, "completion", n_completion)
    ▼ metrics.RecordLLMCost(model, tier, sessionID, userID, taskID, n_prompt, n_completion)
    │     │
    │     ▼ cost = (prompt/1000)*price.InputPer1K + (completion/1000)*price.OutputPer1K
    │     ▼ LLMCostUSD.Add(model, tier, sessionID, userID, taskID, cost)
    │
    ▼ span.End()
```

### 4.3 Tracing Provider 启动与关闭

```
main.go:366
    tracingCfg = &Config{Enabled, Endpoint, ServiceName, SampleRate, Insecure}
    │
    ▼ tracing.NewProvider(cfg, logger)        ← otel.go:87
        ├── if !cfg.Enabled → 返回空 Provider，立即返回 nil
        ├── 创建 OTLP gRPC exporter（10s timeout）
        ├── 构造 resource：service.name + version + host + runtime
        ├── 选 sampler：
        │      SampleRate >= 1.0 → AlwaysSample
        │      SampleRate <= 0   → NeverSample
        │      else              → TraceIDRatioBased(rate)
        ├── 装配 TracerProvider：
        │      Batcher (MaxBatchSize=512, BatchTimeout=5s)
        │      Resource / Sampler
        ├── otel.SetTracerProvider(tp) ← 全局
        └── otel.SetTextMapPropagator(W3C TraceContext + Baggage)
    │
    ▼ （程序运行期间所有 tracer 从全局 provider 获取）
    │
    ▼ main.go defer：traceProvider.Shutdown(ctx)
        └── Batcher flush 未发送的 span 到 OTLP
```

### 4.4 zap 日志输出 + Audit 通道（设计中）

```
zap.NewProduction()  ← main.go:106
   │
   ├── stdout（JSON），生产环境被容器运行时收集 → Loki/ES/CloudWatch
   │
   └── Named("audit").With("log_type", "audit")
         │
         ▼ audit.Logger.Log(event)        ← 注：当前**无 production 调用**
              │
              ▼ zap.Info("audit_event", fields...)
                   │
                   └── fluentbit/vector 按 log_type=audit 路由到长期归档
```

---

## 5. 实现细节

### 5.1 25 个 Prometheus 指标的命名规范

所有指标命名遵循 `code_agent_<subsystem>_<name>_<unit>` 三段式：

| 子系统 | 指标 | 类型 | Labels |
|---|---|---|---|
| llm | request_total | Counter | provider, model, status |
| llm | request_duration_seconds | Histogram | provider, model |
| llm | tokens_used_total | Counter | provider, type(prompt/completion) |
| llm | circuit_breaker_state | Gauge | provider |
| llm | fallback_total | Counter | — |
| llm | cost_usd_total | Counter | model, tier, session, user, task |
| rag | retrieval_duration_seconds | Histogram | — |
| rag | chunks_returned | Histogram | — |
| pruner | tokens_saved_total | Counter | — |
| pruner | chunks_pruned_total | Counter | — |
| session | active_count | Gauge | — |
| session | cold_archive_total | Counter | — |
| session | context_compression_total | Counter | — |
| sandbox | execution_total | Counter | language, status |
| sandbox | execution_duration_seconds | Histogram | language |
| mcp | call_total | Counter | server, tool, status |
| mcp | call_duration_seconds | Histogram | server |
| hitl | approval_total | Counter | decision |
| hitl | pending_count | Gauge | — |
| api | request_total | Counter | method, path, status |
| api | request_duration_seconds | Histogram | method, path |
| api | websocket_connections | Gauge | — |
| tool | execution_duration_seconds | Histogram | tool, source, status |
| tool | execution_total | Counter | tool, source, status |
| planner | steps_total | Counter | action, status |
| planner | revision_total | Counter | — |
| planner | plans_created_total | Counter | — |
| prompt | cache_prefix_hash | Gauge | hash |

总计 **25 个**（核心 metrics.go 19 个 + cost.go 6 个）。

> **疑点**：`PromptCacheHitRatio` 名字带 "Hit_Ratio" 但实际是 `cache_prefix_hash` Gauge，记录的是哈希值而非比率。命名误导，应改为 `prompt_cache_prefix_hash` 才与 Name 字段一致——查 `metrics.go:228-233`，**Name 已经是 `cache_prefix_hash`，Go 变量名 PromptCacheHitRatio 与之不符**。这是历史遗留命名漂移。

### 5.2 promauto 的副作用注册模式

`metrics.go` 全部用 `promauto.NewCounterVec(...)` 而非 `prometheus.NewCounterVec(...) + Register`：

```go
var LLMRequestTotal = promauto.NewCounterVec(...)
```

**效果**：import 包就完成注册。任何 import `internal/metrics` 的代码立即让指标可被 scrape。
**风险**：测试时无法隔离指标——不同测试 share 同一 DefaultRegisterer，counter 会跨测试累加。**`metrics_test.go` 必须 Reset() 或用 testutil 才能稳定**。
**为什么仍这么做**：Prometheus 官方推荐 + 减少样板代码 + Go init-time 是单线程，无并发风险。

### 5.3 Cost 归因的高基数风险

`cost.go:30`：

```go
LLMCostUSD := promauto.NewCounterVec(..., []string{"model", "tier", "session_id", "user_id", "task_id"})
```

假设：
- model: 10 种
- tier: 3 种（heavy/medium/light）
- session_id: 10⁶（活跃用户每月百万会话）
- user_id: 10⁴
- task_id: 10⁵

最坏组合 10 × 3 × 10⁶ × 10⁴ × 10⁵ = 3 × 10¹⁶ —— Prometheus 直接 OOM。
**实际不会发生**：用户量小，且每个 user 只产生有限 session+task 组合。但生产监控必须**在 scrape 后立即 drop 高基数 label**，否则 storage explosion。

`cost.go:9-15` 推荐方案：用 `agent_cost_ledger` PG 表做财务报表，Prometheus 只看聚合 `sum by (model)`。

### 5.4 Audit Logger 是设计完整但未接线

**代码**：`internal/audit/logger.go` 132 行，10 种 EventType，3 个便利方法（`LogApproval` / `LogSandboxExec` / `LogMCPCall`）。

**生产引用**：
```
$ rg -rn 'internal/audit' --type=go -g '!*_test.go' -g '!*_principles.go'
（空结果）
```

**结论**：
- `internal/audit` 包仅被自己的 `_test.go` 引用。
- main.go 不创建 audit logger。
- orchestrator 不调用 audit。
- HITL approval、sandbox 执行、MCP tool 都没记审计日志。

**影响**：合规审计字段全部缺失。任何"谁批准了这个高风险操作"的查询都查不到。
**修复方向**（P1）：
1. `main.go` 创建 `auditLogger := audit.NewLogger(logger)`，注入到 apiServer/orchestrator。
2. `orchestrator.suspendForApproval` 在批准/拒绝时调用 `LogApproval`。
3. `sandbox.Manager.Run` 完成时调用 `LogSandboxExec`。
4. `mcp.Gateway.CallTool` 在 dispatch 时调用 `LogMCPCall`。

### 5.5 Errors 包零调用

**代码**：`internal/errors/errors.go` 提供 `AgentError` + 15 个 Code + `HTTPStatus()` 自动映射。

**生产引用**：
```
$ rg -rn 'internal/errors' --type=go -g '!*_test.go'
（空结果）
```

实际 handlers 全部用裸 `errors.New` / `fmt.Errorf`，HTTP 状态码硬编码在每个 handler 里（17_api 中可见）。

**影响**：
- HTTP status code 散落在 30+ handler，没有单一映射点——添加新错误类型必须改多处。
- 中间件无法做"看到 AgentError.Code=RATE_LIMITED → 返回 429"这种统一处理。
- 错误日志缺乏机器可读分类，metrics 没法 group_by error.code。

**修复方向**（P2）：
1. 在 handler 末尾加一个 `errorHandler` middleware：检查 `c.Errors`，如果 `errors.As(err, &agentErr)` 则用 `agentErr.HTTPStatus()` 写 response。
2. 所有 orchestrator/llm/sandbox 错误返回时包装成 `errors.LLMFailure(cause)` 等。

### 5.6 Pool 包几乎全孤儿

**代码**：`internal/pool/pool.go` 五种 pool（ByteSlice / Buffer / JSONEncoder / RPCRequest / Global singletons）。

**生产引用**：
```
$ rg -n 'internal/pool' --type=go -g '!*_test.go' -g '!*_principles.go'
internal/session/manager.go:64
```

只有 `session/manager.go` 一处使用 `GlobalBufferPool` + `GlobalJSONPool`。

**文档与实际不符**：
- pool.go:14-15 注释 "Used in RAG parsing, sandbox I/O streaming, and MCP JSON-RPC communication"。
- 实际：RAG / sandbox / MCP 都**没有**调用 pool。
- sandbox/doc.go 注释 "经 SmallBytePool 分配 buf 后 flush 到 chan"——文档说有，代码没有。

**影响**：高并发下 GC 压力没被均摊。P99 延迟可能因为 GC pause 抖动。
**修复方向**（P2）：在 MCP `gateway.callTool` / RAG `engine.ingest` / sandbox 读流路径替换 `make([]byte, ...)` 为 `pool.SmallBytePool.Get()`。

### 5.7 Tracing Provider 启动 timeout=10s

`otel.go:93`：

```go
ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
```

OTLP gRPC dial 10 秒超时。如果 collector 不可达（Jaeger 没起、网络不通）：
- exporter 构造失败 → `NewProvider` 返回 error → main.go:375 仅 Warn 然后继续启动（不 fatal）。
- 启动延迟 10s——生产部署在 collector 慢启动时影响 SLO。

**修复方向**：把 timeout 缩到 2s，或改成异步装配（后台 retry 直到 connect）。

### 5.8 Span 错误标记规则

`otel.go:206-213`：

```go
if status >= 500 {
    span.RecordError(errors.New(errMsg))
    span.SetStatus(codes.Error, errMsg)
} else {
    span.SetStatus(codes.Ok, "")
}
```

只有 5xx 标记为 Error，4xx 全部标记 Ok。
**含义**：Jaeger UI 上 401/403/429 不会显示为红色 span，搜 "errors" 时漏掉客户端错误。
**修复方向**：4xx 应标记为 Warning（OTel 无 Warning，可用 attribute `error.kind=client`）。

### 5.9 zap Production Logger 的级别

`zap.NewProduction()` 默认：
- Level: `Info` 及以上。
- Output: stderr。
- Encoder: JSON（与 ELK/Loki 友好）。
- DPanic/Panic/Fatal 仍会终止进程。

**Debug 级别在生产被丢弃**。开发本地用 `zap.NewDevelopment()` 切到 Debug + 友好控制台格式。
当前 `main.go:106` 硬编码 NewProduction，**无环境切换**。生产想临时开 debug log 必须改代码 + 重启。
**修复方向**：根据 `config.Logging.Level` 切换 logger 构造。

### 5.10 Pool 的 cap-cap 限制（避免内存膨胀）

`pool.go:46-49`:

```go
if cap(*bp) > p.size*8 {
    return    // discard oversized
}
```

如果 caller 把池子里取出的 buffer 扩到 8× 初始 cap 以上，Put 时直接丢弃，让 GC 回收。这避免少数大请求让 pool 永久持有 100MB 缓冲区。

`BufferPool.Put` 同理：`cap > 1MB` 直接丢弃。

### 5.11 RPCRequestPool 的 Get 重置策略

`pool.go:152-158`：

```go
req.JSONRPC = "2.0"
req.ID = 0
req.Method = ""
req.Params = nil
```

每次 Get **总是覆盖所有字段**——不能依赖 pool 复用时字段保留。如果某处忘了赋值（比如 `ID`），仍会得到稳定的零值，不会泄露上次请求的数据。
Put 时 `Params = nil` 是为了让 GC 能回收 Params 引用的对象——否则 pool 持有 RPCRequest，间接持有大 params 对象，造成内存泄露。

### 5.12 SSE 长任务可观测性：心跳事件 + Stream Cache 探针（2026-06-08）

长任务（finalize 阶段单次非流式 LLM 调用可能 20+ 分钟，或工具长跑期间 stdout 沉默）的可观测性是「**前端 watchdog 不能误判为静默 + 运维能从外部判定后端是否真活着**」两个目标的合成。本节记录 06-08 起这两个目标各自的探针。

#### 5.12.1 SSE 心跳从注释行升级为业务 `ping` 事件（P1）

历史上 `runSSEHeartbeat`（`internal/api/handlers.go::runSSEHeartbeat`）每 25s 写 `: ping\n\n` 注释行。注释行**不**触发浏览器 `fetch ReadableStream` 的 `onByte` 回调 → 前端 `lastEventAt` 不重置 → 90s watchdog 在"只剩心跳"的连接上误判为静默超时。

2026-06-08 改为合规业务事件：

```
data: {"type":"ping","ts":<unix_millis>}\n\n
```

可观测性副作用：

- 前端 `onByte` 自然触发，`lastEventAt` 重置 → watchdog 不误判；
- 前端 `traceSteps` filter 显式 drop `type=ping`，不入 UI 列表；
- 服务端日志无新增（心跳路径仍保持 zero log，避免 25s/连接的噪音）；
- 抓包 / Network 面板能直接看到心跳节奏（每 25s 一帧 `data:` 行），便于现场排查。

回归测试 `internal/api/sse_ping_test.go`：双重断言「输出含 `data: {"type":"ping"` 前缀 + **不含** `: ping\n` 注释行」防止 P1 被回退。

#### 5.12.2 `/chat/react-stream/status` 加 `last_event_at_ms` 字段（B3）

`StreamCache.LastEventAt`（`internal/orchestrator/stream_cache.go`）用 `XREVRANGE COUNT 1` 拿最近一条事件的 Redis Stream ID 前段 ms 戳（Redis Stream ID 形如 `1717843200000-0`）。`handleChatReactStreamStatus` 响应新增 `last_event_at_ms`：

```json
{
  "running": true,
  "task_id": "task-abc",
  "event_count": 47,
  "last_event_at_ms": 1717843259812
}
```

用途：

- **前端 polling 兜底**（参见 `17_api.md` §5.3）可拿 `last_event_at_ms` 区分"后端真挂了"vs"后端在跑但 SSE 链路有问题"；
- 运维探针：`curl /api/v1/chat/react-stream/status?session_id=xxx` 配合 `event_count` 单调递增即可判断后端心跳节奏，无需翻日志。

回归测试 `internal/orchestrator/stream_cache_test.go::TestStreamCache_LastEventAt` 覆盖空流 → 0、3 次 Append → ms 戳在 ±10s 窗口、nil 接收者 → 0、空 sessionID → 0 四种边界。

#### 5.12.3 工具长跑业务心跳进入 `tool_progress` 事件总线（B1）

`toolRunWorkspaceCmd`（`file_tools.go::toolRunWorkspaceCmd`）的 5s `tick.C` 心跳通过 `progressCb → react_core.WithProgressCallback("tool_progress") → persistingSink` 写入 Redis Stream，与"真"业务事件同一总线。运维副作用：

- `EventCount` / `LastEventAt` 探针对"心跳填充期"和"真实业务事件期"一视同仁；
- Replay 时心跳事件随业务事件一起回放，refresh 后用户看到的 trace 与原始观察一致；
- `streamMaxLen=2000` 截断阈值无需调整（5min × 5s = 60 条心跳 ≈ 5KB）。

---

## 6. 设计权衡

### 6.1 promauto 全局注册 vs 显式 Registry

**当前**：promauto + DefaultRegisterer。
**替代**：每个测试创建独立 Registry，避免污染。

为什么不分：
- Prometheus 标准实践就是单 DefaultRegisterer。
- 测试隔离用 `prometheus.NewPedanticRegistry()` + dependency injection，但 metrics.go 是包级变量，无法 DI。
- 当前测试用 `metrics_test.go` 在每个测试前 Reset Counter，麻烦但够用。

代价：metrics 单元测试有时不稳定（race 跨 case 累加）。**已知缺陷，无致命性**。

### 6.2 Head-based 采样 vs Tail-based

| 维度 | Head-based（当前） | Tail-based |
|---|---|---|
| 何时决定保留 | trace 开始 | trace 结束 |
| 慢请求/错误能 100% 保留 | 否（10% 概率漏） | 是 |
| 需要中间 Collector | 否 | 是（OTel Collector） |
| 运维复杂度 | 低 | 中 |
| 适合规模 | 早期 / 小规模 | 中大规模 |

当前选 head-based 是早期阶段的明智决定。规模上去后切 tail-based 不需要改 Agent 代码，只换 Collector 配置。

### 6.3 Audit Logger 走 zap vs 单独文件

**zap.Named("audit")**：
- 输出同进程同 stdout，靠外部 router 分流。
- 优点：单一日志管道，运维简单。
- 缺点：日志总量翻倍（普通 INFO + audit 一起 stdout），可能触发容器日志限速。

**单独 audit 文件**：
- 写 `/var/log/agent/audit.log`。
- 优点：吞吐独立，归档简单。
- 缺点：容器化时写文件不友好，需要 sidecar 收集。

选 zap 路线是为了云原生友好。**当前 audit 路径未接线，问题尚未浮现**。

### 6.4 OTel SDK 直接用 vs 通过 abstraction

当前 `otel.go` 直接调用 `go.opentelemetry.io/otel/sdk/trace`。
**替代**：包一层 `Tracer` interface 让 SDK 可替换。

为什么不抽象：
- OTel SDK 本身已经是 vendor-neutral 抽象（OTLP 协议）。
- 抽象上加抽象只有理论好处，实际从来不需要换 SDK。
- 直接用 SDK 类型让 Trace API 调用方（orchestrator、llm）直接看到 `trace.Span`，IDE 提示好。

代价：升级 OTel 大版本时需要改多处类型签名。**接受**。

---

## 7. 后续演进

### 7.1 短期（1-2 sprint）

| 项 | 优先级 | 描述 |
|---|---|---|
| 接线 audit logger | P0 | main.go 创建 auditLogger 并注入 orchestrator/api/sandbox |
| HITL approval 写 audit | P0 | orchestrator.handleApprovalResponse → LogApproval |
| sandbox 执行写 audit | P1 | sandbox.Manager.Run 完成时 LogSandboxExec |
| MCP call 写 audit | P1 | mcp.Gateway.CallTool 完成时 LogMCPCall |
| errors 中间件 + 全包替换 | P2 | 加 errorHandler middleware，handlers 改用 AgentError |
| Pool 接入 RAG/MCP/sandbox | P2 | 验证 GC 压力后决定具体位置 |
| Logger level 配置化 | P3 | 支持 `cfg.Logging.Level` 切 zap |
| Tracing endpoint timeout 缩短 | P3 | 10s → 2s + 后台 retry |

### 7.2 中期

| 项 | 描述 |
|---|---|
| Tail-based sampling | 部署 OTel Collector + Tempo，切到 tail-based |
| Cost 指标 relabel drop | scrape 后 drop session_id/user_id/task_id 三个高基数 label |
| Alerting rules | Prometheus 告警规则：LLM 错误率 / 沙箱 OOM / 限流触发 |
| Metrics in test | 引入 `testutil.CollectAndCompare` 替代 Reset hack |
| 4xx → span warning | 改 tracing.GinMiddleware 区分 client/server error |

### 7.3 长期

| 项 | 描述 |
|---|---|
| Continuous profiling | pprof 长期采样接入 Pyroscope/Parca |
| eBPF tracing | 无侵入抓取 syscall / TCP 重传 |
| Exemplars | Histogram + Exemplar 让 Prometheus 链到 trace |
| SLO 报告 | 自动生成 weekly SLO 报告（基于 metrics） |

---

## 8. 设计教训

### 8.1 "为了未来"造的设施很容易腐烂

`audit`、`errors`、`pool` 三个包都体现这个模式：
- 设计：完备，文档清晰，测试齐全。
- 落地：仅一处或零处调用。

**根因**：包是"自上而下设计"产生的，但其他模块的开发者按"自下而上写需要"工作，发现裸 `fmt.Errorf` 已经够用，不会主动来用 `errors.AgentError`。

**教训**：
- 横切包必须**有强制使用机制**（lint、code review template、middleware 强转）。
- 或者必须**有一个高调的早期使用者**（在 review 时就把所有错误转成 AgentError，让后来者照抄）。
- 否则横切包会变成"博物馆陈列"——存在、文档化、不被用。

### 8.2 Metrics 命名一旦发布就被锁死

`PromptCacheHitRatio` 变量名暗示"hit ratio"，实际暴露的 metric 是 `cache_prefix_hash` Gauge。两者不匹配是历史遗留。

**修复成本**：改 Go 变量名容易，但 Prometheus metric name 一旦被 Grafana dashboard / 告警规则引用，重命名就破坏所有下游。`record` rules 可以做兼容，但要排期。

**教训**：metric 命名要在第一版就**确认含义**。"暂时这样命名后续改" = "永远不会改"。

### 8.3 sync.Pool 不是"加上就快"

pool 包写得很好，但 RAG/MCP 没接是有原因的——profiling 没显示 GC 是瓶颈。盲目加 pool 会：
- 让代码复杂（Get/Put 模板）。
- 阻碍 escape analysis（编译器本可以栈分配的，强制堆分配进 pool）。
- 增加错误风险（Put 后还在用、Reset 漏字段）。

**教训**：性能优化设施先做 profiling 找瓶颈，再决定是否用。不要因为"听说 pool 快"就到处用。

### 8.4 Tracing 启动 fail-open 是对的

main.go:375 不让 tracing 失败炸掉主流程。Jaeger / Tempo 在生产环境是常见故障点（重启、扩容、网络），如果 tracing 故障 = Agent 故障，可用性会很难看。
反过来 metrics 失败也是 fail-open（promauto 写入 DefaultRegisterer 永不失败）。zap 失败 fatal 是因为它是其他启动步骤的前提，不是观测设施本身的问题。

**教训**：可观测性 = **辅助** 系统理解，**不应成为** 主流程的依赖。

### 8.5 高基数 label 是"为了 ToB"，不是"为了好看"

`cost.go` 故意打破低基数规矩。**这是有意识的决定**：
- ToB 客户每月账单是核心需求。
- "这个 session 花了多少钱" 是必答的。
- 用 PG 表也可以，但 Prometheus 现成的 cost-over-time 图很美。

代价是 scrape 后必须用 relabel drop 高基数 label，否则 prometheus storage OOM。

**教训**：业务需求可以打破 best practice，但必须**在代码注释里写明白原因 + 缓解方案**，否则后人 audit 会被吓到然后"修复"破坏功能。

---

## 9. 已知缺陷一览

| 编号 | 级别 | 文件 | 行 | 现象 | 修复建议 |
|---|---|---|---|---|---|
| OBS-1 | P0 | 全局 | — | audit logger 无 production 调用方 | 接线到 HITL/sandbox/MCP |
| OBS-2 | P1 | 全局 | — | errors 包零生产调用 | handlers 改用 AgentError + 加 error middleware |
| OBS-3 | P1 | 多处 | — | pool 包仅 session 使用 | RAG/MCP/sandbox 接入 |
| OBS-4 | P2 | `metrics.go` | 228 | `PromptCacheHitRatio` 变量名与 metric name 不匹配 | 改 Go 变量名（不改 metric name 防破坏 dashboard） |
| OBS-5 | P2 | `cmd/agent/main.go` | 106 | logger level 硬编码 production | 按 `cfg.Logging.Level` 切换 |
| OBS-6 | P2 | `otel.go` | 93 | OTLP dial 10s timeout 拖慢启动 | 缩到 2s + 后台 retry |
| OBS-7 | P3 | `otel.go` | 206-213 | 4xx 全部标 Ok | 客户端错误标 warning |
| OBS-8 | P3 | `audit/logger.go` | 113 | `exit_code` 用 `string(rune(exitCode+'0'))` 转换，>9 错误 | 改 `strconv.Itoa(exitCode)` |

> **OBS-8 解释**：`logger.go:114` 当前 `"exit_code": string(rune(exitCode + '0'))` 仅对 0-9 正确。exit_code=10 时变成字符 ':'（ASCII 58）。是个 bug，但因为没接线所以从未触发。

---

## 10. 测试矩阵

| 测试文件 | 行数 | 覆盖范围 |
|---|---|---|
| `metrics/metrics_test.go` | 113 | metric Register + Inc + Observe 基础 |
| `tracing/otel_test.go` | 106 | Provider 启动 / Shutdown / Sampler 选择 |
| `audit/logger_test.go` | 113 | Event 字段编码 + 三个便利方法 |
| `errors/errors_test.go` | 104 | HTTPStatus 映射 + Wrap / IsCode |
| `pool/pool_test.go` | 127 | Get/Put + oversized 丢弃 + 并发 |

**未覆盖**：
- audit / errors / pool 三个包测试齐全但**生产路径未走过它们**——测试有效不代表系统在用。
- tracing 在 collector 不可达时的降级行为（fail-open 路径无 e2e 验证）。
- cost.go 的高基数 label 在 Prometheus 实际抓取时的内存压力。

---

## 11. 配置示例

```yaml
tracing:
  enabled: true
  endpoint: "otel-collector:4317"
  service_name: "code-agent"
  sample_rate: 0.1
  insecure: true

logging:
  level: "info"        # debug / info / warn / error
  format: "json"       # 当前仅支持 json
```

环境变量：
- `OTEL_EXPORTER_OTLP_ENDPOINT` 可覆盖 endpoint。
- `OTEL_TRACES_SAMPLER_ARG` 可覆盖 sample_rate（OTel SDK 自动识别）。

---

## 12. 跨文档引用

- `03_llm.md` §3 — LLMRequestDuration / cost.go 调用点
- `04_rag.md` §5 — RAGRetrievalDuration / chunks_returned
- `05_sandbox.md` §6 — SandboxExecutionTotal / LogSandboxExec（计划接线）
- `06_mcp.md` §4 — MCPCallTotal / LogMCPCall（计划接线）
- `09_orchestrator.md` §5 — HITLApprovalTotal / LogApproval（计划接线）
- `16_store.md` §4 — `agent_cost_ledger` 表作为 cost 的 durable 存储
- `17_api.md` §5 — tracing/metrics middleware 装配
- `18_auth_security.md` §6.6 — fail-open 监控告警依赖本章 metrics

---

下一篇：[`20_deploy.md`](20_deploy.md) —— Docker / docker-compose / Dockerfile.allinone / 健康探针 / 多副本部署的工程实践。
