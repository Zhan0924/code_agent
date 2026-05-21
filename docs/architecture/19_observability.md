# 19 · 可观测性与基础设施 `metrics` + `tracing` + `errors` + `pool`

> 代码：
> - `internal/metrics/metrics.go` (195) — Prometheus 全量指标定义（27 个 metric，9 个子系统）
> - `internal/tracing/otel.go` (180) — OpenTelemetry Provider + Gin Middleware
> - `internal/errors/errors.go` (126) — 统一错误类型 `AgentError` + 15 种 `Code` + HTTP 映射
> - `internal/pool/pool.go` (185) — sync.Pool 封装（ByteSlice / Buffer / JSONEncoder / RPC）
> - 对应测试：`errors_test.go` / `pool_test.go`

---

## 1. 模块定位

**"让系统透明可诊断"** —— 本章把四个"低调但无处不在"的横切包合并讲：

| 包 | 职责 | 用在哪 |
|---|---|---|
| `metrics` | 暴露 Prometheus 指标 | 全系统热点、SLO、HPA 依据 |
| `tracing` | 分布式追踪（OTel → Jaeger） | 排障 / 性能分析 / 跨服务链路 |
| `errors` | 领域错误类型 | 业务层统一错误表达；HTTP 响应码自动映射 |
| `pool` | 对象复用（sync.Pool） | 热路径 GC 降压；P99 延迟平稳 |

这四个包的共同特点：**底层依赖为零、被其他所有包引用**。所以放在架构的最底层，其他模块无感使用。

---

## 1.5 核心设计问题

### Metrics / Tracing / Logs / Audit 四件套各管什么

```
┌ Metrics (Prometheus) ────── "系统健康不健康"
│   聚合 / 低基数 / 报警                   粒度：请求率、延迟 P99、错误率
│
├ Tracing (OTel)  ─────────── "单次请求为什么慢"
│   单次请求全链路 / span 级              粒度：一次 chat 从进入到返回
│
├ Logs (zap) ───────────────── "当时发生了什么"
│   结构化 / 高基数 / 可搜              粒度：单个动作 / 错误详情
│
└ Audit (audit/Logger) ──── "谁做了什么"（合规要求）
    结构化 + 只增不删 + 归档            粒度：每次敏感操作
```

四件套互补：
- **Metrics 告警** → 发现"有异常"
- **Tracing 定位** → 在哪个 span 卡住
- **Logs 详情** → 当时的参数 / 错误
- **Audit 合规** → 事后审计 / 取证

### 为什么不用 statsd / InfluxDB

Prometheus 的 pull-based + label-based + PromQL 生态最成熟：
- Grafana 原生
- Alertmanager 原生
- Kubernetes 默认采集
- 开源社区大量 exporter

InfluxDB 的 push-based 模式更适合"大量唯一 time series"场景（IoT），
Agent 的指标天然 low-cardinality（几十种 metric × 几个 label），
Prometheus 的 memory footprint 更低。

### Label Cardinality 的诅咒

Prometheus 最大陷阱：`counter.WithLabelValues(user_id, session_id, ...).Inc()`
——user_id / session_id 有数百万取值 → 数百万 time series → Prometheus OOM。

**规则**：
- 允许 label：`method`, `path`（用路由模板）, `status`, `provider`, `model`, `tool_name`
- 禁止 label：`user_id`, `session_id`, `request_id`, `tenant_id`, 任何 UUID

高基数字段应该写**日志**（zap 结构化 field）或 tracing（span attribute），
不是 Prometheus label。

### Tracing Sampling：head vs tail

- **Head-based**（本系统当前）：trace 一开始就决定采不采（10% 采样）
  - ✅ 实现简单，exporter / collector 压力小
  - ❌ 慢请求 / 错误请求可能没被采样 → 观测盲区

