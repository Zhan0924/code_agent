# 03 · LLM 模块 `internal/llm`

> 代码：
> - `client.go` (252 行) — 对外客户端 + 熔断器 + fallback
> - `router.go` (232 行) — 三档模型动态路由
> - `openai_provider.go` (244 行) — OpenAI 兼容协议实现
> - `helpers.go` (43 行) — `Complete()` 便捷封装
>
> 测试：`client_test.go` + `router_test.go`（156 行）

---

## 1. 模块定位

**"给整个 Agent 系统提供一条高可用、可观测、成本可控的 LLM 通道。"**

承担四件事：

1. **统一协议** — 用 `Provider` 接口抹平不同厂商差异（OpenAI / Azure / Anthropic / 本地 Ollama）；
2. **高可用** — 熔断器 + 主备降级 + 指数退避重试，保证单个 API 波动不拖垮整个 Agent；
3. **成本控制** — 按任务复杂度路由到 heavy / medium / light 档，简单任务不浪费 GPT-4o；
4. **可观测** — 每次调用记 Prometheus 指标（延迟、token、成功率、熔断器状态）。

---

## 1.5 设计哲学：LLM 层要回答的 4 个根本问题

### Q1 — 抽象到什么粒度？

**选项**：
- (A) 每个 provider 独立 SDK，orchestrator 直接调
- (B) 定义统一 `Provider` 接口，每个 provider 实现
- (C) 再往上再抽一层 `Router`，按负载/成本路由

**决策**：(B)。(A) 让 orchestrator 到处是 `if provider == "openai"`；
(C) 早期 over-engineering，实测主备降级已经覆盖 95% 路由需求。

**原则**：抽象只在**多个具体实现**出现后才落地。目前我们只有 OpenAI
兼容协议这一族实现（OpenAI / Anthropic 代理 / Ollama / vLLM / Azure
都实现这协议），所以 Provider 接口是抽象的最小单位。

### Q2 — 熔断 vs 重试 vs 降级，三选几？

**场景对应**：
- 熔断（circuit breaker）：防止**持续性**故障放大调用量
- 重试（retry）：纠正**瞬时性**故障（网络抖动 / 限流被清零）
- 降级（fallback）：主 provider 挂了用备，保可用性

**决策**：三个都要，但**层级明确**：
```
请求到来
  │
  ▼
[Shared Breaker] ——过载直接 fallback   ← 分布式聚合（P0 #21）
  │
  ▼
[Local gobreaker] ——开路直接 fallback
  │
  ▼
[Provider.ChatCompletion] ——失败
  │
  ├── 瞬时错误（5xx/timeout） → retryWithBackoff（当前禁用，见演进）
  └── 持久错误 → Fallback Provider
              │
              └── 失败 → 返回 wrapped error
```

**设计争议点**：是否应该在**流式**路径也熔断？
当前答案：**否**。一旦 stream 开始（首 chunk 已发给前端），mid-stream 错误
切 fallback 会导致前端看到两段拼接的回答——比错误更糟。所以流式只熔断
**setup 阶段**，setup 成功后错误透传给前端。

### Q3 — Token 计数：精确 vs 够用？

**问题**：每步都要判断"prompt + 预留 completion 还剩多少"，如果算错，
要么 LLM 报 `context_length_exceeded`，要么白白留空间降低并发。

**选项**：
- (A) 精确 tokenize（tiktoken-go，每个 model 查字典）
- (B) 字节 / 4 粗估
- (C) rune 分类加权（我们选的）

**实测误差**：
| 内容 | 字节/4 | rune 加权 | 真实（cl100k） |
|---|---|---|---|
| 英文句子 (44 字符) | 11 | 12 | 9-10 |
| 中文 12 字 (36 字节) | **9 ❌** | 12 | 12-15 |
| JSON 密集标点 | **偏低** | 贴近 | ... |

(B) 被 P0 #20 淘汰。(C) 在误差 ±15% 内，对"pruneMessages 还能塞几条"
决策足够。(A) 留给 P1 待办——精确计费场景才值得 30KB tokenizer 表 +
per-model 查字典开销。

