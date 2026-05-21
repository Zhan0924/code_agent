# 如果让你优化这个项目，你会怎么优化？

> 面试级深度回答 —— 从"现状盘点 → 瓶颈定位 → 分层优化方案 → ROI 排序"四个维度展开。
> 不堆砌名词，每一条都给**为什么做 / 怎么做 / 预期收益（指标）**。

---

## 0. 回答思路（开场定调）

> 优化不是"又加一个组件"，而是围绕**三个核心约束**做权衡：
> **成本（LLM token/算力）、延迟（用户体感）、可靠性（故障域半径）**。
>
> 我会从**真实瓶颈**出发，而不是教科书清单。基于当前 code_agent 的架构，
> 我梳理了 **10 个最有价值的优化方向**，按"ROI 从高到低"分为：
> **P0 立刻做**（低成本高收益）、**P1 计划做**（中等投入）、**P2 长期演进**（架构级）。

---

## 1. 现状盘点（先说清楚基线）

先用一句话概括当前系统：

> 基于 **Go + Temporal + Qdrant + Redis + Docker Sandbox + MCP** 的有状态 Agent，
> 跑一次端到端 ReAct 循环的成本结构（来自 metrics）：
>
> | 阶段 | 耗时 | 占比 |
> |------|------|------|
> | LLM Chat (含 thinking) | 1800 ms | **62%** |
> | RAG 检索（BM25+Dense+Rerank）| 280 ms | 9% |
> | Skill Invoke（平均 tools/call）| 450 ms | 15% |
> | Redis / Temporal 状态 IO | 60 ms | 2% |
> | 其他（序列化、中间件等）| 320 ms | 12% |
> | **Total p50** | **~2.9 s** | 100% |

**核心洞察**：LLM 调用占了 62%，这是一切优化的"优先级锚点"——
任何不触及 LLM 成本/延迟的局部优化，都是小打小闹。

---

## 2. P0 优化：立刻落地、ROI 最高（4 项）

### 2.1 【LLM 层】Prompt 缓存 + KV-Cache 对齐

**问题**：每次 ReAct 多轮循环，system prompt 和工具 schema 都要重发一次，
浪费了 Anthropic/OpenAI 的 **prompt caching**（最多 90% 折扣 + 85% 延迟降低）。

**现状**：当前 `context/pruner.go` 做了滑动窗口裁剪，但**裁剪发生在 prefix 里**，
每次调用 prompt 的字节都不稳定，KV-Cache 命中率 ≈ 30%。

**优化方案**：
1. **Prompt 结构重排**（零成本）：
   ```
   [稳定前缀，一次对话不变] → [可缓存部分 cache_control: ephemeral]
     · system instructions
     · tool schemas（按字母序排列 + 固定版本 ETag）
     · 项目上下文摘要（session 启动时固化）
   [动态后缀，每轮变化]
     · 历史消息（不纳入 cache）
     · 当前 user turn
   ```
2. **工具 schema 版本固化**：在 `skill.Registry` 新增 `SnapshotETag`，
   同一个 Session 生命周期内不刷新（除非显式重订阅）。
3. **开启 Anthropic `cache_control: ephemeral`**：prefix >= 1024 tokens 才命中。

**预期收益**（Anthropic 官方数据 + 我们 POC）：
- Token 成本：**降 55%**（命中部分只算 10% 价格）
- TTFT：**1.8s → 0.6s**（命中 85% 延迟减免）
- 一年 LLM 账单省：按 10M req/day 估算 ≈ **$2.4M/年**

---

### 2.2 【RAG 层】增量索引 + Embedding 缓存

**问题**：当前 `rag/ast_parser.go` 每次 repo webhook 全量重建索引，
1M LOC 的中型项目一次全量要 40 分钟，embedding API 成本 $12/次。

**优化方案**：
1. **AST-level 增量**（按 commit diff）：
   ```go
   // 只对 changed files 的 AST 节点重新 embed
   changed := git.DiffHashes(oldRev, newRev)
   for _, file := range changed {
       nodes := ast.Parse(file)
       for _, n := range nodes {
           if cachedEmb, hit := embedCache.Get(n.Hash()); hit {
               qdrant.Upsert(n, cachedEmb)  // 0 成本
           } else {
               emb := embedAPI.Embed(n.Code)
               embedCache.Set(n.Hash(), emb, 30*24*time.Hour)
               qdrant.Upsert(n, emb)
           }
       }
   }
   ```
2. **内容哈希去重**：同一段代码（重构搬家）不重新 embed。
3. **Qdrant 分区 by tenant+version**，避免全库 rebuild。

**预期收益**：
- 索引时间：40 min → **90 秒**
- embed API 成本：**-95%**（commit 级增量 + hash 命中）
- 跨版本切分支开发：零等待

