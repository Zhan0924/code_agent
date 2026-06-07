# 12 · Session 管理 `internal/session`

> 代码（**以代码为准**，不要把 `_principles.go` 和 `doc.go` 里的设计构想当成实现）：
>
> - `manager.go` — Redis 后端会话生命周期：CRUD + 热冷分层 + Lua 原子追加 + auto-pin + 压缩 + **PG 长期持久化双写**
> - `pg_store.go` — `SessionStore` 接口 + `PGSessionStore`(PostgreSQL `sessions` 表的 JSONB 行)。Manager 通过 `PGStore` 字段持有,作为 Redis hot/cold 过期后的权威源
> - `summarizer.go` — Summarizer 接口 + `LLMSummarizer` / `SimpleSummarizer`
> - `_principles.go` (714 行) — **RFC 风格的设计构想**:里面描绘的 `sess:meta:{sid}` HASH + `sess:msg:{sid}:{shard}` LIST + Redis Cluster `{hash tag}` 全部 **未在 `manager.go` 中实现**。读这个文件时请意识到它是设计草图而非现状
> - `doc.go` (46 行) — 同样**与代码不一致**:宣称的 List + Hash + String 三键分离 / `SET NX PX` 分布式锁 在代码里都没有(**注意**:PostgreSQL 表结构这一项在 2026-06 引入 `pg_store.go` 之后**已落地**)
>
> 测试:`summarizer_test.go`、`pg_store_test.go`、`manager_test.go`。

---

## 1. 模块定位

**"对话的记忆器：把长对话塞进 LLM 窗口而不爆 + 把超出预算的历史降维成摘要。"**

LLM 调用本质无状态——每一轮 ReAct 都要把完整 history 重新塞 prompt 里。
长会话天生与 LLM 为敌：

- **Token 成本** O(n²)：第 n 轮要把前 n-1 轮全送回去；
- **延迟**：128K 上下文的 TTFT 显著高于 8K；
- **Redis 内存**：单 key JSON 破 1MB 后 `GET` 出现尖刺；
- **并发**：同一会话两条消息并发写，read-modify-write 会丢消息；
- **持久化**：过期 session 占着 Redis 不释放，月度成本失控。

本包实际用了 4 招(**比 _principles.go 描绘的更务实**):

1. **Lua 原子追加**(`addMessageLuaScript`):单线程 GET-decode-append-encode-SET 一气呵成,杜绝并发写丢失;
2. **Token 预算驱动的热冷分离**:超 `SummaryThresholdTokens` 时把最旧若干条摘要化并归档到 `coldKey`;
3. **Auto-Pin**:含"always/never/must/important:"等关键词的用户消息自动置位 `Pinned=true`(注意:**当前压缩逻辑并未真正读取 `Pinned`**,是埋点供未来的剪枝器使用);
4. **PG 长期持久化**(`PGStore`,2026-06 引入):Create/AddMessage 时异步双写到 PostgreSQL `sessions` 表,Get 在 hot miss 时从 PG rehydrate 并回写 hot;ListSessions 改以 PG 为权威源——解决了 Redis hot/cold TTL 过期后会话从侧栏永久消失的旧 bug。

**没有实现但 `_principles.go` 声称有的**：

- ❌ 多 key 切分（`sess:meta` HASH + `sess:msg:{shard}` LIST）—— 实际只有 **一个 `sess:hot:<id>` key 装整个 JSON**
- ❌ Redis Cluster hash tag `{sid}` —— 实际 key 没有花括号
- ❌ `msgShardKey` 写入路径 —— 函数定义了但**无任何调用方写入**，只在 `Delete` 时被防御性删除
- ❌ 分布式锁 / `SET NX PX` —— 完全没有
- ❌ TTL 滑动续期 —— `Get` 不 touch TTL；只有 `SET` 时重设 TTL

---

## 1.5 设计哲学：4 个被代码证实的抉择

### Q1 — 为什么 Session 放 Redis？

| 选项 | 读延迟 | 写耐久 | 水平扩容 | TTL | 成本 |
|---|---|---|---|---|---|
| 进程内 map | ns | 0（重启丢） | 不可（绑 pod） | 手工实现 | 0 |
| Redis | <1ms | AOF 可选 | 共享 | 原生 `EXPIRE` | 中 |
| Postgres | 1-5ms | 强 | 共享 | 手工清理 | 高 |

**结论**:Redis(热路径)+ Postgres(长期持久化)双层。`main.go` 显示 Redis 失败直接 fatal,session 包认 Redis 为热路径强依赖;PG 可选,无 PG 时退化为 Redis-only 模式(行为等同 2026-06 前的实现,过期仍会从侧栏消失)。

