# 13 · Context 组装 `internal/context`

> 代码：
> - `pruner.go` (417 行) — `TokenPruner`：四信号代码块打分剪枝 + `PruneMessages` 消息窗口 + `TruncateLargeToolResult` 大工具输出裁剪
> - `prompt_builder.go` (260 行) — `PromptBuilder`：五区段 KV-Cache 友好型 Prompt 装配 + cache_control 标记 + prefix hash 观测
> - `_principles.go` (758 行) + `doc.go` (54 行) — 设计 RFC（与代码大体一致，但权重数字与代码默认值不同，以代码为准）
> - 测试：`pruner_test.go` (163 行)

---

## 1. 模块定位

**"LLM 输入的终极整型器：把 session / RAG / 当前问题编织成一个 KV-cache 友好且不爆窗口的 prompt。"**

上游有人给（session 历史 + summary）、RAG 给（N 条 CodeChunk + relevance score）、用户给（current message），下游 LLM 只认**有序 Message 数组 + 一个 token 上限**。Context 包是这中间那一层"编织 + 剪枝 + 布局"的主力。

两个核心目标：

| 目标 | 手段 |
|---|---|
| **不爆窗口** | `TokenPruner`：四信号加权打分 + 贪心选取（training-free pruning） |
| **省 LLM 钱 + 提速** | `PromptBuilder`：5 区段稳定-前缀布局 + Anthropic `cache_control` 标记 |

为什么 Prompt Caching 值钱？Anthropic 的 prompt cache 让"命中的 prefix tokens"按 25% 计费（cache write 是 125%），可观测的 TTFT 也会显著下降（几百 ms 量级）。**前提是 prefix 逐字节相同**——任何一个 timestamp 或 map 遍历顺序变化都会让缓存 miss。

---

## 1.5 设计哲学：KV Cache 友好性的底层原理

### Q1 — Transformer 视角下的 KV Cache

Transformer 生成每个 token 都要对**所有历史 tokens**算 attention。每层的 Key/Value 矩阵一旦算完不会变。**缓存它们**，下次只对新增 tokens 做增量计算。

- 无缓存：生成 N tokens 是 O(N²)
- 有缓存：增量是 O(N)

Anthropic 实测：10K token prompt
- 无 cache：TTFT 3-5s
- Cache hit：TTFT 0.3-0.8s（**~10× 加速**）

### Q2 — 缓存的关键约束：Prefix 字节级相同

LLM 服务商**不会做"结构感知"缓存**——他们只查：
```
sha256(prompt_bytes[:N]) ∈ cache ? yes : no
```

任意一个字节变了 → cache miss → 全量重算。
这意味着 prompt 的层次结构必须严格按"**变化频率从低到高**"排列。

### Q3 — 5 区段分层（按变化频率排）

```
┌─ Region 1: 永不变 ─────────────────────┐
│ System Prompt                          │  ← 部署时敲死
│  · 不含时间戳                          │
│  · 不含 session_id                     │
│  · 不含 "current date is ..."          │
└────────────────────────────────────────┘
         │ 字节 hash 稳定 → cache 命中
         ▼
┌─ Region 2: 半稳定 ─────────────────────┐
│ Long-Term Memory                       │  ← session.Summary
│  · 仅在 cold separation 后更新         │  │  + 跨 session 记忆
│  · 同 session 内长期不变               │
└────────────────────────────────────────┘
         │
         ▼
┌─ Region 3: 偶尔变 ────────────────────┐
│ Pruned Code Context (RAG)              │  ← 每次 retrieve 可能不同
│  · TokenPruner.PruneCodeChunks         │
│  · 按 composite score 选取            │
└────────────────────────────────────────┘
         │
         ▼
┌─ Region 4: 每步变 ────────────────────┐
│ Recent Conversation (windowed)         │
│  · PruneMessages 滑窗                  │
│  · 保 system + pinned + 尾 4 条        │
└────────────────────────────────────────┘
         │
         ▼
┌─ Region 5: 新输入 ────────────────────┐
│ Current User Message                   │
└────────────────────────────────────────┘
```

### Q4 — 一个破坏 cache 的反例

