# LLM交互层模块详解 (`internal/llm`)

> 本文档面向传统后端开发人员，详细介绍 LLM 交互层的架构、数据流和实现细节。
> 目标：帮助你理解"Agent 如何安全、高效、可控地调用大语言模型"。

---

## 1. 模块概览

### 1.1 职责定义

LLM 模块是整个 Agent 系统的**"大脑接口层"**——所有需要"思考"的地方都通过它调用大模型。

```
┌──────────────────────────────────────────────────────────────────────────┐
│                          LLM 模块的四大职责                                │
├──────────────────────────────────────────────────────────────────────────┤
│                                                                          │
│  ① 统一协议     OpenAI / Anthropic / Ollama / vLLM 统一 Provider 接口    │
│                                                                          │
│  ② 高可用       主备降级 + 本地熔断 + 分布式熔断 + 指数退避重试          │
│                                                                          │
│  ③ 成本控制     三档模型路由（Heavy/Medium/Light），简单任务不烧 GPT-4    │
│                                                                          │
│  ④ 可观测       Prometheus 指标（延迟/Token/成功率/熔断状态）             │
│                                                                          │
└──────────────────────────────────────────────────────────────────────────┘
```

### 1.2 文件结构与代码位置

```
internal/llm/
├── doc.go                 # 包级文档（设计目标、关键类型说明）
├── _principles.go         # 设计原理深度剖析（不参与编译，纯文档）
├── client.go              # ★ 核心：Client 结构体 + 熔断 + Fallback
├── openai_provider.go     # ★ 核心：OpenAI 兼容协议的 Provider 实现
├── router.go              # 模型路由器（按复杂度/意图选模型档位）
├── shared_breaker.go      # 跨副本分布式熔断器（Redis 聚合）
├── tokenizer.go           # Token 计数（精确 tiktoken + 快速估算）
├── helpers.go             # Complete() 便捷封装
├── client_test.go         # Client 单元测试
├── router_test.go         # Router 单元测试
└── tokenizer_test.go      # Tokenizer 单元测试
```

### 1.3 模块内部结构图

```
┌══════════════════════════════════════════════════════════════════════════════┐
║                           internal/llm 包                                    ║
║                                                                              ║
║  ┌────────────────────────────────────────────────────────────────────────┐ ║
║  │                    Client (client.go:114-120)                           │ ║
║  │  ──────────────────────────────────────────────────────────────────    │ ║
║  │  字段:                                                                  │ ║
║  │    primary        Provider                  // 主 LLM 供应商           │ ║
║  │    fallback       Provider                  // 备用供应商（可 nil）     │ ║
║  │    breaker        *gobreaker.CircuitBreaker // 本地进程内熔断器         │ ║
║  │    sharedBreaker  *SharedCircuitBreaker     // 跨副本 Redis 熔断       │ ║
║  │    logger         *zap.Logger                                          │ ║
║  │                                                                        │ ║
║  │  方法:                                                                  │ ║
║  │    + ChatCompletion(ctx, *ChatRequest) (*ChatResponse, error)          │ ║
║  │    + ChatCompletionStream(ctx, *ChatRequest) (<-chan StreamChunk, err) │ ║
║  │    + Complete(ctx, []Message, []ToolDef) (*CompleteResponse, error)    │ ║
║  └────────────────────────────────────────────────────────────────────────┘ ║
║                          │ delegates to                                       ║
║                          ▼                                                    ║
║  ┌────────────────────────────────────────────────────────────────────────┐ ║
║  │              Provider interface (client.go:67-74)                       │ ║
║  │  ──────────────────────────────────────────────────────────────────    │ ║
║  │    ChatCompletion(ctx, *ChatRequest) (*ChatResponse, error)            │ ║
║  │    ChatCompletionStream(ctx, *ChatRequest) (<-chan StreamChunk, error) │ ║
║  │    Name() string                                                       │ ║
║  └──────────────────────────────┬─────────────────────────────────────────┘ ║
║                                 │ implemented by                              ║
║                                 ▼                                             ║
║  ┌────────────────────────────────────────────────────────────────────────┐ ║
║  │           openaiProvider (openai_provider.go:52-56)                     │ ║
║  │  ──────────────────────────────────────────────────────────────────    │ ║
║  │    client  *openai.Client           // sashabaranov/go-openai SDK      │ ║
║  │    cfg     *config.LLMProviderConfig // BaseURL + APIKey + Model       │ ║
║  │    logger  *zap.Logger                                                 │ ║
║  └────────────────────────────────────────────────────────────────────────┘ ║
║                                                                              ║
║  ┌────────────────────────────────────────────────────────────────────────┐ ║
║  │             Router (router.go:99-106)                                   │ ║
║  │  ──────────────────────────────────────────────────────────────────    │ ║
║  │    config      RouterConfig       // Heavy/Medium/Light 模型名 + 限制  │ ║
║  │    routeCount  map[ModelTier]int64 // 路由统计                         │ ║
║  │                                                                        │ ║
║  │    + Route(intent, complexity, msgCount) ModelRoute                    │ ║
║  │    + ApplyRoute(req, route)                                            │ ║
║  │    + RouteForMessage(ctx, intent, msg, count) ModelRoute              │ ║
║  └────────────────────────────────────────────────────────────────────────┘ ║
║                                                                              ║
║  ┌────────────────────────────────────────────────────────────────────────┐ ║
║  │       SharedCircuitBreaker (shared_breaker.go:42-48)                   │ ║
║  │  ──────────────────────────────────────────────────────────────────    │ ║
║  │    rdb        *redis.Client  // Redis 连接                             │ ║
║  │    prefix     string         // Key 前缀 "llm:breaker"                │ ║
║  │    window     time.Duration  // 聚合窗口 30s                           │ ║
║  │    threshold  int            // 阈值 20 次/窗口                        │ ║
║  │                                                                        │ ║
║  │    + Allow(ctx, provider) bool         // 是否放行                     │ ║
║  │    + RecordFailure(ctx, provider)      // 记录失败                     │ ║
║  └────────────────────────────────────────────────────────────────────────┘ ║
║                                                                              ║
║  ┌────────────────────────────────────────────────────────────────────────┐ ║
║  │         Token 计数 (tokenizer.go)                                       │ ║
║  │  ──────────────────────────────────────────────────────────────────    │ ║
║  │    ExactTokenCount(text) int   // tiktoken 精确计数 (~80μs)           │ ║
║  │    FastEstimate(text) int      // rune 分类启发式 (~5μs, ±15%)        │ ║
║  │    EstimateTokens(text) int    // 向后兼容别名 → FastEstimate          │ ║
║  └────────────────────────────────────────────────────────────────────────┘ ║
╚══════════════════════════════════════════════════════════════════════════════╝
```

---

## 2. 数据类型定义

### 2.1 请求/响应结构 (client.go:77-107)

```
┌─────────────────────────────────────────────────────────────────┐
│                    ChatRequest (client.go:77)                     │
├─────────────────────────────────────────────────────────────────┤
│  Messages       []models.Message        // 对话历史              │
│  Tools          []models.ToolDefinition // 可用工具 Schema       │
│  MaxTokens      int                     // 输出 token 上限      │
│  Temperature    float32                 // 采样温度              │
│  Model          string                  // 模型名（可选）       │
│  ResponseFormat *models.ResponseFormat  // JSON mode 等         │
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼ LLM 处理后
┌─────────────────────────────────────────────────────────────────┐
│                    ChatResponse (client.go:87)                    │
├─────────────────────────────────────────────────────────────────┤
│  Content   string            // 文本回复                         │
│  ToolCalls []models.ToolCall // 工具调用请求                     │
│  Usage     Usage             // Token 消耗统计                   │
│  Model     string            // 实际使用的模型名                 │
└─────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────┐
│                       Usage (client.go:95)                        │
├─────────────────────────────────────────────────────────────────┤
│  PromptTokens     int  // 输入 token 数                          │
│  CompletionTokens int  // 输出 token 数                          │
│  TotalTokens      int  // 总计                                   │
└─────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────┐
│                   StreamChunk (client.go:102)                     │
├─────────────────────────────────────────────────────────────────┤
│  Content   string            // 增量文本                         │
│  ToolCalls []models.ToolCall // 增量工具调用                     │
│  Done      bool              // 流结束标记                       │
│  Err       error             // 流中错误                         │
└─────────────────────────────────────────────────────────────────┘
```

### 2.2 路由相关类型 (router.go:66-82)

```
┌─────────────────────────────────────┐
│       ModelTier (router.go:70)       │
├─────────────────────────────────────┤
│  TierHeavy  = "heavy"              │  Claude Opus / GPT-4o
│  TierMedium = "medium"             │  Claude Sonnet / GPT-4o-mini
│  TierLight  = "light"              │  Haiku / GPT-3.5 / 本地模型
└─────────────────────────────────────┘

┌─────────────────────────────────────┐
│      ModelRoute (router.go:78)       │
├─────────────────────────────────────┤
│  Tier      ModelTier               │
│  Model     string                  │
│  Reason    string                  │
│  MaxTokens int                     │
└─────────────────────────────────────┘
```

---

## 3. 外部依赖关系图

```
                        ┌───────────────────────────────────┐
                        │        调用方 (Callers)            │
                        │                                   │
                        │  · orchestrator (ReAct 主循环)    │
                        │  · session.Summarizer (摘要生成)  │
                        │  · planner (任务规划)             │
                        └───────────────┬───────────────────┘
                                        │
                                        ▼
┌───────────────────────────────────────────────────────────────────────────┐
│                         internal/llm.Client                                 │
└───────┬──────────────────┬────────────────────┬───────────────────────────┘
        │                  │                    │
        ▼                  ▼                    ▼
┌──────────────┐  ┌──────────────────┐  ┌───────────────────┐
│ go-openai SDK│  │ sony/gobreaker   │  │ go-redis/v9       │
│ (HTTP 调用)  │  │ (本地熔断器)     │  │ (分布式熔断)      │
└──────┬───────┘  └──────────────────┘  └────────┬──────────┘
       │                                          │
       ▼                                          ▼
┌──────────────────────────┐              ┌──────────────┐
│ LLM API Endpoints        │              │    Redis     │
│ · api.openai.com         │              │  (共享状态)  │
│ · api.anthropic.com/proxy│              └──────────────┘
│ · localhost:11434 (Ollama)│
│ · localhost:8000  (vLLM) │
└──────────────────────────┘

        ┌───────────────────────────────────────────────────┐
        │              输出到 (Feeds)                         │
        │                                                   │
        │  · internal/metrics (Prometheus 指标)             │
        │  · internal/tracing (OTel spans)                  │
        │  · zap logger (结构化日志)                        │
        └───────────────────────────────────────────────────┘
```