- **Tail-based**（生产推荐升级）：trace 完成后根据 trailing metadata 决定
  - ✅ 100% 采样错误和 >P99 请求
  - ❌ OTel Collector 需要缓冲整条 trace，运维复杂

**演进路径**：先 head-based + 高级别错误日志补盲，后续引入 Collector 升级。

---

## 2. 依赖架构

```
┌────────── 业务模块 ──────────┐
│ llm / rag / sandbox / mcp /  │
│ session / orchestrator / api │
└───────────────┬──────────────┘
                │    (调用)
     ┌──────────┼──────────┐
     ▼          ▼          ▼
┌────────┐ ┌────────┐ ┌────────┐
│metrics │ │tracing │ │ errors │   ← 业务主动埋点 / 返回 error
└────┬───┘ └────┬───┘ └────────┘
     │          │
     ▼          ▼
  /metrics   OTLP → Jaeger
  (prom抓)   (可视化)

┌────────────────────────────┐
│  pool (sync.Pool)          │ ← 在 mcp / rag / api handler 等 alloc 热点使用
└────────────────────────────┘
```

---

## 2.5 数据流总览

下图展示一次请求中四个观测信号的产生与汇聚路径：

```text
┌───────────────┐
│ HTTP Request  │
└───────┬───────┘
        │
        ▼
┌─────────────────────────────────────────────────────────────┐
│ Gin Middleware: otelgin.Middleware (Tracing 起点)            │
│  → 创建 root span, 注入 traceID 到 ctx                     │
│  → metrics.HttpRequestTotal.Inc()                           │
└──────────────────────────┬──────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ 业务逻辑 (orchestrator / llm / rag / sandbox)               │
│                                                              │
│  ┌────────────────┐  ┌────────────────┐  ┌──────────────┐  │
│  │ child span     │  │ metrics.*      │  │ zap.Logger   │  │
│  │ (per 子调用)   │  │ .Observe()     │  │ .Info/Error  │  │
│  │ 例: llm_call   │  │ .Inc()         │  │ (结构化字段)  │  │
│  │     rag_query  │  │ 例: duration   │  │ 例: latency  │  │
│  │     sandbox_   │  │     total      │  │     error    │  │
│  │     exec       │  │     in_flight  │  │     tool     │  │
│  └───────┬────────┘  └───────┬────────┘  └──────┬───────┘  │
│          │                   │                   │          │
└──────────┼───────────────────┼───────────────────┼──────────┘
           │                   │                   │
           ▼                   ▼                   ▼
┌────────────────┐  ┌────────────────┐  ┌──────────────────┐
│ 【OTLP gRPC】  │  │ 【Prometheus】 │  │  stdout / file   │
│  → Jaeger      │  │  /metrics 端点 │  │  (JSON Lines)    │
│  (分布式追踪   │  │  → Grafana     │  │  → ELK / Loki   │
│   可视化)      │  │  (看板 + 告警) │  │  (日志检索)      │
└────────────────┘  └────────────────┘  └──────────────────┘
        │                   │                   │
        └───────────────────┼───────────────────┘
                            ▼
              ┌──────────────────────────────┐
              │ audit.Logger.Log(Event)      │
              │  事件类型: login / tool_exec │
              │  / approval / config_change  │
              │  → 独立审计流 (合规留痕)      │
              │  → optional PG 落库          │
              └──────────────────────────────┘


关联分析示例 (一次失败请求):
┌────────┐    traceID     ┌────────────────────────────┐
│ 告警   │──────────────▶ │ Jaeger: 找到失败 span      │
│Grafana │                │ → llm_call span error=503  │
└────────┘                └─────────────┬──────────────┘
                                        │ 同 traceID
                                        ▼
                          ┌────────────────────────────┐
                          │ Loki: 结构化日志           │
                          │ circuit_breaker=open       │
                          │ fallback_provider=used     │
                          └────────────────────────────┘
```

---

## 3. ★ Metrics — 27 个指标，9 个子系统