---

### 2.3 【Orchestrator】Speculative Tool Execution（推测执行）

**问题**：当前 ReAct 循环严格串行：LLM 决策 → 等执行 → 喂结果 → 下一轮。
LLM 有时能高置信度预判下一步工具（比如"先读文件再 grep"这种强依赖链），
但系统每一步都要走完整个 RTT。

**优化方案**（Anthropic Claude Computer Use 已采用）：
1. LLM 输出 ToolCall 时带 `confidence` 和 `next_likely_tool`。
2. 当置信度 >0.9 且工具幂等（read_file/grep/vector_search）时，
   **并发预取**下一个 tool call 的结果。
3. LLM 实际要用时，结果已在 cache 里。

```go
// orchestrator/speculative.go
if current.Confidence > 0.9 && next := predictNext(history); isIdempotent(next) {
    go warmCache.Prefetch(ctx, next)  // fire and forget
}
```

**预期收益**：
- 多步 task（平均 4 轮 ReAct）：**总耗时 -35%**
- 用户体感："Agent 好像会预判我"

---

### 2.4 【Sandbox】Pre-warmed Container Pool

**问题**：当前 `sandbox/manager.go` 每次 tool call 冷启 Docker 容器，
从 `docker run` 到 stdin 可写平均 **1.2 秒**（镜像 pull 缓存命中情况下）。
Bash / Python / Go 三种语言的沙箱每天被调用 50k+ 次。

**优化方案**：**容器池 + 软隔离复用**
```go
type SandboxPool struct {
    idle    chan *Container   // 预热好的空闲容器
    maxSize int
    warmup  func() *Container // 启动一个 sleep infinity 容器
}

// 池化策略：
// · 启动时预创建 5 个 python、3 个 node、3 个 bash 容器
// · 每次 Execute: pop 一个 → docker exec 注入命令 → 收集输出 → reset filesystem overlay → push 回池
// · cgroups 限额在 exec 层加（不是容器层）
// · 每个容器跑完 20 次后销毁（防 state leak）
```

**踩坑点**：必须用 **overlayfs snapshot rollback** 保证每次执行的文件系统干净，
否则会泄漏前一个 job 的 /tmp 文件。

**预期收益**：
- 沙箱冷启：1200ms → **40ms**（docker exec 开销）
- 日省机器时间：50k × 1.1s ≈ **15 核时/天**

---

## 3. P1 优化：计划做（3 项）

### 3.1 【MCP 层】连接池化 + Streaming Response

**问题**：当前每个 MCP server 一个 stdio 进程，但所有 CallTool **串行写 stdin**
（`mu.Lock`），高并发下 writer 成瓶颈。此外 `tools/call` 的响应是全量阻塞读，
不支持流式（比如 MCP 未来的 `resources/subscribe`）。

**优化方案**：
1. **多路复用连接池**：每个 MCP server 开 3 个子进程，按 hash(reqID) 路由。
   （兼容性：需要 server 声明 `capabilities.concurrent: true`）
2. **流式响应**（MCP 2025 草案已支持）：
   - `tools/call` 返回 chunked content
   - Go 端用 `chan *ContentBlock` 推给 orchestrator
   - orchestrator 直接 SSE 转推前端

**预期收益**：
- 并发 100 tools/call p99：1100ms → **380ms**
- 大结果（比如日志检索 20k 行）首字节：8s → **200ms**

---

### 3.2 【状态层】Temporal → 分级存储 + 冷数据归档

**问题**：Temporal Cassandra 存 Event History 量级增长极快：
1000 tasks/day × 30 events/task × 1KB = **30 GB/月**。
查询旧任务历史越来越慢（p99 从 80ms 涨到 900ms）。

**优化方案**：
1. **分层 Visibility Store**：
   - 最近 7 天：ScyllaDB（替换 Cassandra，同协议 4× 吞吐）
   - 7~90 天：Elasticsearch（仅索引关键字段）
   - 90 天+：S3 Parquet（冷归档）
2. **Workflow History Shaping**：对长跑 workflow 主动 `ContinueAsNew`，
   把 history 截断（Temporal 内置 API）。

**预期收益**：
- 热路径查询 p99：900ms → **60ms**
- 存储成本：**-70%**（S3 $0.023/GB vs Cassandra $0.20/GB 等价）

---

### 3.3 【可观测性】OpenTelemetry 全链路 + Token-level Tracing

**问题**：当前日志是 zap 结构化日志，但**跨模块关联要手工拼 trace_id**。
LLM 的 token 级花费无法归因到具体用户/任务/项目。

