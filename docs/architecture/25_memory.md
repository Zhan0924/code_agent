# 25 · 长期记忆系统 `internal/memory`

> 代码：
> - `types.go` (44) — `Memory` / `MemoryType` / `MemoryStore` 接口
> - `math.go` (24) — `CosineSimilarity` 向量相似度
> - `extractor.go` (345) — 从交互对话中蒸馏出"值得记住"的 Memory（LLM + 启发式双模式）
> - `hybrid.go` (152) — `HybridStore` 热/冷双层存储 + 冲突解析
> - `redis_hot.go` (159) — 24h TTL 的 Redis 热层（SCAN + 内存排序）
> - `pg_cold.go` (197) — PostgreSQL + pgvector 冷层
> - `conflict.go` (45) — `ConflictResolver` 高相似度合并
>
> 测试：`extractor_test.go` / `math_test.go` / `conflict_test.go`

---

## 1. 模块定位

**"让 agent 跨 session 记得'上周用户说过他不用 emoji'。"**

`internal/memory` 是 **长期记忆**子系统——session 结束后依然存在的、跨对话可召回的事实库。与 [21_agentloop](21_agentloop.md) 的 `TrajectoryMemory`（工具序列）、[23_toollearn](23_toollearn.md) 的 `ToolPattern`（工具反馈）是**互补**的：

| 维度 | session（02_session） | trajectory（21_agentloop）| toollearn | **memory（本模块）** |
|------|-------------|------------|-----------|---------|
| 内容 | 消息历史 | 成功工具序列 | 工具失败/耗时统计 | **结构化事实**（4 类） |
| 时效 | 30 分钟滑窗 | 50 条 FIFO | 1024 buffer + PG | **永久（带 decay）** |
| 用途 | LLM context | hint prompt | tool advice | **system prompt 注入** |
| 提取方式 | 直接 append | 任务结束时写入 | 每次工具调用 | **LLM 蒸馏 + 启发式** |

四类 Memory：

```go
MemoryPreference   "user prefers tabs not spaces"
MemoryDecision     "we picked Postgres over MongoDB for ACID"
MemoryKnowledge    "auth.go uses session-based, not JWT"
MemoryPattern      "user typically asks me to write tests after refactor"
```

每次用户对话结束，`Extractor.ExtractFromInteraction` 拿 user_msg + assistant_msg 跑一次 LLM 蒸馏，把"值得记住"的几条 Memory 存到 HybridStore。下次同一 user/project 召回时，相关 Memory 注入 system prompt。

---

## 1.5 核心设计问题

### 为什么不直接全量塞历史消息？

会话历史是**对话记录**——大部分都是过程性的（"读文件 → 改文件 → 跑测试"）。LLM 真正需要的是**结晶后的事实**：

- 不需要"用户问了三遍如何写 Go test"——需要"该用户用 testify 做断言"；
- 不需要"我读了 50 个文件"——需要"auth.go 是项目核心"；
- 不需要全量 commit log——需要"上次架构决策选了 ulid 不是 uuid"。

蒸馏 = **压缩**，几百字的对话 → 一句话事实。token 节省 50-100 倍，召回精度更高。

### 为什么 LLM 蒸馏 + 启发式双模式？

LLM 蒸馏（GPT-4o mini 跑 extractionPrompt）质量高但每次对话**多一次 API 调用 + ~1024 token**。在两种场景下切到启发式（关键词匹配）：

1. LLM 不可达（断网 / 配额耗尽 / 主备都挂）；
2. 启动 / 测试期间用户没配 LLM key。

启发式只抓固定短语（"i prefer / 我喜欢 / 从now on / 我以后…"），召回率 < 20% 但**零成本**——总比"完全没有记忆"好。

### 为什么"热/冷"两层而不是只用 Postgres？

- **热层 Redis**（24h TTL）：单次召回 < 5ms，写入 < 2ms。最近对话产生的 Memory 90% 落在这里；
- **冷层 PG + pgvector**：单次召回 ~20ms（ivfflat 索引），但**永久存储 + 向量精确搜索**。

读路径：Redis 优先；命中数足够（≥ limit）直接返回，否则**降级**到 PG。这是经典 cache-aside 模式，跟 [02_session](02_session.md) 的 hot/cold session 是同源设计。

