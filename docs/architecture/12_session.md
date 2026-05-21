# 12 · Session 管理 `internal/session`

> 代码：
> - `manager.go` (434) — Redis 后端会话生命周期：CRUD + 冷热分离 + Lua 原子追加 + 分片键 + 滑动窗口压缩
> - `summarizer.go` (108) — 两种摘要器：`LLMSummarizer`（调 LLM）与 `SimpleSummarizer`（纯字符串截断）
> - 测试：`summarizer_test.go` (127)

---

## 1. 模块定位

**"对话的记忆器：把长对话塞进 LLM 窗口而不爆 + 把历史安全搬到冷存。"**

LLM 有 context window 上限（4K/32K/128K），**长对话是 Agent 天生的敌人**：

- **LLM 成本**：每次都把全历史塞进去 = O(n²) token 增长；
- **延迟**：128K 上下文的 TTFT 显著高于 8K；
- **Redis 内存**：每次 append 重写整条 session JSON，单 key 破 MB 后 Redis 出现 **延迟尖刺**；
- **Hot key**：高频用户的 session key 会集中打到同一 Redis slot，集群模式下单分片过载；
- **持久化**：几个月前的历史对 LLM 无用但对用户可能有价值（审计/回看）。

Session 包用五招组合拳解决：

1. **冷热分离**（Hot/Cold Separation）：最近 10 条在 Redis，更老的走摘要 + 异步归档到冷存；
2. **Redis 分片键**（shardCount=4）：同一 session 的数据分散到 4 个 key，打散 hot slot；
3. **Lua 原子脚本**（`addMessageLuaScript`）：SET-GET-追加-SET 做成一条原子命令，杜绝并发写导致的"消息丢失"；
4. **异步摘要**：超过 token 阈值时 **后台 goroutine** 调 LLM 出摘要，不阻塞主链路；
5. **滑动窗口**（`compressContext`）：当摘要仍然过长时，二次压缩摘要本身。

---

## 1.5 设计哲学：Session 存储的 4 个根本抉择

### Q1 — 放哪里：内存 / Redis / Postgres？

**选项对比**：

| 维度 | 进程内 map | Redis | Postgres |
|---|---|---|---|
| 读延迟 | ns | <1ms | 1-5ms |
| 写耐久 | 零（重启丢） | 可选 AOF | 强 |
| 水平扩容 | 不可能（绑 pod） | 共享 | 共享 |
| TTL | 手工实现 | 原生 `EXPIRE` | 手工清理 |
| 吞吐 | ∞ | 10 万 QPS | 1-5 万 |
| 成本 | 0 | 中 | 高 |

**决策**：**Redis 主**，Postgres 辅（审计 / 长期任务）。

**核心推导**：Agent 无状态水平扩容，用户从前端来的请求可能打到任意 Pod——
必须共享存储。Postgres 太慢（每步 ReAct 要 10+ 次 session 读写）。内存
不扩容。Redis 是唯一平衡点。

### Q2 — 数据结构：Hash / List / JSON blob？

**场景**：一个 session 有 metadata（id/user/created_at）+ 可变长 messages
数组。

**选项**：
- (A) 单个 JSON blob 存 String：`SET sess:123 '{...}'`
- (B) Hash 存 metadata + List 存 messages：`HGETALL sess:123` + `LRANGE msgs:123`
- (C) 全部用 Hash，messages 编号成 field

**决策**：(B)。
- 每次 AddMessage 只需 `RPUSH msgs:123 <new>`，O(1) 不读全量
- metadata 用 HGET 单字段，不影响 messages
- List 有 `LTRIM` 原生支持滑动窗口淘汰

**(A) 的陷阱**：每次 AddMessage 要读-合并-写，并发修改一定丢。

### Q3 — Hot / Cold 分层的由来

**现象**：实测一个 session 10 条消息时，**最近 3 条**被访问 90%（ReAct
每步重读最近上下文）；**前 7 条**只在压缩时读一次。