### Q4 — 是否暴露 provider 细节给上层？

**选项**：`ChatRequest.Model` 让 orchestrator 指定模型
- (A) 暴露——灵活
- (B) 不暴露——只选 provider，model 由配置决定

**决策**：(A)。原因：同一 provider 不同场景需要不同 model（意图分类用
haiku，主聊用 opus）。Orchestrator 知道自己的成本敏感度，Provider 层不
该越俎代庖。但 provider 字符串本身仍然由 config 决定（不是 hard-code
"openai"）。

---

## 2. 依赖架构

```
    ┌──────────────────── orchestrator ────────────────────┐
    │ .ChatCompletion() / .ChatCompletionStream() / .Complete()
    └──────────────────────┬───────────────────────────────┘
                           ▼
             ┌───────────────────────────┐
             │   llm.Client (high-level) │
             │   - circuitBreaker (sony) │
             │   - fallback Provider     │
             │   - metrics hooks         │
             └─────────┬─────────────────┘
                       │ delegates
        ┌──────────────▼──────────────┐
        │   Provider interface        │
        └─────────────────────────────┘
                ▲             ▲
                │             │
   ┌────────────┴──┐   ┌──────┴──────┐
   │ openaiProvider │   │ ...more    │
   │ (primary)      │   │ providers  │
   └────────────────┘   └────────────┘
              ▲
              │ uses github.com/sashabaranov/go-openai
              │ ("OpenAI-compatible" URL)
              ▼
       OpenAI / Azure / Anthropic(proxy) / Ollama / vLLM
```

---

## 2.5 数据流总览

下图将本模块各链路串成一张端到端视图，各步骤详见后续对应章节。

### 2.5.1 ChatCompletion 完整调用链

```text
┌─────────────────────────┐
│ orchestrator.reactLoop  │
│ llm.Client.ChatCompletion(ctx, req)
└────────────┬────────────┘
             │
             ▼
┌─────────────────────────────────────────────────────────────┐
│ Router.SelectProvider(req)                                    │
│  估算 complexity → 选择 tier:                                │
│    Heavy (>8K tokens / 复杂推理) → primary model             │
│    Medium (常规 chat)            → primary model             │
│    Light (<1K / 简单分类)        → 廉价 model (如果配置了)   │
└──────────────────────────┬──────────────────────────────────┘
                           │ (selected Provider)
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ SharedCircuitBreaker (跨 Pod, Redis)                         │
│  ① INCR code_agent:breaker:failures (TTL 60s)              │
│  ② 当前计数 > threshold(10)?                                 │
│     YES → 直接跳到 fallback (不调用 primary)                 │
│     NO  → 继续调用                                           │
└──────────────────────────┬──────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ gobreaker.Execute (本地状态机)                                │
│                                                              │
│  ┌─────────┐  5次失败  ┌────────┐  60s后  ┌────────────┐   │
│  │ Closed  │─────────▶│  Open  │───────▶│ HalfOpen   │   │
│  │(正常放行)│◀─────────│(全拒绝) │◀───────│(试探1次)    │   │
│  └─────────┘  试探成功  └────────┘ 试探失败└────────────┘   │
│                                                              │
│  Closed → 执行 Provider.ChatCompletion                      │
│  Open   → return ErrOpenState → 触发 fallback              │
└──────────────────────────┬──────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ Provider.ChatCompletion (HTTP)                                │
│  构造 OpenAI-compatible request → POST /v1/chat/completions │
│  ┌──────────────────────────────────────────────────────┐   │
│  │ 重试策略 (指数退避):                                  │   │
│  │  429/500/502/503 → sleep 2^n * 1s (max 3次)         │   │
│  │  成功 → reset failure count                          │   │
│  │  彻底失败 → SharedBreaker INCR + return error        │   │
│  └──────────────────────────────────────────────────────┘   │
└──────────────────────────┬──────────────────────────────────┘
                           │
              ┌────────────┴────────────┐
              ▼                         ▼
     ┌──────────────┐        ┌─────────────────────────┐
     │ 调用成功      │        │ primary 失败            │
     │ → parse resp │        │ → fallback Provider     │
     │ → count      │        │   (若配置了备用模型)     │
     │   tokens     │        │   相同流程重试           │
     │ → record     │        │   无 fallback → 上报err │
     │   metrics    │        └─────────────────────────┘
     └──────┬───────┘
            │
            ▼
┌─────────────────────────────────────────────────────────────┐
│ *ChatResponse{Content, ToolCalls, Usage{Prompt,Completion}} │
│  → 返回 orchestrator                                        │
│  → metrics: llm_request_duration / llm_token_total          │
└─────────────────────────────────────────────────────────────┘
```