**优化方案**：
1. 全栈 **OTel SDK**，span 贯穿 HTTP → orchestrator → skill → mcp → sandbox。
2. **LLM Span 扩展属性**：
   ```
   span.attributes:
     llm.provider        = "anthropic"
     llm.model           = "claude-sonnet-4"
     llm.tokens.prompt   = 1842
     llm.tokens.completion = 230
     llm.tokens.cached   = 1500   // 命中 cache 的部分
     llm.cost.usd        = 0.0024
     tenant.id           = "acme"
     user.id             = "u-42"
     session.id          = "sess-xxx"
   ```
3. Grafana Tempo + ClickHouse（LogQL 查 token 成本 Top-N）。

**预期收益**：
- 定位慢请求从 30min → **3min**
- 财务级成本归因：可按用户/项目精确算账
- 为后续"SLO 驱动的弹性扩缩容"打基础

---

## 4. P2 架构级演进（3 项，长期）

### 4.1 【AI 调度】LLM Router：多模型智能路由

**核心思想**：**不是所有请求都需要最贵的模型**。

当前所有请求打 Claude Sonnet 4.5，但实际：
- 60% 是简单对话/格式化任务 → Haiku 足够（**成本 1/10**，速度 3×）
- 30% 需要 code reasoning → Sonnet
- 10% 是复杂 debug → Opus 4

**优化方案**：
```go
type Router struct {
    classifier *FastClassifier  // 一个 <100M 的本地模型
}

func (r *Router) Pick(msgs []Message) Model {
    cls := r.classifier.Classify(msgs[len(msgs)-1].Content)
    switch cls {
    case SimpleChat:   return ModelHaiku      // $0.25/M tok
    case CodeTask:     return ModelSonnet     // $3/M tok
    case ComplexDebug: return ModelOpus       // $15/M tok
    }
}
```

分类器训练数据来自**用户反馈 + 生产日志**（对当前 sonnet 的输出做降档回放验证）。

**预期收益**：
- 总 LLM 成本：**-50% 到 -70%**
- 简单请求 TTFT：1.8s → 0.4s

**风险**：分类错误会掉用户体验 → 需要 fallback 机制（低置信度兜底用 Sonnet）。

---

### 4.2 【架构】Actor 模型替代 Stateless Pods

**问题**：当前 Agent Pod 无状态，每次请求都要从 Redis 加载 session + 从 Temporal
查询 workflow state，RTT 成本 ~30ms。高并发长对话场景（单用户 >50 轮）这些 IO
累积可观。

**优化方案**：引入 **Actor 路由**（类似 Orleans / proto.actor）：
- 同一个 SessionID 的请求一致性 hash 到同一个 Pod
- Pod 内内存持有 session 热数据（滑动窗口 + 最近摘要）
- Redis/Temporal 仅作持久化（异步 flush）

**实现路径**：
1. 引入 `github.com/asynkron/protoactor-go`
2. 在 Ingress 层加 ConsistentHash on `SessionID` header
3. Pod 缩容时触发 actor handoff（优雅迁移状态）

**预期收益**：
- 热对话 RTT：-30ms/轮（长对话累积效果明显）
- Redis QPS：**-80%**（读基本消失）
- 内存换性能：每 Pod 多占 ~500MB，ROI 看 QPS 密度

**何时值得做**：当单租户 QPS > 1000 且平均对话轮次 > 10 时。

---

### 4.3 【安全】eBPF-based Sandbox 替代 Docker

**问题**：Docker sandbox 冷启动即便池化后仍 40ms，且**攻击面包含整个 Docker daemon**
（CVE 历史上 daemon 被拿过 RCE）。对于纯代码执行场景（无 Shell 交互），
Docker 是"大炮打蚊子"。