热层用 SCAN + 内存排序而非 Redis Sorted Set——因为按 cosine 相似度排序需要逐项算（Redis 内建 ZSET 只支持单一标量打分），且热层规模小（每 user/project ≤ 50 条），内存排序代价可接受。

### 为什么要冲突合并而不是允许重复？

用户在两周内说了三次"我用 tabs"。如果每次都新建 Memory：

- 检索 top-5 时**全是同一条**——信息密度归零；
- score 分散在三个 ID 上，热度衰减算法误判；
- token 浪费。

`ConflictResolver` 用 cosine ≥ 0.85 + 同 Type 判定**语义重复**，命中后 `UPDATE` 旧条目（保留 ID 和创建时间，更新 content + score）——这让"用户重复表达偏好"被自然吸收为**强化**而非**重复**。

---

## 2. 依赖架构

```
                ┌────────────────────────────────────────────┐
                │  orchestrator.ProcessMessage 任务结束后    │
                │  ↓                                         │
                │  extractor.ExtractFromInteraction(...)     │
                │     ├─ LLM 蒸馏 → ExtractedMemory[]        │
                │     └─ 启发式 fallback                     │
                └─────────────┬──────────────────────────────┘
                              │ for each candidate:
                              ▼
                ┌────────────────────────────────────────────┐
                │  HybridStore.Store(ctx, &Memory)           │
                │    1. 调用 embedder 生成 1536d 向量         │
                │    2. ColdStore.RetrieveByVector(...)      │
                │    3. ConflictResolver.FindConflicts(≥0.85)│
                │       ├─ 有冲突 → Update 旧条目             │
                │       └─ 无冲突 → Hot.Store + Cold.Store   │
                └────────┬───────────────────────┬───────────┘
                         │                       │
                         ▼                       ▼
              ┌────────────────────┐    ┌─────────────────────────┐
              │ RedisHot           │    │ PGCold                  │
              │ key: memory:u:p:id │    │ table: memories         │
              │ TTL: 24h           │    │ pgvector: ivfflat       │
              │ SCAN 召回，内存排序│    │ 索引: user_id+project_id│
              └────────────────────┘    │       embedding<=>      │
                                        └─────────────────────────┘
                              │ 召回路径
                              ▼
                ┌────────────────────────────────────────────┐
                │  HybridStore.Retrieve(query)               │
                │  → embed(query) → 1536d                    │
                │  → Hot.RetrieveByQuery (cosine 排序)        │
                │  → 不够则 Cold.RetrieveByVector (<=> 距离)   │
                │  → 注入 system prompt: "User preferences:" │
                └────────────────────────────────────────────┘
```

---

## 2.5 数据流总览

```text
═══════════════ 写入路径（任务结束触发） ═══════════════

orchestrator.completeTask():
  go extractor.ExtractFromInteraction(ctx, userID, projID, lastUserMsg, lastAssistMsg)
    │
    ├─ if llm != nil:
    │    prompt = extractionPrompt.replace(USER_MSG, ASSIST_MSG)
    │    resp = llm.ChatCompletion(prompt, T=0.1, MaxTokens=1024)
    │    candidates = parseLLMResponse(resp.Content)   # JSON 数组
    │
    └─ else:
       candidates = extractWithHeuristics(userMsg, assistMsg)
       # 关键词匹配 prefPhrases / decisionPhrases

  for c in candidates:
    if c.Content == "" || c.Importance < 0.3: skip
    if isDuplicate(c.Content): skip  # Jaccard > 0.7 视为重复
    m = Memory{ID: uuid, UserID, ProjectID, Type, Content, Score=Importance, ...}
    store.Store(ctx, &m)


hybrid.Store(ctx, &m):
  if m.Embedding == nil:
    m.Embedding = embedder.Embed([m.Content])  # ~50ms

  if cold != nil and len(m.Embedding) > 0:
    candidates = cold.RetrieveByVector(m.Embedding, ..., 3)
    conflicts = resolver.FindConflicts(&m, candidates)  # ≥0.85 同类型
    if conflicts:
      resolved = resolver.Resolve(&conflicts[0], &m)  # 旧 ID + 新 content
      cold.Update(resolved)
      hot.Store(resolved)
      return  # 已合并

  hot.Store(&m)
  cold.Store(&m)


═══════════════ 召回路径（每次构建 prompt 触发） ═══════════════

orchestrator.buildPromptHints():
  memories = memoryStore.Retrieve(ctx, userID, projID, currentUserMsg, limit=5)
  if memories:
    hint = "[User memories]\n" + join("- " + m.Content for m in memories)
    systemPrompt += "\n\n" + hint


hybrid.Retrieve(ctx, userID, projID, query, limit):
  queryEmb = embedder.Embed(query)
  
  # 热层优先（命中率 ~70%）
  if hot != nil and queryEmb != nil:
    mems = hot.RetrieveByQuery(userID, projID, queryEmb, limit)
    if len(mems) >= limit: return mems

  # 冷层兜底
  if queryEmb != nil:
    return cold.RetrieveByVector(queryEmb, userID, projID, limit)
  return cold.Retrieve(userID, projID, query, limit)  # ILIKE 文本搜索


═══════════════ 衰减路径（定时任务） ═══════════════

每天凌晨：
  hybrid.Decay(olderThan=30*24h, factor=0.95)
    cold.Decay: UPDATE memories SET score = score * 0.95
                WHERE last_accessed_at < now()-30d AND score > 0.01
```