```go
// ❌ 每次 prompt 都 miss
system := fmt.Sprintf("Current time: %s", time.Now())

// ❌ map 遍历无序
for name, def := range toolRegistry.tools {
    tools = append(tools, def)
}

// ❌ RAG 放最前面
prompt := ragResults + systemMessage + history
```

每个错误都让 ReAct 循环每一步 miss cache，成本和延迟各 ×2。

### Q5 — Anthropic `cache_control` 显式标记

Anthropic 不像 OpenAI 那样自动缓存，要在消息里加 `cache_control: {type: "ephemeral"}` 显式标记。
PromptBuilder 在 `enablePromptCaching=true` 时给 Region 1/2/3 都打这个标。
**注意是 ephemeral**：5 分钟 TTL，长会话需要每 5 分钟刷新一次缓存。

---

## 2. 依赖架构

```
        ┌─ orchestrator / planner ─┐
        │                          │
        │ session.GetContextWindow │ → []Message（历史）
        │ rag.Retrieve             │ → []CodeChunk + []score
        │ buildSystemMessage       │ → systemPrompt
        └────────────┬─────────────┘
                     │
                     ▼
         ┌──────────────────────────┐
         │  context.PromptBuilder   │  prompt_builder.go
         │  BuildPrompt(            │
         │     session,             │
         │     codeChunks,          │
         │     relevanceScores,     │
         │     currentMessage,      │
         │  ) []Message             │
         └─────────┬────────────────┘
                   │
                   ▼
         ┌──────────────────────────┐
         │  context.TokenPruner     │  pruner.go
         │  PruneCodeChunks         │  ← Region 3
         │  PruneMessages           │  ← Region 4
         │  TruncateLargeToolResult │  ← 工具结果裁剪
         └──────────────────────────┘
                   │
                   ▼
             []Message → llm.ChatCompletion
```

`context` 包是**纯函数**：没有 I/O、没有数据库、没有外部依赖（除了 logger 和 `llm.FastEstimate`）。
这是它能被高频调用（每个 ReAct step 都要构 prompt）的前提。

---

## 2.5 数据流总览

