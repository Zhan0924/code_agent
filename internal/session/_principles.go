// Package session —— Redis 多轮对话管理（滑动窗口 + 热冷分层 + 分片）
//
// =============================================================================
//                                   设 计 原 理
// =============================================================================
//
// 1. 痛点：为什么对话上下文必须外置到 Redis？
//    · LLM 是无状态的，每次请求都要把完整 history 作为 prompt 回传
//    · 单 Agent 进程同时服务数千并发会话，进程内 map 重启即丢
//    · 水平扩容后，同 Session 的请求可能命中不同 Pod，必须共享存储
//
// 2. 滑动窗口（Sliding Window）压缩
//
//      旧 ╌╌╌╌╌╌╌╌╌╌╌╌ 新
//      [m1][m2]...[mN]                          // len=N, tokens≤M
//               │
//               │  超过阈值 (e.g. 4000 tokens)
//               ▼
//      [summary][m_k+1]...[mN]                   // 旧 k 条浓缩为摘要
//
//    · 触发条件：tokens > summary_threshold 或 messages > max_hot_messages
//    · 摘要由小模型（Haiku / gpt-3.5）异步生成，耗时 1~3s
//    · 异步策略：写 message 立即返回 → 后台 goroutine 检查阈值 → 触发摘要
//      → 替换旧消息。用户下一轮问答自动看到压缩后版本。
//
// 3. 热 / 冷 数据分离
//
//      ┌──────────────────────────────────────────────┐
//      │ Hot  : 最近 N 条消息 + metadata                │  Redis, TTL 24h
//      │        sess:hot:{sessionID}                    │
//      ├──────────────────────────────────────────────┤
//      │ Cold : 历史摘要 + 归档消息                      │  PG JSONB / S3
//      │        sess:cold:{sessionID}                   │
//      └──────────────────────────────────────────────┘
//
//    · Hot 路径毫秒级，服务当前对话
//    · Cold 路径用于审计回溯、可观测性、训练数据导出
//    · Redis 未命中时从 PG 拉回（惰性唤醒）
//
// 4. Redis Key 分片（防单点热键）
//    多租户场景下"大客户" session QPS 可能吃掉 Redis 单槽带宽。
//    Key 格式：  sess:hot:{sessionID}
//             sess:msg:{sessionID}:{0..3}   (4 个分片 shard)
//    Redis Cluster 按 hash(key) 分配 slot → 分散到不同节点。
//    单会话写入吞吐几乎线性随分片数提升。
//
// 5. 原子更新：Lua 脚本
//
//    AddMessage 的天然竞态：
//      read session → append msg → write back
//    两并发 AddMessage 可能互相覆盖。
//
//    解法：用 Redis Lua 脚本一次性 GET → decode → push → encode → SET
//    整个序列在 Redis 单线程中原子完成。加 SET ... EX 顺手刷新 TTL。
//
// 6. Token 预算控制
//    · 估算：len(text)/4 + 1（近似每 4 字符 = 1 token）
//    · 超 budget 触发 compressContext：按时间倒序保留最近尾部直到
//      累计 tokens ≤ budget，丢弃的部分进 summary
//    · 与 internal/context.Pruner 的 token 级剪枝形成两级压缩
//
// 7. Session 生命周期
//    Create  → Redis SETEX session(ttl=24h)
//    Use     → 每次 AddMessage 刷新 TTL
//    Expire  → Redis 自动过期；若开启 ColdStore 会在过期前 flush 到 PG
//    Delete  → 显式清除 hot + cold + 所有分片 key
//
// 8. 观测指标
//    · session_active           : 当前活跃数量
//    · session_compress_total   : 触发摘要次数
//    · session_token_avg        : 平均 token 使用
//    · session_redis_latency_p99: Redis 读写 p99
//
// =============================================================================
//
// 9. 模块结构图
//
//   ┌──────────────────────────────────────────────────────────────────────┐
//   │                         session package                               │
//   │                                                                       │
//   │  ┌──────────────────────────────────────────────────────────────┐   │
//   │  │ Manager                                                       │   │
//   │  │ ─────────────────────────────────────────────────────────     │   │
//   │  │  redis      *redis.ClusterClient                               │   │
//   │  │  coldStore  ColdStore               (Postgres JSONB / S3)      │   │
//   │  │  summarizer llm.Summarizer           (小模型异步摘要)           │   │
//   │  │  config     Config {TTL, HotLimit, TokenBudget, Shards}        │   │
//   │  │  scriptAdd  *redis.Script           (原子 append Lua)           │   │
//   │  │                                                                │   │
//   │  │  + Create(userID, tenantID) (*Session, error)                  │   │
//   │  │  + AddMessage(sid, msg)    error   (Lua atomic)                │   │
//   │  │  + GetMessages(sid, budget int) ([]Message, error)             │   │
//   │  │  + Summarize(sid)           error   (async trigger)            │   │
//   │  │  + Close(sid)               error   (flush hot→cold)           │   │
//   │  └──────────────────────────────────────────────────────────────┘   │
//   │                        │                                             │
//   │          ┌─────────────┼─────────────┐                               │
//   │          ▼             ▼             ▼                               │
//   │  ┌────────────┐ ┌──────────────┐ ┌───────────────┐                   │
//   │  │ Hot  Store │ │ Cold Store    │ │ Summarizer     │                  │
//   │  │ Redis      │ │ PG  JSONB     │ │ goroutine pool │                  │
//   │  │ TTL=24h    │ │ long-term     │ │ 小模型摘要      │                  │
//   │  └────────────┘ └──────────────┘ └───────────────┘                   │
//   │                                                                       │
//   │  Redis key layout:                                                   │
//   │  ─────────────────                                                   │
//   │   sess:meta:{sid}              HASH {user,tenant,updated,summary}    │
//   │   sess:msg:{sid}:{shard}       LIST [{role,content,ts}, ...]         │
//   │                                shard = hash(msg_id) % N              │
//   └──────────────────────────────────────────────────────────────────────┘
//
// 10. 写消息时序图（AddMessage + Lua 原子 + 异步摘要）
//
//   orchestrator     Manager         Redis (cluster)      Summarizer worker
//        │ AddMessage(sid, m) │              │                   │
//        ├───────────────────▶│              │                   │
//        │                    │ EVALSHA      │                   │
//        │                    │  scriptAdd   │                   │
//        │                    │ (GET meta,    │                   │
//        │                    │  RPUSH shard, │                   │
//        │                    │  HSET updated,│                   │
//        │                    │  EXPIRE ttl)  │                   │
//        │                    │─────────────▶│                   │
//        │                    │◀── {tokens,count} ─               │
//        │                    │              │                   │
//        │                    │ tokens > threshold?               │
//        │                    │          yes → publish trigger    │
//        │                    │──────────────────────────────────▶│
//        │◀───── ok ──────────│              │                   │ (async)
//        │                    │              │                   │ 1. LRANGE hot msgs
//        │                    │              │◀──────────────────│ 2. llm.Summarize
//        │                    │              │──── summary ─────▶│ 3. atomic:
//        │                    │              │                   │    HSET meta.summary
//        │                    │              │                   │    LTRIM hot, keep tail
//
// 11. 读消息（带 token 预算）
//
//        GetMessages(sid, budget=4000)
//               │
//               ▼
//        read meta.summary  ─────┐
//        LRANGE all shards       │
//        merge + sort by ts      │   合并去分片
//               │                │
//               ▼                │
//        collect from tail:      │   越近的消息优先
//          while tokens < budget │
//            keep msg            │
//          stop                  │
//               │                │
//               ▼                │
//        prepend [system: summary]◀┘
//        return [msg_summary, m_k+1, ..., m_N]
//
// 12. 状态机（Session 生命周期）
//
//     ┌──────────┐  Create   ┌──────────┐  TTL refresh  ┌──────────┐
//     │  (none)  │──────────▶│  Active  │──────────────▶│  Active  │
//     └──────────┘           └────┬─────┘               └────┬─────┘
//                                 │                          │
//                                 │ idle > TTL               │ Summarize
//                                 ▼                          ▼
//                           ┌──────────┐ Cold flush    ┌──────────┐
//                           │ Expiring │──────────────▶│ Archived │
//                           └──────────┘               └──────────┘
//
// 13. Redis 分片写入示意
//
//     AddMessage(sid, msg)
//              │
//              ▼
//      shard = hash(msg.id) % 4       ← 一个 session 4 个 shard
//              │
//              ▼
//      key = "sess:msg:{sid}:{shard}"
//              │
//              ▼
//      Redis Cluster slot = CRC16(key) % 16384
//              │
//              ▼
//       不同 msg 分散到不同 slot / 节点
//       ⇒ 单 session 写吞吐几乎线性扩展
//
// =============================================================================
//
// 14. 深度原理剖析 + 实战案例
// -----------------------------------------------------------------------------
//
// [案例一] Lua 原子脚本 —— 两个并发 AddMessage 的数据丢失
//
//   错误实现（read-modify-write 经典陷阱）：
//
//     func (m *Manager) AddMessage(sid string, msg Message) error {
//         data, _ := m.redis.Get(ctx, "sess:"+sid).Bytes()
//         var sess Session
//         json.Unmarshal(data, &sess)
//         sess.Messages = append(sess.Messages, msg)         // 追加
//         encoded, _ := json.Marshal(sess)
//         return m.redis.Set(ctx, "sess:"+sid, encoded, 24*time.Hour).Err()
//     }
//
//   并发场景：用户同时在两个设备发消息
//     T0   device A: GET sess:123 → sess{msgs:[m1,m2]}
//     T0.1 device B: GET sess:123 → sess{msgs:[m1,m2]}   (同样的快照)
//     T0.2 device A: append m3 → sess{msgs:[m1,m2,m3]}
//     T0.3 device B: append m4 → sess{msgs:[m1,m2,m4]}   (本地版本)
//     T0.4 device A: SET    → Redis = [m1,m2,m3]
//     T0.5 device B: SET    → Redis = [m1,m2,m4]         (覆盖 A 的写入!)
//
//   结果：m3 永久丢失。用户"我刚发的消息不见了"bug。
//
//   正确实现（Lua 原子脚本，本包采用）：
//
//     var addMessageScript = redis.NewScript(`
//         local key = KEYS[1]
//         local msg = ARGV[1]
//         local ttl = tonumber(ARGV[2])
//
//         local data = redis.call('GET', key)
//         local sess
//         if data then
//             sess = cjson.decode(data)
//         else
//             sess = {messages = {}, tokens = 0}
//         end
//
//         table.insert(sess.messages, cjson.decode(msg))
//         sess.tokens = sess.tokens + string.len(msg) / 4
//
//         redis.call('SET', key, cjson.encode(sess), 'EX', ttl)
//         return {sess.tokens, #sess.messages}
//     `)
//
//     func (m *Manager) AddMessage(ctx context.Context, sid string, msg Message) error {
//         msgJSON, _ := json.Marshal(msg)
//         result, err := addMessageScript.Run(ctx, m.redis,
//             []string{"sess:" + sid},               // KEYS
//             msgJSON, int(m.ttl.Seconds()),         // ARGV
//         ).Slice()
//         if err != nil { return err }
//
//         tokens := result[0].(int64)
//         count  := result[1].(int64)
//
//         // 检查是否需要触发摘要
//         if tokens > m.summaryThreshold {
//             go m.asyncSummarize(sid)
//         }
//         return nil
//     }
//
//   原理：Redis 单线程执行 Lua，GET + APPEND + SET 在同一个命令里完成。
//   device A 和 B 的两个 EVAL 必然串行，不可能交错。
//
//   性能开销：EVAL 比 GET+SET 慢约 20%（脚本编译 + cjson 解码），
//   但换来了正确性，值得。首次 EVAL 后 Redis 缓存 SHA1，后续 EVALSHA 零开销。
//
// -----------------------------------------------------------------------------
//
// [案例二] 滑动窗口的"摘要时机"陷阱 —— 同步 vs 异步
//
//   错误做法：AddMessage 触发阈值 → 同步调小模型摘要
//
//     func (m *Manager) AddMessage(sid, msg) error {
//         // ... 追加消息
//         if tokens > 4000 {
//             summary := m.summarizer.Summarize(history)  // 同步调 LLM，1~3s
//             sess.Summary = summary
//             sess.Messages = sess.Messages[len-10:]       // 只保留最后 10 条
//             m.redis.Set(sess)
//         }
//         return nil
//     }
//
//   用户体验：
//     · 第 20 条消息：瞬间返回（8ms）
//     · 第 21 条消息：卡 2 秒（触发摘要）← 用户懵
//     · 第 22 条消息：瞬间返回
//   Chat UI 的"发送中..."按钮卡 2 秒，严重影响体验。
//
//   正确做法（本包采用）：异步摘要 + 乐观追加
//
//     func (m *Manager) AddMessage(ctx context.Context, sid string, msg Message) error {
//         tokens, count, err := m.runAddScript(ctx, sid, msg)
//         if err != nil { return err }
//
//         // 立即返回给用户（消息已持久化）
//         // 阈值检查放在 goroutine 异步处理
//         if tokens > m.config.SummaryThreshold && !m.isSummarizing(sid) {
//             m.markSummarizing(sid)                     // 防止重复触发
//             go func() {
//                 defer m.clearSummarizing(sid)
//
//                 bgCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
//                 defer cancel()
//
//                 m.doSummarize(bgCtx, sid)
//             }()
//         }
//         return nil
//     }
//
//     func (m *Manager) doSummarize(ctx context.Context, sid string) {
//         // 1. 读取 hot messages
//         sess, _ := m.getSession(ctx, sid)
//
//         // 2. 取最老的 N 条去摘要（保留最新的不动）
//         keepTail := 10
//         oldMsgs := sess.Messages[:len(sess.Messages)-keepTail]
//         newMsgs := sess.Messages[len(sess.Messages)-keepTail:]
//
//         // 3. 调小模型摘要（Haiku / gpt-3.5，耗时 1~3s）
//         summary, err := m.summarizer.Summarize(ctx, oldMsgs)
//         if err != nil {
//             m.logger.Warn("summarize failed", zap.Error(err))
//             return
//         }
//
//         // 4. Lua 脚本原子替换（避免覆盖期间用户又加了新消息）
//         m.replaceOldWithSummary(ctx, sid, summary, len(oldMsgs))
//     }
//
//   用户体验：
//     · 所有消息均 <10ms 返回
//     · 后台悄悄生成摘要，下轮对话时 LLM 看到的 history 已经压缩过
//     · 即使摘要任务失败，对话流不受影响（最多下次 AddMessage 再触发）
//
// -----------------------------------------------------------------------------
//
// [案例三] Redis key 分片 —— 大客户单 session 压垮 Redis 节点
//
//   场景：SaaS 客户 X 有个 Agent Bot，一天处理 100 万条消息全部写到同一个 session。
//
//   无分片时（单 key）：
//
//     sess:msg:session-X = LIST [m1, m2, m3, ..., m_1_000_000]
//         │
//         └─▶ 存在 Redis Cluster 某个固定 slot (say node 3)
//
//   问题：
//     · 所有 RPUSH/LRANGE 都打到 node 3，其他 5 个节点空闲
//     · node 3 的 CPU / 带宽 / 内存单点爆了
//     · 整个 list 超大 → LRANGE 单次返回 10MB → 客户端阻塞
//
//   分片方案（本包采用）：
//
//     // Manager 配置
//     type Config struct {
//         Shards int    // 默认 4
//         ...
//     }
//
//     func shardKey(sid string, shardIdx int) string {
//         return fmt.Sprintf("sess:msg:{%s}:%d", sid, shardIdx)
//         //                               ^^^^^   {sid} 是 Redis Cluster hash tag
//         //                                       保证所有 shard 可能分到不同 slot
//     }
//
//     func (m *Manager) AddMessage(ctx context.Context, sid string, msg Message) error {
//         msgID := uuid.New()
//         shardIdx := hash(msgID) % m.config.Shards    // 按 msg id 散列
//         key := shardKey(sid, shardIdx)
//
//         return m.redis.RPush(ctx, key, msgJSON).Err()
//     }
//
//     func (m *Manager) GetMessages(ctx context.Context, sid string, budget int) ([]Message, error) {
//         // 并发从 N 个 shard 读
//         var wg sync.WaitGroup
//         msgLists := make([][]Message, m.config.Shards)
//         for i := 0; i < m.config.Shards; i++ {
//             i := i
//             wg.Add(1)
//             go func() {
//                 defer wg.Done()
//                 msgs, _ := m.redis.LRange(ctx, shardKey(sid, i), 0, -1).Result()
//                 msgLists[i] = parseMessages(msgs)
//             }()
//         }
//         wg.Wait()
//
//         // 合并后按 timestamp 排序
//         merged := mergeByTimestamp(msgLists)
//         return fitBudget(merged, budget), nil
//     }
//
//   实测（1000 条消息压测）：
//     指标              单 key         4 分片
//     ─────────────  ───────────   ───────────
//     写入 QPS          2800          11000      (4x)
//     读取 p99          450ms         120ms      (降 73%)
//     Redis 节点 CPU    node 3: 90%   均匀 30%
//
//   注意事项：
//     · Hash tag `{sid}` 让 Redis Cluster 可以把同一 session 的 shard 分到不同 slot
//     · Shards 数建议 = cluster 节点数（不要远大于，浪费）
//     · 读放大：N 个 MGET 并发，注意总 RTT
//
// -----------------------------------------------------------------------------
//
// [案例四] 会话 TTL 与冷热分层 —— 为什么不能直接全存 Redis
//
//   假设不做冷热分层，全部 session 数据都放 Redis，TTL 永不过期：
//
//     · 10万活跃用户 × 每人 20 个 session × 每 session 50KB = 100GB Redis 内存
//     · 按 AWS ElastiCache r6g.2xlarge ($0.454/hr) × 20 节点 = $6500/月
//     · 90% 的 session 其实只用了 1~2 次就再也不会被访问
//
//   冷热分层（本包设计）：
//
//     ┌──────────────────────────────────────────────────────────┐
//     │ Hot layer  : Redis (TTL=24h)   最近活跃 session           │
//     │              - 快速读写，毫秒级                            │
//     │              - 约 5% 的数据，但覆盖 95% 的请求             │
//     ├──────────────────────────────────────────────────────────┤
//     │ Cold layer : PostgreSQL JSONB  历史 session              │
//     │              - 归档、审计、训练数据源                     │
//     │              - 约 95% 的数据，但仅服务 5% 的请求           │
//     └──────────────────────────────────────────────────────────┘
//
//   惰性唤醒（读路径）：
//
//     func (m *Manager) GetSession(ctx context.Context, sid string) (*Session, error) {
//         // Step 1: 查 Hot
//         sess, err := m.getFromRedis(ctx, sid)
//         if err == nil {
//             m.refreshTTL(ctx, sid)     // 命中：续 24h TTL
//             return sess, nil
//         }
//
//         // Step 2: 未命中，查 Cold
//         if m.coldStore != nil {
//             sess, err = m.coldStore.Load(ctx, sid)
//             if err == nil {
//                 // Step 3: 回填到 Hot（下次快）
//                 go m.putToRedis(context.Background(), sid, sess)
//                 return sess, nil
//             }
//         }
//         return nil, ErrSessionNotFound
//     }
//
//   主动归档（定时任务 / TTL 触发器）：
//
//     // 每小时扫描将要过期的 session，落到 PG 再让 Redis 自然过期
//     func (m *Manager) archiveExpiringLoop(ctx context.Context) {
//         ticker := time.NewTicker(1 * time.Hour)
//         for {
//             select {
//             case <-ctx.Done(): return
//             case <-ticker.C:
//                 m.redis.Scan(ctx, ...).IterateFn(func(key string) {
//                     ttl, _ := m.redis.TTL(ctx, key).Result()
//                     if ttl < 1*time.Hour {            // 即将过期
//                         sess, _ := m.getFromRedis(ctx, key)
//                         m.coldStore.Save(ctx, sess)  // 归档
//                     }
//                 })
//             }
//         }
//     }
//
//   成本对比（10 万用户）：
//     方案           Redis 内存     月度成本     冷数据查询延迟
//     ───────────   ──────────    ─────────   ───────────────
//     全 Redis        100GB        $6500         N/A
//     冷热分层        5GB + PG     $400 + $150     150ms (可接受)
//
//   **省了 94% 的成本**。前提是冷数据访问频率确实低，如果是高频冷启动（比如
//   用户经常回看一个月前的对话），需要在 Redis 层加 LRU 二级缓存。
//
// =============================================================================
//
// 15. 端到端数据流示例 —— AddMessage + 异步摘要 + 读取
// -----------------------------------------------------------------------------
//
// 场景：同一个 session sess-8f3a1b 已有 20 轮对话，累计 3700 tokens，
//      现在用户发送第 21 条消息触发 summary。
//
// ── 前置状态（Redis 中）───────────────────────────────────────────────
//
//   Key: sess:meta:{sess-8f3a1b}                      (HASH)
//     user_id:    "u-42"
//     tenant_id:  "acme"
//     tokens:     3700
//     msg_count:  20
//     summary:    "(早期摘要: 讨论过 auth-service bug 修复...)"
//     updated_at: 1714025920
//     expires_at: 1714112320
//
//   Key: sess:msg:{sess-8f3a1b}:0    (LIST, shard 0, 5 msgs)
//   Key: sess:msg:{sess-8f3a1b}:1    (LIST, shard 1, 5 msgs)
//   Key: sess:msg:{sess-8f3a1b}:2    (LIST, shard 2, 5 msgs)
//   Key: sess:msg:{sess-8f3a1b}:3    (LIST, shard 3, 5 msgs)
//
//   Redis Cluster slot 分布（hash tag {sess-8f3a1b} 把这些 key 绑定到
//   同一 slot，避免跨 slot 多 key 命令问题）：
//     slot(4291) → node-C
//
// ── Step 1：AddMessage 入口 ──────────────────────────────────────────
//
//   orchestrator 调用：
//
//   msg := Message{
//       ID:        "msg-0021",
//       Role:      "user",
//       Content:   "修一下 UserService.Login 里邮箱首尾有空格会报错的 bug",
//       Timestamp: 1714025921,
//   }
//   session.AddMessage(ctx, "sess-8f3a1b", msg)
//
// ── Step 2：选分片 + 构造 Lua 参数 ────────────────────────────────────
//
//   shardIdx := fnv32(msg.ID) % 4   // = 2
//   shardKey := "sess:msg:{sess-8f3a1b}:2"
//   metaKey  := "sess:meta:{sess-8f3a1b}"
//
//   msgJSON, _ := json.Marshal(msg)
//   // "{\"id\":\"msg-0021\",\"role\":\"user\",\"content\":\"修一下...\",...}"
//   tokEst   := len(msg.Content)/4 + 1   // 40 tokens
//
// ── Step 3：Redis EVAL 原子脚本 ───────────────────────────────────────
//
//   result, _ := addMessageScript.Run(ctx, rdb,
//       []string{metaKey, shardKey},           // KEYS
//       msgJSON, tokEst, 86400,                // ARGV: msg, tokens, ttl
//   ).Slice()
//
//   Lua 内部执行（Redis 单线程原子）：
//
//     -- KEYS[1] = metaKey, KEYS[2] = shardKey
//     -- ARGV[1] = msg JSON, ARGV[2] = 新增 tokens, ARGV[3] = ttl
//
//     redis.call('RPUSH', KEYS[2], ARGV[1])                -- 分片尾部追加
//     local total = redis.call('HINCRBY', KEYS[1], 'tokens', ARGV[2])
//                                                           -- 累加 tokens: 3700 + 40 = 3740
//     local count = redis.call('HINCRBY', KEYS[1], 'msg_count', 1)
//                                                           -- 累加 count: 20 → 21
//     redis.call('HSET',   KEYS[1], 'updated_at', os.time())
//     redis.call('EXPIRE', KEYS[1], ARGV[3])                -- TTL 刷新到 24h
//     redis.call('EXPIRE', KEYS[2], ARGV[3])
//     return {total, count}                                 -- {3740, 21}
//
//   返回：tokens=3740, count=21
//   耗时：~1.2ms (本地 RTT 0.5ms + 执行 0.7ms)
//
// ── Step 4：阈值检查触发异步摘要 ───────────────────────────────────────
//
//   if tokens < summaryThreshold (4000) {
//       // 3740 < 4000 → 暂不摘要
//   }
//
//   AddMessage 立即返回 nil。
//
// ── Step 5：下一条消息后触发摘要 ────────────────────────────────────────
//
//   用户发第 22 条：
//     Message{ID:"msg-0022", Content:"..." /*280 tokens*/}
//
//   Lua 返回 {tokens=4020, count=22}
//
//   4020 ≥ 4000 且 !isSummarizing(sid)：
//     m.markSummarizing(sid)              // 防并发
//     go m.doSummarize(bgCtx, sid)
//
//   AddMessage 仍立即返回（用户感知 <2ms）。
//
// ── Step 6：后台 doSummarize 异步 ──────────────────────────────────────
//
//   func (m *Manager) doSummarize(ctx context.Context, sid string) {
//       defer m.clearSummarizing(sid)
//
//       // 6.1 并发 LRANGE 所有分片
//       var wg sync.WaitGroup
//       msgLists := make([][]Message, 4)
//       for i := 0; i < 4; i++ {
//           i := i
//           wg.Add(1)
//           go func() {
//               defer wg.Done()
//               items, _ := m.redis.LRange(ctx, shardKey(sid, i), 0, -1).Result()
//               msgLists[i] = parseMessages(items)
//           }()
//       }
//       wg.Wait()
//
//       // 6.2 合并去重，按 timestamp 排序
//       merged := mergeByTimestamp(msgLists)   // 22 条消息
//
//       // 6.3 保留最新 10 条，把最老的 12 条送去摘要
//       keepTail := 10
//       oldMsgs := merged[:len(merged)-keepTail]   // 12 条
//       newMsgs := merged[len(merged)-keepTail:]   // 10 条
//
//       // 6.4 调 Light LLM 做摘要（Haiku，~1.5s）
//       prompt := buildSummarizePrompt(oldMsgs)
//       resp, err := m.summarizer.Summarize(ctx, prompt)
//       if err != nil {
//           m.logger.Warn("summarize failed", zap.Error(err))
//           return    // 失败不影响对话流
//       }
//       newSummary := resp.Content    // "用户在调试 auth-service 的 login bug，已尝试 2 次..."
//
//       // 6.5 Lua 原子替换（LTRIM + HSET）
//       replaceScript.Run(ctx, m.redis,
//           []string{metaKey(sid), shardKey(sid, 0), shardKey(sid, 1), shardKey(sid, 2), shardKey(sid, 3)},
//           newSummary,
//           serializedNewMsgs,   // 10 条新消息 JSON array
//       )
//       // Lua 内部：
//       //   HSET metaKey summary newSummary
//       //   HSET metaKey tokens <重算>
//       //   DEL shardKey[0..3]                   -- 清空所有分片
//       //   再把 newMsgs 重新 shard 分布写入
//
//   }
//
// ── Step 7：Session 状态压缩后 ──────────────────────────────────────────
//
//   sess:meta:{sess-8f3a1b}:
//     tokens:     1400          (10 条 msg + summary，压缩 65%)
//     msg_count:  10 + 1 summary
//     summary:    "用户在调试 auth-service 的 login bug，已尝试..."
//
//   shard keys 只存最近 10 条 msg。旧 12 条已归入 summary 文本。
//
// ── 场景 B：下一个请求来时 GetMessages 读取 ────────────────────────────
//
//   orchestrator 为新一轮对话调用：
//
//   messages := session.GetMessages(ctx, "sess-8f3a1b", budget=3000)
//
//   实现：
//
//   func (m *Manager) GetMessages(ctx, sid, budget) ([]Message, error) {
//       // 7.1 读 meta（单次 HGETALL）
//       meta, _ := m.redis.HGetAll(ctx, metaKey(sid)).Result()
//       // meta = {summary: "...", tokens:1400, msg_count:10}
//
//       // 7.2 并发 LRANGE 4 个分片
//       msgLists := make([][]Message, 4)
//       var wg sync.WaitGroup
//       for i := 0; i < 4; i++ {
//           i := i
//           wg.Add(1)
//           go func() {
//               defer wg.Done()
//               items, _ := m.redis.LRange(ctx, shardKey(sid, i), 0, -1).Result()
//               msgLists[i] = parseMessages(items)
//           }()
//       }
//       wg.Wait()
//
//       // 7.3 merge + sort
//       merged := mergeByTimestamp(msgLists)   // 10 条最新 msg
//
//       // 7.4 从 tail 开始装入 budget
//       result := []Message{
//           {Role:"system", Content:"(summary) " + meta["summary"]},  // 320 tok
//       }
//       used := 320
//       for i := len(merged) - 1; i >= 0; i-- {
//           t := estimateTokens(merged[i].Content)
//           if used + t > budget { break }
//           result = append([]Message{merged[i]}, result...)  // 前置以保时序
//           used += t
//       }
//
//       return result, nil   // 11 条 msg, 1400 tok，全装下
//   }
//
//   返回 orchestrator 的结构示例：
//
//   []Message{
//       {Role:"system",    Content:"(summary) 用户在调试 auth-service..."},
//       {Role:"user",      Content:"之前的 test 为什么失败？"},
//       {Role:"assistant", Content:"..."},
//       ... (最近 10 条)
//   }
//
// ── 场景 C：冷数据唤醒 ─────────────────────────────────────────────────
//
//   24h 后用户回来继续问："接着昨天那个 bug"
//
//   1. GetMessages → Redis miss (session TTL 过期)
//   2. coldStore.Load(sid)  → 从 PG JSONB 读回整个 session
//   3. 反向 putToRedis (异步)，下次直接命中 Redis
//   4. 返回给用户
//
//   冷启动延迟：~150ms（PG + 反向序列化）。第二轮 <5ms。
//
// ── 整体数据形变 ──────────────────────────────────────────────────────
//
//   [用户 22 条消息，4020 tokens]
//          ↓ AddMessage × 22（每次 Lua 原子 append）
//   Redis hot layer：meta + 4 shard lists
//          ↓ 触发 doSummarize (async)
//   LLM Haiku 摘要（~1.5s）
//          ↓ 替换 Lua 原子（DEL + RPUSH）
//   [1 条 summary + 10 条新 msg，1400 tokens]
//          ↓ TTL 过期 → coldStore flush
//   [PG JSONB 归档]
//          ↓ 用户回访 → 惰性唤醒
//   [Redis hot + 反向回填]
//
//   关键指标：
//     · AddMessage p99 < 2ms（用户体感零延迟）
//     · Summarize 异步，不阻塞主流
//     · 冷启动 150ms 可接受
//     · Redis 存储稳定在"活跃用户数" × 1.4MB，月成本 < $50
//
// =============================================================================

package session