### 2.5.2 Streaming 变体

```text
┌───────────────────────┐
│ ChatCompletionStream  │
└───────────┬───────────┘
            │ (同上 Router → Breaker → Provider)
            ▼
┌─────────────────────────────────────────────────────────────┐
│ Provider.ChatCompletionStream → HTTP chunked response        │
│  → 逐 chunk 解析 SSE delta                                  │
│  → push to chan StreamChunk{Delta, FinishReason}            │
│  → orchestrator 逐 chunk 转发给前端 SSE                     │
│  → 最后一个 chunk 累加 Usage                                 │
└─────────────────────────────────────────────────────────────┘
```

---

## 3. 核心接口：`Provider`

```go
type Provider interface {
    ChatCompletion(ctx, *ChatRequest) (*ChatResponse, error)
    ChatCompletionStream(ctx, *ChatRequest) (<-chan StreamChunk, error)
    Name() string
}
```

使用的数据类型在同一个包里：

- `ChatRequest{ Messages, Tools, MaxTokens, Temperature, Model }`
- `ChatResponse{ Content, ToolCalls, Usage, Model }`
- `Usage{ PromptTokens, CompletionTokens, TotalTokens }`
- `StreamChunk{ Content, ToolCalls, Done, Err }`

> 与 `models.Message` / `models.ToolDefinition` 共用同一套类型 —— 避免转换样板。

---

## 4. `Client`：高可用壳

### 4.1 结构

```go
type Client struct {
    primary  Provider                      // 主 provider
    fallback Provider                      // 备 provider（可为 nil）
    breaker  *gobreaker.CircuitBreaker     // 仅对 primary 生效
    logger   *zap.Logger
}
```

### 4.2 熔断器配置（sony/gobreaker）

```go
cbSettings := gobreaker.Settings{
    Name:        "llm-primary",
    MaxRequests: uint32(cfg.CircuitBreaker.HalfOpenMaxReqs), // half-open 阶段探针数量
    Interval:    cfg.CircuitBreaker.Timeout,                 // 滑动计数窗口
    Timeout:     cfg.CircuitBreaker.Timeout,                 // open→half-open 的冷却时间
    ReadyToTrip: func(counts gobreaker.Counts) bool {
        return int(counts.ConsecutiveFailures) >= cfg.CircuitBreaker.MaxFailures
    },
    OnStateChange: func(name string, from, to gobreaker.State) {
        logger.Warn("circuit breaker state changed", …)
    },
}
```

三态机：

```
CLOSED ─(连续失败≥MaxFailures)─► OPEN ─(冷却Timeout)─► HALF-OPEN
   ▲                                                         │
   └────────────(MaxRequests 个探针全成功)────────────────────┘
       ◄───(HALF-OPEN 阶段有失败)─────── 回到 OPEN
```

`OnStateChange` 回调推 `metrics.LLMCircuitBreakerState` gauge，Grafana 可一眼看出哪个窗口被熔断。

### 4.3 跨副本熔断器 `SharedCircuitBreaker` (shared_breaker.go, 新增 P0 #21)

> 💡 `gobreaker` 是**进程内**熔断——每个副本独立计数。N 个 Pod 部署时，
> 上游降级场景下每个 Pod 都要独立累积到阈值才跳开，合计打给上游的无效
> 流量会放大 N 倍。`SharedCircuitBreaker` 基于 Redis 做**跨副本聚合**，
> 严格增量式工作在 gobreaker 之上，不替代。