```text
═══════════════════ BuildPrompt 装配 ═══════════════════

orchestrator.buildMessages
       │
       ▼
PromptBuilder.BuildPrompt(session, codeChunks, scores, currentMessage)
       │                                              [prompt_builder.go:137]
       │
       ▼
┌──────────────────────────────────────────────────────────┐
│ Region 1: System Prompt                                  │
│   { Role:System, Content: pb.systemPrompt }              │
│   + 若 enablePromptCaching: cache_control=ephemeral      │
├──────────────────────────────────────────────────────────┤
│ Region 2: Long-Term Memory                               │
│   memoryContent = pb.longTermMemoryPrefix                │
│                  OR fallback to "[Conversation History   │
│                       Summary]\n" + session.Summary      │
│   { Role:System, Content: memoryContent }                │
│   + cache_control                                        │
├──────────────────────────────────────────────────────────┤
│ Region 3: Pruned Code Context                            │
│   prunedChunks = pruner.PruneCodeChunks(chunks, scores)  │
│   builder.WriteString("[Retrieved Code Context]\n")      │
│   for chunk in prunedChunks:                             │
│     "--- <file>:<L1>-<L2> (<symbolType> <symbolName>)\n" │
│     <chunk.Content>\n\n                                  │
│   { Role:System, Content: builder.String() }             │
│   + cache_control                                        │
├──────────────────────────────────────────────────────────┤
│ Region 4: Recent Conversation (动态滑窗)                  │
│   usedTokens = Σ estimateTokens(prev regions)            │
│                + estimateTokens(currentMessage)          │
│   remainingBudget = pruner.maxTokenBudget - usedTokens   │
│   if remainingBudget > 0:                                │
│     PruneMessages(session.Messages, remainingBudget)     │
├──────────────────────────────────────────────────────────┤
│ Region 5: Current User Message                           │
│   { Role:User, Content: currentMessage }                 │
└──────────────────────────────────────────────────────────┘
       │
       ▼
   []Message → orchestrator → llm.ChatCompletion

═══════════════════ TokenPruner.PruneCodeChunks ═══════════════════

[]CodeChunk + []relevanceScore (~30-50 候选)
       │
       ▼
buildCallFrequencyMap(chunks)              [pruner.go:382]
   Phase 1: collect symbolSet
   Phase 2: 每个 chunk 扫一次, 计数 symbol 出现次数  O(n·k)
       │
       ▼
对每个 chunk 计算 4 信号:
   callFreq  = freq[sym] / maxFreq
   scopeScore = 1 / (1 + 0.3*chunk.ScopeDepth)
   relevance  = scores[i]            (default 0.5)
   recency    = i / len(chunks)
       │
       ▼
composite = 0.25*callFreq + 0.15*scope + 0.45*relevance + 0.15*recency
                                          ▲
                                          │ DefaultPrunerConfig 默认值
       │
       ▼
if totalTokens <= budget: return chunks (无需剪枝)
else:
   sort by composite DESC
   贪心选取直到 budget 耗尽 → selected[i] = true
   按原始 index 顺序输出 result
       │
       ▼
返回 prunedChunks (token 预算内)

═══════════════════ PruneMessages 消息滑窗 ═══════════════════

PruneMessages(messages, tokenBudget)         [pruner.go:301]
       │
       │ Step 1: 复制并裁大工具结果
       ▼
compacted[i] = TruncateLargeToolResult(messages[i])
                ├─ if msg.Role != Tool: 不变
                └─ if len(Content) > 2000: 头 1000 + "[truncated XXB]" + 尾 1000
       │
       │ Step 2: 预算够吗?
       ▼
if Σ estimateTokens(compacted) <= budget: return compacted
       │
       │ Step 3: 分桶
       ▼
systemMsgs  := [m for m in compacted if Role==System]
pinnedMsgs  := [m for m in compacted if m.Pinned]
otherMsgs   := [m for m in compacted if !System && !Pinned]
       │
       │ Step 4: 保留尾部最近 4 条
       ▼
keepTail := min(4, len(otherMsgs))
tail     := otherMsgs[len-keepTail:]
       │
       │ Step 5: 剩余预算往中段从新到旧填
       ▼
remainingBudget = budget - systemTok - pinnedTok - tailTok
middle = otherMsgs[:len-keepTail]
keptMiddle = []
for i from len(middle)-1 down to 0:
    if midTokens + msgTokens <= remainingBudget:
        keptMiddle.prepend(middle[i])
        midTokens += msgTokens
       │
       ▼
return systemMsgs ++ pinnedMsgs ++ keptMiddle ++ tail
```

---

## 3. ★ PromptBuilder：5 区段 KV-Cache 布局（prompt_builder.go）

### 3.1 结构（prompt_builder.go:62-83）

```go
type PromptBuilder struct {
    systemPrompt         string         // Region 1（不可变）
    longTermMemoryPrefix string         // Region 2（半稳定）
    prefixHash           string         // sha256(系统 || 长期记忆) hex
    enablePromptCaching  bool           // 是否注入 cache_control
    pruner               *TokenPruner
    logger               *zap.Logger
    builderPool          sync.Pool      // strings.Builder 池（GC 优化）
}
```

### 3.2 `NewPromptBuilder` 的预算缩放（L94-121）

```go
// Scale RAG budget proportionally: 20% of total context window
if cfg.MaxTotalTokens > 0:
    ragBudget := cfg.MaxTotalTokens * 20 / 100
    if ragBudget > prunerCfg.MaxTokenBudget:
        prunerCfg.MaxTokenBudget = ragBudget
```

**含义**：如果 `cfg.MaxTotalTokens=200000`（Claude Opus 4），RAG 预算自动放大到 40000，
而不是死守 `DefaultPrunerConfig.MaxTokenBudget=8000`。
**只放大不缩小**——配置低于 8000 时仍然用 8000。

### 3.3 BuildPrompt 五区段布局（L137-230）