历史钩子 `ColdStore func`(`manager.go`)仍保留:它服务"超 token 阈值压缩归档到 Redis cold"语义,跟"全量持久化"的 `PGStore` 是两条正交路径,共存不冲突。

### Q2 — 为什么是单 JSON blob，不是 HASH + LIST？

代码里 `hotKey()` 单 key 装整条 `models.Session`（含 Messages 数组）。
这与多数教科书"用 LIST 存 messages"建议相反，但出于两个原因合理：

1. **Lua 脚本的复杂度**：HASH + LIST 多键操作，原子追加需要更复杂的 Lua（处理 metadata 和 msgs 列表两边），单 JSON 只需要 `GET → decode → append → encode → SET`
2. **`Get` 是热路径**：每次 ReAct 步骤都 `Get(sessionID)`，单 GET + 单 unmarshal 简单且 cacheline-friendly。如果是 HASH+LIST，要 HGETALL + LRANGE 两次 RTT

**代价**：单 JSON 在 messages 数组很长时 marshal/unmarshal 成本随 N 线性增长。
这就是为什么 `performHotColdSeparation` 在 token 超阈值时**必然要归档**——
不归档不仅仅是 prompt 太长，单纯 JSON 操作就吃不消。

### Q3 — 阈值是 token，不是消息条数

`checkAndArchive`（L302）的触发条件是：
```go
totalTokens > m.cfg.SummaryThresholdTokens && len(session.Messages) > minHotMessages
```

**不是** "≥10 条 → 压缩"。
为什么？因为有些用户的消息是 100 字的代码补丁，有些是 50KB 的 traceback。
按条数压缩会让 50KB 的 traceback 在它单条就超 prompt 时仍然不压缩。
按 token 压缩才能保证 prompt 永远在预算内。

**`minHotMessages = 2`**（L82）是硬下限：保护性兜底，避免单条巨大消息触发"把唯一一条消息也归档"的退化场景。

### Q4 — 摘要为什么异步？

`AddMessage` 末尾（L272）：
```go
go m.checkAndArchive(context.Background(), sessionID)
```

注意 `context.Background()` —— **故意不用 request ctx**：用户的 HTTP 请求 ctx 在 AddMessage 返回后会被取消，
若摘要 goroutine 跟着请求 ctx，就会随请求结束被打断。
归档是后台事务，**它的生命周期独立于发起它的那次写入请求**。

副作用：如果连续两条消息触发归档，会产生 2 个并发归档 goroutine。
当前实现**没有 per-session lock**，所以最坏情况两边都把 `Messages[:keepFrom]` 移走，
靠 Lua 原子写入的最后写入胜出来收敛。**这是已知 P1 风险**，详见 §10。

---

## 2. 依赖架构

```
┌─ orchestrator.ProcessMessage ──────────────────────┐
│  sessionMgr.AddMessage(user msg)                    │
│  sessionMgr.GetContextWindow(sessionID)             │
│  sessionMgr.AddMessage(assistant msg)               │
└────────────┬───────────────────────────────────────┘
             │
             ▼
   ┌────────────────────────────┐
   │ session.Manager             │
   │  - rdb: *redis.Client       │
   │  - cfg: SessionConfig       │
   │  - PGStore: SessionStore    │  ← 2026-06 新增,长期权威源
   │  - ColdStore: callback      │  ← 旧钩子,与 PGStore 正交
   │  - Summarizer: iface        │
   └────────┬────────────────────┘
            │
     ┌──────┼───────┬──────────────┐
     ▼      ▼       ▼              ▼
  ┌──────┐ ┌──────────┐ ┌──────────────┐
  │Redis │ │PG sessions│ │ Summarizer   │
  │ hot+ │ │ table     │ │ (LLM/Simple │
  │ cold │ │ (JSONB)   │ │  字符串)     │
  └──────┘ └──────────┘ └──────────────┘
                            │
                            │ (async, optional, 旧路径)
                            ▼
                       ColdStore(callback)
                         → PostgreSQL / S3
```

**注入点**(`cmd/agent/main.go`):

- `session.NewManager(rdb, &cfg.Session, logger)`
- 把 `llmClient.ChatCompletion`(Temperature=0.2)包装成 `LLMSummarizer` 注入 `sessionMgr.Summarizer`
- `pgStore != nil` 时:`sessionMgr.PGStore = session.NewPGSessionStore(pgStore.DB(), logger)`,启用 PG 长期持久化
- `ColdStore` 字段**在 main.go 中没有任何赋值**——目前是悬空回调,归档只到 `coldKey` Redis(`PGStore` 已替代了它的"长期落 PG"职责,`ColdStore` 留作未来扩展)

