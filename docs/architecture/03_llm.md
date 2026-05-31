# 03 · LLM 模块 `internal/llm`

> 代码：
> - `client.go` (378) — `Client` 客户端 + 双层熔断器 + fallback + provider dispatch + SSRF httpClient
> - `anthropic_provider.go` (453) — Anthropic Messages API **原生协议** Provider（System 抽取 + cache_control + tool_use 双向映射）
> - `openai_provider.go` (564) — OpenAI 兼容协议 Provider（sashabaranov/go-openai SDK）
> - `router.go` (284) — `Router` 三档（Heavy/Medium/Light）动态路由
> - `tokenizer.go` (151) — tiktoken-go 精确计数 + rune 加权快速估算
> - `shared_breaker.go` (162) — 跨副本 Redis Lua 熔断器（fixed-window）
> - `helpers.go` (43) — `Complete()` 便捷封装
> - `_principles.go` (810) — 设计原理与权衡笔记（编译时随包，但仅 `//go:build ignore`-free 的文档常量）
> - `doc.go` (55) — package godoc 入口
>
> 测试：`client_test.go` / `router_test.go` / `tokenizer_test.go` / `anthropic_provider_test.go` (466) / `openai_provider_test.go` (623)

---

## 1. 模块定位

**"给整个 Agent 系统提供一条高可用、低成本、可观测的 LLM 通道——并在 Anthropic 与 OpenAI 兼容这两族协议上同时跑得好。"**

承担五件事：

1. **协议适配** — `Provider` 接口抹平 Anthropic 原生协议与 OpenAI 兼容协议两套世界（不是单一 OpenAI-compat 抽象）；
2. **高可用** — 本地 gobreaker + 跨副本 SharedBreaker 双层熔断 + 主备 fallback，单 provider 抖动不拖垮 Agent；
3. **成本控制** — `Router` 按任务复杂度三档路由（Heavy/Medium/Light），简单任务不浪费 Opus；
4. **结构化输出** — 透传 `ChatRequest.ResponseFormat`（text/json_object/json_schema）让上层拿到机器可解析的回复；
5. **可观测** — 每次调用打 5 个 Prometheus 维度（延迟、token、状态、熔断器状态、路由 tier）。

---

## 1.5 设计哲学：LLM 层要回答的 5 个根本问题

### Q1 — 抽象到什么粒度？

**选项**：
- (A) 每个 provider 独立 SDK 直接给 orchestrator 用
- (B) 定义统一 `Provider` 接口，每个 provider 实现
- (C) 再往上抽 `Router`，按负载/成本路由

**决策**：(B) + (C) 局部启用。(A) 让 orchestrator 到处 `if provider == "openai"`；(C) 不是全局必装，目前作为可选组件给 planner / multiagent 调度使用。

**踩过的坑**：早期假设"所有 provider 都能套 OpenAI-compat"，于是只写了 `openai_provider`。后来发现：
- **Anthropic 的 system 消息不在 messages 数组里**——必须作为顶层 `System` 字段单独发；
- **Anthropic 的 prompt caching 用 `cache_control.type=ephemeral`**——OpenAI 没有对等字段；
- **Anthropic 的 tool_use / tool_result 是 ContentBlock**——OpenAI 用扁平的 `tool_calls` / `tool` role。

强行套 OpenAI 协议代理 Anthropic 会损失 cache（每次重发完整 system 没有 cache hit）+ 牺牲 long context 性能。所以 **P0 #21 修正**：写了独立的 `anthropic_provider.go`（453 行）走原生协议。`Provider` 接口对外保持不变，但内部分支不是"翻译成 OpenAI"。

### Q2 — 熔断 vs 重试 vs 降级，三选几？

**场景对应**：
- 熔断（circuit breaker）：防止**持续性**故障放大调用量；
- 重试（retry）：纠正**瞬时性**故障（网络抖动、限流 zeroed）；
- 降级（fallback）：主 provider 挂了用备，保可用性。

**决策**：三个都要，但**层级明确**：

```
请求到来
  │
  ▼
[SharedCircuitBreaker] ─ 跨副本聚合 > 阈值 → 直接 fallback（完全不碰 primary）
  │
  ▼
[Local gobreaker]      ─ 进程内 ConsecutiveFailures ≥ MaxFailures → fallback
  │
  ▼
[Provider.ChatCompletion]
  │
  ├── 瞬时错误（5xx/timeout） → 当前透传给 Client 由 fallback 接管（retryWithBackoff 已定义但未启用）
  └── 持久错误 → SharedBreaker.RecordFailure + fallback
              │
              └── 失败 → return errors.Join(primaryErr, fallbackErr)
```

**设计争议点**：是否在**流式**路径也熔断？

当前答案：**仅 setup 阶段**。一旦 stream 开始（首 chunk 已发给前端），mid-stream 错误切 fallback 会让前端看到两段拼接的回答——比直接报错更糟。所以 `ChatCompletionStream` 只在 `breaker.Execute` 拿到 channel 之前熔断；channel 一旦返回，错误透传给前端由 SSE 关闭通道。

### Q3 — Token 计数：精确 vs 够用？

**问题**：每步都要判断"prompt + 预留 completion 还剩多少"。算错 → 要么 LLM 报 `context_length_exceeded`，要么白白留空间降低并发。

**双轨方案**（`tokenizer.go`）：

| API | 实现 | 准确率 | 单次耗时 | 适用场景 |
|-----|------|--------|----------|----------|
| `ExactTokenCount(text)` | `pkoukk/tiktoken-go` cl100k_base | 100%（GPT 系列）/ 近似（Claude） | ~80μs | LLM 调用前最终预算校验 |
| `FastEstimate(text)` | rune 加权（CJK=1，ASCII 4 字符=1，标点=1） | ~85% | ~5μs | 批量打分（RAG 检索 1000 chunks）、消息追加时滑窗判断 |