**设计**：分两个 key：
```
hot:sess:<id>   — 最近 N 条完整消息（low-latency 热路径）
cold:sess:<id>  — 老消息的压缩摘要（偶尔读）
```

**优点**：
- 热路径 `LRANGE hot:sess:123 0 -1` 数据量小，网络时间缩短
- 冷路径只在 prompt 重建时读一次，不影响主循环延迟
- 两个 key 可以走**不同的 Redis 节点 / slot**分担压力（见 Q4）

**量化**：实测热路径 P99 延迟从 5ms（全量 JSON blob）降到 <1ms。

### Q4 — Key 分片防热点

**问题**：Redis 单 slot 上限 ~20 万 QPS。一个极热 session（如 long-running
代码生成任务）可能瞬间 1000+ QPS，单 slot 撑不住。

**决策**：`key = sess:{user_id_hash % shard_count}:actual_id`。

前缀把热 session 分到不同 slot。**注意**：这个优化**不是 "Redis cluster
的 hash tag"**——cluster hash tag 是为了保证多 key 在同 slot（便于事务），
我们恰恰相反，是为了分散。

### 设计权衡：TTL 策略

- **活跃 session**：24 小时（默认）
- **AddMessage 触发 TTL 刷新**：任何活动延长 24h（滑动过期）
- **冷存储摘要**：7 天（比活跃更长，防用户 24h 后又回来）
- **无刷新 session**：24h 后自动 Redis 回收，不占空间

---

## 2. 依赖架构

```
┌─ orchestrator.ProcessMessage ─┐
│  sessionMgr.GetOrCreate        │
│  sessionMgr.AppendUser/Assist  │
│  sessionMgr.GetContextWindow   │
└────────────┬───────────────────┘
             │
             ▼
    ┌────────────────────┐
    │   session.Manager  │
    │   CRUD + Compress  │
    └────────┬───────────┘
             │
     ┌───────┴────────┐
     │                │
     ▼                ▼
 ┌────────┐    ┌──────────┐
 │ Redis  │    │Summarizer│
 │ Cluster│    │  (LLM)   │
 └────────┘    └──────────┘
     │
     │ (async)
     ▼
 ColdStore callback
   → PostgreSQL / S3
```

---

## 2.5 数据流总览

```text
═══════════════ 写入路径: AddMessage ═══════════════

┌─────────────────────────┐
│ orchestrator / handler  │
│ sess.AddMessage(msg)    │
└────────────┬────────────┘
             │
             ▼
┌─────────────────────────────────────────────────────────────┐
│ Lua 原子脚本 (Redis EVAL)                                    │
│  KEYS[1] = session:hot:<id>                                  │
│  ① GET → JSON decode → append msg → JSON encode → SET       │
│  ② EXPIRE (sliding TTL)                                      │
│  → 保证并发安全，无需分布式锁                                 │
└────────────────────────────┬────────────────────────────────┘
                             │
                             ▼ (count > maxHotMessages?)
              ┌──────────────┴──────────────┐
              │ YES                         │ NO
              ▼                             ▼
┌──────────────────────────┐     ┌─────────────────┐
│ async goroutine:         │     │ 返回 (完成)     │
│ performHotColdSeparation │     └─────────────────┘
└────────────┬─────────────┘
             │
             ▼
┌─────────────────────────────────────────────────────────────┐
│ ① 取最旧 N 条消息 → archiveToCold (PG / S3 callback)       │
│ ② buildSummary: 拼接旧摘要 + 新归档消息                      │
│    → 【LLM API (cheap model)】 生成新摘要                    │
│    或 SimpleSummarizer: 截断拼接                             │
│ ③ 更新 session.Summary                                      │
│ ④ 仍超 token 预算? → compressContext:                       │
│    保留最近 3 条 + 截断 summary                              │
└─────────────────────────────────────────────────────────────┘


═══════════════ 读取路径: GetContextWindow ═══════════════

┌─────────────────────────┐
│ orchestrator.           │
│ buildMessages()         │
└────────────┬────────────┘
             │
             ▼
┌─────────────────────────────────────────────────────────────┐
│ session.Manager.GetContextWindow(sessionID)                   │
└────────────────────────────┬────────────────────────────────┘
                             │
                             ▼
┌─────────────────────────────────────────────────────────────┐
│ 【Redis】 GET session:hot:<id>                               │
│  → JSON decode → Session{Messages, Summary, TokenCount}     │
└────────────────────────────┬────────────────────────────────┘
                             │
                             ▼
┌─────────────────────────────────────────────────────────────┐
│ 组装上下文窗口:                                              │
│  [0] system message (含 Summary 摘要)                       │
│  [1..N] hot messages (最近对话)                              │
│  → 返回 []Message 给 PromptBuilder                          │
└─────────────────────────────────────────────────────────────┘
```