---

## 2.5 数据流总览

```text
═══════════════ 写入路径: AddMessage ═══════════════════════════════════

orchestrator.processStep
       │
       ▼
sessionMgr.AddMessage(ctx, sid, msg)                    [manager.go:239]
       │
       │ 1. estimateTokens(msg.Content)       ← llm.FastEstimate
       │ 2. msg.ID = uuid; msg.Timestamp = now
       │ 3. shouldAutoPin(msg)                ← 关键词嗅探
       │ 4. json.Marshal(msg)
       │
       ▼
addMessageLuaScript.Run(KEYS=[hotKey], ARGV=[json, ttl, now])  [L218]
       │   Lua 单线程:
       │     data = GET hotKey                ← 整条 Session JSON
       │     session = cjson.decode(data)
       │     table.insert(session.messages, msg)
       │     SET hotKey cjson.encode(session) EX ttl
       │     return #session.messages
       │
       │ if err → addMessageFallback (非原子兜底)         [L278]
       │
       ├─ go checkAndArchive(Background(), sid)            [L272 异步]
       │      │
       │      ▼
       │   Get(sid) → totalTokens > SummaryThresholdTokens ?
       │      │  YES
       │      ▼
       │   performHotColdSeparation                        [L310]
       │      ├ 从尾向头扫描，找 keepFrom（保留尾部最近若干条 ≤ targetTokens=0.75*threshold）
       │      ├ go archiveToCold(coldMsgs, summary)        [L438 异步]
       │      │     → SET coldKey JSON, TTL=cfg.TTL*2
       │      │     → if ColdStore != nil: ColdStore(ctx, sid, coldData)
       │      ├ session.Summary = buildSummary(prev, coldMsgs)
       │      │     ├ if Summarizer: 调 LLM → 失败回退到字符串拼接
       │      │     └ else: 拼接 "[role]: content" 截断
       │      ├ session.Messages = session.Messages[keepFrom:]
       │      └ totalTokens 仍超? → compressContext (二级压缩)   [L503]
       │            按 MaxHistoryTokens/2 预算从尾装回头
       │
       ▼ return nil

═══════════════ 读取路径: GetContextWindow ═══════════════════════════════

orchestrator.buildMessages
       │
       ▼
sessionMgr.GetContextWindow(ctx, sid)                   [manager.go:385]
       │
       ▼
Get(sid)                                                [manager.go:180]
       ├ rdb.Get(hotKey)                ← 主体 Session JSON
       ├ rdb.Get(coldKey)               ← Cold 摘要（可选）
       └ 合并：session.Summary = cold.Summary + "\n" + session.Summary
       │
       ▼
组装上下文窗口（manager.go:391-401）:
   if session.Summary != "":
     [0] system "[Previous conversation summary]: <summary>"
   [1..N] session.Messages...
   → 返回 []Message 给 PromptBuilder
```

---

## 3. Redis 键名规划（**以代码为准**）

```go
// manager.go:127-149
hotKey(sessionID)       → "sess:hot:<sessionID>"      // 整条 Session JSON
coldKey(sessionID)      → "sess:cold:<sessionID>"     // ColdSessionData JSON
msgShardKey(sid, idx)   → "sess:msg:<sid>:<shard>"    // 定义了但写路径不用
sessionShard(sid)       = fnv32(sid) % shardCount     // shardCount=4
```

⚠️ **`msgShardKey` 是定义未启用**：grep 全包 `msgShardKey(` 只在 `Delete`（防御删除）里出现，
**无任何写入路径产生这种 key**。`_principles.go` 描绘的"按 shard 分发消息"是设计构想，不是现状。

⚠️ **没有 Redis Cluster hash tag**：实际 key 没有 `{}`，所以多个 key（hot + cold）
在 Cluster 模式下可能落到**不同 slot**——`Delete` 用 `pipe.Del` 多 key 在 Cluster 上
会触发 `CROSSSLOT` 错误。**单机 Redis 模式没问题，Cluster 模式是 P1 隐患**。

---

## 4. 数据模型

### 4.1 `Manager` 结构(manager.go)