`EstimateTokens(text)` 是 `FastEstimate` 的 backward-compat 别名（`client.go:337`）。

**延迟初始化**：`globalTokenizer` 通过 `sync.Once` 初始化；离线环境下载 tiktoken 数据失败 → `ExactTokenCount` **自动降级** `FastEstimate`，不报错（`tokenizer.go:88`）。

**为什么不在所有场景都用 ExactTokenCount？** 80μs × 1000 chunks = 80ms，对 RAG 检索 hot path 是灾难。tiktoken 在"预算最终校验"这种 1 次/请求 的位置才划算。

### Q4 — 是否暴露 provider 细节给上层？

**选项**：`ChatRequest.Model` 是否让 orchestrator 指定模型？
- (A) 暴露——灵活
- (B) 不暴露——只选 provider，model 由配置决定

**决策**：(A)。同一 provider 不同场景需要不同 model（意图分类用 haiku，主聊用 opus）。Orchestrator 知道自己的成本敏感度，Provider 层不该越俎代庖。但 `provider` 字符串本身由 config 决定（`cfg.Primary.Provider = "anthropic" | "openai"`，client.go:143）。

### Q5 — 结构化输出怎么穿透到 provider？

**问题**：orchestrator 偶尔需要让 LLM 直接返回 JSON（agentloop 的 trajectory hint / planner 的 DAG / generator 的项目骨架）。OpenAI 用 `response_format: {type: "json_schema", json_schema: {...}}`；Anthropic 没有显式 ResponseFormat，但有 `tool_use` 强约束输出。

**决策**：`ChatRequest.ResponseFormat *models.ResponseFormat`（指针，可选）从 Client 透传到 Provider，由各 Provider 自己决定怎么实现：

- **OpenAI Provider**：透传给 SDK `openai.ChatCompletionRequest.ResponseFormat`；
- **Anthropic Provider**：当前实现忽略（依赖 caller 用 `Tools` + 强约束 prompt 达到 JSON 输出，待 P1 完善为自动转 `tool_use` 强制 schema）。

这样上层只关心"我要 JSON"，下层选合适手段实现。

---

## 2. 依赖架构

```
    ┌──────────── orchestrator / planner / generator ────────────┐
    │ .ChatCompletion(req) / .ChatCompletionStream(req) / .Complete(...)
    └────────────────────┬───────────────────────────────────────┘
                         ▼
        ┌────────────────────────────────────────┐
        │ llm.Client                             │
        │  - sharedBreaker *SharedCircuitBreaker │  (跨副本 Redis Lua)
        │  - breaker       *gobreaker.CircuitBreaker (本地 5 次连失败跳)
        │  - primary       Provider              │
        │  - fallback      Provider              │
        │  - logger        *zap.Logger           │
        └──────────┬───────────────┬─────────────┘
                   │ Switch cfg.Primary.Provider
       ┌───────────┴──┐         ┌──┴────────────┐
       ▼              ▼         ▼               ▼
 ┌────────────┐  ┌────────────┐  ┌──────────────────────┐
 │ anthropicProvider │  │ openaiProvider │  │ (future: gemini/etc) │
 │ Messages API  │  │ go-openai SDK │  │                      │
 │ 原生协议     │  │ OpenAI-compat │  │                      │
 └──────┬───────┘  └──────┬───────┘  └──────────────────────┘
        │                 │
        │ httpClient (SSRF-protected, optional)
        ▼                 ▼
   Anthropic API     OpenAI / Azure / Ollama / vLLM / 任意 compat 代理
```

**关键观察**：
- Provider 不是单一 OpenAI-compat 抽象。**两族独立实现**（anthropic_provider 453 行 ≠ openai_provider 564 行 的简单换皮）；
- `httpClient` 由外部注入（`NewClientWithOptions`），用于做 **SSRF 防御**——出站请求经过 CIDR 白名单的 `*http.Client`，禁止打到内网；
- Router 是**独立可选组件**，不在 Client 调用链上（Router 决定 model 字符串后由调用方填到 `ChatRequest.Model`）。

---

## 2.5 数据流总览

### 2.5.1 `ChatCompletion` 完整调用链（Anthropic primary）

```text
caller                Client          SharedBreaker    gobreaker    anthropicProvider     Anthropic API
  │                      │                  │             │                │                    │
  │── ChatCompletion ────▶                  │             │                │                    │
  │                      │── Allow(prov)────▶                              │                    │
  │                      │ Lua EVAL: GET key                               │                    │
  │                      │◀──── n < threshold ──                            │                    │
  │                      │                  │             │                │                    │
  │           [shared open?]                              │                │                    │
  │              ├─ yes ─▶ fallbackChat() ──── fallback provider ─────────▶│                    │
  │              │       └─── return ◀                                     │                    │
  │              └─ no ─┐                    │             │                │                    │
  │                      │── breaker.Execute(fn) ──────────▶                │                    │
  │                      │                  │ Counts.ConsecutiveFailures   │                    │
  │                      │                  │ < MaxFailures(5)             │                    │
  │                      │                  │  ────────────▶ fn()         │                    │
  │                      │                  │             │  convertMessages(msgs):              │
  │                      │                  │             │    extract System messages           │
  │                      │                  │             │    build []MessageParam              │
  │                      │                  │             │    cache_control=ephemeral on system │
  │                      │                  │             │    cache_control on m.CacheControl   │
  │                      │                  │             │  POST /v1/messages ─────────────────▶│
  │                      │                  │             │                │  ...   ◀───────────│
  │                      │                  │             │  for block in resp.Content:          │
  │                      │                  │             │    "text" → content.WriteString      │
  │                      │                  │             │    "tool_use" → ToolCall{ID,Name,Args}│
  │                      │                  │             ◀── *ChatResponse                      │
  │                      │                  ◀────── result, err ─────                            │
  │           [err != nil?]                                                                       │
  │              ├─ yes ─▶ sharedBreaker.RecordFailure(prov)                                     │
  │              │         (Lua: INCR + EXPIRE window)                                           │
  │              │       ─▶ fallbackChat()                                                       │
  │              └─ no ─▶ metrics.LLMRequestTotal{provider,model,"success"}.Inc()                │
  │                      ─▶ LLMTokensUsed{provider,"prompt"}.Add(InputTokens)                    │
  │                      ─▶ LLMTokensUsed{provider,"completion"}.Add(OutputTokens)               │
  │                      ─▶ recordBreakerState() Gauge = 0|1|2                                   │
  │◀──── ChatResponse / err ───────                                                              │
```