---

## 3. `Memory` 数据结构

```go
type Memory struct {
    ID             string     // uuid v4
    UserID         string     // 多租户隔离
    ProjectID      string     // 项目级隔离
    Type           MemoryType // preference / decision / knowledge / pattern
    Content        string     // "User prefers TypeScript with strict mode"
    Embedding      []float32  // 1536d (text-embedding-3-small)
    Score          float64    // 0-1 重要性 + decay 衰减后的当前权重
    AccessCount    int        // Touch 增加
    CreatedAt      time.Time
    LastAccessedAt time.Time
}
```

### 3.1 为什么 UserID + ProjectID 双键？

多租户场景的**最小隔离粒度**：

- 一个 user 在多个 project（每个 git repo）做事，project-A 的记忆不应该污染 project-B；
- 多个 user 在同一 project 协作，user-A 的 preference 不应该影响 user-B。

所有查询 SQL 都按 `(user_id, project_id)` 复合索引——`idx_memories_user_project`。

### 3.2 为什么 Embedding 是 1536d？

`text-embedding-3-small` 的输出维度。Schema 写死在 `CREATE TABLE memories (embedding vector(1536))`——换 embedding 模型（如 `text-embedding-3-large` 3072d）需要 schema migration。

启动时 `embedder` 是 `nil`（开发环境）也能工作——Memory 仍存到 PG（embedding 列 NULL），召回降级到 ILIKE 文本搜索（精度降但不崩）。

### 3.3 为什么 Score 是 float64 而不是 int？

Score 同时承载两个语义：

1. **初始重要性**：LLM 给 0-1 浮点（"importance": 0.9 critical, 0.5 useful, 0.3 minor）；
2. **衰减后的权重**：每次 `Decay()` 乘以 factor（如 0.95），float 才能平滑衰减；int 量化误差累积太快。

`score > 0.01` 的下限避免无限衰减——score 跌到 0.01 以下时停止 decay，保留为"几乎不用但还能召回"的状态。

---

## 4. `Extractor` —— LLM 蒸馏

### 4.1 提示词模板

```
Analyze this interaction and extract important memories worth remembering...
Focus on:
- User preferences (coding style, tools, language, workflow preferences)
- Technical decisions (architecture choices, library selections, design patterns)
- Project knowledge (file structure insights, domain-specific facts, constraints)
- Behavioral patterns (common requests, recurring problems, workflow habits)

Rules:
- Only extract genuinely useful, specific information (not generic facts)
- Each memory should be a concise, self-contained statement (1-2 sentences max)
- Score importance 0.0-1.0: 1.0 = critical preference/decision, 0.5 = useful context, 0.3 = minor detail
- If nothing worth remembering, return empty array

Respond with ONLY a JSON array:
[{"type": "preference|decision|knowledge|pattern", "content": "...", "importance": 0.0-1.0}]
```

**关键设计**：

- **空数组合法**：很多对话（"读这个文件"、"运行测试"）没什么值得记的——强制 LLM 蒸馏会产出垃圾；
- **importance 阈值 0.3**：低于这个不存——LLM 偶尔会给鸡毛蒜皮的事 0.1-0.2 重要性；
- **Temperature 0.1**：要求稳定输出，不要"创造性"；
- **MaxTokens 1024**：4-8 条 memory 已足够；多了反而稀释信号。

### 4.2 LLM 输出解析的容错

```go
// 1. 剥 markdown ``` fence
if strings.HasPrefix(content, "```"):
    content = strip first/last lines