### 3.1 命名规范

所有指标遵循：

```
code_agent_{subsystem}_{name}[_unit]
```

**9 个 Subsystem**：`llm` / `rag` / `pruner` / `session` / `sandbox` / `mcp` / `hitl` / `api` / `prompt`

### 3.2 指标目录

| Subsystem | 指标 | 类型 | 关键 label |
|---|---|---|---|
| **llm** | `request_total` | CounterVec | provider, model, status |
| | `request_duration_seconds` | HistogramVec | provider, model |
| | `tokens_used_total` | CounterVec | provider, type (prompt/completion) |
| | `circuit_breaker_state` | GaugeVec | provider  (0=closed 1=half 2=open) |
| | `fallback_total` | Counter | — |
| **rag** | `retrieval_duration_seconds` | Histogram | — |
| | `chunks_returned` | Histogram | — |
| **pruner** | `tokens_saved_total` | Counter | — |
| | `chunks_pruned_total` | Counter | — |
| **session** | `active_count` | Gauge | — |
| | `cold_archive_total` | Counter | — |
| | `context_compression_total` | Counter | — |
| **sandbox** | `execution_total` | CounterVec | language, status (success/failed/timeout/oom) |
| | `execution_duration_seconds` | HistogramVec | language |
| **mcp** | `call_total` | CounterVec | server, tool, status |
| | `call_duration_seconds` | HistogramVec | server |
| **hitl** | `approval_total` | CounterVec | decision (approved/rejected/timeout) |
| | `pending_count` | Gauge | — |
| **api** | `request_total` | CounterVec | method, path, status |
| | `request_duration_seconds` | HistogramVec | method, path |
| | `websocket_connections` | Gauge | — |
| **prompt** | `cache_prefix_hash` | GaugeVec | hash (Prompt KV 缓存命中诊断) |

### 3.3 Histogram Buckets 定制

**不同子系统用不同 buckets**（不统一用 DefBuckets），因为各自 latency 分布差异巨大：

| 子系统 | Buckets (秒) | 理由 |
|---|---|---|
| llm | 0.1, 0.5, 1, 2, 5, 10, 30, 60 | LLM 请求 100ms~60s 都正常 |
| rag | 0.01, 0.05, 0.1, 0.25, 0.5, 1 | 向量检索应该亚秒 |
| sandbox | 0.1, 0.5, 1, 5, 10, 30, 60, 120 | 代码执行短则 100ms，长可 2 min |
| mcp | 0.05, 0.1, 0.5, 1, 5, 10 | RPC 调用典型 100ms~5s |
| api | `prometheus.DefBuckets` (5ms~10s) | 标准 HTTP latency |

**Bucket 错了**会导致 P99 失真（全进最大 bucket 或都进最小），是初学者最容易踩的坑。

### 3.4 使用方式（`promauto`）

```go
import (
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promauto"
)

var LLMRequestTotal = promauto.NewCounterVec(...)  // 自动注册到默认 Registry
```

`promauto` 比 `prometheus.NewCounterVec` 的好处：**自动 Register**，写一行即用，不需要在 init 函数里 `Register`。

### 3.5 业务里的埋点模式

```go
// LLM 调用
start := time.Now()
resp, err := provider.Chat(ctx, req)
metrics.LLMRequestDuration.WithLabelValues("openai", "gpt-4").Observe(time.Since(start).Seconds())
if err != nil {
    metrics.LLMRequestTotal.WithLabelValues("openai", "gpt-4", "error").Inc()
} else {
    metrics.LLMRequestTotal.WithLabelValues("openai", "gpt-4", "success").Inc()
    metrics.LLMTokensUsed.WithLabelValues("openai", "prompt").Add(float64(resp.Usage.PromptTokens))
    metrics.LLMTokensUsed.WithLabelValues("openai", "completion").Add(float64(resp.Usage.CompletionTokens))
}
```