### 2.5.2 `ChatCompletionStream`（仅 setup 阶段熔断）

```text
ChatCompletionStream
   │
   ▼
[SharedBreaker.Allow?]
   │ no  → fallback.ChatCompletionStream()
   │ yes
   ▼
[breaker.Execute(fn)] fn = anthropicProvider.ChatCompletionStream:
   ├─ params := build MessageNewParams (含 System + cache_control + Tools)
   ├─ stream := client.Messages.NewStreaming(timeoutCtx, params)
   ├─ ch := make(chan StreamChunk, 64)
   └─ go func() {
        for stream.Next() {
          event := stream.Current()
          switch event.Type {
            case "content_block_start":
              if event.ContentBlock.Type == "tool_use":
                toolCallsMap[idx] = &ToolCall{ID, Name, Args:""}
            case "content_block_delta":
              if delta.Type == "text_delta":
                ch <- StreamChunk{Content: delta.Text}    ← 实时 push 文本
              if delta.Type == "input_json_delta":
                toolCallsMap[idx].Args += delta.PartialJSON  ← 累积 JSON
            case "content_block_stop":
              ch <- StreamChunk{ToolCalls: [*toolCallsMap[idx]]}  ← 整工具调用
            case "message_stop":
              ch <- StreamChunk{Done: true}; return
            case "error":
              ch <- StreamChunk{Err: ..., Done: true}; return
          }
        }
      }()
   ▼
return ch  ← setup 已成功，错误从此透传给 channel
```

**关键设计**：Anthropic SSE 用 `content_block_*` 事件分块描述 tool_use 的 JSON 参数（增量传输），需要按 index 在 map 里**累积**直到 `content_block_stop` 才能 emit 整 ToolCall。OpenAI 的 SDK 是自己 concat 好的，所以 anthropicProvider 这部分要手动写状态机（`anthropic_provider.go:237-301`）。

---

## 3. 核心接口：`Provider`

```go
type Provider interface {
    ChatCompletion(ctx context.Context, req *ChatRequest) (*ChatResponse, error)
    ChatCompletionStream(ctx context.Context, req *ChatRequest) (<-chan StreamChunk, error)
    Name() string
}
```

数据类型同包：

```go
type ChatRequest struct {
    Messages       []models.Message        `json:"messages"`
    Tools          []models.ToolDefinition `json:"tools,omitempty"`
    MaxTokens      int                     `json:"max_tokens,omitempty"`
    Temperature    float32                 `json:"temperature,omitempty"`
    Model          string                  `json:"model,omitempty"`
    ResponseFormat *models.ResponseFormat  `json:"response_format,omitempty"`  // NEW(P0 #18)
}

type ChatResponse struct {
    Content   string            `json:"content"`
    ToolCalls []models.ToolCall `json:"tool_calls,omitempty"`
    Usage     Usage             `json:"usage"`
    Model     string            `json:"model"`
}

type Usage struct {
    PromptTokens     int
    CompletionTokens int
    TotalTokens      int
}

type StreamChunk struct {
    Content   string            `json:"content,omitempty"`
    ToolCalls []models.ToolCall `json:"tool_calls,omitempty"`
    Done      bool              `json:"done"`
    Err       error             `json:"-"`
}
```

> 与 `models.Message` / `models.ToolDefinition` / `models.ResponseFormat` 共用一套类型——避免 OpenAI 类型在业务代码扩散。

---

## 4. `Client`：双层熔断 + 主备壳

### 4.1 结构（`client.go:115`）

```go
type Client struct {
    primary       Provider
    fallback      Provider               // 可 nil
    breaker       *gobreaker.CircuitBreaker  // 本地：仅包 primary
    sharedBreaker *SharedCircuitBreaker      // 跨副本：可 nil
    logger        *zap.Logger
}
```

### 4.2 三个构造器（`client.go:124-193`）

| 构造器 | 用途 |
|--------|------|
| `NewClient(cfg, logger)` | 最简——无 SharedBreaker、用默认 HTTP client |
| `NewClientWithSharedBreaker(cfg, shared, logger)` | 启用跨副本熔断 |
| `NewClientWithOptions(cfg, shared, httpClient, logger)` | 全选项——传入自定义 `*http.Client`（SSRF 防御）|

所有构造器最终都走 `NewClientWithOptions`。Provider dispatch 在这里发生：

```go
switch cfg.Primary.Provider {
case "anthropic":
    primary, err = newAnthropicProvider(&cfg.Primary, httpClient, logger)
default:
    // openai / ollama / vllm / azure / 任意 OpenAI-compat 代理
    primary, err = newOpenAIProvider(&cfg.Primary, httpClient, logger)
}
```

Fallback 同样按 `cfg.Fallback.Provider` dispatch（`client.go:155-166`），但 fallback 创建失败**不致命**——日志 Warn 后 `fallback == nil`，Client 退化为"仅 primary"模式。

### 4.3 SSRF 防御：`httpClient` 参数（P0 #19）