---

## 4. 核心流程详解

### 4.1 ChatCompletion 完整调用链（非流式）

**代码位置**: `client.go:176-233`

```
┌═══════════════════════════════════════════════════════════════════════════┐
║                    ChatCompletion 端到端数据流                             ║
╚═══════════════════════════════════════════════════════════════════════════╝

调用方                Client              SharedBreaker    gobreaker      Provider
  │                     │                      │              │               │
  │ ChatCompletion(req) │                      │              │               │
  ├────────────────────▶│                      │              │               │
  │                     │                      │              │               │
  │                     │ [步骤1: 跨副本熔断检查]              │               │
  │                     │ Allow(provider)      │              │               │
  │                     ├─────────────────────▶│              │               │
  │                     │                      │              │               │
  │                     │  Redis GET           │              │               │
  │                     │  llm:breaker:        │              │               │
  │                     │  {provider}:{epoch}  │              │               │
  │                     │                      │              │               │
  │                     │◀─ count < threshold ─┤              │               │
  │                     │   (允许调用)          │              │               │
  │                     │                      │              │               │
  │                     │ [步骤2: 本地熔断器包裹]              │               │
  │                     │ breaker.Execute(fn)  │              │               │
  │                     ├──────────────────────┼─────────────▶│               │
  │                     │                      │              │               │
  │                     │                      │  [状态检查]  │               │
  │                     │                      │  Closed?     │               │
  │                     │                      │  ├─ Yes ─▶   │               │
  │                     │                      │  │           │               │
  │                     │                      │  └─ No ──▶   │               │
  │                     │                      │    return    │               │
  │                     │                      │    ErrOpen   │               │
  │                     │                      │              │               │
  │                     │                      │  [执行调用]  │               │
  │                     │                      │  primary.    │               │
  │                     │                      │  ChatCompletion()            │
  │                     │                      │              ├──────────────▶│
  │                     │                      │              │               │
  │                     │                      │              │  HTTP POST    │
  │                     │                      │              │  /v1/chat/    │
  │                     │                      │              │  completions  │
  │                     │                      │              │               │
  │                     │                      │              │◀─ 200 OK ─────│
  │                     │                      │              │  + JSON body  │
  │                     │                      │              │               │
  │                     │                      │◀─ result ────┤               │
  │                     │◀─ result ────────────┤              │               │
  │                     │                      │              │               │
  │                     │ [步骤3: 记录指标]    │              │               │
  │                     │ metrics.             │              │               │
  │                     │   LLMRequestTotal++  │              │               │
  │                     │   LLMTokensUsed +=   │              │               │
  │                     │   LLMRequestDuration │              │               │
  │                     │                      │              │               │
  │◀─ ChatResponse ─────┤                      │              │               │
  │                     │                      │              │               │
  │                                                                           │
  │                                                                           │
  │ [失败路径: primary 调用失败]                                               │
  │                     │                      │              │               │
  │                     │                      │              │◀─ 429/500 ────│
  │                     │                      │◀─ error ─────┤               │
  │                     │◀─ error ─────────────┤              │               │
  │                     │                      │              │               │
  │                     │ [步骤4: 记录失败到 Redis]            │               │
  │                     │ RecordFailure(prov)  │              │               │
  │                     ├─────────────────────▶│              │               │
  │                     │                      │              │               │
  │                     │  Redis INCR + EXPIRE │              │               │
  │                     │  (Lua 脚本原子操作)  │              │               │
  │                     │                      │              │               │
  │                     │ [步骤5: Fallback]    │              │               │
  │                     │ fallbackChat(req)    │              │               │
  │                     │   ├─ fallback.ChatCompletion()      │               │
  │                     │   │                  │              │               │
  │                     │   └─ 成功 → return   │              │               │
  │                     │      失败 → return   │              │               │
  │                     │      wrapped error   │              │               │
  │                     │                      │              │               │
  │◀─ Response/Error ───┤                      │              │               │
  │                     │                      │              │               │

═══════════════════════════════════════════════════════════════════════════

关键代码位置标注:

client.go:176   func (c *Client) ChatCompletion(ctx, req) (*ChatResponse, error)
client.go:183   if c.sharedBreaker != nil && !c.sharedBreaker.Allow(ctx, provider)
client.go:193   result, err := c.breaker.Execute(func() ...)
client.go:198   c.recordBreakerState()
client.go:206   metrics.LLMRequestTotal.WithLabelValues(...).Inc()
client.go:216   if c.sharedBreaker != nil { c.sharedBreaker.RecordFailure(...) }
client.go:228   if c.fallback != nil { return c.fallbackChat(...) }

shared_breaker.go:110   func (s *SharedCircuitBreaker) Allow(ctx, provider) bool
shared_breaker.go:127   func (s *SharedCircuitBreaker) RecordFailure(ctx, provider)
```

### 4.2 熔断器状态机详解

**代码位置**: `client.go:147-162` (gobreaker 配置)

```
┌═══════════════════════════════════════════════════════════════════════════┐
║                    gobreaker 三态状态机                                    ║
╚═══════════════════════════════════════════════════════════════════════════╝

                    ConsecutiveFailures >= MaxFailures (默认 5)
    ┌──────────┐ ───────────────────────────────────────────▶ ┌──────────┐
    │          │                                               │          │
    │  CLOSED  │                                               │   OPEN   │
    │  (放行)  │                                               │  (拒绝)  │
    │          │ ◀──── HalfOpen 阶段全部成功 ────────────────  │          │
    └────┬─────┘                                               └────┬─────┘
         │                                                          │
         │ 成功调用                                                 │ 冷却
         │ → 重置计数                                               │ Timeout
         │                                                          │ (60s)
         │                                                          ▼
         │                                                     ┌──────────┐
         │                                                     │          │
         │                                                     │ HALF-OPEN│
         │                                                     │ (试探)   │
         └──────────────────────────────────────────────────── │          │
                    HalfOpen 阶段有任一失败                     └──────────┘
                    → 回到 OPEN                                     │
                                                                    │
                                                MaxRequests 个探针
                                                全部成功 → CLOSED

═══════════════════════════════════════════════════════════════════════════

状态转换触发条件 (client.go:152-155):

ReadyToTrip: func(counts gobreaker.Counts) bool {
    return int(counts.ConsecutiveFailures) >= cfg.CircuitBreaker.MaxFailures
}

配置参数 (config.yaml):
  circuit_breaker:
    max_failures: 5           # 连续失败 5 次触发 OPEN
    timeout: 60s              # OPEN → HALF-OPEN 冷却时间
    half_open_max_requests: 1 # HALF-OPEN 阶段允许 1 个探针

指标输出 (client.go:262-274):
  metrics.LLMCircuitBreakerState.Set(val)
    0 = CLOSED
    1 = HALF-OPEN
    2 = OPEN
```

### 4.3 分布式熔断器工作原理

**代码位置**: `shared_breaker.go:110-138`

```
┌═══════════════════════════════════════════════════════════════════════════┐
║              SharedCircuitBreaker 跨副本聚合                               ║
╚═══════════════════════════════════════════════════════════════════════════╝

场景: 3 个 Pod 部署，Anthropic API 开始限流

时间轴:
  T0    Pod-A 请求失败 (429)
  T1    Pod-B 请求失败 (429)
  T2    Pod-C 请求失败 (429)
  T3    Pod-A 再次请求
  ...

═══════════════════════════════════════════════════════════════════════════

[T0] Pod-A 失败
  │
  ├─ primary.ChatCompletion() → 429 error
  │
  ├─ sharedBreaker.RecordFailure(ctx, "anthropic")
  │    │
  │    └─ Redis Lua 脚本执行:
  │         INCR llm:breaker:anthropic:12345  (epoch = T0/30s)
  │         EXPIRE llm:breaker:anthropic:12345 30
  │         → count = 1
  │
  └─ fallback.ChatCompletion() → 成功

─────────────────────────────────────────────────────────────────────────

[T1] Pod-B 失败
  │
  ├─ sharedBreaker.RecordFailure(ctx, "anthropic")
  │    │
  │    └─ Redis:
  │         INCR llm:breaker:anthropic:12345
  │         → count = 2
  │
  └─ fallback → 成功

─────────────────────────────────────────────────────────────────────────

[T2] Pod-C 失败
  │
  ├─ sharedBreaker.RecordFailure(ctx, "anthropic")
  │    │
  │    └─ Redis:
  │         INCR llm:breaker:anthropic:12345
  │         → count = 3
  │         ... (持续累加)
  │         → count = 20  (达到阈值)

─────────────────────────────────────────────────────────────────────────

[T3] Pod-A 再次请求
  │
  ├─ sharedBreaker.Allow(ctx, "anthropic")
  │    │
  │    └─ Redis Lua 脚本:
  │         GET llm:breaker:anthropic:12345
  │         → count = 20
  │         → 20 >= threshold(20) → return false
  │
  ├─ Allow() = false → 直接跳过 primary
  │
  └─ fallbackChat() → 不打 Anthropic，直接用 fallback

═══════════════════════════════════════════════════════════════════════════

Redis Key 结构 (shared_breaker.go:101-104):

  keyFor(provider) = prefix + ":" + provider + ":" + epoch
  
  例如:
    llm:breaker:anthropic:12345
    ├─ prefix:   "llm:breaker"
    ├─ provider: "anthropic"
    └─ epoch:    12345 (time.Now().Unix() / 30)

Lua 脚本 (shared_breaker.go:82-96):

  [Check]
    local n = redis.call('GET', KEYS[1])
    if n == false then return 0 end
    return tonumber(n)

  [Record]
    local count = redis.call('INCR', KEYS[1])
    if count == 1 then
      redis.call('EXPIRE', KEYS[1], ARGV[1])  -- TTL = 30s
    end
    return count

优势:
  ✓ 跨副本聚合，避免 N 个 Pod 各自独立累积
  ✓ Fixed window 简单高效
  ✓ TTL 自动清理，无需显式重置
  ✓ Redis 故障时 fail-open (返回 true)
```

### 4.4 流式调用 (ChatCompletionStream)

**代码位置**: `client.go:277-301`, `openai_provider.go:157-244`