**数据流**：

```text
 Pod-A 失败              Pod-B 请求
   │                         │
   ▼                         ▼
RecordFailure()          Allow()
   │                         │
   ▼                         ▼
 Lua EVAL                  Lua EVAL (GET)
   INCR + EXPIRE             count = ?
   │                         │
   ▼                         │
┌─────────────────────────────────────┐
│ Redis key: llm:breaker:{provider}:{window_epoch} │
│ TTL = window (e.g. 30s)                           │
└─────────────────────────────────────┘
```

**API**：
```go
func (s *SharedCircuitBreaker) Allow(ctx, provider) bool
func (s *SharedCircuitBreaker) RecordFailure(ctx, provider)
```

**与 gobreaker 的组合**：
```go
func (c *Client) ChatCompletion(ctx, req) (*ChatResponse, error) {
    // L1: 先问 Redis 聚合计数
    if c.sharedBreaker != nil && !c.sharedBreaker.Allow(ctx, provider) {
        // 直接 fallback，完全不碰主 provider
        return c.fallbackChat(ctx, req, start, nil)
    }

    // L2: gobreaker 本地快速跳开
    result, err := c.breaker.Execute(func() (interface{}, error) {
        return c.primary.ChatCompletion(ctx, req)
    })

    if err != nil && c.sharedBreaker != nil {
        c.sharedBreaker.RecordFailure(ctx, provider)  // 聚合到 Redis
    }
    ...
}
```

**设计权衡**：
- **Fixed window** 而不是 sliding window：实现简单（`INCR + EXPIRE` 一次
  Lua EVAL），精确度够用于 DoS / 降级检测。真正需要精确时可升级到
  Redis Sorted Set 的 sliding log。
- **Fail-open on Redis error**：Redis 挂了 → Allow 永真，不阻塞正常请求。
  可用性 > 严格执行。
- **只 record 失败、不 record 成功**：TTL 到期自动清理，无需显式成功减
  计数。这是刻意简化，对"连续错误"场景足够。

**配置**：
```go
NewSharedCircuitBreaker(SharedBreakerConfig{
    Rdb:       redisClient,
    Prefix:    "llm:breaker",
    Window:    30 * time.Second,
    Threshold: 20,     // 30s 内整个集群累积 20 次失败即跳开
})
```

传 `Rdb=nil` 直接返回 `nil` → Client 当作"无 shared breaker"，只用
gobreaker。这让 SharedBreaker 可选 —— 开发环境或单副本部署无需 Redis
就能正常跑。

### 4.4 调用路径：`ChatCompletion(ctx, req)`

```
1. t0 = now
2. breaker.Execute {
     primary.ChatCompletion(ctx, req)
   }
3. recordBreakerState() → gauge
4. metrics.LLMRequestDuration.Observe(now-t0)
5. err == nil?
      └── YES: 记 success + token usage → return resp
      └── NO:
            a. metrics.LLMRequestTotal{status="error"}++
            b. logger.Warn("primary failed, try fallback")
            c. fallback != nil?
                  YES: fallback.ChatCompletion(ctx, req)
                        成功 → 记 fallback success + tokens → return
                        失败 → return 合并 err ("both primary and fallback failed")
                  NO:  return primary err
```

重点：

- **熔断只包主链路**。fallback 不走熔断 —— 否则主备都断掉就彻底无响应；
- **metrics 同时记 primary 和 fallback**，Grafana 可按 `provider` 标签分开看；
- **token usage 只在成功分支累加**，避免熔断器误拒请求也被计费。

### 4.5 流式：`ChatCompletionStream`

同样的熔断+fallback 包裹逻辑，但返回 `<-chan StreamChunk` —— `orchestrator` 可以边接边推 SSE 给前端。失败会带着 `StreamChunk.Err` 关闭管道。

### 4.6 指数退避重试：`retryWithBackoff`

```go
for i := 0; i <= maxRetries; i++ {
    if err := fn(); err == nil { return nil }
    if i < maxRetries {
        backoff := (1 << i) * 500ms      // 500ms, 1s, 2s, 4s, 8s…
        select {
        case <-ctx.Done(): return ctx.Err()
        case <-time.After(backoff):
        }
    }
}
```