### 3.6 关键 SLO 指标组合

```
Availability:  sum(rate(api_request_total{status=~"5.."}[5m])) / sum(rate(api_request_total[5m]))
P95 latency:   histogram_quantile(0.95, rate(api_request_duration_seconds_bucket[5m]))
LLM 健康度:     max by (provider) (llm_circuit_breaker_state)
HITL 积压:      hitl_pending_count
Token 经济性:   sum(rate(llm_tokens_used_total[1h])) by (type)
Sandbox OOM 率: rate(sandbox_execution_total{status="oom"}[5m])
```

这些都可以直接做成 Grafana Dashboard。

---

## 4. ★ Tracing — OpenTelemetry → Jaeger

### 4.1 `tracing.Provider` 初始化（otel.go:53）

```
NewProvider(cfg):
    if !cfg.Enabled: return noop Provider    // 软关

    exporter := otlptracegrpc.New(...)       // OTLP gRPC → Jaeger/collector
    resource := semconv(service.name, service.version, host, runtime)
    sampler := cfg.SampleRate 选:
        >=1 → AlwaysSample
        <=0 → NeverSample
        else → TraceIDRatioBased(rate)
    tp := sdktrace.NewTracerProvider(
        WithBatcher(exporter, MaxBatchSize=512, BatchTimeout=5s),
        WithResource(res),
        WithSampler(sampler),
    )
    otel.SetTracerProvider(tp)
    otel.SetTextMapPropagator(CompositeTextMapPropagator{
        TraceContext, Baggage,
    })
```

**关键决策**：

| 点 | 选择 |
|---|---|
| 协议 | **OTLP/gRPC**（而非 HTTP/JSON） — 性能更好；Jaeger native 支持 |
| 采样率 | 默认 **10%** — 平衡观测颗粒度和 APM 成本 |
| 传播器 | `TraceContext` + `Baggage` — W3C 标准；兼容所有 OTel 生态 |
| Batch 参数 | 512 spans / 5 秒 — 既不高频打爆 collector，也不丢最近 trace |
| 关闭时 | `Shutdown(ctx)` flush 未导出 span — 不丢数据 |

### 4.2 Gin Middleware（otel.go:135）

每个 HTTP 请求创建一个 **Server Span**：

```
GinMiddleware(serviceName):
    tracer := otel.Tracer(serviceName)
    return func(c):
        // 1. 从 HTTP header 提取父 trace（W3C traceparent）
        ctx := propagator.Extract(c.Request.Context(), HeaderCarrier(c.Request.Header))

        // 2. 创建 Server span
        spanName := c.FullPath() || c.Request.URL.Path
        ctx, span := tracer.Start(ctx, method + " " + spanName,
            WithSpanKind(Server),
            WithAttributes(http.request.method, url.path, client.address),
        )
        defer span.End()

        // 3. 注入 traced context 到 request
        c.Request = c.Request.WithContext(ctx)

        c.Next()

        // 4. 记录响应
        status := c.Writer.Status()
        span.SetAttributes(http.response.status_code, body.size)
        if status >= 500:
            span.RecordError(errors.New(...))
            span.SetStatus(codes.Error, ...)
        else:
            span.SetStatus(codes.Ok, "")
```

### 4.3 Trace Propagation（全链路）

```
[Client]
    │ traceparent: 00-<trace_id>-<span_id>-01
    ▼
[Gin middleware]
    Extract → Create Server Span → c.Request.Context()
    │
    ▼
[handler] → [orchestrator]
    │ 继承 ctx
    ▼
[llm.Client.Chat(ctx)]
    tracer.Start(ctx, "llm.chat")     // Child span
    │ HTTP 出站 自动注入 traceparent
    ▼
[OpenAI API]   ← 外部，不被我们追踪

[orchestrator] → [sandbox.Execute(ctx)]
    tracer.Start(ctx, "sandbox.run")
    │ Docker API 调用也继承

[orchestrator] → [mcp.Call(ctx, server, tool)]
    tracer.Start(ctx, "mcp.call")
    │ OTLP carrier 会把 traceparent 注入 JSON-RPC 请求
```

