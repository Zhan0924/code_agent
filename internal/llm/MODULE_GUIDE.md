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
    half_open_max_reqs: 1     # HALF-OPEN 阶段允许 1 个探针

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