```go
type Manager struct {
    rdb    *redis.Client       // ⚠️ 不是 ClusterClient
    cfg    *config.SessionConfig
    logger *zap.Logger
    PGStore    SessionStore                                   // 2026-06 新增,可选
    ColdStore  func(ctx, sessionID, *ColdSessionData) error  // 可选,旧钩子
    Summarizer Summarizer                                     // 可选
}
```

`PGStore`/`ColdStore`/`Summarizer` 都是**可选字段**:

- `Summarizer == nil` → `buildSummary` 走字符串拼接 fallback
- `ColdStore == nil` → 归档只写 Redis `coldKey`,不落 PG / S3
- `PGStore == nil` → 退化为 Redis-only 模式:Create/AddMessage 不双写,Get 不 rehydrate,ListSessions 走旧路径(含 stale ZREM)。**生产环境强烈建议接入 PGStore**——否则会话超过 Redis hot TTL(24h)后从侧栏永久消失

### 4.2 `SessionConfig`（config.go:255-261）

```go
type SessionConfig struct {
    MaxHistoryTokens       int           // 总上下文上限
    SummaryThresholdTokens int           // 触发归档的 token 阈值
    TTL                    time.Duration // hotKey 的 EXPIRE 时间
    CompactionMode         string        // "truncate" / "summarize" (未在 manager.go 中分支使用)
}
```

`validate.go:96-102` 强制：
- `MaxHistoryTokens > 0`
- `SummaryThresholdTokens > 0`
- `SummaryThresholdTokens ≤ MaxHistoryTokens`

⚠️ **`CompactionMode` 是声明未读**：grep `CompactionMode` 在 manager.go 里**完全不出现**，
当前实现的压缩策略是硬编码的"先摘要 + 必要时滑窗"，**不响应这个配置**。

### 4.3 常量（manager.go:78-88）

```go
const (
    minHotMessages = 2     // 硬下限：归档后 hot 必留至少 2 条
    shardCount     = 4     // msgShardKey 用，但目前只在 Delete 时被引用
)
```

⚠️ **`maxHotMessages = 10`** 是旧 doc 杜撰的常量，**代码里不存在**。
实际控制热数据规模的是 `SummaryThresholdTokens` 阈值，不是消息条数。

---

## 5. ★ 原子追加 `AddMessage`（manager.go:239）

### 5.1 Lua 脚本（manager.go:218-233）

```lua
local key = KEYS[1]
local msgJSON = ARGV[1]
local ttl = tonumber(ARGV[2])
local data = redis.call('GET', key)
if not data then
    return redis.error_reply("session not found")
end
local session = cjson.decode(data)
local msg = cjson.decode(msgJSON)
table.insert(session.messages, msg)
session.updated_at = ARGV[3]
local encoded = cjson.encode(session)
redis.call('SET', key, encoded, 'EX', ttl)
return #session.messages
```

**注意**：脚本在 session 不存在时**直接 error**——不会自动创建。所以 `Create` 必须先于第一次 `AddMessage`。

### 5.2 失败降级（manager.go:266-268）

```go
if err != nil {
    return m.addMessageFallback(ctx, sessionID, msg)
}
```

`addMessageFallback`（L278）走 `Get → append → saveHot`：**不原子**，
仅作为 Lua 失败时（脚本编译错误、Redis 版本不支持等）的兜底。
**生产环境正常情况绝不应该走到这里**。

### 5.3 并发写丢失问题图解

没有 Lua 时：
```
A: GET session       →  {msgs: [m1,m2]}
B: GET session       →  {msgs: [m1,m2]}   ← 同一快照
A: append mA; SET    →  {msgs: [m1,m2,mA]}
B: append mB; SET    →  {msgs: [m1,m2,mB]} ★ mA 丢
```

有 Lua 时：
```
A: EVAL(append mA)  ←─┐
B: EVAL(append mB)  ←─┴─ Redis 单线程串行
→ {msgs:[m1,m2,mA,mB]}
```

Redis Lua 单线程是这种 read-modify-write 的最简洁原子方案。

### 5.4 Auto-Pin 机制（manager.go:566-583）

```go
shouldAutoPin(msg) =
  msg.Role == User &&
  msg.Content 包含以下任一关键词（不区分大小写）:
    "always", "never", "must", "required", "critical",
    "important:", "note:", "remember:", "constraint:",
    "do not", "don't forget", "make sure"
```

命中即 `msg.Pinned = true`。

⚠️ **但当前压缩逻辑不读 `Pinned`**：`performHotColdSeparation` 的 `keepFrom` 计算
（L327-360）只看 token 累加，**不会跳过 Pinned 消息**。
这是埋点字段，等未来 context.Pruner 接管时再消费。