**效果**：在 Jaeger UI 里，一次 `/chat` 请求能看到完整 waterfall：`api → orchestrator → llm → rag → mcp → sandbox`，每一段的耗时、错误都直观显示。

### 4.4 与 Metrics 的**互补关系**

|  | Metrics | Tracing |
|---|---|---|
| **采样** | 全量聚合（无采样） | 10% 采样 |
| **粒度** | 聚合（count / sum / histogram bucket） | 单请求 |
| **查询** | PromQL（时序） | TraceID 查特定请求 |
| **成本** | 极低（几 KB/min） | 高（每 span ~1KB，存储压力大） |
| **用途** | 监控、告警、SLO、HPA | 排障、性能分析 |

二者 **不是替代关系**，生产环境都需要。

---

## 5. ★ Errors — 分层错误模型

### 5.1 `AgentError` 结构

```go
type AgentError struct {
    Code    Code    `json:"code"`      // 机器可读
    Message string  `json:"message"`   // 人类可读
    Detail  string  `json:"detail,omitempty"`
    Cause   error   `json:"-"`         // wrap 底层 err
}

func (e *AgentError) Error() string
func (e *AgentError) Unwrap() error    // 支持 errors.Is/As
func (e *AgentError) HTTPStatus() int  // 自动映射 HTTP 状态码
```

### 5.2 `Code` → HTTP Status 映射表

| Code | HTTP | 场景 |
|---|---|---|
| `NOT_FOUND` | 404 | session / task / skill 不存在 |
| `INVALID_INPUT` | 400 | JSON 解析失败、参数非法 |
| `UNAUTHORIZED` | 401 | JWT 缺失/无效 |
| `FORBIDDEN` | 403 | RBAC 拒绝 |
| `CONFLICT` | 409 | 重复创建、并发写冲突 |
| `RATE_LIMITED` | 429 | 限流 |
| `TIMEOUT` | 504 | 上游超时 |
| `UNAVAILABLE` | 503 | LLM/Qdrant/Docker down |
| `INTERNAL` | 500 | 兜底 |
| `LLM_FAILURE` | 500 | LLM 专类 5xx |
| `SANDBOX_FAILURE` | 500 | 沙箱异常 |
| `RAG_FAILURE` | 500 | Qdrant 查询失败 |
| `MCP_FAILURE` | 500 | MCP server 调用失败 |
| `APPROVAL_PENDING` | **202 Accepted** | 任务挂起等待审批 |
| `APPROVAL_DENIED` | 403 | 拒绝授权 |

**202 是刻意设计** —— HITL 挂起既不是成功（还没结果）也不是失败，用 202 "Accepted but not yet processed" 语义最合适。

### 5.3 Constructor Helpers

避免业务代码到处写 `&AgentError{Code: ...}`：

```go
errors.New(code, msg)
errors.Wrap(code, msg, cause)                    // 包原 err
errors.NotFound(resource, id)                    // "session not found: abc"
errors.InvalidInput(detail)
errors.Unavailable(service, cause)
errors.LLMFailure(cause)
errors.SandboxFailure(cause)
errors.RateLimited()
errors.IsCode(err, CodeNotFound)                 // 适配 errors.Is 语义
```

### 5.4 API 层统一处理

```go
// handler
if err != nil {
    var agentErr *errors.AgentError
    if errors.As(err, &agentErr) {
        c.JSON(agentErr.HTTPStatus(), agentErr)
        return
    }
    // fallback: 未包装的 err 视作 500
    c.JSON(500, gin.H{"code": "INTERNAL", "message": err.Error()})
}
```

**设计点**：