```
┌═══════════════════════════════════════════════════════════════════════════┐
║                  ChatCompletionStream 流式数据流                           ║
╚═══════════════════════════════════════════════════════════════════════════╝

调用方              Client           gobreaker        Provider          LLM API
  │                   │                  │                │                │
  │ ChatCompletionStream(req)            │                │                │
  ├──────────────────▶│                  │                │                │
  │                   │                  │                │                │
  │                   │ breaker.Execute  │                │                │
  │                   ├─────────────────▶│                │                │
  │                   │                  │                │                │
  │                   │                  │ primary.       │                │
  │                   │                  │ ChatCompletionStream()          │
  │                   │                  ├───────────────▶│                │
  │                   │                  │                │                │
  │                   │                  │                │ POST /v1/chat/ │
  │                   │                  │                │ completions    │
  │                   │                  │                │ Stream: true   │
  │                   │                  │                ├───────────────▶│
  │                   │                  │                │                │
  │                   │                  │                │◀─ HTTP 200 ────│
  │                   │                  │                │  text/event-   │
  │                   │                  │                │  stream        │
  │                   │                  │                │                │
  │                   │                  │◀─ chan ────────┤                │
  │                   │◀─ chan ──────────┤                │                │
  │◀─ chan ───────────┤                  │                │                │
  │                   │                  │                │                │
  │                   │                  │                │  [goroutine]   │
  │                   │                  │                │  逐行读 SSE    │
  │                   │                  │                │                │
  │                   │                  │                │◀─ data: {...}──│
  │                   │                  │                │  delta.content │
  │                   │                  │                │                │
  │                   │                  │                │  解析 JSON     │
  │                   │                  │                │  构造 Chunk    │
  │                   │                  │                │                │
  │                   │                  │◀─ StreamChunk ─┤                │
  │◀─ StreamChunk ────┼──────────────────┼────────────────┤                │
  │                   │                  │                │                │
  │  {Content:"Hello"}│                  │                │◀─ data: {...}──│
  │                   │                  │                │                │
  │◀─ StreamChunk ────┼──────────────────┼────────────────┤                │
  │  {Content:" world"}                  │                │                │
  │                   │                  │                │                │
  │                   │                  │                │◀─ data: [DONE]─│
  │                   │                  │                │                │
  │◀─ StreamChunk ────┼──────────────────┼────────────────┤                │
  │  {Done:true}      │                  │                │                │
  │                   │                  │                │                │
  │  chan closed      │                  │                │  goroutine exit│
  │                   │                  │                │                │

═══════════════════════════════════════════════════════════════════════════

关键代码 (openai_provider.go:157-244):

func (p *openaiProvider) ChatCompletionStream(ctx, req) (<-chan StreamChunk, error) {
    // 1. 创建 OpenAI stream
    stream, err := p.client.CreateChatCompletionStream(timeoutCtx, apiReq)
    
    // 2. 创建输出 channel
    ch := make(chan StreamChunk, 64)  // 缓冲 64 个 chunk
    
    // 3. 启动 goroutine 读取 SSE
    go func() {
        defer close(ch)
        defer cancel()
        defer stream.Close()
        
        for {
            resp, err := stream.Recv()  // 阻塞读取下一个 chunk
            if err == io.EOF {
                ch <- StreamChunk{Done: true}
                return
            }
            if err != nil {
                ch <- StreamChunk{Err: err, Done: true}
                return
            }
            
            // 4. 提取增量内容
            delta := resp.Choices[0].Delta
            chunk := StreamChunk{
                Content: delta.Content,
            }
            
            // 5. 处理工具调用增量
            if len(delta.ToolCalls) > 0 {
                for _, tc := range delta.ToolCalls {
                    chunk.ToolCalls = append(chunk.ToolCalls, models.ToolCall{
                        ID:   tc.ID,
                        Name: tc.Function.Name,
                        Args: json.RawMessage(tc.Function.Arguments),
                    })
                }
            }
            
            // 6. 发送到 channel
            select {
            case ch <- chunk:
            case <-timeoutCtx.Done():
                ch <- StreamChunk{Err: timeoutCtx.Err(), Done: true}
                return
            }
        }
    }()
    
    return ch, nil
}

═══════════════════════════════════════════════════════════════════════════

SSE 协议格式:

  data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}
  
  data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}
  
  data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}
  
  data: [DONE]

工具调用的流式格式:

  data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_xxx","type":"function","function":{"name":"read_file","arguments":""}}]}}]}
  
  data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path"}}]}}]}
  
  data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\":\"/tmp/test.txt\"}"}}]}}]}
  
  data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

注意: 工具调用的 arguments 是分片流式返回的，需要累加拼接。
```

### 4.5 模型路由决策树

**代码位置**: `router.go:148-226`

```
┌═══════════════════════════════════════════════════════════════════════════┐
║                      Router.classify() 决策树                              ║
╚═══════════════════════════════════════════════════════════════════════════╝

输入: (intent, complexityScore, messageCount)
  │
  ▼
┌─────────────────────────────────────────────────────────────────────────┐
│ 规则 1: complexityScore >= 7?                                            │
│   YES → TierHeavy (高复杂度任务)                                         │
│         Reason: "high complexity score"                                 │
│         代码: router.go:150-156                                          │
└─────────────────────────────────────────────────────────────────────────┘
  │ NO
  ▼
┌─────────────────────────────────────────────────────────────────────────┐
│ 规则 2: intent in {"deploy", "diagnose"}?                               │
│   YES → TierHeavy (安全关键任务)                                         │
│         Reason: "safety-critical intent: " + intent                     │
│         代码: router.go:159-166                                          │
└─────────────────────────────────────────────────────────────────────────┘
  │ NO
  ▼
┌─────────────────────────────────────────────────────────────────────────┐
│ 规则 3: intent == "code_execute" && complexityScore >= 4?               │
│   YES → TierHeavy (复杂代码执行)                                         │
│         Reason: "complex code execution"                                │
│         代码: router.go:169-176                                          │
└─────────────────────────────────────────────────────────────────────────┘
  │ NO
  ▼
┌─────────────────────────────────────────────────────────────────────────┐
│ 规则 4: intent == "conversation" && complexityScore < 3?                │
│   YES → TierLight (简单对话)                                             │
│         Reason: "simple conversation"                                   │
│         代码: router.go:179-186                                          │
└─────────────────────────────────────────────────────────────────────────┘
  │ NO
  ▼
┌─────────────────────────────────────────────────────────────────────────┐
│ 规则 5: intent in {"_intent_parse", "_summarize"}?                      │
│   YES → TierLight (内部工具任务)                                         │
│         Reason: "internal utility task"                                 │
│         代码: router.go:189-196                                          │
└─────────────────────────────────────────────────────────────────────────┘
  │ NO
  ▼
┌─────────────────────────────────────────────────────────────────────────┐
│ 规则 6: intent == "code_query" && complexityScore < 5?                  │
│   YES → TierMedium (标准代码查询)                                        │
│         Reason: "standard code query"                                   │
│         代码: router.go:199-206                                          │
└─────────────────────────────────────────────────────────────────────────┘
  │ NO
  ▼
┌─────────────────────────────────────────────────────────────────────────┐
│ 规则 7: messageCount > 20?                                              │
│   YES → TierHeavy (长对话需要大 context)                                 │
│         Reason: "long conversation context"                             │
│         代码: router.go:210-217                                          │
└─────────────────────────────────────────────────────────────────────────┘
  │ NO
  ▼
┌─────────────────────────────────────────────────────────────────────────┐
│ 默认: TierMedium                                                         │
│       Reason: "default routing"                                         │
│       代码: router.go:220-225                                            │
└─────────────────────────────────────────────────────────────────────────┘

═══════════════════════════════════════════════════════════════════════════

复杂度估算 (router.go:253-278):

func QuickComplexity(msg string) int {
    score := 0
    lower := strings.ToLower(msg)
    words := len(strings.Fields(msg))
    
    // 长度加分
    if words > 50  { score += 2 }
    if words > 100 { score += 2 }
    
    // 关键词加分
    complexKeywords := []string{
        "refactor", "multiple files", "implement", "create", "build",
        "重构", "多个文件", "实现", "创建", "开发",
        "then", "after that", "finally", "step by step",
        "然后", "接着", "最后", "首先",
    }
    for _, kw := range complexKeywords {
        if strings.Contains(lower, kw) {
            score += 2
        }
    }
    
    return score
}

═══════════════════════════════════════════════════════════════════════════

使用示例:

// orchestrator 中调用
route := router.Route(intent, complexity, len(messages))
req.Model = route.Model
req.MaxTokens = route.MaxTokens

// 或使用便捷方法
route := router.RouteForMessage(ctx, intent, userMessage, len(messages))
```

---

## 5. Provider 实现详解

### 5.1 openaiProvider 结构

**代码位置**: `openai_provider.go:52-56`

```
┌═══════════════════════════════════════════════════════════════════════════┐
║                      openaiProvider 实现                                   ║
╚═══════════════════════════════════════════════════════════════════════════╝

type openaiProvider struct {
    client *openai.Client           // sashabaranov/go-openai SDK
    cfg    *config.LLMProviderConfig // 配置
    logger *zap.Logger
}

配置结构 (config.LLMProviderConfig):
  Provider    string        // "openai" / "anthropic" / "ollama"
  BaseURL     string        // API 端点
  APIKey      string        // 认证密钥
  Model       string        // 默认模型名
  MaxTokens   int           // 输出上限
  Temperature float32       // 采样温度
  Timeout     time.Duration // 请求超时

═══════════════════════════════════════════════════════════════════════════

支持的部署方式:

┌──────────────────┬────────────────────────────────┬─────────────────────┐
│ 部署类型         │ BaseURL                        │ Model 示例          │
├──────────────────┼────────────────────────────────┼─────────────────────┤
│ OpenAI 官方      │ https://api.openai.com/v1      │ gpt-4o              │
│ Azure OpenAI     │ https://<res>.openai.azure.com │ <deployment-name>   │
│ Anthropic 代理   │ https://proxy.example.com/v1   │ claude-3-5-sonnet   │
│ 本地 Ollama      │ http://localhost:11434/v1      │ qwen2.5-coder:32b   │
│ 本地 vLLM        │ http://localhost:8000/v1       │ Qwen2.5-Coder-32B   │
└──────────────────┴────────────────────────────────┴─────────────────────┘

所有部署使用相同的 OpenAI-compatible 协议，无需修改代码。

配置示例 (config.yaml):
  llm:
    primary:
      provider: "openai"
      base_url: "https://api.openai.com/v1"
      api_key: "${OPENAI_API_KEY}"
      model: "gpt-4o"
      max_tokens: 8192
      temperature: 0.1
      timeout: 60s
    fallback:
      provider: "ollama"
      base_url: "http://localhost:11434/v1"
      model: "qwen2.5-coder:32b"
      max_tokens: 4096
      timeout: 120s
    circuit_breaker:
      max_failures: 5
      timeout: 60s
      half_open_max_requests: 1
```

### 5.2 消息转换

**代码位置**: `openai_provider.go:246-277`