---

## 6. ★ 热冷分离 `performHotColdSeparation`（manager.go:310）

### 6.1 触发条件（manager.go:299-304）

```go
checkAndArchive:
  totalTokens := calculateTotalTokens(session)
  if totalTokens > cfg.SummaryThresholdTokens &&
     len(session.Messages) > minHotMessages:
       performHotColdSeparation(ctx, sid)
```

**两个条件 AND**：token 超阈值 + 至少 3 条消息（minHotMessages=2，>2 即 ≥3）。

### 6.2 keepFrom 算法（manager.go:327-356）

```go
targetTokens := SummaryThresholdTokens * 3 / 4   // 留 25% 顶部空间
runningTokens := estimateTokens(session.Summary)
keepFrom := len(session.Messages)

// 从尾部向头部扫描
for i := len-1; i >= minHotMessages; i-- {
    runningTokens += msg[i].TokenCount
    if runningTokens > targetTokens:
        keepFrom = i + 1
        break
    keepFrom = i
}

// 保证 keepFrom ∈ [minHotMessages, len-minHotMessages]
if keepFrom < minHotMessages: keepFrom = minHotMessages
if keepFrom > len-minHotMessages: keepFrom = len-minHotMessages
```

含义：
- 保留**尾部最近若干条**，累计 token ≤ targetTokens（threshold 的 75%）
- 头部前 `keepFrom` 条全部归档
- 至少保留 minHotMessages（2 条），即使头部极大也不全归档

### 6.3 归档与摘要（manager.go:362-381）

```go
archiveCount := keepFrom
coldMsgs := session.Messages[:archiveCount]

go archiveToCold(Background(), sid, coldMsgs, session.Summary)
        // 异步 + Background ctx：用户请求结束不影响归档

session.Summary  = buildSummary(session.Summary, coldMsgs)
session.Messages = session.Messages[archiveCount:]

// 二级压缩
if calculateTotalTokens(session) > SummaryThresholdTokens:
    compressContext(session)

saveHot(ctx, session)
```

### 6.4 `buildSummary`（manager.go:466）

```go
if Summarizer != nil:
    summary, err := Summarizer.Summarize(Background(), messages, existing)
    if err == nil:
        return summary
    // LLM 失败 → fallback 到字符串拼接

// 字符串拼接 fallback
"Archived <N> messages. Key exchanges covered: [role]: content...; [role]: content..."
// 总长度截断到 500 字符
```

⚠️ **`Summarizer.Summarize` 用 `context.Background()`** —— 与上层 ctx 解耦，
所以即使主请求超时，摘要 LLM 调用仍会跑完（带超时由 LLMSummarizer 内部 ctx 兜底）。
**当前 `LLMSummarizer` 没有内置超时**，靠 `llm.Client` 的 30s timeout 兜底。

### 6.5 `compressContext`（manager.go:503）— 二级压缩

只在归档后 token 仍超阈值时触发：

```go
budget := MaxHistoryTokens / 2
// 从尾装回，累计 token > budget 时停止
// 头部丢弃部分拼入 Summary
```

是"摘要再摘要"的兜底机制。

### 6.6 `archiveToCold`（manager.go:438）

```go
cold := &ColdSessionData{SessionID, Messages, Summary, ArchivedAt}
data := json.Marshal(cold)
rdb.Set(ctx, coldKey(sid), data, TTL*2)   // cold key TTL 是 hot 的 2 倍

if ColdStore != nil:
    ColdStore(ctx, sid, cold)               // 投递到 PG / S3
```

**注意 TTL=`cfg.TTL*2`**：cold 比 hot 多撑 1 倍——用户 24h 后回来看历史还能找到。
不是 _principles.go 说的"30 天"或"长期归档"。

---

## 7. 其他 CRUD

| 方法 | 行号 | 关键行为 |
|---|---|---|
| `Create(userID, projectID)` | L154 | 生成 UUID，空 `projectID` 默认 `"default"`；写 hotKey |
| `Get(sid)` | L180 | GET hotKey + GET coldKey 合并 Summary；**不刷 TTL**（防止 zombie session） |
| `GetContextWindow(sid)` | L385 | `Get` + 把 Summary 包成 system message 前置 + Messages 拼接 |
| `Ping(ctx)` | L405 | `rdb.Ping`，被 `/healthz` 调 |
| `Delete(sid)` | L410 | Pipeline 删 hotKey + coldKey + 所有 msgShardKey（防御） |
| `PinMessage(sid, msgID)` | L586 | GET → 找到 msgID 改 Pinned=true → SET |
| `UnpinMessage(sid, msgID)` | L591 | 同上设 false |