- JSON 序列化时 `Cause` 标注 `-`，**不把内部原因泄给客户端**（安全）；
- `Message` 是面向用户的简短描述；`Detail` 可选放更多线索；
- `errors.Is / As` 完全支持（因为实现了 `Unwrap`）。

---

## 6. Pool — sync.Pool 封装

### 6.1 4 种池

| Pool | 用途 | 初始容量 | 上限（超则丢弃） |
|---|---|---|---|
| `ByteSlicePool` | 通用字节切片 | 可配 | `size * 8` |
| `BufferPool` | `bytes.Buffer` | 4KB | 1MB |
| `JSONEncoderPool` | 包装 BufferPool 做 JSON 编码 | — | — |
| `RPCRequestPool` | JSON-RPC 2.0 请求结构体 | — | — |

### 6.2 全局单例（pool.go:170）

```go
SmallBytePool    = NewByteSlicePool(4096)    // 4K：sandbox I/O、短响应
LargeBytePool    = NewByteSlicePool(65536)   // 64K：AST chunks、代码文件
GlobalBufferPool = NewBufferPool()
GlobalJSONPool   = NewJSONEncoderPool()
GlobalRPCPool    = NewRPCRequestPool()
```

业务方直接 `pool.SmallBytePool.Get()` / `.Put(bp)`，不需要各自建 Pool。

### 6.3 两个关键防坑点

#### (a) Put 时检查容量上限

```go
func (p *ByteSlicePool) Put(bp *[]byte) {
    if cap(*bp) > p.size*8 {
        return   // 巨大切片丢弃，防 pool 无限吃内存
    }
    *bp = (*bp)[:0]
    p.pool.Put(bp)
}
```

**问题**：sync.Pool 自身不限制对象大小；如果业务偶尔放进去一个 100MB 的切片，pool 就长期持有那么大内存 —— 等同于"内存泄漏"。

**解法**：Put 时检查 `cap > 阈值` 直接 discard。

#### (b) Put 前清引用，防 GC 抓不到

```go
func (p *RPCRequestPool) Put(req *RPCRequest) {
    req.Params = nil   // 清掉 interface 引用，params 底下的对象才能被 GC
    p.pool.Put(req)
}
```

**如果不 nil**：pool 里的 RPCRequest 会一直持有上次的 Params（可能是个大 JSON AST），GC 抓不到 —— 同样是内存泄漏。

### 6.4 何时值得用池？Benchmark 原则

**不是所有分配都值得池化**。经验法则：

1. **单次 alloc 大**（> 1KB）：值得池；
2. **频率极高**（>1k/s）：值得池；
3. **对象**重置简单（reset 成本 << 分配成本）：值得池；
4. **生命周期短**（ms 级）：值得池；
5. 其他情况：**不值得**，让 GC 自己管更简单。

本项目池化的是：
- Sandbox I/O 行缓冲（每个容器 stdout 每行都用）；
- MCP JSON-RPC 消息（每次 Tool Call 几百 byte）；
- AST chunks（indexer 批量扫仓库时）；
- Gin API handler 里的 JSON 编解码缓冲。

这些都满足 4 条以上。

---

## 7. 合力：一次失败请求的观测全景

**场景**：用户发 `/chat` 请求，orchestrator 触发 LLM 调用，LLM provider 返回 503。