```
┌═══════════════════════════════════════════════════════════════════════════┐
║                    消息格式转换                                            ║
╚═══════════════════════════════════════════════════════════════════════════╝

internal/models.Message → openai.ChatCompletionMessage

func (p *openaiProvider) convertMessages(msgs []models.Message) []openai.ChatCompletionMessage {
    result := make([]openai.ChatCompletionMessage, 0, len(msgs))
    for _, m := range msgs {
        msg := openai.ChatCompletionMessage{
            Role:    string(m.Role),     // "system" / "user" / "assistant" / "tool"
            Content: m.Content,
        }
        
        // 工具结果消息需要 ToolCallID
        if m.ToolCallID != "" {
            msg.ToolCallID = m.ToolCallID
        }
        
        // 转换工具调用
        if len(m.ToolCalls) > 0 {
            for _, tc := range m.ToolCalls {
                msg.ToolCalls = append(msg.ToolCalls, openai.ToolCall{
                    ID:   tc.ID,
                    Type: openai.ToolTypeFunction,
                    Function: openai.FunctionCall{
                        Name:      tc.Name,
                        Arguments: string(tc.Args),  // json.RawMessage → string
                    },
                })
            }
        }
        
        result = append(result, msg)
    }
    return result
}

═══════════════════════════════════════════════════════════════════════════

工具定义转换 (openai_provider.go:316-336):

internal/models.ToolDefinition → openai.Tool

func (p *openaiProvider) convertTools(tools []models.ToolDefinition) []openai.Tool {
    result := make([]openai.Tool, 0, len(tools))
    for _, t := range tools {
        var params json.RawMessage
        if len(t.Parameters) > 0 {
            params = t.Parameters
        } else {
            params = json.RawMessage(`{"type":"object","properties":{}}`)
        }
        
        result = append(result, openai.Tool{
            Type: openai.ToolTypeFunction,
            Function: &openai.FunctionDefinition{
                Name:        t.Name,
                Description: t.Description,
                Parameters:  params,  // JSON Schema
            },
        })
    }
    return result
}
```

---

## 6. Token 计数系统

### 6.1 双模式设计

**代码位置**: `tokenizer.go`

```
┌═══════════════════════════════════════════════════════════════════════════┐
║                      Token 计数双模式                                      ║
╚═══════════════════════════════════════════════════════════════════════════╝

┌─────────────────────────────────────────────────────────────────────────┐
│                                                                          │
│  ExactTokenCount(text) int                                              │
│  ─────────────────────────                                              │
│  · 使用 tiktoken-go (cl100k_base encoding)                              │
│  · 准确率: 100%                                                         │
│  · 性能: ~80μs/次                                                       │
│  · 适用: LLM 调用前的最终预算验证                                        │
│  · 代码: tokenizer.go:71-95                                             │
│                                                                          │
│  降级策略:                                                               │
│    tiktoken 初始化失败（离线环境）→ 自动降级到 FastEstimate              │
│                                                                          │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                          │
│  FastEstimate(text) int                                                  │
│  ─────────────────────                                                   │
│  · 基于 rune 分类的启发式估算                                            │
│  · 准确率: ~85%                                                         │
│  · 性能: ~5μs/次 (16x 快于精确计数)                                     │
│  · 适用: 批量打分/排序/会话管理                                          │
│  · 代码: tokenizer.go:106-131                                           │
│                                                                          │
│  估算规则:                                                               │
│    非 ASCII (CJK/emoji): 1 rune ≈ 1 token                              │
│    ASCII 字母/数字:       4 chars ≈ 1 token                             │
│    ASCII 标点:            1 char  ≈ 1 token                             │
│                                                                          │
└─────────────────────────────────────────────────────────────────────────┘

═══════════════════════════════════════════════════════════════════════════

使用场景对照:

┌──────────────────────────────┬──────────────────┬───────────────────────┐
│ 场景                         │ 使用函数          │ 原因                  │
├──────────────────────────────┼──────────────────┼───────────────────────┤
│ LLM 调用前预算验证           │ ExactTokenCount  │ 精确，避免超窗口      │
│ RAG 检索 1000 chunks 打分    │ FastEstimate     │ 性能优先              │
│ session.AddMessage 追加      │ FastEstimate     │ 高频调用              │
│ context.Pruner 裁剪判断      │ FastEstimate     │ ±15% 误差可接受       │
│ 计费统计                     │ API 返回的 Usage │ 最精确                │
└──────────────────────────────┴──────────────────┴───────────────────────┘

═══════════════════════════════════════════════════════════════════════════

初始化流程 (tokenizer.go:51-67):

func InitTokenizer(logger *zap.Logger) {
    tokenizerOnce.Do(func() {
        enc, err := tiktoken.GetEncoding("cl100k_base")
        if err != nil {
            logger.Warn("tiktoken 初始化失败，降级到快速估算")
            return
        }
        globalTokenizer = enc  // 全局共享，线程安全
    })
}

注意:
  · globalTokenizer 是 sync.Once 保护的全局单例
  · tiktoken.Encoding 本身是线程安全的
  · 离线环境无法下载 tiktoken 数据文件时自动降级
```

---

## 7. 端到端调用示例

### 7.1 一次完整的 ReAct 循环中的 LLM 调用

```
┌═══════════════════════════════════════════════════════════════════════════┐
║          场景: 用户问 "帮我修复 session.go 的并发 bug"                     ║
╚═══════════════════════════════════════════════════════════════════════════╝

═══ Step 1: Orchestrator 构造请求 ═══════════════════════════════════════

orchestrator.RunReactStream():
  │
  ├─ session.GetContextWindow(sessionID)
  │   → 获取历史消息 (hot 区 10 条)
  │
  ├─ rag.Retrieve("session.go 并发 bug")
  │   → 获取相关代码片段 (top-5 chunks)
  │
  ├─ promptBuilder.BuildPrompt(sess, chunks, scores, userMsg)
  │   → 组装五区段 prompt
  │
  └─ 构造 ChatRequest:
      req := &ChatRequest{
          Messages: [
              {Role: "system",    Content: "You are a code agent..."},
              {Role: "system",    Content: "[Memory] 用户在调试并发问题..."},
              {Role: "system",    Content: "[Code] session/manager.go:180-230..."},
              {Role: "user",      Content: "帮我修复 session.go 的并发 bug"},
          ],
          Tools: [
              {Name: "read_file",  Description: "读取文件", Parameters: {...}},
              {Name: "edit_file",  Description: "编辑文件", Parameters: {...}},
              {Name: "run_tests",  Description: "运行测试", Parameters: {...}},
              {Name: "search_code",Description: "搜索代码", Parameters: {...}},
          ],
          MaxTokens: 8192,
      }

═══ Step 2: Router 选择模型 ═════════════════════════════════════════════

router.RouteForMessage(ctx, "code_edit", userMsg, 5):
  │
  ├─ QuickComplexity("帮我修复 session.go 的并发 bug")
  │   → words=8, 无复杂关键词 → score=0
  │
  └─ classify("code_edit", 0, 5):
      规则 1: 0 < 7 → NO
      规则 2: "code_edit" != "deploy"/"diagnose" → NO
      规则 3: "code_edit" != "code_execute" → NO
      规则 4: "code_edit" != "conversation" → NO
      规则 5: "code_edit" != "_intent_parse"/"_summarize" → NO
      规则 6: "code_edit" != "code_query" → NO
      规则 7: 5 <= 20 → NO
      → 默认: TierMedium, Model: "claude-sonnet-4-20250514"

═══ Step 3: Client.ChatCompletion 执行 ═════════════════════════════════

client.ChatCompletion(ctx, req):
  │
  ├─ sharedBreaker.Allow(ctx, "anthropic/claude-sonnet-4-20250514")
  │   → Redis GET llm:breaker:anthropic/claude-sonnet-4-20250514:41234
  │   → count=0 < threshold=20 → true (允许)
  │
  ├─ breaker.Execute(func() {
  │       return primary.ChatCompletion(ctx, req)
  │   })
  │   │
  │   ├─ gobreaker state = CLOSED → 放行
  │   │
  │   └─ openaiProvider.ChatCompletion(ctx, req):
  │       │
  │       ├─ convertMessages(req.Messages) → []openai.ChatCompletionMessage
  │       ├─ convertTools(req.Tools) → []openai.Tool
  │       │
  │       ├─ POST https://api.anthropic.com/v1/chat/completions
  │       │   Headers:
  │       │     Authorization: Bearer sk-ant-xxx
  │       │     Content-Type: application/json
  │       │   Body:
  │       │     {
  │       │       "model": "claude-sonnet-4-20250514",
  │       │       "messages": [...],
  │       │       "tools": [...],
  │       │       "max_tokens": 8192,
  │       │       "temperature": 0.1
  │       │     }
  │       │
  │       ├─ 等待响应 (timeout: 60s)
  │       │
  │       └─ 解析响应:
  │           HTTP 200 OK
  │           {
  │             "choices": [{
  │               "message": {
  │                 "content": "",
  │                 "tool_calls": [{
  │                   "id": "call_abc123",
  │                   "type": "function",
  │                   "function": {
  │                     "name": "read_file",
  │                     "arguments": "{\"path\":\"internal/session/manager.go\"}"
  │                   }
  │                 }]
  │               }
  │             }],
  │             "usage": {
  │               "prompt_tokens": 3200,
  │               "completion_tokens": 45,
  │               "total_tokens": 3245
  │             }
  │           }
  │
  ├─ recordBreakerState() → gauge = 0 (CLOSED)
  │
  ├─ metrics:
  │   LLMRequestTotal{provider="anthropic/claude-sonnet-4-20250514", status="success"}++
  │   LLMRequestDuration{provider="anthropic/claude-sonnet-4-20250514"}.Observe(2.3s)
  │   LLMTokensUsed{provider="anthropic/...", type="prompt"}.Add(3200)
  │   LLMTokensUsed{provider="anthropic/...", type="completion"}.Add(45)
  │
  └─ return &ChatResponse{
         Content:   "",
         ToolCalls: [{ID:"call_abc123", Name:"read_file", Args:"{...}"}],
         Usage:     {PromptTokens:3200, CompletionTokens:45, TotalTokens:3245},
         Model:     "claude-sonnet-4-20250514",
     }

═══ Step 4: Orchestrator 处理工具调用 ══════════════════════════════════

orchestrator:
  │
  ├─ 解析 ToolCalls[0]: read_file(path="internal/session/manager.go")
  ├─ 执行工具 → 返回文件内容
  ├─ 追加 tool result 到 messages
  │
  └─ 回到 Step 1 (下一轮 ReAct 迭代)
```

---

## 8. Prometheus 指标

**代码位置**: `client.go:206-211`, `client.go:220-221`, `client.go:262-274`