// 2. 严格 JSON 解析
json.Unmarshal(content, &memories)
if err: return nil  // 解析失败直接放弃，不重试

// 3. Clamp importance 到 [0, 1]
for m in memories:
    if m.Content == "": skip
    if m.Importance < 0: m.Importance = 0
    if m.Importance > 1: m.Importance = 1
```

**为什么不重试？** 失败有两种：

1. LLM 输出格式错误（罕见）；
2. LLM 拒绝输出（敏感内容触发安全策略）。

两种都不适合重试——重试要么得到同样格式错误，要么继续拒绝。直接放弃，等下一次对话再试。

### 4.3 启发式 fallback

```go
prefPhrases = []{
    {"from now on", 0.9},  {"please always", 0.85},
    {"i prefer", 0.8},  {"i like", 0.6},
    {"我偏好", 0.8}, {"我喜欢", 0.6}, {"以后", 0.8},
    ...
}

decisionPhrases = []{
    {"i've decided", 0.85}, {"let's go with", 0.8},
    {"架构决策", 0.9}, {"决定使用", 0.85},
    ...
}
```

匹配后用 `extractSentence(text, phrase)` 抠出"包含该短语的整句"作为 Memory 内容。中英文混编是有意的——本项目用户群覆盖。

**召回率约束**：启发式只能抓**显式声明**的 preference/decision；隐式偏好（"我每次都让你用 type annotation"）抓不到。这是 LLM 模式的存在意义。

### 4.4 去重

```go
func isDuplicate(ctx, userID, projectID, content):
    existing = store.Retrieve(ctx, userID, projectID, content, 5)
    for m in existing:
        if textSimilarity(lower(content), lower(m.Content)) > 0.7:
            return true
    return false
```

`textSimilarity` 是简单 Jaccard（按空格分词，长度 > 2 的词进 set）。这是**入库前**的快速过滤——0.7 阈值放宽，防止"用户偏好 tabs"和"用户喜欢 tabs 缩进"被都存进去。

**真正的语义去重**在 HybridStore.Store 内（cosine ≥ 0.85），用 embedding 比 Jaccard 精确得多。但 Jaccard 是**零成本**的预过滤——避免每条新 candidate 都跑一次 embedding 调用。

---

## 5. `HybridStore` —— 热/冷双层

### 5.1 写路径决策树

```
Store(m):
  ① 无 embedding 时调 embedder 生成 (cost ~50ms / API call)
  ② cold 可用 + embedding 有 → 查冷层 top-3 候选
     ③ ConflictResolver.FindConflicts (cosine ≥ 0.85 + same type)
        ④ 有冲突 → Resolve（旧 ID 接管新 content）→ cold.Update + hot.Store
        ⑤ 无冲突 → hot.Store + cold.Store
  ⑥ embedder 不可用 → 跳过冲突检查，直接 hot.Store + cold.Store
```

Hot 写失败只记 Debug 日志（**不阻塞**）——hot 是缓存，丢一条无所谓；Cold 写失败直接 return err（持久层必须可靠）。

### 5.2 读路径决策树

```
Retrieve(query, limit):
  queryEmb = embedder.Embed(query)
  
  if hot && queryEmb:
    mems = hot.RetrieveByQuery (cosine 内存排序)
    if len(mems) >= limit: return mems   # 热层命中足够直接返回
  
  if hot && !queryEmb:
    mems = hot.Retrieve (按 key 字典序最近)
    if len(mems) >= limit: return mems   # 无 query embedding 的降级
  
  if cold && queryEmb:
    return cold.RetrieveByVector (pgvector <=> cosine 距离)
  
  return cold.Retrieve(query)            # ILIKE 文本搜索（最弱）