```
[客户端]
    ├─ 收到：HTTP 503  {code:"LLM_FAILURE",message:"upstream unavailable"}
    │                (来自 AgentError 的 JSON 序列化)
    │
    └─ Headers 里有 X-Request-ID: 7f3a... (requestIDMiddleware 注入)

[服务端日志]
    ├─ [audit]     type=llm_failure request_id=7f3a... session=abc
    ├─ [access]    method=POST path=/chat status=503 latency=2.3s request_id=7f3a...
    └─ [error]     "llm chat failed: connection refused" stack=...

[Metrics]
    code_agent_llm_request_total{provider="openai", model="gpt-4", status="error"} +1
    code_agent_llm_circuit_breaker_state{provider="openai"} = 2 (open)    ← 熔断触发
    code_agent_llm_fallback_total = +1                                    ← 路由切换到备用
    code_agent_api_request_total{method="POST", path="/chat", status="503"} +1

[Tracing (Jaeger)]
    Trace 7f3a... ────────────────────────────── 2.3s
      ├─ POST /chat                             [Server, 2.3s]
      │   ├─ orchestrator.ProcessMessage      [Internal, 2.25s]
      │   │   ├─ session.GetMessages            [Internal, 5ms]
      │   │   ├─ context.BuildPrompt            [Internal, 10ms]
      │   │   ├─ llm.Chat (openai)              [Client, 2.1s]  ❌ ERROR
      │   │   │     status.code = ERROR
      │   │   │     error = "connection refused"
      │   │   └─ llm.Chat (fallback claude)     [Client, 0.1s]  ❌ ERROR (circuit open)

[Grafana 面板]
    LLM 5xx rate 警报触发 → PagerDuty → 值班人收到
    → 点开 trace ID 7f3a... 直达 Jaeger → 10 秒定位到 openai 不可用
```

**关键点**：

- **请求 ID + Trace ID 贯穿所有观测面**，让人可以"顺藤摸瓜"；
- Metrics 负责告警；Tracing 负责深挖；Audit + Log 负责合规审计；四者互补。

---

## 8. 设计权衡

| 抉择 | 动机 |
|---|---|
| Metrics 用 **promauto** 而非手动 Register | 一行代码即用；避免 init 函数长链 |
| Metrics **按 subsystem 分 namespace** | Grafana 面板好组织；PromQL 通配简单 |
| Histogram **buckets 按子系统定制** | LLM 60s vs RAG 1s 用同 bucket 会失真 |
| Tracing **OTLP/gRPC** 而非 zipkin/jaeger native | OTel 统一协议；多后端兼容 |
| 默认 **10% 采样** | 100% 存储成本 × 10；低于 1% 排障时漏关键 trace |
| **Batcher 512/5s** | 平衡吞吐 + 延迟 |
| 传播器 **TraceContext + Baggage** | W3C 标准；兼容面最广 |
| 5xx 自动 `SetStatus(Error)` + `RecordError` | Jaeger UI 失败 trace 自动高亮 |
| AgentError **JSON 序列化时隐藏 Cause** | 防内部信息泄露；客户端只见 code+message |
| Error Code **用常量 string 而非 int** | gRPC/HTTP/日志场景都可读；未来对外暴露友好 |
| `APPROVAL_PENDING → 202` | 语义精准；和 200/202/4xx 语义对应清晰 |
| 错误构造 helper（`NotFound`/`RateLimited`/...） | 减少业务方手写 struct 样板；风格统一 |
| Pool 有**容量上限丢弃机制** | 防止"巨物"长期占用 pool ≈ 内存泄漏 |
| Pool Put 前**清引用** | sync.Pool 本身不清；不清就变泄漏 |
| Pool 提供 **Global 单例** | 大部分调用方无需自建 pool；减少实例数 |
| 四包 **底层依赖为零** | 可被其他任何模块引用，无循环依赖风险 |

---

## 9. 后续演进