⚠️ **`PinMessage`/`UnpinMessage` 非 Lua 原子**(L596-633):
读-改-写 序列,**与并发 AddMessage 竞争会导致 Pin 状态丢失**。
原因:Pin 操作不是高频,未被设计成原子。**已知 P1**。

---

## 7.5 ★ ListSessions / Get 的 PG 长期持久化路径(2026-06)

### 7.5.1 旧 bug:Redis-only 时为何会话从侧栏消失

旧 `ListSessions` 流程:遍历 `sess:idx:<userID>` ZSET 拿 sessionID → 逐条 `Get(hotKey)` → **hot miss 则 ZREM 索引项**。
hot TTL=24h,用户两天没回访的会话被认定"stale"主动 ZREM,**索引一旦删除就不可恢复**——即使 cold key 还在 Redis 里也找不回来,即使 user 重启服务也救不回来。

### 7.5.2 新流程:PG 为权威源,Redis 仅做 hot warming

`PGStore != nil` 时,`ListSessions` 不再走 Redis ZSET 路径,而是:

```
PGStore.ListByUser(userID, limit)
  ├ SELECT id,user_id,project_id,message_count,last_role,last_preview,created_at,updated_at
  │ FROM sessions WHERE user_id=$1 ORDER BY updated_at DESC LIMIT $2
  ├ 不读 data JSONB(避免大列扫描)
  └ 返回 []*SessionSummary
对每条 PG summary,opportunistic 从 hot 取实时 message_count/last_preview 覆盖
→ 返回最终列表
```

**关键改动**:
- ✅ 不再 ZREM——hot miss 不会触发索引清理,会话永不"被动消失"
- ✅ PG 是权威源,Redis 仅当"加速层"使用
- ✅ `PGStore.ListByUser` 失败时**优雅回落**到旧的 Redis ZSET 路径(只是没有 stale prune,见 §7.5.4)

### 7.5.3 Get 的 rehydrate

```
Get(sid):
  data := rdb.Get(hotKey)
  if redis.Nil && PGStore != nil:
      session := PGStore.Get(sid)   // 从 PG JSONB 反序列化整 Session
      saveHot(session)              // 回写 hot,后续 Get 走 fast path
      return session
  ...
```

PG 命中后自动回写 hot,意味着用户点开一个超过 24h 的会话,**第一次 Get 慢一点(多 1 个 PG round-trip),后续都是热路径**。

### 7.5.4 双写策略

| 写入点 | 同步 Redis | 异步 PG | 备注 |
|---|---|---|---|
| `Create` | ✅ saveHot + 入 ZSET | ✅ `go m.asyncPGUpsert(session)` | 失败仅 Warn |
| `AddMessage` | ✅ Lua 原子追加 | ✅ Lua 后 Get 一次拿快照,再 asyncPGUpsert | 多 1 次 Redis GET 的成本 |
| `Delete` | ✅ Pipeline 删 hot/cold | ✅ 5s timeout context 异步 Delete | 失败仅 Warn |

**异步 PG 写一律深 copy `Messages` slice 后再丢 goroutine**,避免与后续 AddMessage 的 slice mutation 竞争。

### 7.5.5 PG 不可用退化

`PGStore == nil` 时,Manager 行为**完全等同 2026-06 前**:
- Create/AddMessage/Delete 不双写
- Get 在 hot miss 时直接 return not-found
- ListSessions 走旧 Redis ZSET 路径,**保留 stale prune**(Redis-only 模式下索引膨胀的唯一回收手段)

`main.go` 在 PG DSN 为空时 Warn:"session PG long-term store disabled (no Postgres DSN); sessions will be lost after Redis TTL"——这是显式提示,不是 Fatal。

---

## 8. 摘要器 `summarizer.go`

### 8.1 接口（summarizer.go:18-20）

```go
type Summarizer interface {
    Summarize(ctx, messages, existingSummary) (string, error)
}
```

### 8.2 两个实现

| 实现 | 行 | 质量 | 成本 | 用途 |
|---|---|---|---|---|
| `LLMSummarizer` | L23 | 高 | 调 LLM | 生产；main.go 默认注入 |
| `SimpleSummarizer` | L77 | 字符串截断 | 0 | 测试 / LLM 不可用 |

### 8.3 `LLMSummarizer.Summarize`（summarizer.go:39）