```
┌────────────────────────────────────────────────────────┐
│ Region 1: System Prompt (IMMUTABLE)                    │
│   pb.systemPrompt                                       │
│   + cache_control 若开启                                │
├────────────────────────────────────────────────────────┤
│ Region 2: Long-Term Memory (SEMI-STABLE)                │
│   pb.longTermMemoryPrefix 优先                          │
│   fallback: "[Conversation History Summary]\n"+summary  │
│   + cache_control                                       │
├────────────────────────────────────────────────────────┤
│ Region 3: Pruned Code Context (per-turn variable)       │
│   "[Retrieved Code Context]\n"                          │
│   for chunk in pruner.PruneCodeChunks(...):             │
│     "--- file.go:12-45 (function Foo) ---\n"           │
│     <chunk.Content>\n\n                                 │
│   + cache_control                                       │
├────────────────────────────────────────────────────────┤
│ Region 4: Recent Conversation (windowed)                │
│   pruner.PruneMessages(session.Messages, remaining)     │
│   ⚠️ 不打 cache_control（频繁变）                       │
├────────────────────────────────────────────────────────┤
│ Region 5: Current User Message (NEW every turn)         │
│   { Role:User, Content: currentMessage }                │
└────────────────────────────────────────────────────────┘
```

### 3.4 Region 2 的 fallback 逻辑（L158-171）

```go
memoryContent := pb.longTermMemoryPrefix
if memoryContent == "" && session != nil && session.Summary != "":
    memoryContent = fmt.Sprintf("[Conversation History Summary]\n%s", session.Summary)
if memoryContent != "":
    append Region 2 message
```

**含义**：
- 如果上层主动调了 `UpdateLongTermMemory(summary)` → 用 `longTermMemoryPrefix`
- 否则从 session 的现 Summary 兜底
- 都没有 → Region 2 完全跳过（messages 数组不出现这一条）

这意味着**新 session 的前几次对话 Region 2 不存在**，等 cold separation 触发后 Region 2 才出现——但这一变化就会让 prefix hash 改变，触发一次 cache invalidation。

### 3.5 cache_control 的精准注入位置

cache_control 标记在 Region 1/2/3 都打了，Region 4/5 不打。**为什么不打 4？**

Anthropic 的规则：cache_control 标记的是"截止到此处的 prefix 应被缓存"。
Region 4（最近对话）每步都变，标记在那里反而让缓存被 frequently invalidate。
策略是**只在变化频率最低的几段末尾打标**，命中率最大化。

### 3.6 prefix hash 用途（L235-260）

```go
hashPrefix():
    h := sha256.New()
    h.Write([]byte(pb.systemPrompt))
    h.Write([]byte(pb.longTermMemoryPrefix))
    return fmt.Sprintf("%x", h.Sum(nil))   // 完整 64 字符 hex
```

观测用——日志打前 8 字符 `prefix_hash=a1b2c3d4`，前后比对就知道是不是同一个 prefix。
`UpdateLongTermMemory` 在 hash 变化时打 info 日志：
```
"prompt prefix changed, KV cache will be partially invalidated"
old_hash=ab12cd34 new_hash=ef56gh78
```

⚠️ **prefix hash 没接进 metrics**——目前只能从日志聚合，没有 `prompt_prefix_cache_hit_total` 之类的 Prometheus 指标。**已知 P1**。

### 3.7 `builderPool`（L82-117）

```go
builderPool: sync.Pool{New: func() any { return &strings.Builder{} }}
```

Region 3 拼接 N 个 chunk header + content，每次 prompt 构建分配一个 4KB Builder。
ReAct 每步都构 prompt，QPS 高时 `sync.Pool` 让 Builder 复用——省 GC。

⚠️ **bug 风险**：L177-183 用 `defer` 把 builder 还回池：
```go
builder := pb.builderPool.Get().(*strings.Builder)
builder.Reset()
defer func() {
    builder.Reset()
    pb.builderPool.Put(builder)
}()
```