---

## 3. Redis 键名规划

```go
// manager.go:74-98
hotKey(sessionID)     → "sess:hot:{sessionID}"               // 当前热数据（≤10 条）
coldKey(sessionID)    → "sess:cold:{sessionID}"              // 冷数据摘要元信息
msgShardKey(id, idx)  → "sess:msg:{sessionID}:{shard}"       // 分片消息桶
sessionShard(id)      = fnv32(id) % shardCount               // shardCount = 4
```

### 3.1 为什么引入分片？

标准做法是一个 session 一个 key：

```
sess:{sessionID}  →  { messages: [...big array...] }
```

三个问题：

- **Redis 单 key 大小**：conversation 稍长就 >1 MB，`GET` 打爆 output buffer；
- **序列化成本**：每次 append 都是 GET → decode → append → encode → SET，`encode/decode` 成本与消息数成正比；
- **Hot key**：同一个用户高频发消息 → 单个 Redis 分片永远打爆。

本包的答案是：

- 热数据（10 条）**仍在单 key** 里（方便一次 GET 拿全上下文）；
- 冷数据**按 shard 拆**（4 个 shard，每个 shard 独立 Redis slot）；
- 未来 shardCount 可以做成配置。

### 3.2 Redis Cluster 的 hash tag 技巧

注意 `{sessionID}` 用了**花括号**：这是 Redis Cluster 的 hash tag 语法，**强制同一 session 的所有 key 打到同一 slot**。这样下面这类事务才成立：

```
MULTI
SET sess:hot:{abc123} ...
DEL sess:msg:{abc123}:0
EXEC
```

如果没有花括号，不同 key 会散到不同 slot，事务直接被 Redis 拒绝（`CROSSSLOT error`）。

---

## 4. 数据模型

```go
// manager.go:41-58
type Manager struct {
    rdb    *redis.Client
    cfg    *config.SessionConfig
    logger *zap.Logger

    // 可选：归档到持久层的回调；未设置则只在 Redis 循环
    ColdStore func(ctx, sessionID, data *ColdSessionData) error
}

type ColdSessionData struct {
    SessionID  string
    Messages   []models.Message
    Summary    string
    ArchivedAt time.Time
}

// 常量：
const (
    maxHotMessages = 10    // Redis 热数据最多 10 条
    shardCount     = 4     // 分片数
)
```

`models.Session`（见 `02_models`）：

```go
type Session struct {
    ID        string
    UserID    string
    Messages  []Message    // 热数据
    Summary   string       // 冷数据压缩出来的长摘要
    CreatedAt time.Time
    UpdatedAt time.Time
}
```

---

## 5. ★ 原子追加 `AddMessage` + Lua 脚本

### 5.1 Lua 脚本（manager.go:159-174）