```go
// chatFn 是个函数变量，避免 session 包导入 llm 包形成循环
chatFn func(ctx, systemPrompt, userPrompt string) (string, error)
```

**为什么不直接持有 `*llm.Client`**？`llm` 包可能反向引用 `models` 或其他包，
session 直接依赖 `llm.Client` 会形成循环。`func` 型注入是 Go 项目里**避免循环依赖的标准手法**。

**Prompt 构造**：
```
[Previous Summary]: <existingSummary>

[New messages to summarize (<N> messages)]:
- [<role>]: <content 截断到 200 chars>
...
```

System prompt 强制：
- 保留技术决策、文件名/函数名/symbol、tool 结果、未解决问题
- 摘要 ≤ 200 词
- 事实精确

LLM 失败 → **fallback 到 `SimpleSummarize`**（**不返回错误**，保证主流程不阻塞）。

### 8.4 `SimpleSummarize`（summarizer.go:85）

```
"Archived <N> messages. Key exchanges: [role]: content(80 chars)...; ..."
// 总长度截断到 500 chars
```

确定性输出，单元测试友好。

---

## 9. 辅助函数

### 9.1 `estimateTokens`（manager.go:540）

```go
func estimateTokens(text string) int {
    return llm.FastEstimate(text)
}
```

**注意是 `llm.FastEstimate`**（外部函数），**不是 `len(text)/4`**。
具体行为见 `internal/llm/tokens.go`（一般是 byte-based ≈ char/4 经验值）。
这是项目里唯一的 tokenizer 中心，所有包都走这个函数，保证口径一致。

### 9.2 `calculateTotalTokens`（manager.go:490）

```go
total := estimateTokens(session.Summary)
for msg in session.Messages:
    total += msg.TokenCount if > 0 else estimateTokens(msg.Content)
return total
```

`msg.TokenCount > 0` 时直接复用（AddMessage 时已经算过），避免重复估算。

### 9.3 `shouldAutoPin`（manager.go:566）—— §5.4 已讲

---

## 10. 实现剖析与改进方向

### 10.1 当前实现的真实利弊

**优势(验证过的)**
- ✅ Lua 原子 AddMessage:并发写不丢消息
- ✅ Token 预算驱动归档:单条 50KB 消息也能触发压缩
- ✅ Summarizer 失败自动降级:LLM 挂不影响主流程
- ✅ 异步 `Background()` ctx 归档:用户请求结束不打断归档
- ✅ Cold key TTL=hot*2:用户跨天回访还能拿到摘要
- ✅ **PG 长期持久化(2026-06)**:Redis hot/cold 过期不丢会话;ListSessions 不再 ZREM stale;Get 透明 rehydrate

**已知风险**
- ⚠️ **并发归档无 lock**：连续两条 AddMessage 触发的 `performHotColdSeparation` 可能并发执行，最终靠"最后一次 SET hotKey 胜出"收敛——少量消息可能被复制归档
- ⚠️ **Cluster 模式 CROSSSLOT**：`Delete` 用 pipeline 跨多 key，Cluster 模式会报错（key 无 hash tag）
- ⚠️ **Pin/Unpin 非原子**：与 AddMessage 竞争会丢 Pin 状态
- ⚠️ **CompactionMode 配置失效**：声明了但 manager.go 不响应
- ⚠️ **`msgShardKey` 是死代码**：声明了从不写入，只在 Delete 时防御删除
- ⚠️ **doc.go 与 _principles.go 描述与代码严重不符**：维护者读旧文档会被严重误导

### 10.2 优先级修复建议

**P0（生产风险）**
1. 并发归档加 per-session lock（Redis `SET NX` 或 sync.Map）
2. Cluster 模式给所有 key 加 hash tag `{sid}`

**P1（代码质量）**
3. 删除 `msgShardKey` 死代码或真正实现 sharded 写入
4. PinMessage/UnpinMessage 改 Lua 原子脚本
5. 把 `CompactionMode` 配置接入压缩逻辑（或删除字段）

**P2（设计完善）**
6. 增量摘要：每次归档时摘要只包含**新增的 K 条**而非 `prev_summary + new K`，避免 Summary 二次膨胀
7. 实现 `Pinned` 跳过逻辑（compressContext 不归档 Pinned）
8. Redis 故障双写降级到 Postgres
9. `metrics`：`session_archive_total / session_token_budget_exceeded_total / session_lua_fallback_total`

---

## 11. 设计权衡