`NewClientWithOptions` 的 `httpClient *http.Client` 由 `cmd/agent/main.go` 注入。当 `cfg.Security.EgressEnabled = true` 时构造链如下（`cmd/agent/main.go:147-163`）：

```go
policy := &security.EgressPolicy{
    Enabled:       true,
    DefaultAction: "deny",
    AllowedHosts:  cfg.Security.EgressAllowedHosts,   // 域名/通配符白名单
    DNSAllowed:    true,
}
egressValidator, _ := security.NewEgressValidator(policy, logger)
egressHTTPClient := security.NewEgressHTTPClient(egressValidator, cfg.LLM.Primary.Timeout)
llmClient, _ := llm.NewClientWithOptions(&cfg.LLM, nil, egressHTTPClient, logger)
```

`security/egress.go` 是**两层防御**：
- **L1**：`EgressTransport` 包装 `http.RoundTripper`，请求阶段按 host 校验 `AllowedHosts` 白名单；
- **L2**：自定义 `DialContext` 解析出实际 IP 后再查一遍（白名单 IP / CIDR、私有网段、loopback、link-local）—— 这是**真正的 SSRF 防御**，防止域名→IP 解析阶段被攻击者污染。

这防止"用户上传一段 prompt，里面让 LLM 调用工具，工具又通过 baseURL 配置打到 169.254.169.254 拿 AWS IAM token"这种 SSRF 攻击链。`EgressEnabled=false` 时 `egressHTTPClient = nil`，Client 退化为用 SDK 默认 HTTP client（开发态常态）。

### 4.4 本地熔断器（`client.go:169-184`）

```go
cbSettings := gobreaker.Settings{
    Name:        "llm-primary",
    MaxRequests: uint32(cfg.CircuitBreaker.HalfOpenMaxReqs),
    Interval:    cfg.CircuitBreaker.Timeout,    // ⚠️ 已知 bug：与 Timeout 共用
    Timeout:     cfg.CircuitBreaker.Timeout,    // OPEN → HALF-OPEN 冷却
    ReadyToTrip: func(counts gobreaker.Counts) bool {
        return int(counts.ConsecutiveFailures) >= cfg.CircuitBreaker.MaxFailures
    },
    OnStateChange: func(name, from, to) { logger.Warn(...) },
}
```

**状态机**：

```
CLOSED ─(连续失败≥MaxFailures)→ OPEN ─(冷却 Timeout)→ HALF-OPEN
   ▲                                                          │
   └──────(MaxRequests 个探针全成功)─────────────────────────┘
       ◄────(HALF-OPEN 阶段任一失败)─────── 回到 OPEN
```

`OnStateChange` 回调里 `metrics.LLMCircuitBreakerState.Set(0|1|2)`，Grafana 一眼看出哪个 provider 被熔断。

**已知配置 bug**：`Interval = Timeout`。两字段语义不同：
- `Interval`：滑动计数窗口（过了就清零计数）；
- `Timeout`：OPEN → HALF-OPEN 冷却时间。

间歇性抖动场景下统计不准（应该 `Interval = 2 × Timeout`）。见 §12 P0 修复列表。

### 4.5 跨副本熔断 `SharedCircuitBreaker`（`shared_breaker.go`）

> 💡 **为什么需要**：N 个 Pod 部署时，每个 Pod 看到的失败 ≈ 全集群的 1/N。即使集群整体已 degraded，单个 Pod 永远达不到本地阈值。N 个 Pod 同时砸 sick upstream → 放大灾难。

**数据结构**：固定窗口计数器，键为 `(provider, window_epoch)`：

```
Redis key: llm:breaker:{provider}:{window_epoch}
TTL = window (e.g. 30s)
```

`window_epoch = time.Now().Unix() / window.Seconds()`——同一 30 秒窗口的所有副本共享同一 key。窗口滚动通过 TTL 自然过期，无需显式 reset。

**Lua 脚本**（原子操作）：

```lua
-- sharedBreakerCheckScript
local n = redis.call('GET', KEYS[1])
if n == false then return 0 end
return tonumber(n)

-- sharedBreakerRecordScript
local count = redis.call('INCR', KEYS[1])
if count == 1 then
  redis.call('EXPIRE', KEYS[1], ARGV[1])
end
return count
```

`INCR` 在 key 不存在时自动从 0 开始计——只在第一次 INCR 时设 TTL（避免每次失败都续期导致永不过期）。

**API**：

```go
sharedBreaker.Allow(ctx, provider) bool       // n < threshold
sharedBreaker.RecordFailure(ctx, provider)    // INCR + EXPIRE
```

**与 gobreaker 的组合**（`client.go:198-255`）：

```
1. sharedBreaker.Allow(provider)?       // L1: 跨副本聚合
   no  → 直接 fallbackChat
   yes ↓
2. breaker.Execute(fn)                  // L2: 本地状态机
   primary.ChatCompletion(ctx, req)
3. err != nil:
     sharedBreaker.RecordFailure(prov)  // 写入跨副本计数
     fallbackChat                       // fallback 不走 sharedBreaker
   err == nil:
     metrics 记录 → return resp
```

**设计权衡**：
- **Fixed window** 而非 sliding window：实现简单（一次 Lua EVAL 搞定），精确度够用于 DoS / 降级检测；
- **Fail-open on Redis error**：Redis 挂了 → Allow 永真。可用性 > 严格执行；
- **只 record 失败、不 record 成功**：TTL 到期自动清理，无需显式成功减计数。粗糙但够用；
- **fallback 不走 sharedBreaker**：避免"主备都熔断"的死局，fallback 总有机会跑。

**配置**：

```yaml
llm:
  shared_breaker:
    window: 30s
    threshold: 20    # 30s 内整集群累计 20 次失败即跳开
```

`shared == nil` → Client 退化为仅本地 gobreaker，无 Redis 依赖。