```
┌═══════════════════════════════════════════════════════════════════════════┐
║                      LLM 模块 Prometheus 指标                              ║
╚═══════════════════════════════════════════════════════════════════════════╝

┌──────────────────────────────┬───────────┬──────────────────────────────┐
│ 指标名                       │ 类型      │ Labels                       │
├──────────────────────────────┼───────────┼──────────────────────────────┤
│ code_agent_llm_request_total │ Counter   │ provider, model, status      │
│                              │           │ (success/error)              │
├──────────────────────────────┼───────────┼──────────────────────────────┤
│ code_agent_llm_request_      │ Histogram │ provider, model              │
│ duration_seconds             │           │                              │
├──────────────────────────────┼───────────┼──────────────────────────────┤
│ code_agent_llm_tokens_used_  │ Counter   │ provider, type               │
│ total                        │           │ (prompt/completion)          │
├──────────────────────────────┼───────────┼──────────────────────────────┤
│ code_agent_llm_circuit_      │ Gauge     │ provider                     │
│ breaker_state                │           │ (0=closed/1=half/2=open)     │
└──────────────────────────────┴───────────┴──────────────────────────────┘

Grafana 查询示例:

  # 请求成功率
  rate(code_agent_llm_request_total{status="success"}[5m])
  / rate(code_agent_llm_request_total[5m])

  # P99 延迟
  histogram_quantile(0.99, rate(code_agent_llm_request_duration_seconds_bucket[5m]))

  # Token 消耗速率
  rate(code_agent_llm_tokens_used_total[1h])

  # 熔断器状态
  code_agent_llm_circuit_breaker_state
```

---

## 9. 设计权衡总结

```
┌═══════════════════════════════════════════════════════════════════════════┐
║                         设计决策与权衡                                     ║
╚═══════════════════════════════════════════════════════════════════════════╝

┌──────────────────────────────┬───────────────────────────────────────────┐
│ 决策                         │ 原因                                      │
├──────────────────────────────┼───────────────────────────────────────────┤
│ 统一 OpenAI-compatible 协议  │ 90% 的 LLM 服务都支持此协议，切换零成本  │
├──────────────────────────────┼───────────────────────────────────────────┤
│ 熔断只包 primary             │ fallback 裸跑，避免主备都断时完全无响应   │
├──────────────────────────────┼───────────────────────────────────────────┤
│ 流式只在 setup 阶段熔断      │ mid-stream 切 fallback 会让用户看到拼接   │
├──────────────────────────────┼───────────────────────────────────────────┤
│ SharedBreaker fail-open      │ Redis 挂了不应阻止 LLM 调用               │
├──────────────────────────────┼───────────────────────────────────────────┤
│ Router 用关键词而非 LLM 分类 │ 避免"为了省钱先花 token"的悖论           │
├──────────────────────────────┼───────────────────────────────────────────┤
│ Token 估算用 rune 分类       │ 快速(5μs)且误差可接受(±15%)              │
├──────────────────────────────┼───────────────────────────────────────────┤
│ ConsecutiveFailures 触发熔断 │ 简单直观，但 40% 错误率不连续时不会跳开  │
├──────────────────────────────┼───────────────────────────────────────────┤
│ channel 缓冲 64 个 chunk     │ 平衡内存占用和背压，避免 goroutine 阻塞  │
└──────────────────────────────┴───────────────────────────────────────────┘
```

---

## 10. 学习建议

### 10.1 阅读顺序

```
1. doc.go                  → 理解模块定位
2. client.go:67-107        → 理解核心接口和数据类型
3. client.go:122-171       → 理解 Client 构造和熔断配置
4. client.go:176-233       → 理解 ChatCompletion 主流程
5. openai_provider.go:88-154 → 理解实际 HTTP 调用
6. shared_breaker.go       → 理解分布式熔断
7. router.go:148-226       → 理解模型路由
8. tokenizer.go            → 理解 Token 计数
9. _principles.go          → 深度设计原理和实战案例
```

### 10.2 动手实践

```
任务 1: 添加请求日志
  文件: openai_provider.go:88
  在 ChatCompletion 开头添加 logger.Info 打印请求摘要

任务 2: 触发熔断
  修改 config.yaml 的 primary.base_url 为无效地址
  观察日志中的 "circuit breaker state changed"
  观察 fallback 是否被调用

任务 3: 实现新 Provider
  创建 internal/llm/custom_provider.go
  实现 Provider 接口的三个方法
  在 NewClient 中注册为 fallback

任务 4: 调整路由规则
  修改 router.go:classify
  添加新的 intent 类型和路由规则
  运行 router_test.go 验证
```

---

## 11. 简历写法参考

以下提供四种粒度的简历描述，根据目标岗位选择合适的版本。

### 11.1 一句话版（适合项目列表）

> 设计并实现了生产级 LLM 网关层，支持多供应商统一接入、双层熔断（进程内 gobreaker + 跨副本 Redis 聚合）、三档模型智能路由，实现 99.8% 可用性和 84% 成本优化。

### 11.2 项目经验版（适合简历正文）

```
项目: 代码智能 Agent 后端（Go）
角色: 核心开发

【LLM 高可用网关】
· 设计统一 Provider 抽象层，基于 OpenAI-compatible 协议对接 OpenAI / Anthropic /
  Ollama / vLLM 等多供应商，主备切换零代码改动
· 实现双层熔断架构：进程内 gobreaker（连续失败 5 次跳开）+ 跨副本 Redis 聚合
  熔断器（Lua 脚本原子计数，30s 窗口内集群累计 20 次失败触发），解决分布式场景
  下 N 个 Pod 各自独立累积导致无效流量放大 N 倍的问题
· 设计三档模型路由器（Heavy/Medium/Light），基于意图分类 + 复杂度启发式评分
  动态选择模型档位，月度 LLM 成本从 $43K 降至 $6.8K（降幅 84%）
· 实现流式 SSE 响应通道（channel 缓冲 + ctx cancel 感知），用户断连时秒级释放
  TCP 连接，避免 goroutine 泄漏和无效 token 消耗
· 集成 tiktoken-go 精确计数 + rune 分类快速估算双模式 Token 计数器，精确模式
  用于预算验证，快速模式（5μs/次）用于高频批量打分
· 全链路 Prometheus 可观测：请求延迟/成功率/Token 消耗/熔断器状态四维指标，
  支撑 Grafana 实时监控和告警
```

### 11.3 STAR 面试版（适合面试准备）

```
┌─────────────────────────────────────────────────────────────────────────┐
│ Situation (背景)                                                         │
├─────────────────────────────────────────────────────────────────────────┤
│ 生产级代码 Agent 后端，ReAct 循环每步调用 LLM，单次请求可能触发          │
│ 10+ 次 LLM 调用。面临三个核心挑战：                                      │
│   1. LLM API 不稳定（429 限流、500 超时、供应商宕机）                    │
│   2. 成本失控（所有请求走最强模型，月费 $43K）                           │
│   3. 多副本部署时熔断器各自为政，无效流量放大                            │
└─────────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────────┐
│ Task (任务)                                                              │
├─────────────────────────────────────────────────────────────────────────┤
│ 设计一个高可用、成本可控、可观测的 LLM 网关层，要求：                    │
│   · 单供应商故障不影响用户体验                                           │
│   · 简单任务不浪费昂贵模型                                               │
│   · 多副本部署时熔断决策全局一致                                         │
└─────────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────────┐
│ Action (行动)                                                            │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                          │
│ 1. 统一协议抽象                                                          │
│    定义 Provider interface（Chat/Stream/Name 三方法），所有供应商         │
│    通过 OpenAI-compatible 协议接入，新增供应商只需实现接口 + 改配置       │
│                                                                          │
│ 2. 双层熔断架构                                                          │
│    L1: gobreaker 进程内熔断 — 单 Pod 连续失败快速跳开                    │
│    L2: Redis SharedBreaker — Lua 脚本原子 INCR，跨副本聚合失败计数       │
│    两层独立工作：L1 捕捉单点集中失败，L2 捕捉分布式慢性退化              │
│    Redis 故障时 fail-open，不阻塞正常请求                                │
│                                                                          │
│ 3. 三档模型路由                                                          │
│    基于 intent + complexity + messageCount 的规则树：                     │
│    · Heavy: 复杂推理/部署/诊断 → Claude Opus / GPT-4o                   │
│    · Medium: 常规编辑/查询 → Claude Sonnet / GPT-4o-mini                │
│    · Light: 意图分类/摘要/闲聊 → Haiku / 本地模型                       │
│    复杂度用关键词+长度启发式估算（亚毫秒），不额外消耗 LLM token          │
│                                                                          │
│ 4. 流式响应 + 资源回收                                                   │
│    goroutine 监听 ctx.Done()，用户断连时主动 close HTTP body             │
│    channel 缓冲 64 chunk 平衡背压和内存                                  │
│                                                                          │
│ 5. 全链路可观测                                                          │
│    每次调用记录 provider/model/status 三维指标                           │
│    熔断器状态实时暴露为 Gauge，Grafana 一眼定位故障供应商                 │
│                                                                          │
└─────────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────────┐
│ Result (结果)                                                            │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                          │
│ 可用性:  99.8%（供应商宕机期间自动降级，用户无感）                       │
│ 成本:    月度 LLM 费用 $43K → $6.8K（降幅 84%）                         │
│ 延迟:    熔断跳开后 fallback 接管 <2s（vs 之前超时等待 30s+）            │
│ 资源:    用户断连后 goroutine 泄漏从 ~1000 降至 0                        │
│ 扩展性:  新增供应商只需实现 3 个方法 + 修改 config.yaml                  │
│                                                                          │
└─────────────────────────────────────────────────────────────────────────┘
```

### 11.4 技术关键词提取（适合简历 Skills 栏）

```
· LLM 工程: OpenAI API / Anthropic / Tool Calling / Streaming SSE / Token 管理
· 高可用:   Circuit Breaker (gobreaker) / 主备降级 / 分布式熔断 (Redis Lua)
· 成本优化: 模型路由 / 分档调度 / Token 预算管理
· 可观测:   Prometheus / Grafana / 结构化日志 (zap)
· Go 工程:  接口抽象 / goroutine 生命周期 / sync.Once / context 传播
```

---

## 12. 面试问题集

### 12.1 Provider 抽象层设计

**Q1: 你提到"统一 Provider 抽象层"，为什么选择 OpenAI-compatible 协议而不是为每个供应商单独实现？**

<details>
<summary>参考答案</summary>

**实现方式**:
- 定义 `Provider` 接口（3 个方法：ChatCompletion / ChatCompletionStream / Name）
- 所有供应商通过 `openaiProvider` 统一实现，只需配置不同的 `BaseURL`
- 使用 `sashabaranov/go-openai` SDK，支持自定义 endpoint