- 2^i × 500ms，无上限（调用方通过 `ctx` 控顶）；
- 尊重 `ctx.Done()` —— 请求被取消立刻返回；
- **不包在 `ChatCompletion` 里**：retry 由 `openaiProvider` 内部决定用在哪几种可重试错误上（429、5xx、EOF），外层的 `Client` 只处理熔断/fallback 维度。

---

## 5. `openaiProvider`：OpenAI 兼容实现

### 5.1 SDK 选型

用 [`sashabaranov/go-openai`](https://github.com/sashabaranov/go-openai)。该 SDK 原生允许自定义 `BaseURL`，所以以下部署都走同一份代码：

| 部署            | `BaseURL`                          | `Model`                 |
|-----------------|------------------------------------|-------------------------|
| OpenAI 官方     | `https://api.openai.com/v1`        | `gpt-4o`                |
| Azure OpenAI    | `https://<resource>.openai.azure.com/...` | 部署名         |
| Anthropic 代理  | 任意 OpenAI-compat 代理             | `claude-3-5-sonnet…`    |
| 本地 Ollama     | `http://localhost:11434/v1`        | `qwen2.5-coder:32b`     |
| 本地 vLLM       | `http://<host>:8000/v1`            | `Qwen2.5-Coder-32B-Instruct` |

### 5.2 消息/工具转换

```go
(p) convertMessages(msgs []models.Message) []openai.ChatCompletionMessage
(p) convertTools(tools []models.ToolDefinition) []openai.Tool
```

- `Role=tool` 消息会被映射成 OpenAI 的 `tool` 角色 + `tool_call_id`；
- `ToolDefinition.Parameters` (JSON Schema) 直接透传给 `openai.Tool.Function.Parameters`；
- 回来的 `ToolCall` 同样反向 `Args json.RawMessage ↔ openai.FunctionCall.Arguments`。

这一层的存在让 **orchestrator 永远只看 `models.*`**，不用 import OpenAI 类型。

### 5.3 流式

`ChatCompletionStream` 用 SDK 的 `CreateChatCompletionStream` 拿 `stream`，再起 goroutine 把 delta 聚合成 `StreamChunk`：

- 文本内容实时往 chan 送；
- 工具调用的 argument 字段是按 token 流回来的，SDK 会自动 concat；当 `finish_reason != ""` 时才发带 `ToolCalls` 的 chunk；
- 发完 `Done:true` 再关 chan。

---

## 6. `Router`：分档成本控制

### 6.1 三档 `ModelTier`

```go
TierHeavy  // Claude Opus / GPT-4o — 跨文件重构、复杂推理
TierMedium // Claude Sonnet / GPT-4o-mini — 单文件编辑、Q&A
TierLight  // Haiku / GPT-3.5 / 本地小模型 — 摘要、意图分类
```

### 6.2 `ModelRoute` 规则（`classify` 函数内节选）

| 条件                                         | 路由                  |
|----------------------------------------------|-----------------------|
| `complexityScore >= 7`                        | Heavy                |
| `intent in {code_execute, deploy}` 且复杂 ≥4  | Heavy                |
| `intent == conversation` 或 `complexity <= 2` | Light                |
| `intent == summarize` (session 摘要)          | 固定 Light           |
| `messageCount > 20`                           | 降一档（多轮时节流） |
| 默认                                         | Medium               |

### 6.3 复杂度估计：`QuickComplexity(msg string) int`

- 基于关键词、代码块标记、字符长度加权打分；
- 零依赖、亚毫秒级完成；
- **故意不用 LLM 做二次分类** —— 那会让"省 token"本身先浪费一次 token。

### 6.4 统计：`Stats() map[ModelTier]int64`

`metrics/cost.go` 读取这个 map，按档位把累计 token 聚合成 USD 成本曲线。

---

## 7. `helpers.go` — 便捷层

```go
func (c *Client) Complete(ctx, msgs []Message, tools []ToolDefinition) (*CompleteResponse, error)
```

- 自动把 `helpers.Message` (role+content+name) 转成 `models.Message`；
- 拿 `ChatCompletion` 的结果组装 `CompleteResponse`；
- 存在目的：**单测/脚手架/planner 生成阶段**只关心"问 → 答"，不想碰完整 Message 结构。

---

## 8. 指标一览（Prometheus）

| 指标                       | 类型      | Label                         | 含义 |
|----------------------------|-----------|-------------------------------|------|
| `llm_request_total`        | Counter   | provider, model, status       | 请求数 |
| `llm_request_duration_sec` | Histogram | provider, model               | 端到端延迟 |
| `llm_tokens_used_total`    | Counter   | provider, kind(prompt/completion) | token 累计 |
| `llm_circuit_breaker_state`| Gauge     | —                             | 0=closed 1=half 2=open |
| `llm_route_total`          | Counter   | tier                          | 每档路由计数 |

Grafana 仪表盘 `deploy/grafana/llm.json` 直接消费这些（下篇 `19_observability.md` 细讲）。

---

## 9. 单元测试要点

- **`client_test.go`** 用 mock `Provider`：
  - 注入一个 "总是失败" 的 primary，断言 fallback 被调用且成功；
  - 模拟 primary 连续失败 N 次，断言熔断器 Trip → 下一次调用 bypass primary 直接走 fallback。
- **`router_test.go`** 针对每条 `classify` 规则写独立用例 + `QuickComplexity` 的快照测试。

测试里**不真的发 HTTP**，因此 CI 环境零依赖。

---

## 10. 设计权衡

| 抉择 | 动机 |
|---|---|
| 熔断只保 primary，fallback 裸跑 | 主备都断时仍允许给用户一个错误而非静默超时 |
| retry 和 circuit breaker 分层 | retry 应对瞬时抖动（TCP RST、EOF），CB 应对长期不可用；混一起会互相放大延迟 |
| 统一 OpenAI-compatible 协议 | 企业内私有化 Qwen/Claude-proxy/vLLM 零代码接入 |
| Router 的复杂度打分不用 LLM | 避免"为了省钱再花 token"的悖论；关键词+长度足够覆盖 90% 场景 |
| `EstimateTokens = len/4` | 粗估但**稳定**；精确分词（tiktoken）放在 `context/pruner.go` 只给裁剪用 |
| 所有 provider 转换层在 `openai_provider.go` | orchestrator 对接时完全不知道底下是哪个厂商 |

---

## 11. 后续演进

- [ ] 支持 **多 provider 注册表**：不止 primary+fallback，可以按模型名路由到不同 provider；
- [ ] 引入 **token 预算 budgeter**：超出 session / user 的预算时主动降档或拒绝（骨架在 `metrics/cost.go`）；
- [ ] 支持 **请求合并**：同一 session 在 200ms 内的多个小请求可以打包成一次 long-context 调用；
- [ ] **Anthropic 原生协议** provider（目前通过 OpenAI-compat 代理）；
- [ ] 响应 **缓存**：对 embedding/摘要等可缓存场景加 Redis 缓存（key=hash(prompt+model)）。

---

## 12. 实现剖析与改进方向

### 一次 ChatCompletion 调用的完整时序

```text
caller                    Client        SharedBreaker   gobreaker    Provider
  │                         │                │              │           │
  │─ ChatCompletion(req) ──▶│                │              │           │
  │                         │─ Allow(prov) ──▶              │           │
  │                         │◀──── bool ─────│              │           │
  │                         │                │              │           │
  │             [shared open?]                              │           │
  │                ├─ yes ─▶ fallbackChat()  ─ fallback provider ────▶   │
  │                │        └─── return ◀                                │
  │                └─ no ─┐                   │              │           │
  │                        │─ breaker.Execute(fn) ─────────▶            │
  │                        │                                │─ HTTP ───▶│
  │                        │                                │   ...     │
  │                        │                                │◀── json ──│
  │                        │◀──── result / err ─────────────│           │
  │          [gobreaker 已统计成败]                         │           │
  │                        │                                             │
  │             err != nil?                                              │
  │                ├─ yes ─▶ RecordFailure(prov) ─▶        │           │
  │                │        fallbackChat()                  │           │
  │                │        └─ return                       │           │
  │                └─ no ─▶ record metrics + tokens         │           │
  │                                                                      │
  │◀──── ChatResponse / err ────                                         │
```

**关键观察**：
- SharedBreaker 是 **check-then-act** 而非事务。两次 check 之间可能有另
  一 Pod 写入——但 fixed-window 的粗粒度已经容忍这点不精确。
- gobreaker 在 `breaker.Execute` 里内部统计成败；SharedBreaker 在外部
  手动 `RecordFailure`。两个相互独立。
- **metrics 记录点**：成功/失败路径都记录 duration，不能只记成功——
  否则看到的 P99 会失真。

### 内部状态：gobreaker 状态机

```text
                 ConsecutiveFailures ≥ MaxFailures
    ┌─────────┐ ─────────────────────────────────▶ ┌─────────┐
    │ CLOSED  │                                     │  OPEN   │
    │  放行   │ ◀─── HalfOpen 阶段全部成功 ──────  │  拒绝   │
    └─────────┘                                     └────┬────┘
         ▲                                               │ 冷却 Timeout 后
         │  HalfOpen 阶段有任一失败                      ▼
         │                                          ┌─────────┐
         └──────────────────────────────────────────│HALF-OPEN│
                                                    │ 探针放行 │
                                                    │ (MaxReq) │
                                                    └─────────┘
```

**已知配置 bug**：`Interval = Timeout`（`client.go:88-89`）。两字段语义不同：
- `Interval`：滑动计数窗口（过了就清零计数）
- `Timeout`：OPEN → HALF-OPEN 冷却时间

目前两者用同一值，间歇性抖动场景下统计不准（应该 `Interval` 比 `Timeout` 长）。

### 利弊评估

**优势（Pros）**
- ✅ 统一 Provider 抽象，切换供应商零改业务代码
- ✅ Shared + Local 双层熔断，覆盖单副本+集群两种失效模式
- ✅ Metrics 打点齐全（按 provider/model/status 分维度）
- ✅ Fallback 自动降级，供应商挂掉不中断服务
- ✅ Streaming 和 non-streaming 共享熔断逻辑

**代价（Cons）**
- ⚠️ gobreaker 用 `ConsecutiveFailures` 而非 `FailureRate`—— 40% 错误率
  但不连续不会跳开
- ⚠️ Streaming 的 fallback 只在 setup 阶段；mid-stream 错误直接透传
  （可能给用户显示半截错位响应）
- ⚠️ 没有请求级 timeout 独立配置（全部用 ctx）
- ⚠️ `retryWithBackoff` 定义了但**从未被调用**（代码冷却）
- ⚠️ Token 估算误差 ±15%，不适合精确计费

### 可改进点（按优先级）

**P0 — 马上能做**
1. 修 `gobreaker.Settings` 的 Interval/Timeout 配置：`Interval = 2×Timeout`
2. 启用 `retryWithBackoff`：对瞬时错误（5xx / timeout）重试 1-2 次
3. 给 retry 加 jitter（`rand.Intn(baseBackoff/2)`）避免雷群

**P1 — 数周内**
4. SharedBreaker 接入 streaming 路径（目前只 non-stream 接入）
5. gobreaker 换用 `FailureRate ≥ 0.3` 的判据而非 ConsecutiveFailures
6. 请求级 timeout 单独配置：`req.Timeout = 60s`，区别于 ctx deadline

**P2 — 季度级**
7. 接入 tiktoken-go 替换启发式 EstimateTokens
8. 按 intent 自动选模型（简单查询 → haiku 省 $）
9. 按 provider 冷启动预热连接池
10. SharedBreaker 增加 "请求计数 + 失败率" 而非仅失败计数（更精准）

---

下一篇：`04_rag.md` —— AST 解析 + Qdrant 向量库 + BM25 + 交叉编码器重排。
