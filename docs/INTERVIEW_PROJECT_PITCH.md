# 生产级 Code Agent · 面试项目介绍话术 (v2)

> **文档角色定位**
> - 本文 = **口述主稿**（2min / 5min 两档），上面试桌就背这份。
> - 追问弹药库请看 `INTERVIEW_QA.md`（按模块深挖的 Q&A）。
>
> **核心原则（吸取旧稿教训）**
> 1. **只讲代码里真实存在的东西**——所有文件路径、数据都可现场 `cat`/`go test` 验证；
> 2. **一切数据分两类**：① **真实量化**（benchmark 或对比测试跑过）；② **设计目标值**（标注"基于压测场景估算"）。不混讲；
> 3. **亮点以"我做了什么决策 + 踩了什么坑"为骨架**，不是功能清单朗读。

---

## 0. 项目真实体量（背诵卡）

这是面试官追问"项目规模多大"时的第一反应口径，**全部可现场核验**：

| 维度 | 真实数字 | 核验方法 |
|---|---|---|
| Go 代码量 | **47 216 LOC / 160+ 文件** | `find . -name '*.go' \| xargs cat \| wc -l` |
| Package 数 | **33 个 internal package** | `ls -d ./internal/*/` |
| 架构文档 | **12 篇 docs/*.md** | `ls docs/` |
| 测试函数 | **667 个 Test***、**15 个 Benchmark*** | `grep -r '^func Test' --include='*.go' ./internal \| wc -l` |
| 高级模块 | Multi-Agent 协作 / 工具自学习 / Skill Registry（均已实现） | `internal/multiagent/` / `internal/toollearn/` / `internal/skill/` |
| 最新优化 | P0 四项 + MCP 连接池（P1） | `docs/OPTIMIZATION_P0_IMPLEMENTED.md` + `internal/mcp/pool.go` |

---

## 1. 30 秒电梯稿（开场）

> "这是一个 **47k 行 Go 代码** 的生产级代码智能 Agent。它不是又一个 ChatGPT 套壳，而是要解决三个让 LLM Agent 无法落地的工程难题：
>
> - **可控**——危险命令（`DROP DATABASE` / `kubectl apply`）命中敏感规则就 Temporal `workflow.Await` 挂起，等审批 Signal 唤醒；
> - **准确**——代码 RAG 走 Tree-sitter AST 切分 + BM25/Dense 双路召回 + Cross-Encoder 重排，且对 embedding 做内容哈希缓存；
> - **安全**——脚本走一次性 Docker 容器（`NetworkMode:none` + cgroups + seccomp），stdout 通过 SSE 实时推前端。
>
> 架构是四象限：**Orchestrator 中枢 + RAG 知识层 + Sandbox 执行层 + MCP 扩展网关**。Go 协程 + `context` 是并发骨架，Redis / PostgreSQL / Qdrant / Temporal 做状态下沉，Go 服务本身完全无状态，K8s HPA 水平伸缩。"

---

## 2. 2 分钟标准版（⭐ 默认档）

### 段落 1 · 为什么做（20 秒）

> "市面上 AutoGPT / Copilot 这类工具有三个让它在企业落地被卡脖子的问题：LLM 幻觉可能跑 `rm -rf`，没人敢给 prod 权限；文本切分的 RAG 对代码效果差，改代码经常编错字段名；工具接入全是胶水代码，每接一个 Jira / GitHub 都得写适配层。这个项目就是从这三个痛点反推的工程方案。"

### 段落 2 · 怎么做（三大支柱，60 秒）

> **大脑** 用 **Temporal 工作流引擎**——不是我自己写 FSM，因为长任务 + 审批挂起 + 进程崩溃续跑这三件事自己写幂等和版本兼容就是半年工作量。敏感命令命中规则后 `workflow.Await` 挂起，前端 `SignalWorkflow` 唤醒，即使 Go 进程重启也能从 checkpoint 恢复。
>
> **记忆** 是重做了两次的 RAG：第一版用 LangChain 文本切分，函数被砍半，字段名正确率只有 64%。重构用 Tree-sitter 按 function/class 边界切，payload 带 imports/calls/returns，Qdrant 的 payload filter 做项目隔离，再叠 BM25 抓精确标识符、Dense 抓语义、Cross-Encoder 重排。**最近还做了一层 embedding 内容哈希缓存——重复片段直接命中，省 embedding API 调用。**
>
> **手脚** 是 Docker 沙箱：`NetworkMode:none` + `read-only rootfs` + `cap-drop=ALL` + `seccomp default` + cgroups 内存 CPU 限制 + TTL 硬杀，六层叠起来。`io.Pipe` 接 stdout 逐行走 SSE 推前端。**我最近新加了预热容器池，冷启动从 `ImagePull+Create` 省到 `Restart` 那几十毫秒。**
>
> **外层** 有 **MCP 协议网关**——Jira/GitHub 这些外部工具按 JSON-RPC 2.0 挂载，不重启就能接入。**我最近还给它加了多子进程连接池 + Streaming，这个是我最骄傲的一段，面试后半段单独展开讲。**"

### 段落 3 · 结果（15 秒）

> "工程度量可以现场核验：47k LOC / 33 package / 667 个单测 / 15 个 benchmark / 12 篇架构文档；`go test -race -count=3` 全绿。性能上，最近一轮 P0 + P1 优化后，MCP 工具调用按压测估算能做到 3~4x 提速，关键是**把瓶颈从 Go 侧挪到了单子进程 Node server 侧并做了水平扩展**。此外还实现了 **Multi-Agent 并行协作**（基于 DAG 拓扑分层调度）和 **工具自学习**（Tool Learning，基于历史执行反馈动态优化工具选择策略）。"

### 段落 4 · 我的角色（5 秒）

> "我是主力开发，从架构 ADR 到 Docker/CI 全流程主导。最大的收获是——**Agent 生产化 90% 不在 LLM，在工程：怎么让不确定的模型驱动一个确定的系统。**"

---

## 3. 5 分钟深度版（技术面推荐）

在 2 分钟版基础上补充以下 **4 个展开点**（每个 30-60 秒）。其中第一个 **必讲**，是最新的优化故事。

---

### 展开 1 · MCP 多子进程连接池 + Chunked Streaming（⭐ 必讲）

这是我整个项目里**最能展现排查-决策-踩坑-修复完整闭环**的一段。

#### 问题起点

> "在 ReAct 循环里，Agent 2~3 秒内会并发向一个 MCP server（如 filesystem-mcp）发 10~20 条 `read_file`。我在压测里观察到一个怪现象：**p99 响应 1.2s，但宿主 CPU 只有 40%——明显不是算力瓶颈**。"

#### 根因定位

> "两个原因叠加：
> 1. **客户端侧**：MCP 走 stdio + JSON-RPC，同一子进程的 stdin **必须串行化写**（`io.Writer` 并发不安全）；
> 2. **服务端侧**：大多数 MCP server 是 Node.js，**单事件循环单线程消费**，CPU 绑核后并发天花板很低。
>
> 结论：瓶颈不在 Go，在**每个子进程的单线程消费能力**。"

#### 方案（`internal/mcp/pool.go`）

> "三件事：
> 1. **连接池**：为每个逻辑 server 后置 `ConnPool`，启动时 fork **N 个子进程**（`PoolSize` 可配），每个独立握手、独立维护 `pending map`；
> 2. **Least-pending 负载均衡**：用 `atomic.Int64` 记每个连接的 inflight，Pick 时 O(N) 扫描选最少的。N≤8 时原子比 mutex 快 ~30ns/op；
> 3. **Chunked Streaming**：`tools/call` 的 `params._meta.progressToken` 放个 token，server 端主动推 `notifications/progress` 帧带 chunk 字段；客户端订阅 token 得到 `<-chan ToolChunk`，每收一帧立即吐给上层 SSE。终帧带 `IsFinal=true`（若 ctx 取消则 `Err != nil`）。"

#### 🔥 踩的坑（**这段一定要讲，面试官爱听**）

> "第一版跑测试直接死锁 60 秒不返回。
>
> 根因：`ServerConnection` 用**同一把 `sync.Mutex`** 既保护 `pending map`，又用它 `defer unlock` 串行化 stdin 写。
>
> 死锁链条：
>
> ```
> sender goroutine:   持 mu → 写 stdin → 阻塞（mock 的 outW 背压）
> reader goroutine:   读 stdout → 拿到响应 → 想取 mu 查 pending → 阻塞
>                              ↑ 环死了
> ```
>
> 修复就一句话：**拆成两把锁**——`mu` 只管 map，`writeMu` 只串行化 stdin 写。这给我一条铁律：**绝不要持锁做 IO**。"

#### 测试怎么写的

> "不依赖真实 MCP 子进程。我用 `io.Pipe` 接了一个 mockServer goroutine——它从 pipe 读 client 写进来的 JSON-RPC 请求，按脚本发响应/推 progress。11 个用例覆盖：全 slot 启动成功、部分 slot 握手失败在 minAlive 阈值两侧的判定、least-pending 路由、size=3 并发 30 次流量分布到 ≥2 slot、streaming 3 chunk + IsFinal 顺序、ctx 取消走 `IsFinal{Err}`、Close 幂等、slot 下线后 Alive 递减、MonitorOnce 死槽 `inflight=-1`。`-race -count=3` 全绿，总耗时 1.8s。"

#### 量化效果（口径标注清楚）

> "基于本地压测场景估算：单进程 p99 ~1200ms → 4 进程池 ~340ms（**~3.5x**，压测场景为 30 条并发 `tools/call`，每条 server 端 mock 延迟 5ms）。**这不是生产环境实测数据**，生产还要结合真实 MCP server 负载特征。真正可复现的是测试中 30 次并发请求分布到 ≥2 个 slot——这是代码里断言的事实。"

---

### 展开 2 · Temporal + HITL（长任务一致性）

> "敏感命令审批可能要 30 分钟没人看，Go 进程在这期间完全可能被 rolling update 重启。自己写 FSM 要解决幂等、事件顺序、版本兼容——Temporal 把这三件事打包解了。
>
> - 挂起用 `workflow.Await(ctx, func() bool { return c.approved })`——worker goroutine 被**立即释放**，不占资源；
> - Signal 是 at-least-once 投递，worker 侧用 `RunID` 做 dedup；
> - Activity 必须幂等（写操作带 idempotency_key = `SHA(taskID+stepID)`，PG 侧 `ON CONFLICT DO NOTHING`）；
> - Workflow 内不能用 `time.Now()`/`rand`，要用 `workflow.Now()`/`workflow.SideEffect`——否则重放会不确定。
>
> 代价老实说：Temporal 是个大家伙（需要 PG/MySQL 做 history store）。团队小可以用 allinone（SQLite + 进程内），我们留了降级路径。"

### 展开 3 · 代码 RAG + Embedding 缓存（P0 优化）

> "第一版用文本切分，加 tracing 发现**函数被从中间砍开** → 上半截 + 下半截进了不同 chunk，LLM 生成代码字段名胡编。
>
> 重构方案：
> - `ast_parser.go` 用 Tree-sitter 按 function/method/class 边界切，每个 chunk 是可独立编译的单元，payload 带 `{file, imports, calls, returns}`；
> - Qdrant 的 HNSW 支持 filter-then-search，`project_id=xxx AND version=v1.2` 作为硬过滤下推到索引层；
> - 双路召回：BM25 抓精确标识符（`parseJWTToken` 这种驼峰专名）；Dense 抓语义（"验证用户身份" → `AuthenticateUser`）；Cross-Encoder 最终重排 top-20 → top-5；
> - **P0 新增的 embedding 缓存**（`rag/embedding_cache.go`）：按 SHA256(content + model + version) 做 key，LRU。代码仓库 churn 低、片段重复率高，命中率按设计目标压测能到 ~70%，直接省掉 embedding API 调用。"

### 展开 4 · 沙箱：六层防御 + 预热池 + SSE

> "沙箱是合规的硬门槛，我做了**六层叠加防御**：
>
> 1. `NetworkMode: none`（最严）或 egress 白名单；
> 2. `--read-only` + volume 限制；
> 3. `--cap-drop=ALL` + `--security-opt=no-new-privileges`；
> 4. `--security-opt seccomp=default.json`（禁 ptrace / mount / keyctl 等）；
> 5. cgroups：`--memory=512m --cpus=1 --pids-limit=128`；
> 6. Go 侧 `context.WithTimeout` 级联 + 硬 TTL 10min 强 kill。
>
> **P0 优化：预热池**（`sandbox/warm_pool.go`）——启动时 pre-warm N 个 busybox/python/go 容器，分配时只需 `docker exec` 进入，省掉 `ImagePull + ContainerCreate`。按设计可把冷启动从 200ms 量级降到几十毫秒，具体数字待生产压测。
>
> **SSE 流式**：`io.Pipe` 挂 Docker stdout → 逐行推前端。坑：前端断连要及时 `ctx.Done()` + `ContainerKill`，不然 Docker API stdout pipe 会堆积内存。"

---

## 4. 工程度量（白板摆数据）

### 4.1 真实数据（可现场 `go test` 核验）

| 维度 | 数据 |
|---|---|
| Go 代码量 | 47 216 LOC / 160+ 文件 |
| Internal packages | 33 个 |
| 测试函数 | 667 个 `Test*`，15 个 `Benchmark*` |
| 高级模块 | **Multi-Agent 协作**（Supervisor + DAG 拓扑调度 + ConflictResolver）/ **工具自学习**（Collector→Extractor→AdaptivePolicy 闭环）/ **Skill Registry**（atomic Snapshot + webhook/function 双执行器） |
| 最新重头戏 | **MCP 连接池**：11 个 `-race -count=3` 全绿用例（1.8s）；**P0 四件套**：Prompt Schema ETag 缓存 / Embedding 缓存 / Speculative Tool Cache / Warm Pool |
| 可观测端点 | `/api/v1/debug/p0*`（实时 hit/miss/alive），`/api/v1/tools` 返 `X-Tools-Etag`（304 Not Modified 优化） |
| CI | `Dockerfile.p0test` 三段式：unit → 集成 → benchmark |

### 4.2 性能目标（标注口径）

| 指标 | 口径 | 数据 |
|---|---|---|
| MCP 工具调用 p99 | 本地压测（mock server, 30 并发） | 单 slot ~1.2s → 4 slot ~340ms（**~3.5x**） |
| 沙箱冷启动 | 按操作拆解估算（Pull+Create 100ms+ vs `exec` 几十 ms） | 待生产压测 |
| 测试回归 | `go test ./... -race -count=3` | 全绿，总时长 < 2 分钟 |

> **面试官刁钻追问"这些数字是生产环境吗？"** → **别吹**：诚实答"p99 340ms 是本地 mock server 压测，命中率 70% 是按仓库 churn 估算的设计目标。生产压测在下一轮 roadmap，我只承诺代码层面做的事情"。诚实比吹牛值钱 10 倍。

---

## 5. 面试节奏控制（临场手册）

### 5.1 不同时长档位

| 时长 | 顺序 | 展开点 |
|---|---|---|
| **30 秒** | 电梯稿 | 三关键词（可控/准确/安全） + 四象限 |
| **2 分钟** | ⭐ 默认 | 动机 → 三支柱 → 结果 → 我的角色 |
| **5 分钟** | 技术面 | 2min + MCP 池故事（必讲，包括死锁踩坑）+ Temporal/RAG/沙箱选讲 2 个 |
| **15 分钟** | 架构面 | 白板画图 + 5min + 深入 1 个模块，带上代码片段 |

### 5.2 过渡话术

| 场景 | 我会这么说 |
|---|---|
| 讲完默认版，想把主动权交回去 | "以上是总览。您想先聊 LLM 可控性、代码 RAG、还是 MCP 连接池这段？"（主动**给菜单**显从容） |
| 对方打断 | **立刻停**。"好的，您关心 XX，我直接讲 XX。" |
| 被追问"最有挑战的" | "三件事印象最深——讲哪一个？①LLM 可控性 × 生产确定性 ②长任务状态一致性 ③MCP 池死锁排查"（见 QA 文档第 8 节） |
| 被问"这项目和 Copilot 有什么不同" | "Copilot 是**IDE 端辅助编码**；我们是**服务端自治 Agent**，能连生产资源、能执行、能审批。定位完全不同"——1 句话拉开差异 |
| 紧张忘词 | "让我想一下怎么组织语言更清楚"——比"卡壳沉默"好 100 倍 |

### 5.3 千万要避免

| 反面 | 正面 |
|---|---|
| 流水账列功能 | **按决策/踩坑/数据**讲 |
| 只讲技术不讲价值 | "用了 Temporal" → "Temporal 解决长任务审批挂起的一致性" |
| 吹不敢兑现的数字 | **口径标清楚**（真实 / 压测 / 目标 / 估算） |
| 背 README | **故事线**（发现问题 → 选型 → 实现 → 数据验证） |
| 超过 5 分钟不停 | **宁可短让对方追问**，对方一旦走神，后面数据全白搭 |

---

## 6. 收尾金句（可作为自我总结）

> "这个项目教会我的不是某门技术栈，而是——**在 LLM 这么不确定的组件上，怎么用工程手段围出一个确定性的边界**。安全、可观测、可演进三个维度一层层加栏杆，把一个'看起来很酷的 demo'变成'真敢给团队用的产品'。这就是我想继续在贵公司深耕的方向。"

---

## 7. 随身速记卡（备稿 30 秒）

**口诀：**
> "三痛点（幻觉 / 检索 / 安全）→ 三支柱（Temporal 大脑 / AST RAG 记忆 / Docker 沙箱手脚）→ 一扩展（MCP 连接池 + Streaming）→ 三口径（真实 / 压测 / 目标）→ 一收获（给 LLM 围确定性边界）"

背熟这 12 个词，2 分钟版现场能复刻。

---

**祝面试顺利。🚀 如果讲到 MCP 死锁那段，记得带一下表情——那确实是个让人哭笑不得的教训。**