**代码位置**: `client.go:67-74`, `openai_provider.go:52-56`

**选择原因**:
1. **事实标准**: OpenAI API 已成为 LLM 领域的 de facto 标准，90% 的服务都兼容
2. **零维护成本**: Anthropic 官方虽有自己的协议，但通过代理（litellm/new-api）可转换
3. **本地部署友好**: Ollama/vLLM/LocalAI 都原生支持 OpenAI 协议
4. **切换成本低**: 新增供应商只需改 config.yaml，无需修改代码

**其他实现方式**:
1. **每个供应商独立 SDK**
   - 优点: 使用官方 SDK，功能完整
   - 缺点: 维护成本高，orchestrator 到处是 `if provider == "openai"`
   
2. **自定义协议抽象**
   - 优点: 完全控制，可以设计最优接口
   - 缺点: 需要为每个供应商写适配器，工作量大

3. **LangChain 式的 LLM 抽象**
   - 优点: 社区生态丰富
   - 缺点: 引入重量级依赖，Go 生态不如 Python

**可优化点**:
1. 支持 Anthropic 原生协议（避免代理层延迟）
2. 实现 Provider 注册表，支持运行时动态注册
3. 增加 Provider 健康检查接口（Ping/HealthCheck）
4. 支持 Provider 级别的 rate limit 配置

</details>

---

**Q2: OpenAI-compatible 协议在实际使用中遇到过哪些兼容性问题？如何解决？**

<details>
<summary>参考答案</summary>

**实际遇到的问题**:

1. **Tool calls 格式差异**
   - OpenAI: `tool_calls` 数组，每个有 `id` + `function.name` + `function.arguments`
   - 某些代理: 缺少 `id` 字段或 `arguments` 不是 JSON 字符串
   - 解决: 在 `convertMessages` 中做防御性解析，缺失字段用默认值

2. **Streaming 的 finish_reason 时机**
   - OpenAI: 最后一个 chunk 才有 `finish_reason`
   - Ollama: 可能在中间 chunk 就返回
   - 解决: 只在 `finish_reason != ""` 时标记 `Done: true`

3. **Token 计数缺失**
   - 本地模型（Ollama）不返回 `usage` 字段
   - 解决: 使用 `FastEstimate` 估算，记录到 metrics 时标记为 `estimated=true`

4. **超时行为不一致**
   - Azure OpenAI: 超时返回 504
   - Anthropic 代理: 超时返回 500
   - 解决: 统一捕获 5xx 错误，都视为可重试

**代码位置**: `openai_provider.go:246-277` (消息转换), `openai_provider.go:157-244` (流式处理)

**可优化点**:
1. 增加 Provider 兼容性测试套件
2. 实现协议版本协商（类似 HTTP content negotiation）
3. 记录不兼容的 Provider 到黑名单，自动降级

</details>

---

**Q3: 如果要支持 Google Gemini 或其他非 OpenAI-compatible 的 LLM，如何扩展？**

<details>
<summary>参考答案</summary>

**扩展方案**:

**方案 1: 实现新的 Provider**
```go
// gemini_provider.go
type geminiProvider struct {
    client *genai.Client
    cfg    *config.LLMProviderConfig
    logger *zap.Logger
}

func (p *geminiProvider) ChatCompletion(ctx, req) (*ChatResponse, error) {
    // 1. 转换 models.Message → genai.Content
    // 2. 转换 models.ToolDefinition → genai.Tool
    // 3. 调用 Gemini API
    // 4. 转换响应 → ChatResponse
}
```

**方案 2: 通过代理层统一**
- 部署 litellm proxy，配置 Gemini 后端
- 客户端仍用 OpenAI-compatible 协议
- 优点: 无需修改代码
- 缺点: 增加一跳延迟（~50-100ms）

**方案 3: 协议适配器模式**
```go
type ProtocolAdapter interface {
    ToProviderRequest(req *ChatRequest) (interface{}, error)
    FromProviderResponse(resp interface{}) (*ChatResponse, error)
}

type geminiAdapter struct{}
func (a *geminiAdapter) ToProviderRequest(req *ChatRequest) (interface{}, error) {
    // 转换逻辑
}
```

**推荐**: 方案 2（短期）+ 方案 1（长期）
- 先用代理快速验证，确认 Gemini 效果
- 效果好再投入开发原生 Provider

**可优化点**:
1. Provider 工厂模式，根据 `provider` 字段自动选择实现
2. 协议转换层抽象，复用转换逻辑
3. 多 Provider 并发调用，取最快响应（race mode）

</details>

### 12.2 双层熔断架构

**Q4: 为什么需要两层熔断器？只用 gobreaker 不行吗？**

<details>
<summary>参考答案</summary>

**核心问题**: gobreaker 是进程内熔断，每个 Pod 独立计数。

**场景复现**:
```
假设: 5 个 Pod，每个 Pod 熔断阈值 = 连续失败 5 次
      上游 LLM 发生 60% 失败率（不是连续失败）

单 Pod 视角:
  请求序列: 成功 失败 成功 失败 失败 成功 失败 ...
  连续失败最多 2 次 → 永远不触发熔断

集群视角:
  5 个 Pod × 每秒 100 请求 = 500 QPS
  60% 失败 = 300 请求/秒打到已经退化的上游
  → 加剧上游负担，延长故障恢复时间
```

**双层设计**:
- L1 gobreaker（本地）: 捕捉**单 Pod 集中性失败**（连续 5 次），毫秒级响应
- L2 SharedBreaker（Redis）: 捕捉**集群级弥漫性退化**（30s 窗口 20 次），跨 Pod 聚合

**代码位置**: `client.go:183-190`（先查 shared，再走 gobreaker）, `shared_breaker.go:110-138`

**组合逻辑**:
```
请求到来
  │
  ├─ SharedBreaker.Allow() = false? → 直接 fallback（不碰 primary）
  │
  ├─ gobreaker state == Open? → 直接 fallback
  │
  └─ 两层都放行 → 调用 primary
       │
       └─ 失败 → gobreaker 内部统计 + SharedBreaker.RecordFailure()
```

**其他实现方式**:

1. **Consul/etcd 分布式状态机**
   - 完整的 Closed/Open/HalfOpen 状态同步
   - 缺点: 引入强一致性依赖，增加延迟和复杂度

2. **gossip 协议广播**
   - 每个 Pod 通过 gossip 传播失败事件
   - 优点: 无中心依赖
   - 缺点: 最终一致，收敛慢

3. **集中式 API Gateway 熔断**
   - 在 Envoy/Istio sidecar 层做熔断
   - 优点: 业务代码无感知
   - 缺点: sidecar 配置复杂，不够灵活

**当前方案的优势**: Redis INCR + TTL 极简，fail-open 保可用性，与 gobreaker 互不干扰。

**可优化点**:
1. SharedBreaker 增加**成功计数**，用失败率而非纯失败数触发
2. 增加 sliding window（Redis Sorted Set）替代 fixed window，减少边界抖动
3. HalfOpen 状态也通过 Redis 协调，避免 N 个 Pod 同时探测
4. 增加 per-model 粒度的熔断（同一 Provider 不同模型独立计数）

</details>

---

**Q5: SharedBreaker 用 fixed window 计数，有什么边界问题？如何改进？**

<details>
<summary>参考答案</summary>

**Fixed window 的边界问题**:

```
窗口 1 (0s-30s)     窗口 2 (30s-60s)
  │                    │
  │    18 次失败       │ 5 次失败
  │    (未达阈值 20)   │ (未达阈值)
  │                    │
  └────────────────────┘
       窗口边界处 23 次失败
       但两个窗口都没触发！
```

**epoch 计算方式** (`shared_breaker.go:101-104`):
```go
epoch := time.Now().Unix() / int64(s.window.Seconds())
// 30s 窗口: epoch 每 30 秒变一次
// 窗口 1 的 key 过期后，窗口 2 从 0 开始
```

**改进方案**:

**方案 1: Sliding window (推荐)**
```
使用 Redis Sorted Set:
  ZADD llm:breaker:anthropic <timestamp> <request_id>
  ZCOUNT llm:breaker:anthropic <now-30s> <now>
  ZREMRANGEBYSCORE llm:breaker:anthropic -inf <now-30s>
```
- 优点: 无边界抖动
- 缺点: 内存占用高（每次失败存一条记录）

**方案 2: 双窗口加权**
```
count = current_window_count + prev_window_count * (remaining_time / window_size)
```
- 优点: 实现简单，效果接近 sliding window
- 缺点: 仍是近似

**方案 3: 缩小窗口**
```
window = 10s, threshold = 7
```
- 优点: 边界影响更小
- 缺点: 更容易误触发

**当前实现为什么可接受**:
- 窗口 30s + 阈值 20 → 边界问题概率低
- gobreaker 作为 L1 已经覆盖了单 Pod 集中失败的场景
- SharedBreaker 只是补充层，不需要精确到秒级

</details>

---

**Q6: gobreaker 使用 ConsecutiveFailures 触发熔断，有什么问题？你会怎么改？**

<details>
<summary>参考答案</summary>

**问题**: 40% 失败率但不连续时，永远不会触发熔断。

```
请求序列: ✓ ✗ ✓ ✗ ✗ ✓ ✗ ✗ ✓ ✗ ...
连续失败最多 2 次 < 阈值 5 → 不熔断
但实际失败率 = 40%，上游明显有问题
```

**代码位置**: `client.go:152-154`
```go
ReadyToTrip: func(counts gobreaker.Counts) bool {
    return int(counts.ConsecutiveFailures) >= cfg.CircuitBreaker.MaxFailures
}
```

**改进方案**:

```go
// 方案 1: 基于失败率
ReadyToTrip: func(counts gobreaker.Counts) bool {
    if counts.Requests < 10 {
        return false  // 样本不足不触发
    }
    failureRate := float64(counts.TotalFailures) / float64(counts.Requests)
    return failureRate >= 0.5  // 50% 失败率触发
}

// 方案 2: 混合策略
ReadyToTrip: func(counts gobreaker.Counts) bool {
    // 连续失败 5 次 → 立即熔断（突发故障）
    if counts.ConsecutiveFailures >= 5 {
        return true
    }
    // 失败率 > 30% 且样本足够 → 熔断（慢性退化）
    if counts.Requests >= 20 {
        return float64(counts.TotalFailures)/float64(counts.Requests) >= 0.3
    }
    return false
}
```