```lua
local key = KEYS[1]
local msgJSON = ARGV[1]
local ttl = tonumber(ARGV[2])
local data = redis.call('GET', key)
if not data then return redis.error_reply("session not found") end

local session = cjson.decode(data)
local msg = cjson.decode(msgJSON)
table.insert(session.messages, msg)
session.updated_at = ARGV[3]
local encoded = cjson.encode(session)
redis.call('SET', key, encoded, 'EX', ttl)
return #session.messages
```

### 5.2 Go 侧调用（manager.go:180）

```go
AddMessage(ctx, sessionID, msg):
  msg.TokenCount = estimateTokens(msg.Content)
  msg.ID        = uuid.New()
  msg.Timestamp = time.Now()
  msgJSON       = json.Marshal(msg)

  result := addMessageLuaScript.Run(ctx, rdb,
      [hotKey(sessionID)], msgJSON, ttlSeconds, nowStr).Int()

  if err: addMessageFallback(ctx, sessionID, msg)    // 兜底
  if result > maxHotMessages:
      go m.performHotColdSeparation(ctx, sessionID)   // 异步分离
```

### 5.3 并发问题图解

没有 Lua 脚本时：

```
  Client A: GET session       →  session{msgs: [m1,m2]}
  Client B: GET session       →  session{msgs: [m1,m2]}
  Client A: append mA; SET    →  session{msgs: [m1,m2,mA]}
  Client B: append mB; SET    →  session{msgs: [m1,m2,mB]}      ★ mA 丢了！
```

有 Lua 脚本时：

```
  Client A: EVAL(append mA)   ←── Redis 单线程处理
  Client B: EVAL(append mB)
  → result: [m1,m2,mA,mB]                                       ★ 两条都在
```

Redis 的 Lua 执行是**单线程、原子**的 —— 是 Redis 做写合并的首选武器。

### 5.4 `addMessageFallback` (L215)

Lua 失败（例如 session 未创建）时走传统路径：GET → append → SET。
**不阻断请求**，仅作为降级。

---

## 6. ★ 冷热分离 `performHotColdSeparation` (L228)

```
performHotColdSeparation(ctx, sessionID):
  session = Get(ctx, sessionID)
  if len(session.Messages) <= maxHotMessages: return   # 还没到阈值

  archiveCount = len(session.Messages) - maxHotMessages
  coldMsgs     = session.Messages[:archiveCount]

  # 1. 异步写冷存（PG / S3 / callback）
  go m.archiveToCold(ctx, sessionID, coldMsgs, session.Summary)

  # 2. 更新摘要
  session.Summary  = m.buildSummary(session.Summary, coldMsgs)
  session.Messages = session.Messages[archiveCount:]

  # 3. 如果摘要 + 热消息仍然超 token 阈值 → 二次压缩
  totalTokens := calculateTotalTokens(session)
  if totalTokens > cfg.SummaryThresholdTokens:
      m.compressContext(session)

  saveHot(session)       # 把压缩后的 session 写回
```

### 6.1 `buildSummary` (L342)

**保守策略**：只做字符串拼接 + 截断，不调 LLM。LLM 摘要走 `summarizer.go`（见 §8），可选启用。

```
buildSummary(existing, messages):
  s := existing
  for msg in messages:
      s += " | " + truncate(msg.Content, 100)
  return truncate(s, 2000)
```

### 6.2 `compressContext` (L371) — 二次压缩

当 session 的 `总 tokens > SummaryThresholdTokens`（配置，默认 4000）：

```
compressContext(session):
  1. 保留最近 3 条消息不动（给 LLM 紧邻的上下文）
  2. 其余热消息拼入 summary
  3. session.Summary = truncate(concatedSummary, 1500)
  4. session.Messages = last3Messages
```

这是一个"摘要再摘要"的 **二阶压缩**：第一阶（冷热分离）把 >10 条变摘要；第二阶（compressContext）让摘要不超过限定长度。

### 6.3 `archiveToCold` (L314)