| 抉择 | 动机 |
|---|---|
| **单 JSON blob** 而非 HASH+LIST | Lua 原子追加简洁 + 单 GET 拿全上下文 |
| **Lua 脚本原子 AddMessage** | Redis 单线程 = 天然原子；并发写下唯一正确解 |
| **失败 fallback 非原子路径** | 可用性 > 强一致；极少数 race 也能被 LLM 鲁棒性吸收 |
| **`Background()` ctx 归档** | 主请求结束不打断归档 |
| **`Summarizer.Summarize` 内部 fallback** | LLM 挂不影响主流程 |
| **`chatFn` 函数注入** 而非持有 `*llm.Client` | 避免 session ↔ llm 循环依赖 |
| **Token 预算 而非消息计数** 触发归档 | 50KB 单条消息也能正确压缩 |
| **minHotMessages=2** | 硬下限，保护退化场景 |
| **Get 不刷 TTL** | 防止 zombie session 永不过期 |
| **Cold TTL = hot×2** | 跨天回访保护，月级归档交给 ColdStore 回调 |
| **`ColdStore` 是可选回调** 而非内置 PG | 核心能力不强依赖 PG / S3 |
| **Auto-Pin 仅打标** 不强制保留 | 埋点字段，给未来 Pruner 用 |

---

## 12. 后续演进

- [ ] **per-session 归档 lock**：Redis `SETNX archiving:<sid> 1 EX 60`，避免并发归档
- [ ] **Cluster hash tag**：所有 key 改 `sess:hot:{<sid>}`、`sess:cold:{<sid>}`
- [ ] **增量摘要**：摘要只覆盖新归档的 K 条，旧 summary 不重写
- [ ] **`Pinned` 真接入**：compressContext 跳过 Pinned 消息
- [ ] **session 分支**：`ForkSession(parentID)` 让用户开探索分支
- [ ] **跨 session 用户级汇总**：embedding + 检索
- [ ] **Redis Streams 替代 JSON 数组**：天然 RPUSH + XRANGE，无 marshal 成本
- [ ] **Redis 双写 Postgres**：故障降级
- [ ] **精确 tokenizer**：把 `llm.FastEstimate` 替成 `tiktoken`/`anthropic-tokenizer`
- [ ] **Metrics**：归档计数 / 时间 / token 直方图 / Lua fallback 计数
- [ ] **doc.go / _principles.go 与代码对齐**：删除多 key + hash tag + 分布式锁 这些未实现描述

---

## 13. 设计教训

1. **设计 RFC 与实现要分文件且清晰标注**：`_principles.go` 大量 RFC 内容（多 key / hash tag / 分布式锁 / shard 写入）从未实现，**新人读会以为这就是现状**，浪费排查时间。要么真正实现，要么放进 `docs/rfc/` 并显式标注 "design only"。

2. **Lua 原子脚本是 Redis 写合并的首选武器**：用 Lua 杜绝并发写丢失，比"加 Go 侧 mutex"通用、比"用 WATCH+MULTI"简洁、比"用 RedLock"轻量。代价是 EVAL 比 GET+SET 慢 ~20%，但首次后 EVALSHA 接近零开销。

3. **Token 阈值 vs 消息计数**：用 token 触发归档才能处理"单条巨大消息"的退化场景；消息计数是教科书写法但生产场景会爆。

4. **`context.Background()` 用在异步后台任务**：用户请求 ctx 会随 HTTP 响应取消，绑请求 ctx 的异步 goroutine 跑到一半被打断。后台事务必须独立 ctx + 自带超时。

5. **回调式可选依赖**：`ColdStore func(...) error` 让核心包不强依赖 PG/S3，单元测试不需要起 Postgres。这是 Go 项目常见的"依赖反转"做法。

6. **失败降级而非失败传递**：`Summarizer.Summarize` LLM 失败时**不返回 error**，自动调用 SimpleSummarize 返回字符串结果。session 这种基础设施挂了对整个系统是灾难，必须设计成永远有结果。

7. **死代码立刻清理**：`msgShardKey` 留着不写入只在 Delete 防御，新维护者必然怀疑"是不是漏接了什么写路径"。声明未用的字段或函数应该立刻删，或 `// TODO(YYYY): impl shard write` 显式标记。

8. **配置字段必须连接到行为**：`CompactionMode` 是声明未读的典型，validate 通过但实现不响应——这种"假配置"比"无配置"更糟，因为用户调它没效果还不知道为啥。

---

下一篇：[`13_context.md`](13_context.md) —— Context 组装：PromptBuilder 把 session 历史 / RAG 召回 / Skill 模板 / Project Rules 拼成最终 LLM 输入的全过程。