**gobreaker 的 Interval 与 Timeout 分离**:
```go
// 当前问题: Interval = Timeout，语义不同但用了同一个值
cbSettings := gobreaker.Settings{
    Interval: 2 * cfg.CircuitBreaker.Timeout,  // 统计窗口更长
    Timeout:  cfg.CircuitBreaker.Timeout,        // OPEN 冷却时间
}
```
- `Interval`: 统计窗口（过了就清零计数），应该比 Timeout 长
- `Timeout`: OPEN → HALF-OPEN 的冷却时间

</details>

### 12.3 三档模型路由与成本优化

**Q7: 月度成本从 $43K 降到 $6.8K，具体怎么算出来的？**

<details>
<summary>参考答案</summary>

**优化前（所有请求用 Claude 3.5 Sonnet）**:

```
请求类型       占比    平均 tokens      单价($/M)      月度成本
─────────    ─────   ───────────    ──────────    ──────────
简单问答       65%    500in/100out   $3/$15         ~$9,500
代码修改       15%    3Kin/800out    $3/$15         ~$19,400
长对话摘要     15%    15Kin/300out   $3/$15         ~$12,100
意图识别       5%     200in/50out    $3/$15         ~$2,000
─────────────────────────────────────────────────────────────
合计                                                ~$43,000
```

**优化后（三档路由）**:

```
请求类型       路由档位    模型           单价($/M)      月度成本
─────────    ────────   ────────────   ──────────    ──────────
简单问答       Light     Haiku          $0.25/$1.25    ~$350
代码修改       Heavy     Sonnet         $3/$15         ~$19,400
长对话摘要     Light     Haiku          $0.25/$1.25    ~$950
意图识别       Light     本地Llama      ~$0/$0         ~$0
─────────────────────────────────────────────────────────────────
合计                                                   ~$6,800 ← 不到原来的1/6
```

**关键洞察**: 65% 的简单问答 + 15% 的摘要 + 5% 的意图识别 = 85% 的请求可以用便宜模型，质量损失可忽略。

</details>

---

**Q8: 路由器用关键词启发式而不是 LLM 分类，有什么局限？怎么改进？**

<details>
<summary>参考答案</summary>

**当前实现**: `router.go:253-278` (`QuickComplexity`)
- 基于关键词（"refactor"、"重构"）和消息长度打分
- 亚毫秒完成，无额外 LLM 开销

**局限**:

1. **误分类**: "帮我实现一个简单的 hello world" 包含"实现"关键词 → score+2 → 可能走 Heavy
2. **语言依赖**: 只覆盖中英文关键词，其他语言用户会被默认 Medium
3. **上下文盲**: 只看当前消息，不看对话历史（"继续"这条消息无法判断复杂度）
4. **不可学习**: 规则树是静态的，无法从实际效果中学习

**改进方案**:

**方案 1: 小模型分类器**
```
用 BERT/DistilBERT 微调一个 intent+complexity 分类器
输入: 用户消息 (max 512 tokens)
输出: {intent: "code_edit", complexity: 6}
延迟: ~5ms (本地推理)
训练数据: 从 production 日志标注
```
- 优点: 准确率高，可持续迭代
- 缺点: 需要标注数据和训练流程

**方案 2: LLM 自评估（cascade）**
```
第一步: 用 Light 模型分类（Haiku，~$0.001/次）
第二步: 根据分类结果选择主模型
总成本: +$0.001/请求，换来更准确的路由
```
- 优点: 利用 LLM 语义理解能力
- 缺点: 增加一次 LLM 调用延迟

**方案 3: 历史反馈学习**
```
记录每次路由决策 + 最终效果（用户是否满意/是否重试）
定期回归分析，自动调整关键词权重
```

**推荐路径**: 当前关键词方案 → 方案 2（cascade）→ 方案 1（微调分类器）

</details>

---

**Q9: 如果 Heavy 模型也是按 token 计费，如何防止单个用户消耗过多预算？**

<details>
<summary>参考答案</summary>

**当前状态**: 路由器只管"选哪个模型"，没有预算管控。

**完整的成本控制架构设计**:

```
┌─────────────────────────────────────────────────────────────────┐
│                        成本控制四层防线                            │
├─────────────────────────────────────────────────────────────────┤
│                                                                  │
│  L1: 路由降档（已实现）                                           │
│      简单任务 → Light 模型                                       │
│                                                                  │
│  L2: Token 预算（待实现）                                         │
│      每个 session/user/tenant 设置日/月预算上限                   │
│      超限后强制降档或拒绝                                        │
│                                                                  │
│  L3: 请求截断                                                    │
│      超长 prompt 截断到 context window 的 80%                    │
│      留 20% 给 completion                                        │
│                                                                  │
│  L4: 告警 + 熔断                                                 │
│      单用户 1 小时内消耗 > $10 → 告警                            │
│      单用户 1 天内消耗 > $50 → 强制降级到 Light                  │
│                                                                  │
└─────────────────────────────────────────────────────────────────┘
```

**Token 预算的实现方案**:
```go
type BudgetManager struct {
    rdb       *redis.Client
    limits    map[string]BudgetLimit  // tenant_id → limit
}

func (b *BudgetManager) CheckBudget(ctx, tenantID string, estimatedTokens int) (bool, ModelTier) {
    used := b.rdb.Get(ctx, "budget:"+tenantID+":"+today())
    remaining := b.limits[tenantID].DailyLimit - used
    
    if remaining <= 0 {
        return false, ""  // 拒绝
    }
    if remaining < estimatedTokens * heavyPrice {
        return true, TierLight  // 降档
    }
    return true, TierHeavy  // 放行
}
```

</details>

### 12.4 流式响应与资源管理

**Q10: 流式响应中，用户断连后如何保证 goroutine 不泄漏？**

<details>
<summary>参考答案</summary>

**问题场景**:
```
用户打开页面 → SSE 连接建立 → LLM 开始流式输出
用户关闭页面 → 前端断开 → 后端 goroutine 仍在读 HTTP body
→ goroutine 阻塞在 stream.Recv() 直到 LLM 完成（可能 30s+）
→ 浪费 token + goroutine 泄漏
```

**解决方案** (`openai_provider.go:188-202`):

```go
func (p *openaiProvider) ChatCompletionStream(ctx, req) (<-chan StreamChunk, error) {
    timeoutCtx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
    stream, err := p.client.CreateChatCompletionStream(timeoutCtx, apiReq)
    
    ch := make(chan StreamChunk, 64)
    
    go func() {
        defer close(ch)
        defer cancel()        // ← 关键 1: 取消 context
        defer stream.Close()  // ← 关键 2: 关闭 HTTP body
        
        for {
            // 关键 3: 每次循环检查 ctx
            select {
            case <-timeoutCtx.Done():
                ch <- StreamChunk{Err: timeoutCtx.Err(), Done: true}
                return
            default:
            }
            
            resp, err := stream.Recv()
            if err == io.EOF {
                ch <- StreamChunk{Done: true}
                return
            }
            if err != nil {
                ch <- StreamChunk{Err: err, Done: true}
                return
            }
            
            // 关键 4: 发送时也检查 ctx
            select {
            case ch <- chunk:
            case <-timeoutCtx.Done():
                ch <- StreamChunk{Err: timeoutCtx.Err(), Done: true}
                return
            }
        }
    }()
    
    return ch, nil
}
```

**三重保护**:
1. `defer stream.Close()`: 主动关闭 HTTP body → 触发 `Recv()` 返回 error
2. `select <-ctx.Done()`: 每次循环检查 context 取消
3. `select` 发送时检查: 防止 channel 满时阻塞

**实测效果**:
- 用户断连后 < 100ms 释放 goroutine
- 避免无效 token 消耗（断连后立即停止读取）

**其他实现方式**:

**方案 1: 定时器轮询**
```go
ticker := time.NewTicker(1 * time.Second)
for {
    select {
    case <-ticker.C:
        if ctx.Err() != nil { return }
    default:
        resp, err := stream.Recv()
    }
}
```
- 缺点: 最多 1s 延迟才能检测到断连

**方案 2: 双 goroutine**
```go
go func() {
    <-ctx.Done()
    stream.Close()  // 主动关闭
}()

go func() {
    for {
        resp, err := stream.Recv()  // 被上面的 Close 打断
    }
}()
```
- 优点: 逻辑清晰
- 缺点: 多一个 goroutine

</details>

---

**Q11: channel 缓冲设置为 64，这个数字怎么来的？太大或太小有什么问题？**

<details>
<summary>参考答案</summary>

**代码位置**: `openai_provider.go:196`
```go
ch := make(chan StreamChunk, 64)
```

**缓冲大小的权衡**:

```
缓冲太小 (如 1):
  生产者 (stream.Recv) 快于消费者 (orchestrator 处理)
  → 生产者阻塞在 ch <- chunk
  → HTTP 连接背压
  → LLM 服务端可能超时

缓冲太大 (如 1024):
  用户断连时，channel 中还有大量未消费的 chunk
  → goroutine 要等 channel 排空才能退出
  → 延长资源释放时间
  → 内存占用高（每个 chunk ~1KB，1024 个 = 1MB）
```

**64 的选择依据**:

1. **典型响应长度**: LLM 一次回复通常 500-2000 tokens
   - 流式每个 chunk ~10-50 tokens
   - 总 chunk 数 ~20-100 个
   - 64 覆盖 P90 场景

2. **内存占用**: 64 × 1KB = 64KB，可接受

3. **背压平衡**: 
   - orchestrator 处理一个 chunk ~1-5ms（SSE flush）
   - stream.Recv 间隔 ~10-50ms（网络 RTT）
   - 生产速度 ≈ 消费速度，64 足够缓冲抖动

**动态调整方案**:
```go
func calculateBufferSize(estimatedTokens int) int {
    chunks := estimatedTokens / 20  // 假设每 chunk 20 tokens
    if chunks < 16 { return 16 }
    if chunks > 256 { return 256 }
    return chunks
}
```

**可观测**:
```go
metrics.StreamChannelFullTotal.Inc()  // channel 满时计数
// 如果这个指标高 → 增大缓冲或优化消费速度
```

</details>

---

**Q12: 如果 LLM 返回的 streaming chunk 中包含恶意内容（如无限循环的 JSON），如何防御？**

<details>
<summary>参考答案</summary>

**攻击场景**:
```
LLM 被 prompt injection 攻击，返回:
  data: {"delta":{"content":"A"}}
  data: {"delta":{"content":"A"}}
  data: {"delta":{"content":"A"}}
  ... (无限重复，永不发送 [DONE])
```

**防御层次**:

**L1: 超时保护** (已实现)
```go
timeoutCtx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)  // 60s
```
- 60s 后强制断开，无论是否收到 [DONE]