但 `builder.String()` 在 L196 被引用。Go 的 `strings.Builder.String()` 是 **指向同一段 byte slice**，
defer Reset 不会真的清空消息内容（slice 还在，只是 builder 长度归 0）。
**但 `ragMsg.Content`（`builder.String()` 的返回值）已经被赋值给 messages 数组**——
这是一个 `string` 类型，Go 字符串是不可变的，**所以这里是安全的**：
即使 builder 被 reset 后内部 slice 被覆写，已经取到的 string 还是独立的拷贝。
（实际 Go 实现里 `Builder.String()` 用了 `unsafe.Pointer` 跳过拷贝，但底层 byte slice 由 GC 持有，
Reset 只是把 `len` 归 0，不影响已经 stable 引用的 string。）

---

## 4. ★ TokenPruner：四信号代码块打分（pruner.go）

### 4.1 四信号加权模型（pruner.go:134-176）

每个 CodeChunk 算一个 composite score，**保高分、丢低分**直到塞满预算：

```
composite = w_call·CallFreq + w_scope·ScopeScore + w_rel·Relevance + w_rec·Recency
```

| 信号 | 计算公式 | 含义 |
|---|---|---|
| **CallFreq** | `freq[symbol] / maxFreq` | 被引用越多 → 越接近"结构核心" |
| **ScopeScore** | `1 / (1 + 0.3·ScopeDepth)` | 浅层（顶层函数）≥ 深层闭包；ScopeDepth=0 → 1.0, 1 → 0.77, 2 → 0.63 |
| **Relevance** | `queryRelevanceScores[i]`（默认 0.5） | RAG cosine 分数直接复用 |
| **Recency** | `i / len(chunks)` | chunk 在原列表里越靠后越新（基于"RAG 召回结果按时间排"的假设） |

⚠️ **Recency 是基于 index 不是真实时间**——`chunk.ModifiedAt` 字段是有的但**没用**。
如果 RAG 不保证按时间排，这个信号几乎是噪声。**已知 P2**。

### 4.2 默认权重（pruner.go:97-105）

```go
func DefaultPrunerConfig() *PrunerConfig {
    return &PrunerConfig{
        MaxTokenBudget:  8000,
        WeightCallFreq:  0.25,
        WeightScope:     0.15,
        WeightRelevance: 0.45,  // 主导信号
        WeightRecency:   0.15,
    }
}
```

**Relevance 是主导（0.45）**——本质上"先靠 RAG 召回排序，再用其他信号微调"。
权重总和 = 1.0，但代码**没强制校验**总和，外部传任意权重都接受。

### 4.3 灵感来源

注释明确（pruner.go:60-63）：
> Inspired by multi-modal LLM visual token pruning: evaluate code token importance
> across multiple signals (call frequency, scope depth, query relevance) and
> dynamically prune redundant tokens without retraining the LLM.

把"视觉 token 显著性"换成"代码片段重要性"。
**training-free**：纯启发式打分，不需要训练任何模型。

### 4.4 贪心选取（pruner.go:194-216）

```go
sort scores DESC by composite
selected := make(map[int]bool)
usedTokens := 0
for s in scores:
    if usedTokens + s.TokenCount > budget: continue
    selected[s.Index] = true
    usedTokens += s.TokenCount

// 按原始 index 顺序输出
result := make([]CodeChunk, 0, len(selected))
for i, chunk := range chunks:
    if selected[i]: result = append(result, chunk)
```

**按原始顺序**输出：LLM 看到的 chunks 仍然是召回的自然顺序，**不是按 score 排序**。
否则 chunks 会变成"打分高的拼前面，低的拼后面"，LLM 推理时上下文会乱。

### 4.5 `scorePool` 对象池（pruner.go:116-122）

```go
scorePool: sync.Pool{New: func() any { s := make([]TokenScore, 0, 64); return &s }}
```

每次 Prune 取一个预分配的 slice，避免高 QPS 下 `[]TokenScore` 的反复分配。
**容量 64** 是经验值——大多数 RAG 召回不超过 64 个 chunks。

### 4.6 `buildCallFrequencyMap` 的 O(n·k) 优化（pruner.go:382-401）

朴素 O(n²)：
```
for i: for j: if chunk[j].Content contains chunk[i].SymbolName: freq[i]++
```