### 4.6 调用路径：`ChatCompletion(ctx, req)`（`client.go:198-255`）

```
t0 = now
provider = primary.Name()

if sharedBreaker != nil && !sharedBreaker.Allow(ctx, provider):
    log.Warn("shared breaker open")
    if fallback != nil: return fallbackChat(ctx, req, t0, nil)
    return error "tripped by shared breaker"

result, err = breaker.Execute(func() (interface{}, error) {
    return primary.ChatCompletion(ctx, req)
})
recordBreakerState()  // Gauge 0|1|2

model = req.Model || "default"
if err == nil:
    metrics.LLMRequestTotal{provider, model, "success"}.Inc()
    metrics.LLMRequestDuration.Observe(now - t0)
    metrics.LLMTokensUsed{provider, "prompt"}.Add(resp.Usage.PromptTokens)
    metrics.LLMTokensUsed{provider, "completion"}.Add(resp.Usage.CompletionTokens)
    return resp, nil

// err != nil
sharedBreaker.RecordFailure(ctx, provider)
metrics.LLMRequestTotal{provider, model, "error"}.Inc()
metrics.LLMRequestDuration.Observe(now - t0)

if fallback != nil: return fallbackChat(ctx, req, t0, err)
return error wrap(err)
```

**重点**：
- **熔断只包主链路**。fallback 不走熔断——否则主备都断掉就彻底无响应；
- **metrics 同时记 primary 和 fallback**，Grafana 可按 `provider` 标签分开看；
- **token usage 只在成功分支累加**——熔断器拒掉的请求不算计费。

### 4.7 fallback 路径：`fallbackChat()`（`client.go:261-281`）

```
fbStart = now
fbProvider = fallback.Name()
resp, fbErr = fallback.ChatCompletion(ctx, req)
if fbErr != nil:
    metrics.LLMRequestTotal{fbProvider, model, "error"}.Inc()
    if primaryErr != nil:
        return wrap("both primary and fallback failed: primary=%w, fallback=%v", primaryErr, fbErr)
    return wrap("fallback failed: %w", fbErr)
metrics.LLMRequestTotal{fbProvider, model, "success"}.Inc()
metrics.LLMRequestDuration{fbProvider, model}.Observe(now - fbStart)
metrics.LLMTokensUsed{fbProvider, "prompt"|"completion"}.Add(...)
return resp, nil
```

注意 `primaryErr` 可能是 nil（shared breaker 触发的预降级路径）——此时 fallback 错误用普通 `wrap`，不要打印 "both failed"。

### 4.8 流式：`ChatCompletionStream`（`client.go:299-334`）

同样的熔断+fallback 包裹逻辑，但返回 `<-chan StreamChunk`。失败会带着 `StreamChunk.Err` 关闭管道。**setup 后错误透传**，不切 fallback（见 §1.5 Q2）。

### 4.9 指数退避重试：`retryWithBackoff`（已定义但**未启用**）

```go
for i := 0; i <= maxRetries; i++ {
    if err := fn(); err == nil { return nil }
    if i < maxRetries {
        backoff := (1 << i) * 500ms          // 500ms, 1s, 2s, 4s, 8s ...
        select {
        case <-ctx.Done(): return ctx.Err()
        case <-time.After(backoff):
        }
    }
}
```

**冷代码警告**：函数在 `client.go:362` 定义但全包搜索无调用方。原计划在 fallback 之前重试 1-2 次瞬时错误（429/5xx/EOF），目前未接入。见 §12 P0 启用计划。

---

## 5. `anthropicProvider`：Anthropic 原生协议（P0 #21 新增）

### 5.1 SDK 选型