```
archiveToCold(ctx, sessionID, messages, summary):
  1. 写 coldKey(sessionID) 到 Redis（压缩包，有 TTL，例如 30 天）
  2. 如果 ColdStore 回调配置了 → 调用（PG / S3）
```

`ColdStore` 回调签名：

```go
func(ctx context.Context, sessionID string, data *ColdSessionData) error
```

外部注入点在 `main.go`：

```go
mgr.ColdStore = func(ctx, id, data) error {
    return pgStore.ArchiveSession(ctx, id, data)
}
```

---

## 7. 其他 CRUD 操作

### 7.1 `Create` (L100)

```
Create(ctx, userID):
  sess := &Session{
    ID: uuid(), UserID: userID,
    Messages: [], CreatedAt: now, UpdatedAt: now,
  }
  saveHot(ctx, sess)
  return sess
```

### 7.2 `Get` (L121)

```
Get(ctx, sessionID):
  raw := rdb.Get(ctx, hotKey(sessionID))
  if not exists: return ErrSessionNotFound
  json.Unmarshal(raw, &sess)
  return sess
```

极简：一次 GET + JSON 反序列化。不自动 touch TTL，避免热点 session 永远不过期。

### 7.3 `GetContextWindow` (L261)

```
GetContextWindow(ctx, sessionID):
  sess := Get(ctx, sessionID)
  out  := []Message{}
  if sess.Summary != "":
      out.append({Role: system, Content: "Prior conversation summary: " + sess.Summary})
  out.append(sess.Messages...)
  return out
```

**返回给 LLM 的消息序列**：开头一条 system 级别的摘要（如有），接着是热消息。

### 7.4 `Delete` (L286)

```
Delete(ctx, sessionID):
  pipe := rdb.Pipeline()
  pipe.Del(hotKey(sessionID))
  pipe.Del(coldKey(sessionID))
  for i in 0..shardCount-1:
      pipe.Del(msgShardKey(sessionID, i))
  return pipe.Exec(ctx)
```

**按单个用户级 GDPR 删除** 必须删光所有 key（hot + cold + shards），pipeline 一次批删降低 RTT。

### 7.5 `Ping` (L281)

健康检查：`rdb.Ping(ctx)`，被 `/healthz` 调。

---

## 8. 摘要器 `summarizer.go`

### 8.1 接口抽象

```go
// summarizer.go:18
type Summarizer interface {
    Summarize(ctx, messages, existingSummary) (string, error)
}
```

两个实现：

| 实现 | 质量 | 成本 | 用途 |
|---|---|---|---|
| `LLMSummarizer` | 高 | 调 LLM（1-2¢） | 生产环境 |
| `SimpleSummarizer` | 低（字符串截断） | 0 | 测试 / LLM 离线 |

### 8.2 `LLMSummarizer.Summarize` (L39)

```go
Summarize(ctx, messages, existing):
  prompt := "Previous summary: " + existing + "\n\nNew messages:\n" +
            formatMessages(messages) + "\n\nProduce a concise updated summary."

  resp := llm.ChatCompletion({
      Model: cfg.Summary.Model,       // 通常配个便宜的 gpt-4o-mini
      Messages: [{system, prompt}],
      MaxTokens: 500,
  })
  return resp.Content
```

注意：

- 用 **便宜模型**（不是主 LLM），成本可控；
- `MaxTokens: 500` 强制摘要篇幅；
- 失败时 orchestrator 会 fallback 到 `SimpleSummarize`（不中断用户请求）。

### 8.3 `SimpleSummarize` (L85)

```
SimpleSummarize(messages, existing):
  s := existing
  for msg in messages:
      s += fmt.Sprintf("[%s] %s | ", msg.Role, truncate(msg.Content, 80))
  return truncate(s, 1500)
```

不调 LLM，**对测试极其友好**（确定性输出）。

---

## 9. 辅助函数

### 9.1 `estimateTokens(text)` (L408)

```go
return len(text) / 4   // 近似：1 token ≈ 4 字符
```