优化版 O(n·k)：
```
Phase 1: symbolSet = unique non-empty SymbolName
Phase 2: for each chunk:
           for each sym in symbolSet:
             if sym != chunk.SymbolName && chunk.Content contains sym:
               freq[sym]++
```

k 是平均符号数，远小于 n。实测 n=200 chunks 时：O(n²)=40000 次 scan，O(n·k)≈2000 次，**20× 提速**。

⚠️ **`strings.Contains` 在 Content 上的扫描仍然是 O(content_len)** —— 真正复杂度是 O(n·k·content_avg)。
对超长 chunk（>10KB）仍然慢。**已知 P2**：可改 Aho-Corasick 多模式匹配一次扫描出所有 symbol 命中。

---

## 5. `PruneMessages` 消息滑窗（pruner.go:301）

### 5.1 算法

```
PruneMessages(messages, tokenBudget):
  1. compacted[i] = TruncateLargeToolResult(messages[i])   # 先压大输出
  2. if totalTokens(compacted) <= budget: return compacted

  3. 分桶:
     systemMsgs  : Role==System (必留)
     pinnedMsgs  : msg.Pinned   (必留)
     otherMsgs   : 其他

  4. tail = otherMsgs[最后 4 条]   # 当前对话尾部

  5. remainingBudget = budget - systemTokens - pinnedTokens - tailTokens
  6. middle = otherMsgs[除尾 4 条以外]
     从 middle 末尾向头部装填，直到耗尽 remainingBudget

  7. return systemMsgs ++ pinnedMsgs ++ keptMiddle ++ tail
```

### 5.2 为什么「保头保尾保固定，中间挖空」？

- **头（system）**：规则、角色设定，丢了 LLM 立刻失忆；
- **固定（pinned）**：用户标记的关键决策/约束，丢了违反用户意图；
- **尾（last 4）**：最近对话上下文，丢了当前问题无法衔接；
- **中间**：历史 ReAct 步骤，session.Summary 已经摘要过，丢一些不影响理解。

### 5.3 Pinned 消息的来源

`models.Message.Pinned` 字段由两个路径设置：

1. **Auto-pin**：`session.Manager.AddMessage` → `shouldAutoPin(msg)`
   检测 "always / never / must / important: / note: / remember: / constraint:" 等关键词
2. **手动 pin**：`session.Manager.PinMessage(sid, msgID)`，由 API endpoint 调用

### 5.4 与 session.Summary 的两级压缩配合

```
原始 100 条消息
     │
     ▼ session.performHotColdSeparation
头部 N 条被压成 session.Summary，热 ~5-15 条留在 hot key
     │
     ▼ orchestrator.GetContextWindow
返回 [system "Summary:" + summary] + hot messages
     │
     ▼ PromptBuilder.BuildPrompt
Region 2 装载 summary（Role=System）
Region 4 调 PruneMessages 对 hot messages 二次窗口
     │
     ▼ LLM
```

**两级压缩职责分离**：
- session：粗粒度（"老消息整体降维为摘要"）
- context：细粒度（"热消息内按 token 预算选最近的"）

---

## 6. `TruncateLargeToolResult`（pruner.go:243-260）

```go
const MaxToolResultBytes = 2000

TruncateLargeToolResult(msg):
  if msg.Role != Tool: return msg
  if len(msg.Content) <= MaxToolResultBytes: return msg

  headLen := MaxToolResultBytes / 2   // = 1000
  tailLen := MaxToolResultBytes - headLen  // = 1000
  truncated := Content[:1000]
             + "\n\n[... truncated <humanBytes> ...]\n\n"
             + Content[len-1000:]
  return msg.Content = truncated
```

**前 1000 + 中间标记 + 尾 1000**——总长度略超 2000（标记占几十字节），但这是设计内的容差。

### 6.1 为什么放 PruneMessages 第一步

如果不先压大输出，后续 `estimateTokens(msg.Content)` 会把 50KB 工具结果算作真实 token 数，
导致 budget 算偏，**好消息被错误地挤掉**。
先压大输出再算 budget，预算视角准确。

### 6.2 humanBytes（pruner.go:262-271）

把字节数转 `"1024B" / "1KB" / "2MB"`，给裁剪标记里用。
内部用 `itoa`（避免引 `fmt` 包，热路径优化）。