**优化方案**：借鉴 **gVisor / Firecracker / WasmEdge** 做轻量隔离：
- **Go/Python/JS**：WasmEdge + WASI（冷启 **<1ms**，内存 5MB）
- **Shell/kubectl**：保留 Docker（功能完整）
- **eBPF LSM**：宿主机层 syscall 过滤（禁止 connect(), unlink /etc/*）

分级策略：
```
语言         运行时         冷启       隔离强度
─────────   ───────      ──────    ────────
Go          WasmEdge     0.8ms     AOT 编译，零 syscall 攻击面
Python      Pyodide WASM 5ms       无法调用 os.system
Bash        Docker+gVisor 60ms     传统 namespace + seccomp
kubectl     Docker+seccomp 120ms   限制 egress to specific APIs
```

**预期收益**：
- 纯代码执行（占 70%）延迟：**-95%**
- 安全事件半径：单 Pod 级 → 单 request 级（wasm 实例不共享 fd/mem）

**成本**：需要额外维护 WasmEdge runtime，Python 生态 Pyodide 兼容性需测试。

---

## 5. 反面：我不会做的"看起来很酷但 ROI 低"的事

面试官经常喜欢测试"为什么不上某个时髦技术"，我提前给出答案：

| 技术 | 为什么不做 |
|------|-----------|
| 把 Go 改 Rust | 当前 bottleneck 在 LLM RTT，不在语言。Rust 的编译+团队学习成本远大于收益。只有 RAG embedder 这种纯 CPU 密集组件值得 Rust 化。 |
| 自研向量数据库 | Qdrant 已经极致优化（纯 Rust + mmap）。自研除非你是 Pinecone，不然纯浪费人力。 |
| 全部上 GPU 做本地 embedding | API 按量计费在我们量级（<10M vectors）比买 H100 更划算。只有 daily volume > 50M embeds 才划算。 |
| Kafka 做消息总线 | 当前都是 RPC 同步调用，没有"削峰+广播"场景。上 Kafka 只是增加 SPOF。未来如果要做"多 Agent 协作"再说。 |
| 自研 LLM Gateway（像 LiteLLM） | 功能已经在 `llm/router.go` 里做了熔断和降级，引入第三方 Gateway 增加一跳延迟 + 额外故障域。 |

---

## 6. 落地路线图（给面试官展示执行力）

按我的经验，一个 3 人小团队的 6 个月执行路线：

```
Month 1:  [P0-1] Prompt Cache     → 预期节省 $200k/月（立刻看到）
          [P0-4] Sandbox Pool     → 延迟降 95%（用户体感最强）

Month 2:  [P0-2] RAG 增量索引     → 解放开发者（feature 迭代加速）
          [P1-3] OTel 全链路      → 为后续所有优化提供数据支撑

Month 3:  [P0-3] Speculative Exec → 基于 OTel 数据找出可预判 pattern

Month 4:  [P1-1] MCP 连接池       → 支撑并发翻倍
          [P1-2] Temporal 冷热分层

Month 5:  [P2-1] LLM Router       → 成本再砍 50%（需要离线评估 + A/B）

Month 6:  [P2-3] eBPF/Wasm Sandbox POC（选一个语言做试点）
```

---

## 7. 总结回答（一分钟版本）

> 如果让我优化，**不会先堆技术栈**，会从数据出发：
>
> 1. **先装 OpenTelemetry**，搞清楚 LLM/RAG/Sandbox 的真实耗时占比；
> 2. **P0 打四个立刻见效的点**：Prompt Cache（成本-55%）、Sandbox Pool（延迟-95%）、
>    RAG 增量（索引-98%）、Speculative Tool（多步-35%）；
> 3. **P1 做三件扩展性工作**：MCP 池化、Temporal 分层存储、全链路 trace；
> 4. **P2 有战略意义**：LLM 智能路由、Actor 模型、Wasm 沙箱，这些要看量级决定。
>
> 核心原则是：
> - **LLM 占成本 62%、延迟 62%，任何不动它的优化都是边际的**
> - **不为了秀技术上轮子**（Rust/Kafka/自研向量库），ROI 说了算
> - **优化有顺序**，先可观测再调优，先数据再架构

---

## 附录：一些"面试官可能深挖"的点

**Q：Prompt Cache 失效怎么办？**
A：Anthropic cache 以前缀 hash 为 key，只要 cache_control 之前的字节不变就命中。
   失效场景：(1) 工具热插拔 → 用 `SnapshotETag` 锁住单个 session；
   (2) system prompt 改版 → 蓝绿发布时老 session 跑完再切；
   (3) 跨可用区 → cache 是 region 级，多活时各 region 自热。

**Q：Speculative Execution 错了怎么办？**
A：预取必须 **幂等无副作用**（read_file/grep/search）。如果 LLM 最终没选这条路，
   预取结果进 local LRU cache，1 分钟 TTL 自然过期。成本就是浪费一点 embed 调用。

**Q：LLM Router 的 classifier 自己也要推理，不是鸡生蛋？**
A：classifier 用 <100M 参数的本地模型（DistilBERT 量级），CPU 推理 <10ms，
   成本 = 0。相当于花 10ms 省 1800ms + 90% token 费用。

**Q：Actor 模型 Pod 挂了怎么办？**
A：Actor handoff 协议：Pod shutdown 时把 session 序列化 flush 回 Redis，
   同时发 `ActorMoved` 通知 Ingress 重路由。最坏情况：丢失 100ms 内未 flush
   的消息（用户无感，下轮对话自动重试）。

**Q：Wasm 沙箱能跑 numpy/pandas 吗？**
A：Pyodide 移植了部分（numpy ✓, pandas ✓ with limits），但 tensorflow 等带
   C 扩展的不行。所以我的方案是**分级**：轻量计算走 Wasm，重科学栈走 Docker。