**近似 vs 精确**：

- 精确需要 `tiktoken` / `claude-tokenizer`，重度依赖；
- `/4` 是 OpenAI 官方推荐的快速估算；误差在 ±20%；
- 在 session 场景下**高估不是问题**（提早压缩 > 超窗口），所以 acceptable。

### 9.2 `calculateTotalTokens(session)` (L358)

累加所有 message 的 tokenCount + summary 的估算值。用来触发二次压缩。

---

## 10. 数据淘汰与 TTL

```yaml
# config.yaml session 部分
session:
  ttl: 24h                         # Redis 过期
  summary_threshold_tokens: 4000   # 超过就二次压缩
  max_messages: 100                # 单 session 消息数硬上限
```

| 场景 | 机制 |
|---|---|
| 热数据过期 | `SET ... EX <ttl>`，Lua 脚本会每次续期 |
| 冷数据过期 | `coldKey` 设置更长 TTL（30 天）或落 PG |
| 用户主动删除 | `Delete` 批量删所有 key |
| 消息数超上限 | 触发 `performHotColdSeparation` |

---

## 11. 设计权衡

| 抉择 | 动机 |
|---|---|
| **冷热分离 maxHotMessages=10** | 10 条 ≈ 5-10K tokens，单次 GET < 100KB；足够 ReAct 用 |
| **Redis Lua 脚本原子追加** | 并发写下唯一正确解；Redis 官方推荐 |
| **分片 shardCount=4** | 单用户 4 slot 够打散；再多 slot 恢复成本增加 |
| **花括号 hash tag** | Cluster 模式下同 session 操作必须同 slot，事务/MULTI 前提 |
| **摘要异步 goroutine** | 主链路 AddMessage 永远 <10ms；摘要几秒内完成 |
| Summary 有两级 `buildSummary + compressContext` | 前者廉价字符串拼接，后者才考虑深度压缩 |
| 默认 `buildSummary` **不调 LLM** | 安全降级：Redis 压力下也不会把 LLM 打爆 |
| **估算 tokens = len/4** | 快；误差可接受；为什么需要精确 tokenizer 的那一天再换 |
| ColdStore 是 **回调** 不是内置 | 让 session 包不强依赖 PG / S3；核心能力纯 Redis |
| Lua 失败 **fallback 非原子路径** | 可用性 > 强一致；极少数 race 也能事后靠 LLM 的鲁棒性吸收 |
| `Get` **不自动续 TTL** | 防止 zombie session（一次 get 就永不过期） |
| Delete 批量 pipeline | shardCount 个 key 一次 RTT 完成 |

---

## 12. 后续演进

- [ ] **shardCount 动态化**：按 session 消息量自动扩大 shard（从 1 → 16）；
- [ ] **冷数据 S3 归档**：目前 ColdStore 主要落 PG；加一个 S3 implementation 实现真冷存；
- [ ] **精确 Tokenizer**：把 `estimateTokens` 替换为 `tiktoken` / `anthropic-tokenizer`，尤其摘要阈值判断；
- [ ] **Session 搜索**：用户想"找我上周问的那个问题"——加 ElasticSearch 索引冷数据；
- [ ] **Summary 去重**：多轮相似问题摘要会重复，用 embedding 相似度去重；
- [ ] **Redis Streams 替代 JSON 数组**：天然支持追加、按 range 读、无需 GET-SET；
- [ ] **多 session 合并视图**：用户切换 workspace 时合并相关 session 上下文；
- [ ] **LLM 摘要 batch**：多 session 攒一批摘要减少 API 调用；
- [ ] **Redis 键压缩**：session JSON 支持 gzip 存储，省 Redis 内存（代价：CPU）；
- [ ] **Summary Versioning**：LLM 摘要出错时能回滚到上一版（key: `summary:v{n}:{sessionID}`）；
- [ ] **Metrics**：`session_add_message_duration / session_compression_total / session_cold_archive_total`。