### 6.3 ⚠️ MaxToolResultBytes 是 byte 不是 token

`len(msg.Content)` 是 byte 长度，对 UTF-8 中文是 3-4 字节/字符。
2KB 限制 ≈ 500 中文字符 ≈ ~600 token 或 ~1500 英文 token。
**对中文用户偏激进**，对英文 stack trace 偏宽松。**已知 P2**。

---

## 7. 辅助函数

### 7.1 `estimateTokens(text)`（pruner.go:415）

```go
func estimateTokens(text string) int {
    return llm.FastEstimate(text)
}
```

委托 `internal/llm/tokens.go` 的统一 tokenizer。
**和 session 包用同一个函数**——口径一致。

### 7.2 `humanBytes(n)` / `itoa(n)`（pruner.go:262-294）

热路径：避免 `fmt.Sprintf` 反射开销，手写整数转字符串。
ReAct 高频路径上每次 PruneMessages 调一次 humanBytes，省下来的 alloc 累计可观。

---

## 8. 使用场景全景

```go
// orchestrator.ProcessMessage (示意)
func (o *Orchestrator) ProcessMessage(ctx, task) {
    // A. 拉历史（已经过 session 冷热分离）
    sess, _ := o.sessionMgr.Get(ctx, task.SessionID)

    // B. RAG 检索（如启用）
    chunks, scores := []models.CodeChunk{}, []float64{}
    if o.ragEngine != nil {
        results := o.ragEngine.Retrieve(ctx, task.Message)
        chunks = results.Chunks
        scores = results.Scores
    }

    // C. ★ 装配 prompt
    messages := o.promptBuilder.BuildPrompt(sess, chunks, scores, task.Message)

    // D. 调 LLM（注意 messages 已经 KV-cache 友好且预算内）
    resp, _ := o.llmClient.ChatCompletion(ctx, &llm.ChatRequest{
        Messages: messages,
        Tools:    o.toolRegistry.Definitions(),
    })
    ...
}
```

Orchestrator 把 `buildSystemMessage`（含 project rules）+ session 历史 + RAG 召回送给 PromptBuilder，
PromptBuilder 内部调用 TokenPruner 做剪枝，最终输出 `[]Message`。

---

## 9. 设计权衡

| 抉择 | 动机 |
|---|---|
| **5 区段 KV-Cache 布局** | 命中 prefix cache 省 50-75% 输入 token 费用（Anthropic 25% 折扣） |
| **Long-Term Memory 独立 Region 2** | summary 变化不污染 Region 1 的 cache |
| **cache_control 只打 Region 1/2/3** | Region 4/5 频繁变，打标反而增加 cache write 成本 |
| **代码块用四信号加权** 而非纯 RAG | 召回集内还要再排序；CallFreq 是"结构核心"的强 proxy |
| **Relevance 权重 0.45 主导** | RAG 本身已经做了语义匹配，其他信号是微调 |
| **权重可配置** `PrunerConfig` | 不同项目（微服务 vs monorepo）偏好不同 |
| **贪心而非 DP/ILP** | 贪心 O(n log n) 几乎 optimal；精确解跑不动 |
| **输出保留原始顺序** | LLM 认文件内位置，打乱后变成 code salad |
| **PruneMessages 保 system + pinned + 尾 4** | 系统消息丢不得；尾部当前问题；Pinned 是用户意图 |
| **大工具输出先压再算 budget** | 50KB tool result 不能把真历史挤掉 |
| **builderPool + scorePool** | ReAct 每步都构 prompt，省 GC |
| **buildCallFrequencyMap O(n·k)** | n=200 时 20× 加速 |
| **prefix hash 可观测** | 日志打前 8 字符，方便定位 cache 失效 |
| **estimateTokens 委托 llm.FastEstimate** | 全项目统一 tokenizer 口径 |

---

## 10. 后续演进