```

**关键**：热层只在"命中数 ≥ limit"时直接返回——少于 limit 时穿透到冷层。这是因为热层只缓存了 24h 内的，老 Memory 必须从冷层捞。

### 5.3 Promote / Demote

```go
Promote(m): hot.Store(m)         // 把 cold 中的 m 推到 hot
Demote(m):  hot.Delete(m.ID)     // 从 hot 删除（cold 保留）
```

**当前未自动调用** —— 预留给将来的"LRU 提升"策略：召回时把 cold 命中且 score 高的条目自动 Promote 到 hot，让"重要的老记忆"也能 5ms 召回。

---

## 6. `RedisHot` —— 24h TTL 热层

### 6.1 Key 设计

```
memory:{userID}:{projectID}:{memoryID}   = JSON(Memory)  TTL=24h
```

`SCAN pattern="memory:{u}:{p}:*"` 拿到所有该 user/project 的 Memory keys。**为什么不用 SET / HASH 维护索引？**

- SET 索引要双写（每次 Store 同时 SADD + SET 单条），多一次往返；
- TTL 由 Redis 单 key 控制；用 SET 索引时需要手动剔除已 TTL 失效的成员；
- 当前规模（每 user/project ≤ 50 条）SCAN 完全够用，加索引复杂度收益太低。

### 6.2 为什么用 SCAN 而不是 KEYS？

`KEYS pattern` 在大型 Redis 实例上 O(N) 阻塞所有命令；SCAN 是渐进式扫描，cursor 分批拿。这是 `redis-cli` 文档反复警告的"不要在生产用 KEYS"——本项目从一开始就走 SCAN。

`Iterator()` 内部循环直到 cursor=0 或拿够 `limit*2`——多取 2 倍是为后续按 key 排序裁剪到 limit 留余量。

### 6.3 RetrieveByQuery 的内存排序

```go
// 拿到所有 hot key
keys = SCAN(pattern, max=50)
// pipeline GET
items = pipe.Get(keys...)
// 内存计算 cosine
for each item:
    sim = CosineSimilarity(queryEmb, item.Embedding)
    results.append({memory, sim})