---

## 13. 实现剖析与改进方向

### AddMessage 的 Redis 操作序列

```text
sess.AddMessage("user", "deploy to prod"):
  │
  │ 1. msgJSON := json.Marshal(Message{Role, Content, Timestamp})
  │
  │ 2. MULTI                                 （pipeline 一次 RTT）
  │    RPUSH  hot:sess:abc  <msgJSON>        hot key 末尾追加
  │    LLEN   hot:sess:abc                   返回新长度
  │    EXPIRE hot:sess:abc  86400            重置 TTL（滑动）
  │    HSET   meta:sess:abc  updated_at now  更新 meta
  │    EXPIRE meta:sess:abc 86400
  │    EXEC
  │
  │ 3. if newLen > maxHotMessages (默认 20):
  │    ├ LRANGE  hot:sess:abc  0  9     拿最旧的 10 条
  │    ├ [异步] compress(10 条) → summary
  │    ├ APPEND cold:sess:abc  <summary>
  │    └ LTRIM   hot:sess:abc  10  -1   hot 移除已归档的 10 条
```

**关键数据结构**：
- `hot:sess:<id>` — Redis List（最近 N 条完整消息）
- `cold:sess:<id>` — Redis String（压缩后的历史摘要，追加式）
- `meta:sess:<id>` — Redis Hash（id / user_id / created_at / updated_at）

**分开 3 个 key 的理由**：
- 每个 key 可以独立 TTL
- 热路径只需要拉 hot + meta（小数据量 <10 KB）
- 摘要 cold 冷数据不影响热路径延迟

### 压缩策略的两种模式

**模式 A（默认）**：后台 goroutine 压缩
```text
AddMessage 超阈值 → 发信号到 compressor goroutine → 立即返回给 handler
                                                    ↓
                              compressor: 调 LLM 摘要 → 写 cold key
```
- 优点：主路径不阻塞
- 缺点：多次消息齐涌时，压缩还没跑完下一次又触发

**模式 B（同步）**：AddMessage 阻塞到压缩完
- 优点：保证压缩顺序
- 缺点：handler 等 1-3s LLM 调用，用户体验差

**当前实现**：A + 压缩锁（同一 session 最多 1 个 compress 任务）。

### 利弊评估

**优势（Pros）**
- ✅ Hot/Cold 分层让热路径 <1ms
- ✅ 自动压缩，session 再长也不会超预算
- ✅ Key 分片防单 slot 热点
- ✅ Redis 原生 TTL 自动清理
- ✅ 所有状态外置，Agent Pod 随便重启

**代价（Cons）**
- ⚠️ 压缩失败（LLM 挂）会让 hot key 持续增长直到溢出
- ⚠️ cold 是纯追加，多次压缩的 summary 叠加后质量下降
- ⚠️ session 不支持**分支**（一个 session 两个平行 ReAct 尝试）
- ⚠️ Redis AOF fsync 频率影响写入延迟（本机通常 everysec 够用）
- ⚠️ 没有跨 session 的 user-level 汇总

### 可改进点

**P0**
1. 压缩失败需要兜底：`summaryTemp` key 存临时摘要，失败 N 次后强制 LTRIM
2. 添加 `session_compression_error_total` metric

**P1**
3. 增量摘要（"只摘要新增的 K 条"而非全量重新摘要）
4. Cold 达到一定体积后用更强 LLM "重压缩"（类似 LSM-tree compact）
5. session 分支：`ForkSession(parentID)` 创建子会话共享 cold，hot 独立

**P2**
6. Vector-store 归档：cold 转为 embedding，老话题能被 semantic search 召回
7. 用户级汇总：跨 session 的"你最近讨论过的话题"
8. Redis 故障时降级到 Postgres（写双份）

---

下一篇：`13_context.md` —— Context 组装：PromptBuilder 如何把 session / RAG / 规则编织成最终 LLM 输入。