- [x] **Token 估算统一**：已用 `llm.FastEstimate`（统一委托）
- [x] **Prompt Caching 标记**：已在 Region 1-3 注入 `cache_control: ephemeral`
- [x] **动态预算缩放**：已根据 `MaxTotalTokens` 按 20% 比例放大 RAG 预算
- [x] **消息固定**：`Pinned` 字段 + auto-pin 关键词 + API endpoint
- [ ] **Prefix Cache Metrics**：`prompt_prefix_cache_hit_total{prefix_hash=...}` Prometheus 指标
- [ ] **Recency 用真实时间**：从 `chunk.ModifiedAt` 计算，而非 index 位置
- [ ] **CompactionMode 联动**：把 session 的 `CompactionMode` 配置接入 PruneMessages 行为
- [ ] **Aho-Corasick 多模式扫描**：`buildCallFrequencyMap` 一次扫描出所有 symbol 命中
- [ ] **大输出按 token 而非 byte 限制**：`MaxToolResultBytes` 改 `MaxToolResultTokens`，避免中文偏激进
- [ ] **Tool result 智能摘要**：大输出不简单裁剪，调 LLM 一句话摘要
- [ ] **Region 3 结构化模板**：用 XML/JSON 固定 schema，LLM 解析更稳
- [ ] **DP 求最优选取**：背包 DP O(n·B) 求严格最优
- [ ] **Long-Term Memory 分层**：user-level + project-level + session-level 嵌套
- [ ] **Cache Warmer**：启动后主动调一次空消息预热 prefix
- [ ] **权重自适应**：按历史命中率/任务类型动态调 `wCallFreq / wRelevance`

---

## 11. 设计教训

1. **KV Cache 的字节级敏感性**：现代 LLM 缓存按 prefix 字节 hash 命中，任何"看似无害"的不稳定（时间戳、map 顺序、float 精度）都会让缓存全部 miss。设计 prompt 装配函数时要把"是否字节稳定"当成 PR 评审清单的一项。

2. **变化频率分层**：把上下文按变化频率从低到高排，**不是按"重要性"排**。系统提示词最稳定但不一定最关键，最近用户消息最重要但变化最频繁——按频率排让缓存命中率最大化。

3. **Pruner 输出保留原序**：LLM 在 code 上下文里依赖**文件内位置上下文**——函数 A 在 B 之前出现是有意义的。即使按 score 排序更"逻辑清晰"，打乱原序会让 LLM 推理变差。

4. **大输出先压再算预算**：高 token 的工具结果（go test、grep）如果不先裁剪，会让 PruneMessages 错估预算，挤掉真正有用的对话。**裁剪是 budget 计算的前置条件**，不是事后清理。

5. **Object pool 的字符串安全性**：`sync.Pool` 复用 `strings.Builder` 看起来危险（builder.Reset 会不会破坏已经取出的 string？），实际安全——Go string 是不可变的、`Builder.String()` 取出后底层 byte slice 由 GC 独立持有，Builder.Reset 只是把 builder 内部 len 归 0，不影响已经 stable 引用的 string。

6. **观测点埋在关键变化处**：`UpdateLongTermMemory` 检测到 hash 变化时打 info 日志——这是诊断"为什么 prompt cache 命中率突然降"的金线索。但没接 metrics 就是 P1 未做完。

7. **多信号加权的权重总和**：四信号权重默认 0.25+0.15+0.45+0.15=1.0，但代码不强制校验。外部传 [0.5, 0.5, 0.5, 0.5] 不会报错，但 composite 会被放大 2 倍，相对排序仍正确——不影响 selection。这是"放任非数学严格"换"配置自由度"的权衡，但**应该在 README/doc 提示**用户最好保持总和 1。

8. **Training-free 启发式 vs LLM-based 重要性**：用启发式（CallFreq / ScopeDepth）评估代码片段重要性，比"调 LLM 判断"快 1000 倍、便宜 1 个数量级。代价是有些"语义重要但 CallFreq 低"的 chunk 会被错杀（比如新写的入口函数还没被引用）。但在 ReAct 高频路径上，这个 tradeoff 值得。

---

下一篇：[`14_workspace.md`](14_workspace.md) —— Workspace 管理器：项目目录隔离、文件系统沙箱、git 自动初始化、autonomous mode 的临时工作区。