sort by sim desc
return top-limit
```

热层规模 ≤ 50 条 × 1536 dim × 4 bytes ≈ 300KB——内存里跑 50 次 cosine 计算大约 200μs。**不需要 Redis Stack / RediSearch 的向量索引**——那是为 100K+ 规模设计的，本场景过度工程。

### 6.4 数据丢失语义

- TTL 24h 到期 Redis 自动删除——冷层 PG 仍有；下次召回穿透到冷层依然能拿到。
- Redis 重启 / FLUSHDB → 热层全空，召回降级到冷层，性能下降但**不影响功能**。

**为什么 TTL 而不是手动管理 LRU？** Redis 内建 TTL 是免费的——不需要写额外的回收 goroutine。代价是 24h 是**绝对时间**而非"最近访问"——这是为了让热层规模可预测（每 user/project × 24h ≤ 几十条）。

---

## 7. `PGCold` —— 永久存储 + 向量搜索

### 7.1 Schema

```sql
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE memories (
    id UUID PRIMARY KEY,
    user_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    type TEXT NOT NULL,
    content TEXT NOT NULL,
    embedding vector(1536),
    score FLOAT DEFAULT 1.0,
    access_count INT DEFAULT 0,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    last_accessed_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX idx_memories_user_project ON memories(user_id, project_id);
CREATE INDEX idx_memories_type ON memories(type);
CREATE INDEX idx_memories_score ON memories(score DESC);
CREATE INDEX idx_memories_embedding ON memories
    USING ivfflat (embedding vector_cosine_ops) WITH (lists = 100);
```

**索引选择**：

- `idx_memories_user_project` —— 几乎所有查询都过滤这两列，复合索引必备；
- `idx_memories_type` —— 按类型筛选（"只看 preference"）；
- `idx_memories_score DESC` —— 排序辅助；
- `idx_memories_embedding ivfflat lists=100` —— pgvector 的近似最近邻索引。

### 7.2 为什么 ivfflat 不是 hnsw？

`pgvector` 提供两种向量索引：

| 索引 | 召回率 | 写延迟 | 内存占用 | 适用规模 |
|------|--------|--------|----------|----------|
| ivfflat | ~95% | 低 | 低（基于聚类）| < 10M 行 |
| hnsw | ~99% | 高 | 高（每行 ~100B 额外）| > 1M 行 |

Memory 表预期规模：每 user 每 project 几百条；1000 user × 10 project × 500 = 5M 行。完全在 ivfflat 舒适区。`lists=100` 是经验值（约 √行数 / 10）——pgvector 文档推荐。

### 7.3 cosine 距离查询

```sql
SELECT ... FROM memories
WHERE user_id = $1 AND project_id = $2 AND embedding IS NOT NULL
ORDER BY embedding <=> $3
LIMIT $4
```

`<=>` 是 pgvector 的 cosine 距离运算符（1 - cosine_similarity）。`ORDER BY <=>` 才能使用 ivfflat 索引（其他距离运算符 `<#>` `<-> ` 需要不同的索引类型）。

### 7.4 `formatVector` / `parseVector` —— 文本协议

```go
// Go float32 → pgvector text: [0.1,0.2,0.3]
func formatVector(v []float32) string {
    b.WriteByte('[')
    for i, f := range v {
        if i > 0 { b.WriteByte(',') }
        fmt.Fprintf(&b, "%g", f)
    }
    b.WriteByte(']')
}
```

pgvector 通过 PostgreSQL 的 `text` 类型协议传输向量——driver/sql 不需要知道 vector 类型，把它当字符串发就行。`%g` 用最短的浮点表示（自动选择 e 或 f 格式）——比 `%f` 紧凑 30%+。

### 7.5 衰减算法

```sql
UPDATE memories SET score = score * 0.95
WHERE last_accessed_at < $cutoff AND score > 0.01
```

- 只衰减"久未访问"的（30 天前的）；
- 下限 0.01——防止无限趋近 0；
- 操作幂等，每天跑一次：score 走 0.95^N 几何衰减；
- 召回时按 score DESC 排序——衰减直接影响排序权重。

`Touch(id)` 在召回时调用：`access_count += 1`、`last_accessed_at = NOW()`——重新激活"最近被用过"的 Memory，下次 Decay 跑不到它。

---

## 8. `ConflictResolver` —— 高相似度合并

### 8.1 检测

```go
func FindConflicts(newMem, candidates):
    for c in candidates:
        if c.Embedding == nil || c.Type != newMem.Type: skip
        sim = CosineSimilarity(newMem.Embedding, c.Embedding)
        if sim >= 0.85: conflicts.append(c)
```

**两个约束**：

1. **同 Type 才算冲突** —— preference 不应该被 knowledge 覆盖；
2. **0.85 阈值** —— 0.95 太严（漏报多）、0.75 太宽（误合并不同概念）。0.85 是经验值。

### 8.2 解析

```go
func Resolve(old, new) *Memory:
    old.Content = new.Content
    old.Embedding = new.Embedding
    old.Score = new.Score
    return old
```

**就地更新**——保留 `old.ID` / `old.CreatedAt` / `old.AccessCount`。这样：

- 历史上的引用（如审计日志记录的 memory ID）依然有效；
- 创建时间不会"被刷新"——便于按真实创建时间排序（"这个 preference 是用户多久前说的"）；
- access_count 累加——"用户重复表达"被视为"经常访问"，提升权重。

`Score` 用新值（不取 max）—— 因为新值通常是 LLM 蒸馏出的最新 importance，反映"用户当前对这件事的重视程度"。

---

## 9. 启动接入

```go
// cmd/agent/main.go:423
memAdapter := NewMemoryAdapter(rdb, pgStore, embedder, logger)
if memAdapter != nil {
    orch.SetMemoryStore(memAdapter)
}
if memAdapter != nil {
    memoryExtractor := memory.NewExtractor(memAdapter.HybridStore(), llmClient, logger)
    orch.SetMemoryExtractor(memoryExtractor)
}
```

`NewMemoryAdapter` 在 `cmd/agent/memory_adapter.go` 里——它把 `RedisHot` + `PGCold` 包装成符合 `orchestrator.MemoryStore` 接口的 adapter。所有持久化都是 **optional**：

- `rdb == nil` → 只有冷层
- `pgStore == nil` → 只有热层（24h 后全丢，仅适合 demo）
- 都没有 → `NewMemoryAdapter` 返回 nil → orchestrator 跳过整套 memory 逻辑

启动时 `PGCold.Migrate()` 自动建表 / 索引——idempotent（`IF NOT EXISTS`）。

---

## 10. 与其他模块的边界

### 10.1 上游：orchestrator

```go
type Orchestrator struct {
    memoryStore     memory.MemoryStore        // HybridStore 实现
    memoryExtractor *memory.Extractor          
}

// 任务结束时蒸馏
defer func() {
    go o.memoryExtractor.ExtractFromInteraction(ctx, userID, projID, userMsg, asstMsg)
}()

// 构建 prompt 时召回
mems := o.memoryStore.Retrieve(ctx, userID, projID, currentMsg, 5)
systemPrompt += formatMemories(mems)
```

蒸馏是 **fire-and-forget goroutine**——不阻塞用户响应（蒸馏失败也不影响主流程）。

### 10.2 下游：embedder

`HybridStore.embedder` 接口是 `func Embed(ctx, []string) ([][]float32, error)`——与 `internal/rag` 的 embedder **共享同一实例**（main.go 注入同一个 `rag.Embedder`）。这样：

- 缓存（如果 embedder 内部带）共享；
- 配额走同一个计数器；
- 升级 embedding 模型一处生效。

### 10.3 平行：session manager

session 是**消息历史**，memory 是**结晶事实**。两者**没有数据流连接**——session 由 orchestrator 直接管理（写入 / 召回 / 裁剪），memory 由 extractor 单独从对话中蒸馏。**故意解耦**：session 是工作记忆，memory 是知识库。

---

## 11. 设计权衡

| 抉择 | 动机 |
|------|------|
| LLM 蒸馏 + 启发式双模式 | LLM 精度高但可能不可用；启发式作为永远在线的底线 |
| 4 类 MemoryType 硬编码 | 软分类（embedding）成本高；preference/decision/knowledge/pattern 覆盖 95% 场景 |
| 热/冷分层 + Redis SCAN | 与 session 同源；热层规模小到无需向量索引 |
| Cosine ≥ 0.85 合并 | 经验阈值；同类型 + 高相似 = 重复表达 |
| 就地 Update 而非新建 | 保留 ID + CreatedAt + AccessCount，"重复表达" → "强化" |
| Score float64 + decay | int 衰减误差累积；0.95 几何衰减平滑 |
| Embedder 可空 → 文本搜索降级 | 开发 / 测试环境不强制配 embedding；功能不崩 |
| importance < 0.3 不存 | LLM 偶尔产 0.1-0.2 的鸡毛蒜皮；阈值过滤 |
| Extractor goroutine fire-and-forget | 蒸馏耗时 (~LLM 调用 + embedding) 不阻塞用户响应 |
| 1536d hard-coded schema | text-embedding-3-small 的维度；换模型需 migration |
| `idx_memories_score DESC` 单列索引 | 实际查询多按 user_id+project_id 先过滤再排序；单列索引覆盖率有限——可考虑去掉 |

---

## 12. 后续演进

- [ ] **多 embedding 模型支持**：用 `embedding_model` 列区分不同维度的向量，schema 改 `vector(NULL)` 动态长度
- [ ] **跨 project 共享 preference**：当前严格按 user_id+project_id 隔离；"我永远不用 emoji" 这种应该跨 project 共享
- [ ] **召回时自动 Promote**：cold 命中且 score > 0.7 时自动写入 hot，下次 5ms 响应
- [ ] **LLM 召回排序**：当前 cosine 排序，召回 top-10 后让 LLM rerank 选出 top-5——质量提升但成本翻倍
- [ ] **Negative memory**：当前只学正向（用户喜欢 X）；"用户**反对**用 Y"同样有价值
- [ ] **审计**：每次 Memory 注入 prompt 时记 audit log，便于解释"为什么 agent 突然知道我的偏好"
- [ ] **数据导出**：用户能查看/删除自己的 Memory（GDPR 合规）
- [ ] **重要性自适应**：access_count 高的 Memory 自动提升 score（"反复用上的偏好"权重高）
- [ ] **冲突解析策略可配置**：当前默认"覆盖式"；可加"merge mode"——把旧 content 和新 content 拼起来
- [ ] **Hot 层主动失效**：用户改 preference 后老 Memory 应主动 Demote（当前依赖 24h TTL 自然过期）

---

## 13. 与人类记忆的类比

`internal/memory` 的设计粗略对应**陈述性记忆**（declarative memory）：

| 人脑 | 本模块 |
|------|--------|
| 短期记忆（working memory） | session 消息历史 |
| 海马体 → 皮层 巩固 | extractor 蒸馏对话 → 写入 store |
| 长期记忆（事实+事件） | Memory{Type, Content} |
| 检索激活 | cosine 召回 + LLM rerank |
| 遗忘曲线 | Decay() 几何衰减 |
| 重复 = 强化 | ConflictResolver 就地 Update + AccessCount |

agent 没有海马体——extractor 替代它做"哪些值得记 / 怎么压缩"的判断。`Decay` 实现"用进废退"，与艾宾浩斯曲线异曲同工。

---

下一篇：[`26_pty.md`](26_pty.md) —— PTY 终端会话：状态持久化的 shell 工具。