- [ ] **Metrics exemplars**：把 traceID 嵌入 histogram observation → Grafana 从 Metrics 一键跳 Trace；
- [ ] **Runtime metrics**：`go_gc_duration / go_goroutines / go_memstats` 已由 prom client 自动注册，但需额外暴露；
- [ ] **Request-level sampling**：按 userRole / path 动态调整采样率（debug=100%、prod readonly=1%）；
- [ ] **Log correlation**：zap logger 自动注入 `trace_id` / `span_id` 字段（现在靠 request_id，trace 关联还缺一步）；
- [ ] **AgentError → gRPC Status**：未来多服务通讯时做映射；
- [ ] **结构化 error catalog**：所有 Code 集中文档化，前端 i18n 按 code 翻译；
- [ ] **Metric 告警预置**：给 prometheus 打包 alerting rule（P99 SLO / error rate / circuit open）；
- [ ] **Pool 监控**：pool hit/miss rate、平均对象大小 —— 目前 sync.Pool 不暴露统计；
- [ ] **eBPF 零侵入 profiling**：用 parca / pyroscope 做持续 profiling；
- [ ] **OpenTelemetry Logs**：把 zap log 也通过 OTLP 发出去，与 trace 合并视图；
- [ ] **Metrics cardinality 守门员**：label 组合爆炸会把 prom OOM；加 lint 规则或 relabel_configs；
- [ ] **Tracing 出站 HTTP 自动注入**：给 http.Client 套 `otelhttp.NewTransport`，目前只 inbound 有 middleware；
- [ ] **Pool benchmark 基线**：CI 跑 `go test -bench -benchmem`，allocation 回归就 fail；
- [ ] **Error code 对 i18n**：目前 Message 硬编码英文，前端承担翻译；
- [ ] **Prometheus 高基数 label 审计**：`path` label 如果含 `:id` 参数会爆炸（handler 已用 `c.FullPath()` 规避，但需 lint 保证）。

---

## 11. 实现剖析与改进方向

### 一次请求的"可观测性足迹"

```text
POST /chat    X-Request-ID=abc
  │
  ├─ Tracing: start span "api.chat"  traceID=xyz
  │   ├─ span event "auth.validate"
  │   ├─ child span "orch.ProcessMessage"
  │   │    ├─ child span "rag.Retrieve"
  │   │    ├─ child span "llm.ChatCompletion" (attributes: provider, model, tokens)
  │   │    └─ child span "sandbox.Execute"
  │   └─ span.End(duration=2.4s)
  │
  ├─ Metrics:
  │   api_request_total{method=POST, path=/chat, status=200}++
  │   api_request_duration_seconds{..} observe 2.4
  │   llm_request_total{provider=anthropic, model=claude-opus-4-6, status=success}++
  │   llm_tokens_used_total{provider=..., kind=prompt}+=800
  │
  ├─ Logs (zap):
  │   [info] msg="request" path=/chat status=200 duration=2.4s request_id=abc
  │   [info] msg="llm call" provider=anthropic duration=1.2s tokens=800
  │
  └─ Audit (如果涉及 HITL):
      EventApprovalRequested task_id=... user=... reason="deploy pattern"
```

**关联四件套的关键字段**：
- `X-Request-ID` — 日志可搜索
- `trace_id` — Tracing UI 关联
- `span_id` — 单次操作级别

### Pros
- ✅ 四件套覆盖完整（metrics / tracing / logs / audit）
- ✅ OTel propagation 自动（Gin middleware 注入）
- ✅ zap 结构化日志配合 Loki / Elasticsearch 好搜
- ✅ promauto 简化 metric 注册

### Cons
- ⚠️ Head-based sampling 漏掉慢请求 / 错误请求（10% 采样）
- ⚠️ Log 没有 level 动态调整（改级别要改 config 重启）
- ⚠️ 没有 RED method（Rate/Errors/Duration）专用 dashboard
- ⚠️ Audit log 仅本地，重启丢数据

### 改进方向
- **P0** — Error 日志自动 100% 记录（不走 sample）
- **P1** — Tail-based sampling：部署 OTel Collector，按 duration > P99 全采样
- **P1** — 默认 Grafana dashboard（infra-as-code 化）
- **P2** — Audit 写 Kafka → S3 长期归档
- **P2** — 按 env 动态调日志级别（`GET /debug/log-level`）

---

下一篇：`20_deploy.md` —— Dockerfile 多阶段 / docker-compose 三种形态 / K8s 清单 / CI 流水线 / Makefile。