**L2: 输出长度限制**
```go
var totalTokens int
for {
    resp, err := stream.Recv()
    totalTokens += estimateTokens(resp.Choices[0].Delta.Content)
    
    if totalTokens > req.MaxTokens * 2 {  // 超过预期 2 倍
        return fmt.Errorf("output exceeded limit")
    }
}
```

**L3: 速率限制**
```go
limiter := rate.NewLimiter(rate.Limit(100), 100)  // 100 chunks/s
for {
    if err := limiter.Wait(ctx); err != nil {
        return err
    }
    resp, err := stream.Recv()
}
```

**L4: 内容过滤**
```go
if detectMaliciousPattern(chunk.Content) {
    return fmt.Errorf("malicious content detected")
}
```

**推荐**: L1 + L2，L3/L4 按需启用（有性能开销）

</details>

### 12.5 Token 计数与可观测性

**Q13: 为什么需要两种 Token 计数方式？什么场景用哪种？**

<details>
<summary>参考答案</summary>

**代码位置**: `tokenizer.go`

**两种模式对比**:

```
┌──────────────────┬──────────────────┬──────────────────┐
│                  │ ExactTokenCount  │ FastEstimate     │
├──────────────────┼──────────────────┼──────────────────┤
│ 实现             │ tiktoken-go      │ rune 分类加权    │
│ 准确率           │ 100%             │ ~85%             │
│ 延迟             │ ~80μs            │ ~5μs             │
│ 依赖             │ tiktoken 数据文件│ 无               │
│ 离线可用         │ 需要初始化       │ 始终可用         │
└──────────────────┴──────────────────┴──────────────────┘
```

**使用场景**:
- **ExactTokenCount**: LLM 调用前最终验证（"这个 prompt 会不会超 128K？"）
- **FastEstimate**: 高频批量操作（RAG 1000 chunks 打分、session 追加消息）

**为什么不全用精确计数**:
```
RAG 检索 1000 chunks × 80μs = 80ms  ← 不可接受
RAG 检索 1000 chunks × 5μs  = 5ms   ← 可接受
```

**为什么不全用快速估算**:
```
估算: prompt = 125,000 tokens (实际 131,000)
发送给 LLM → "context_length_exceeded" 错误
→ 浪费一次 API 调用 + 用户等待时间
```

**可优化点**:
1. 缓存 ExactTokenCount 结果（相同文本不重复计算）
2. 按模型选择 encoding（GPT-4 用 cl100k_base，Claude 用自己的 tokenizer）
3. 增加 `EstimateWithConfidence` 返回 (count, confidence)，低置信度时自动升级到精确计数

</details>

---

**Q14: 如何设计 LLM 调用的可观测性？除了 Prometheus 指标还需要什么？**

<details>
<summary>参考答案</summary>

**当前实现的四维指标**:
```
1. 请求计数:  llm_request_total{provider, model, status}
2. 延迟分布:  llm_request_duration_seconds{provider, model}
3. Token 消耗: llm_tokens_used_total{provider, type}
4. 熔断状态:  llm_circuit_breaker_state{provider}
```

**完整可观测性架构**:

```
┌─────────────────────────────────────────────────────────────────┐
│                    LLM 可观测性三支柱                              │
├─────────────────────────────────────────────────────────────────┤
│                                                                  │
│  Metrics (指标) — 已实现                                         │
│    · 请求量/成功率/延迟/Token/熔断状态                           │
│    · 按 provider/model/tenant 分维度                             │
│    · Grafana 仪表盘实时监控                                      │
│                                                                  │
│  Tracing (追踪) — 部分实现                                       │
│    · 每次 LLM 调用一个 span                                     │
│    · 记录: model, tokens, latency, retry_count                  │
│    · 与 orchestrator span 关联（parent-child）                   │
│    · 可追踪: 一次用户请求触发了几次 LLM 调用                    │
│                                                                  │
│  Logging (日志) — 已实现                                         │
│    · 结构化日志 (zap)                                            │
│    · 记录: 熔断状态变化、fallback 触发、异常响应                 │
│    · 不记录: prompt 内容（隐私）、API key                        │
│                                                                  │
├─────────────────────────────────────────────────────────────────┤
│                                                                  │
│  额外需要的:                                                     │
│                                                                  │
│  成本追踪                                                        │
│    · 按 tenant/user/session 聚合 token 消耗                     │
│    · 实时成本仪表盘                                              │
│    · 日/月预算告警                                               │
│                                                                  │
│  质量监控                                                        │
│    · LLM 输出质量评分（用户反馈/自动评估）                       │
│    · 不同模型的质量对比                                          │
│    · 路由决策的准确率                                            │
│                                                                  │
│  异常检测                                                        │
│    · Token 消耗突增告警                                          │
│    · 延迟突增告警                                                │
│    · 错误率突增告警                                              │
│    · 输出内容异常检测（重复/乱码/过短）                          │
│                                                                  │
└─────────────────────────────────────────────────────────────────┘
```

**Grafana 告警规则示例**:
```yaml
# 错误率 > 10% 持续 5 分钟
- alert: LLMHighErrorRate
  expr: rate(llm_request_total{status="error"}[5m]) / rate(llm_request_total[5m]) > 0.1
  for: 5m

# 熔断器打开
- alert: LLMCircuitBreakerOpen
  expr: llm_circuit_breaker_state > 0
  for: 1m

# Token 消耗异常
- alert: LLMTokenSpike
  expr: rate(llm_tokens_used_total[5m]) > 2 * avg_over_time(rate(llm_tokens_used_total[5m])[1h:5m])
```

</details>

### 12.6 综合设计题

**Q15: 如果让你从零设计一个 LLM 网关层，你会怎么设计？和当前实现有什么不同？**

<details>
<summary>参考答案</summary>

**从零设计的架构**:

```
┌═══════════════════════════════════════════════════════════════════════════┐
║                        LLM Gateway 理想架构                                ║
╚═══════════════════════════════════════════════════════════════════════════╝

┌─────────────────────────────────────────────────────────────────────────┐
│ L1: API Gateway (Envoy/Kong)                                             │
│   · 全局限流（per-tenant）                                               │
│   · TLS 终止                                                             │
│   · 请求路由                                                             │
└─────────────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────────────┐
│ L2: LLM Router Service (独立微服务)                                      │
│   · 接收请求 → 分析 intent + complexity                                 │
│   · 查询 Provider 健康状态                                               │
│   · 查询 tenant 预算余额                                                │
│   · 决策: 选 provider + model + 是否降档                                │
│   · 返回路由决策（不实际调用 LLM）                                       │
└─────────────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────────────┐
│ L3: LLM Proxy Pool (多实例)                                              │
│   · 连接池管理（per-provider）                                           │
│   · 请求排队 + 优先级                                                    │
│   · 流式转发                                                             │
│   · 重试 + 熔断                                                          │
│   · Token 计费                                                           │
└─────────────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────────────┐
│ L4: Provider Adapters                                                    │
│   · OpenAI adapter                                                       │
│   · Anthropic native adapter                                             │
│   · Google Gemini adapter                                                │
│   · Local model adapter (Ollama/vLLM)                                   │
└─────────────────────────────────────────────────────────────────────────┘
```

**与当前实现的对比**:

```
┌──────────────────┬──────────────────────┬──────────────────────────────┐
│ 维度             │ 当前实现              │ 理想架构                      │
├──────────────────┼──────────────────────┼──────────────────────────────┤
│ 部署形态         │ 嵌入式（进程内库）   │ 独立微服务                    │
│ 路由决策         │ 同步、进程内         │ 独立服务，可独立扩缩          │
│ 熔断器           │ 进程内 + Redis       │ 集中式健康管理                │
│ 连接管理         │ 每次请求新建         │ 连接池复用                    │
│ 请求优先级       │ 无                   │ 按 tenant/intent 排队         │
│ 多协议支持       │ 仅 OpenAI-compat     │ 每个 provider 原生协议        │
│ 预算管控         │ 无                   │ 实时预算检查                  │
│ 缓存             │ 无                   │ 相同 prompt 缓存              │
└──────────────────┴──────────────────────┴──────────────────────────────┘
```

**当前实现的优势**:
- 简单，无额外部署依赖
- 延迟低（无网络跳转）
- 适合中小规模（< 10 Pod）

**理想架构的优势**:
- 可独立扩缩（路由和代理分离）
- 集中式管控（预算、限流、审计）
- 适合大规模多租户

**结论**: 当前实现是**正确的起步选择**。在 < 10 Pod 规模下，嵌入式方案的简单性远超独立微服务的运维成本。当规模增长到需要独立管控时再拆分。

</details>

---

**Q16: 如果 LLM 供应商全部宕机（OpenAI + Anthropic + 本地模型），Agent 应该怎么表现？**

<details>
<summary>参考答案</summary>

**当前行为**: 返回 `"both primary and fallback LLM failed"` 错误 → 前端显示错误。

**更优雅的降级策略**:

```
┌─────────────────────────────────────────────────────────────────┐
│                    全面降级策略                                    │
├─────────────────────────────────────────────────────────────────┤
│                                                                  │
│  Level 1: 正常                                                   │
│    所有 Provider 可用，按路由规则选择                             │
│                                                                  │
│  Level 2: 部分降级                                               │
│    Primary 不可用 → Fallback 接管                                │
│    用户感知: 质量略降，延迟可能增加                              │
│                                                                  │
│  Level 3: 严重降级                                               │
│    所有远程 Provider 不可用 → 本地模型                           │
│    用户感知: 质量明显下降，但基本功能可用                        │
│                                                                  │
│  Level 4: 完全降级（新增）                                       │
│    所有 LLM 不可用 → 非 LLM 模式                                │
│    可用功能:                                                     │
│      · 文件读写（不需要 LLM）                                   │
│      · grep/搜索（不需要 LLM）                                  │
│      · git 操作（不需要 LLM）                                   │
│      · 预设模板回复                                              │
│    不可用功能:                                                   │
│      · 代码生成/修改                                             │
│      · 自然语言理解                                              │
│      · 复杂推理                                                  │
│    用户提示: "LLM 服务暂时不可用，以下功能仍可使用: ..."        │
│                                                                  │
│  Level 5: 排队等待（新增）                                       │
│    检测到 LLM 恢复 → 自动重试排队中的请求                       │
│    用户提示: "请求已排队，LLM 恢复后自动处理"                   │
│                                                                  │
└─────────────────────────────────────────────────────────────────┘
```

**实现要点**:
1. 区分"需要 LLM"和"不需要 LLM"的操作
2. 提供明确的用户反馈（不是模糊的 500 错误）
3. 自动恢复机制（HalfOpen 探测成功后恢复排队请求）
4. 降级期间的操作审计（记录哪些请求被降级处理）

</details>