用 [`anthropics/anthropic-sdk-go`](https://github.com/anthropics/anthropic-sdk-go)（官方 SDK），通过 `option.WithHTTPClient(httpClient)` 注入 SSRF 受保护的 HTTP client。

### 5.2 Name 格式

```go
func (p *anthropicProvider) Name() string {
    return fmt.Sprintf("anthropic/%s", p.cfg.Model)  // e.g. "anthropic/claude-sonnet-4-20250514"
}
```

Metrics 标签里 `provider=anthropic/claude-sonnet-4-20250514`，按模型粒度做 P99 / 成本曲线区分。

### 5.3 消息转换 `convertMessages`（`anthropic_provider.go:308-414`）

Anthropic Messages API 的几个**关键差异**让转换层比 OpenAI 复杂：

#### (a) System 不在 messages 数组里

```go
var systemParts []string
var messages []anthropic.MessageParam
for _, m := range msgs {
    if m.Role == models.RoleSystem {
        systemParts = append(systemParts, m.Content)
        continue
    }
    // ... 其他 role 加入 messages
}
systemText := strings.Join(systemParts, "\n\n")
```

返回 `(messages, systemText)`，调用方把 systemText 单独塞 `params.System`：

```go
if systemText != "" {
    systemBlock := anthropic.TextBlockParam{Text: systemText}
    if p.cfg.EnablePromptCaching {
        systemBlock.CacheControl = anthropic.CacheControlEphemeralParam{Type: "ephemeral"}
    }
    params.System = []anthropic.TextBlockParam{systemBlock}
}
```

#### (b) Prompt caching：`cache_control` 标记

`models.Message.CacheControl *CacheControl` 字段被映射到 Anthropic 的 `cache_control.type=ephemeral`：

- 长 system prompt 默认打 cache_control（前提 `cfg.EnablePromptCaching=true`）；
- `m.CacheControl != nil` 时给该消息打 ephemeral 标记；
- 工具结果消息 (`Role=tool`) 也支持 cache_control（`anthropic_provider.go:345`）。

**意义**：Anthropic 收到带 ephemeral 标记的 prefix 后会缓存 5 分钟，下次请求同 prefix 走 cache 价格（input token 折扣 90%）。这是 long context（200k 窗口）下省钱的关键。

#### (c) Tool 调用是 ContentBlock，不是扁平字段

Assistant 消息含 tool_calls 时：

```go
for _, tc := range m.ToolCalls {
    content = append(content, anthropic.ContentBlockParamUnion{
        OfToolUse: &anthropic.ToolUseBlockParam{
            ID:    tc.ID,
            Name:  tc.Name,
            Input: tc.Args,  // json.RawMessage 直接透传
        },
    })
}
```

Tool 结果消息（`Role=tool`）映射为 user role + tool_result block：

```go
toolResultBlock := anthropic.ToolResultBlockParam{
    ToolUseID: m.ToolCallID,
    Content:   []anthropic.ToolResultBlockParamContentUnion{{
        OfText: &anthropic.TextBlockParam{Text: m.Content},
    }},
}
```

#### (d) 必须以 user role 开头

Anthropic 严格要求 messages 数组首条是 user 角色。`convertMessages` 末尾自动补一条 `"(continue)"` user 消息如果第一条是 assistant（`anthropic_provider.go:395-408`）——这种情况发生在历史被裁切到只剩 assistant 回复之后。

### 5.4 工具转换 `convertTools`（`anthropic_provider.go:417-453`）

`models.ToolDefinition.Parameters json.RawMessage` 解析后映射到 `anthropic.ToolInputSchemaParam`：
- 提取 `properties` map 给 SDK 的 `InputSchema.Properties`；
- 提取 `required` 数组给 `InputSchema.Required`；
- `Description` 透传。

### 5.5 流式：自己写状态机

OpenAI SDK 帮你 concat 好 tool_calls 的参数字段；Anthropic SDK 给你**原生 SSE 事件流**，要手写状态机累积：

| 事件类型 | 处理 |
|----------|------|
| `content_block_start` | 若 `block.Type=="tool_use"`，初始化 `toolCallsMap[idx] = &ToolCall{ID, Name, Args:""}` |
| `content_block_delta` (text_delta) | 立刻 `ch <- StreamChunk{Content: delta.Text}` 推给前端 |
| `content_block_delta` (input_json_delta) | `toolCallsMap[idx].Args += delta.PartialJSON` 累积 |
| `content_block_stop` | 整 ToolCall 推出：`ch <- StreamChunk{ToolCalls: [*toolCallsMap[idx]]}` |
| `message_stop` | `ch <- StreamChunk{Done: true}` 后 return |
| `error` | `ch <- StreamChunk{Err: ..., Done: true}` 后 return |

详见 `anthropic_provider.go:237-302`。

---

## 6. `openaiProvider`：OpenAI 兼容协议

### 6.1 SDK 选型

用 [`sashabaranov/go-openai`](https://github.com/sashabaranov/go-openai)。原生允许自定义 `BaseURL`，所以以下部署都走同一份代码：

| 部署            | `BaseURL`                          | `Model`                 |
|-----------------|------------------------------------|-------------------------|
| OpenAI 官方     | `https://api.openai.com/v1`        | `gpt-4o`                |
| Azure OpenAI    | `https://<resource>.openai.azure.com/...` | 部署名         |
| Anthropic 代理  | 任意 OpenAI-compat 代理             | `claude-3-5-sonnet…`（不推荐，建议直接走 anthropicProvider） |
| 本地 Ollama     | `http://localhost:11434/v1`        | `qwen2.5-coder:32b`     |
| 本地 vLLM       | `http://<host>:8000/v1`            | `Qwen2.5-Coder-32B-Instruct` |

### 6.2 消息/工具转换

```go
(p) convertMessages(msgs []models.Message) []openai.ChatCompletionMessage
(p) convertTools(tools []models.ToolDefinition) []openai.Tool
```

- `Role=tool` 消息映射成 OpenAI 的 `tool` 角色 + `tool_call_id`；
- `ToolDefinition.Parameters`（JSON Schema）直接透传给 `openai.Tool.Function.Parameters`；
- `models.ResponseFormat` 透传给 `openai.ChatCompletionRequest.ResponseFormat`（支持 `text` / `json_object` / `json_schema`，Strict 模式可用）；
- 回来的 `ToolCall` 反向 `Args json.RawMessage ↔ openai.FunctionCall.Arguments`。

这一层让 **orchestrator 永远只看 `models.*`**，不用 import OpenAI 类型。

### 6.3 流式

`ChatCompletionStream` 用 SDK 的 `CreateChatCompletionStream` 拿 `stream`，起 goroutine 把每个 delta 转成 `StreamChunk` 即时下推（`openai_provider.go:226-269`）：
- 每个 `stream.Recv()` 拿到 `resp.Choices[0].Delta`，包含 `delta.Content` 和 `delta.ToolCalls`；
- `chunk.Content = delta.Content`；
- 若 `len(delta.ToolCalls) > 0`，**当前 chunk 直接带上 ToolCalls 列表**（每个 tc.Function.Arguments 是这一 delta 的片段，**不在 provider 内聚合**）；
- `stream.Recv()` 返回 `io.EOF` → `ch <- StreamChunk{Done: true}` 后关 chan；
- 任意错误 → `ch <- StreamChunk{Err, Done: true}` 后关 chan。

**与 anthropicProvider 的差异**：OpenAI 路径把 tool_call 增量片段直接透传给 channel——provider 不在内做按 ID 累积；Anthropic 走 `content_block_*` 状态机在 provider 内累完整 ToolCall 再 emit（见 §5.5）。**当前代码**：orchestrator 的 `ProcessMessageStream`（`orchestrator.go:732`）只把 chunk 转发给前端 + 累积 `Content` 写 session，**未对 OpenAI 路径的 tool_call 片段做拼接**——这是已知缺陷，因为 ReAct 主循环走的是非流式 `ChatCompletion`（在 agentloop / orchestrator.reactLoopCore），流式 API 目前主要服务 "纯对话无工具" 场景。若上层要在流式路径用工具，调用方需自己按 `tc.ID` concat Args。

---

## 7. `Router`：分档成本控制

### 7.1 三档 `ModelTier`（`router.go:65-74`）

```go
TierHeavy  // Claude Opus / Sonnet / GPT-4o — 跨文件重构、复杂推理
TierMedium // Claude Sonnet / GPT-4o-mini — 单文件编辑、Q&A
TierLight  // Haiku / GPT-3.5 / 本地小模型 — 摘要、意图分类
```

### 7.2 `RouterConfig`（`router.go:86-96`）

```go
RouterConfig{
    HeavyModel:      "claude-sonnet-4-20250514",
    MediumModel:     "claude-sonnet-4-20250514",
    LightModel:      "claude-3-5-haiku-20241022",
    HeavyMaxTokens:  16384,
    MediumMaxTokens: 8192,
    LightMaxTokens:  4096,
}
```

未配置时默认 `16384 / 8192 / 4096`（`router.go:111-119`）。

### 7.3 决策规则（`classify` 节选）

| 条件                                         | 路由                  |
|----------------------------------------------|-----------------------|
| `complexityScore >= 7`                        | Heavy                |
| `intent in {code_execute, deploy}` 且复杂 ≥4  | Heavy                |
| `intent == conversation` 或 `complexity <= 2` | Light                |
| `intent == summarize` (session 摘要)          | 固定 Light           |
| `messageCount > 20`                           | 降一档（多轮节流）   |
| 默认                                         | Medium               |

### 7.4 `QuickComplexity(msg string) int`

- 基于关键词（"重构"/"refactor"/"分析"/"全部文件"）、代码块标记（` ``` `）、字符长度加权打分；
- 零依赖、亚毫秒级；
- **故意不用 LLM 二次分类**——那会让"省 token"本身先浪费一次 token。

### 7.5 统计：`Stats() map[ModelTier]int64`

返回 `routeCount` 的快照拷贝（`router.go:240-246`），目前**仅供单元测试 / 调试断言使用**——未被 Prometheus 或 `metrics/cost.go` 消费。生产成本归因走 §9 描述的 `LLMCostUSD` 显式 Add 路径，与 Router.Stats() 是两条独立的链。未来若要把 tier 暴露为 Counter，需要先在 `internal/metrics` 里新增一个带 `tier` 标签的 CounterVec，再在 `router.SelectModel()` 里递增——当前 `internal/metrics` 中没有现成的路由计数指标可复用。

---

## 8. `helpers.go` — 便捷层

```go
func (c *Client) Complete(ctx, msgs []Message, tools []ToolDefinition) (*CompleteResponse, error)
```

- 自动把 `helpers.Message`（role+content+name）转成 `models.Message`；
- 拿 `ChatCompletion` 结果组装 `CompleteResponse`；
- 存在目的：**单测/脚手架/planner 生成阶段**只关心"问→答"，不想碰完整 Message 结构。

---

## 9. 指标一览（Prometheus）

| 指标                       | 类型      | Label                         | 含义 |
|----------------------------|-----------|-------------------------------|------|
| `code_agent_llm_request_total`        | Counter   | provider, model, status (success/error) | 请求数 |
| `code_agent_llm_request_duration_seconds` | Histogram | provider, model               | 端到端延迟 |
| `code_agent_llm_tokens_used_total`    | Counter   | provider, type (prompt/completion) | token 累计 |
| `code_agent_llm_circuit_breaker_state`| Gauge     | provider                      | 0=closed 1=half 2=open |
| `code_agent_llm_fallback_total`       | Counter   | —                             | fallback provider 被调用次数 |

`Router` 的路由分档统计目前**仅在内存 `routeCount map[ModelTier]int64`** 中（`router.go:105`），未导出为 Prometheus 指标，也未被 `metrics/cost.go` 消费——`Stats()` 方法存在但当前调用方仅限单元测试。

**成本归因走的是不同路径**：`metrics/cost.go` 提供 `LLMCostUSD{model, tier, session_id, user_id, task_id}` Counter，由 caller 在每次 LLM 调用后显式 `Add(estimatedUSD)`；高基数标签（session_id / user_id）经 Prometheus relabel_configs 在 remote-write 层裁剪。Grafana 仪表盘配置见 [`19_observability.md`](19_observability.md)。

---

## 10. 单元测试覆盖

| 测试文件 | 测什么 |
|----------|--------|
| `client_test.go` (69) | mock Provider；注入"总是失败"的 primary 断言 fallback 被调用；连续失败 N 次后 gobreaker Trip → 下一次调用 bypass primary |
| `router_test.go` (122) | 每条 `classify` 规则独立用例 + `QuickComplexity` 快照测试 |
| `tokenizer_test.go` (126) | tiktoken 可用时 ExactTokenCount 准确；不可用时降级 FastEstimate；rune 加权对 CJK / 标点 / 长 ASCII 的偏差 |
| `anthropic_provider_test.go` (466) | System 消息抽取、cache_control 注入、tool_use 双向映射、SSE 事件状态机（content_block_*/message_stop/error）、首条 assistant 自动补 "(continue)" |
| `openai_provider_test.go` (623) | 消息/工具转换、ResponseFormat 透传、SSE chunk 聚合、Azure deployment 路径处理 |

CI 零外部依赖（不真发 HTTP）。

---

## 11. 设计权衡

| 抉择 | 动机 |
|------|------|
| 熔断只保 primary，fallback 裸跑 | 主备都断时仍允许给用户一个错误而非静默超时 |
| retry 和 circuit breaker 分层 | retry 应对瞬时抖动（TCP RST、EOF），CB 应对长期不可用；混一起会互相放大延迟 |
| **独立 anthropicProvider 而非 OpenAI-compat 代理** | 保留 cache_control（input token 9 折）+ 原生 long context 性能；走代理会丢两者 |
| Router 复杂度打分不用 LLM | 避免"为了省钱再花 token"的悖论；关键词+长度足够覆盖 90% 场景 |
| `FastEstimate` 默认，`ExactTokenCount` 关键路径 | 80μs × 1000 chunks 对 RAG hot path 是灾难；只在最终预算校验时切精确 |
| 所有 provider 转换层在自己文件 | orchestrator 对接时完全不知道底下是哪个厂商，无 OpenAI / Anthropic 类型扩散 |
| **SSRF 防御走 httpClient 注入** | provider 不感知安全策略，security 包统一管控 CIDR 白名单 |
| **SharedBreaker fail-open** | Redis 是基础设施而非主路径；Redis 抖动不能让 LLM 全集群停摆 |
| **SharedBreaker 只 record 失败** | 成功不减计数，靠 TTL 自然滚动；省 Lua 一半逻辑 |
| `ChatRequest.ResponseFormat` 指针可选 | 不需要 JSON 输出时不强制构造空对象，向下兼容老调用方 |

---

## 12. 后续演进

**P0 — 马上能做**

- [ ] **修 gobreaker.Settings 配置 bug**：`Interval = 2×Timeout`（当前两者同值，间歇抖动统计不准）；
- [ ] **启用 `retryWithBackoff`**：对瞬时错误（5xx / EOF / timeout）重试 1-2 次再走 fallback；
- [ ] **给 retry 加 jitter**：`rand.Intn(baseBackoff/2)` 避免雷群效应；
- [ ] **anthropicProvider 实现 ResponseFormat**：当前忽略；用 `tool_use` 强制 schema 模拟 JSON 输出。

**P1 — 数周内**

- [ ] **SharedBreaker 接入 streaming 路径**：当前仅 `ChatCompletion` 接入，`ChatCompletionStream` 未走 RecordFailure；
- [ ] **gobreaker 换用 `FailureRate ≥ 0.3` 判据**：当前 `ConsecutiveFailures`——40% 错误率但不连续不会跳；
- [ ] **请求级 timeout 单独配置**：`req.Timeout = 60s` 与 `ctx deadline` 区分；
- [ ] **支持多 provider 注册表**：不止 primary + fallback，按模型名路由到不同 provider（如 embedding 用 OpenAI、聊天用 Anthropic）；
- [ ] **token 预算 budgeter**：超出 session / user 预算时主动降档或拒绝（骨架在 `metrics/cost.go`）；
- [ ] **响应缓存**：embedding / 摘要等 deterministic 场景加 Redis 缓存（key=hash(prompt+model)）。

**P2 — 季度级**

- [ ] **按 intent 自动选模型**：Router 接 orchestrator 的 TaskIntent，简单 conversation → haiku 省 $；
- [ ] **按 provider 冷启动预热连接池**：避免首次请求的 TLS 握手延迟；
- [ ] **SharedBreaker 增加 "请求计数 + 失败率"**：当前仅失败计数，加上总计数可算 rate 更精准；
- [ ] **请求合并**：同 session 200ms 内的多个小请求打包成一次 long-context 调用；
- [ ] **Gemini 原生 provider**：当前 Gemini 只能走 OpenAI-compat 代理；
- [ ] **多模态扩展**：`models.Message.Content` 升级为 `[]ContentBlock`（text/image/audio），适配 Vision API。

---

## 13. 设计教训

**教训 1：OpenAI-compat 不是万能转接头。** 早期假设"所有 provider 都能套 OpenAI 协议"——结果 Anthropic 代理时丢了 cache_control（每次重发 system 没 cache hit，long context 成本爆炸 10×）+ 丢了 tool_use 的结构化输入。**协议适配层不能省**，写两套 Provider 比维护一个"什么都做不好"的中间层 cleaner。

**教训 2：熔断器要分层。** 只靠 gobreaker → N 副本时每个 Pod 独立计数，从不 trip。只靠 Redis → 单副本起步成本高。两层叠加才能兼顾"快速本地反应"+"全集群聚合判断"。SharedBreaker 强制 fail-open 是必须的——基础设施不能成为业务路径的依赖。

**教训 3：token 估算 ≠ token 计费。** FastEstimate 误差 ±15% 对"还能塞几条消息"决策足够；但 user 看到的账单必须精确——所以 metrics 的 `LLMTokensUsed` 用的是 **API 返回的 `Usage.PromptTokens` / `CompletionTokens`**，不是 EstimateTokens。两个数字代表不同问题，不能混用。

**教训 4：retryWithBackoff 写好了没用 = 反模式。** 冷代码暴露的真正问题是"没想清楚 retry 和 fallback 的分工"——retry 应对 1-3 秒的瞬时故障，fallback 应对持久 outage。当前 fallback 在 1 次失败就触发，等于跳过 retry 阶段，瞬时抖动直接吃 fallback 成本（备用模型更弱）。P0 修复要把 retryWithBackoff 拉到 primary 调用层。

**教训 5：cache_control 的注入位置很重要。** 早期版本把 cache_control 写在 Provider 内部所有 message 上 → 每条消息都 ephemeral，反而 cache prefix 不稳定（cache hit 取决于 prefix 完全相同）。正确做法：**system prompt + 用户显式标记的 message 才打 cache_control**，让 prefix 稳定累积。这要求 `models.Message.CacheControl` 字段由上层 prompt builder 主动注入，不在 Provider 里偷偷加。

---

下一篇：[`04_rag.md`](04_rag.md) —— AST 解析 + Qdrant 向量库 + BM25 + 交叉编码器重排。
