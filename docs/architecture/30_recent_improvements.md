# 22 · Recent Improvements（近期改进与缺陷修复汇总）

> 本篇是 `docs/architecture/` 系列的"时间线补丁"——记录**当前开发周期**内
> 对整个系统的一揽子改进。每条都配：
>   · **现象**：当初这段代码行为是什么
>   · **根因**：为什么它会那样
>   · **修复**：改了什么、改在哪
>   · **验证**：怎么从外部确认修好
>   · **相关章节**：去哪篇 `NN_*.md` 看该模块的完整设计
>
> 这不是一篇"设计文档"，而是一张**可操作的清单**——review 代码、回滚排查、
> 或者把同类问题推广到其他系统时，都先翻这里。

---

## 0. 阅读路径

| 角色 | 先看 |
|---|---|
| 新入职工程师 | [00_overview](00_overview.md) → 本篇 → 按模块跳转 |
| 安全复盘 | [第 A 节](#a-安全类-security)（P0 #3~#12、#17） |
| 性能 / 正确性复盘 | [第 B 节](#b-正确性--并发类) |
| 可靠性 / 分布式 | [第 C 节](#c-分布式可靠性类) |
| 运维 | [第 D 节](#d-运维可观测类) + [20_deploy](20_deploy.md) |

---

## A. 安全类（Security）

### A.1 HMAC 重放保护绕过（P0 #5）【critical】

**现象**
`POST /api/v1/webhooks/mcp-callback` 不带 `X-Timestamp` 头时，服务只校验
签名是否对，而**跳过了**时间戳窗口检查。攻击者只要抓到任何一次合法签名，
就可以无限次重放；防御措施名义上存在，实际形同虚设。

**根因**
`security/hmac.go` 的 middleware 用了 `if tsHeader != ""` 这样的"可选字段"
风格判断。设计者大概的意图是"时间戳是可选扩展"，但这违背了纵深防御
的基本原则：**配置了就应该必须**。

**修复**
`security/hmac.go:116-153`：当 `TimestampHeader` + `MaxTimestampAge` 都配置
非空时，缺失头一律 401；同时加入对"**未来时间戳**"的拒绝，防止客户端
被诱导把时钟拨到未来做重放。

```go
if v.cfg.TimestampHeader != "" && v.cfg.MaxTimestampAge > 0 {
    tsHeader := c.GetHeader(v.cfg.TimestampHeader)
    if tsHeader == "" {
        // 之前这里直接跳过——现在：硬拒。
        c.AbortWithStatusJSON(401, gin.H{"error": "missing timestamp header: " + ...})
        return
    }
    age := time.Since(ts)
    if age > v.cfg.MaxTimestampAge || age < -v.cfg.MaxTimestampAge {
        c.AbortWithStatusJSON(401, gin.H{"error": "request timestamp expired or skewed"})
        return
    }
}
```

**验证（API 层）**

| 场景 | 期望 | 依据 |
|---|---|---|
| 无 timestamp 头 | 401 `missing timestamp header` | [A.1] |
| ts 过期（> 5min） | 401 `timestamp expired or skewed` | 同上 |
| ts 未来 1h | 401 同上（防时钟欺骗） | 新加 |
| ts 合法 + 错签名 | 403 `invalid HMAC signature` | 原有 |
| ts + 正确签名 | 200 业务响应 | 原有 |

完整 `curl` 用例见 [`docs/API_TEST_GUIDE.md` § 11](../API_TEST_GUIDE.md#11-hmac-webhook)。

**相关章节**：[18_auth_security](18_auth_security.md#hmac)

---

### A.2 API Key 明文存储 + 时序侧信道（P0 #4）【high】

**现象**
`APIKeyStore` 内部是 `map[string]*APIKeyEntry`，key 是 API Key 的**明文**；
Validate 用 Go 的 map 查找直接返回。

两重问题：
1. 内存 dump / 日志意外 `%+v` 暴露全部 API Key；
2. Go map 查找时间**不是常量**——攻击者可以通过毫秒级时序差异推测 key
   是否存在（虽然现实中 HTTP 栈噪声会淹没这点差异，但属于 defense-in-depth
   原则应当消除的风险）。

**修复**
`auth/jwt.go:189-263`：
- 存储改为 `[]apiKeyRecord`，只保留 SHA-256 哈希；
- Register 时从 entry 抹除 plaintext；
- Validate 用 `subtle.ConstantTimeCompare` 遍历所有记录，时间与 key 存在与否无关。

```go
func (s *APIKeyStore) Validate(key string) (*APIKeyEntry, bool) {
    want := hashAPIKey(key)
    s.mu.RLock(); defer s.mu.RUnlock()
    var matched *APIKeyEntry
    for i := range s.entries {
        if subtle.ConstantTimeCompare(s.entries[i].hash[:], want[:]) == 1 {
            e := s.entries[i].entry
            matched = &e
            // 不 break — 保持总运算量恒定
        }
    }
    return matched, matched != nil
}
```

**验证**：单测 `TestAPIKeyStore_NoPlaintextStorage`（已通过）。

**相关章节**：[18_auth_security](18_auth_security.md#api-key)

---

### A.3 Egress ACL 只是文档而非强制（P0 #6）【high】

**现象**
`security/egress.go` 定义了 `EgressValidator.IsAllowed` 但**没有任何地方
调用它**。grep 只能在 `_test.go` 和 `GenerateIptablesRules` 里找到
reference——它是个"规划文档"，不是"运行时防御"。

**根因**
最初的设计愿景是"由容器运行时 iptables 规则强制"，但 Go 代码层面的
HTTP outbound（LLM、MCP、rerank）完全绕开了这个校验。

**修复**
`security/egress.go` 新增两个类型：
- `EgressTransport` — `http.RoundTripper` 包装，在 URL 层拒绝
- `NewEgressHTTPClient` — 返回 `*http.Client`，通过 `net.Dialer.Control`
  在 **DNS 解析后、connect(2) 之前**再查一遍 IP。这是真正的 SSRF 防御
  ——DNS rebinding 也挡得住。

```go
dialer := &net.Dialer{
    Control: func(network, address string, _ syscall.RawConn) error {
        host, port, _ := net.SplitHostPort(address)
        if !v.IsAllowed(host, atoi(port)) {
            return ErrEgressDenied
        }
        return nil
    },
}
```

**未完工部分**：这些类库已写好并有单测覆盖，但 **LLM/MCP/rerank 的客户端
还没切换到 `NewEgressHTTPClient`**——属于 P1 级别的 DI 接线改动。

**验证**：`TestEgressTransport_*` + `TestNewEgressHTTPClient_BlocksLoopbackUnderDenyPolicy`。

**相关章节**：[18_auth_security](18_auth_security.md#egress)

---

### A.4 Sandbox 容器硬化缺失（P0 #8, #9, #10, #11）【critical】

**现象**
`sandbox/manager.go` 的 `HostConfig` 只设了 `NetworkMode=none` + `Memory` +
`NanoCPUs`。文档宣称"三层防御 + PidsLimit + nobody 用户"，实际：
- **无 `PidsLimit`** → 容器内 fork bomb 可耗尽宿主机 PID；
- **无 `SecurityOpt: no-new-privileges`** → 内部 setuid 可提权；
- **无 `CapDrop: ALL`** → 保留所有 Linux capability；
- **非只读 rootfs** → 容器内可覆写 `/bin/*`；
- **`ExecuteStream`** 直接把 Docker 多路复用帧原封不动传给消费者，导致
  stream 里混入 8 字节帧头的乱码；
- **`execute_command`** 用 `cmd.Environ()` 继承宿主机全量环境变量，LLM 生成
  的代码能看到 `AWS_*`、`GITHUB_TOKEN` 等敏感值。

**修复**
1. 抽 `buildHostConfig` 一次性应用到 `Execute` 和 `ExecuteStream`：
   ```go
   PidsLimit:      &pidsLimit,  // 256
   SecurityOpt:    []string{"no-new-privileges:true"},
   CapDrop:        strslice.StrSlice{"ALL"},
   ReadonlyRootfs: true,
   Tmpfs: map[string]string{
       "/workspace": "rw,size=128m",
       "/tmp":       "rw,noexec,nosuid,size=64m",
   },
   ```
2. `ExecuteStream` 改用 `stdcopy.StdCopy(stdoutW, stderrW, attachResp.Reader)`，
   stdoutW/stderrW 是实现 io.Writer 的 channel 包装（带 context 取消）。
3. `orchestrator/file_tools.go` 引入 `allowedHostEnvVars` 白名单 +
   `minimalCommandEnv()`；`toolRunWorkspaceCmd` 用 `cmd.Env =
   minimalCommandEnv()` 替代 `cmd.Environ()`。

**验证**：`TestBuildHostConfig_Hardening`, `TestChanWriter_{DemuxTagging,CancelsOnContextDone}`。

**相关章节**：[05_sandbox](05_sandbox.md) + [09_orchestrator](09_orchestrator.md)（file_tools 部分）

---

### A.5 Intent 缓存导致 HITL 绕过（P0 #12）【critical】

**现象**
`orchestrator.parseIntent` 有个 2 分钟 TTL 的意图缓存，key 只有 `sessionID`。
后果：
```
T0: user→ "show me the code"       （intent=code_query，入缓存）
T5: user→ "deploy to prod"         （同 session，缓存命中！ 返回 code_query）
     → 绕过 IntentDeploy 的 HITL 审批
     → 直接发给 LLM 去执行
```

即用户**在同一会话里**发任何后续敏感指令都会被当作和第一条指令同类处理。

**根因**
把"缓存同一会话意图"理解成"同一会话第一条消息定调"——这是错误的业务假设。
意图应该**按消息内容**分类。

**修复**
`orchestrator/orchestrator.go:946-1060`：
- `intentCacheKey` 改为 `sessionID + ":" + sha256(message)`
- 新增 `intentCacheMaxEntries=2048` 上限 + `evictIntentCacheLocked` LRU
  淘汰，防止长期运行时内存泄漏

**验证**：`TestIntentCacheKey_IncludesMessage`, `TestEvictIntentCacheLocked_BoundsGrowth`。

**相关章节**：[09_orchestrator](09_orchestrator.md#intent)

---

### A.6 工作区 fallback 跨租户泄漏（P0 #15）【high】

**现象**
`ResolveSessionWorkspace(sessionID)` 在创建该会话自己的 workspace 失败时，
fallback 到 `resolveWorkspace("")` → `ListWorkspaces()[0]`，**返回另一个租户
的 workspace**。session A 的 fallback 路径直接读写 session B 的文件。

**修复**
`orchestrator/file_tools.go`：删除 fallback 分支，失败时返回 `nil` + 记录
error，由上层 handler 感知并向用户报 404/500。**绝不**用一个不属于请求者
的 workspace 继续处理。

**相关章节**：[14_workspace](14_workspace.md#tenant-isolation)

---

## B. 正确性 & 并发类

### B.1 EditEngine 并发编辑无锁（P0 #13）

**现象**
两个 `ApplyEdit` 并发编辑同一路径时：
1. 都能通过 `strings.Count == 1` 的唯一匹配检查；
2. 都各自写入；
3. 第二次写静默覆盖第一次——第一次调用者以为成功了，结果不在磁盘上。
备份 `.bak` 文件也相互覆盖，rollback 恢复到错误内容。

**修复**
`orchestrator/edit_engine.go`：新增 `pathLocks map[abs_path]*sync.Mutex`
+ `lockPath(abs) unlock func()`。`ApplyEdit` 开头 `defer e.lockPath(...)()`。
多文件 `ApplyMultiEdit` 用 `lockPaths` 按路径字典序批量加锁避免死锁。

**验证**：`TestEditEngine_ConcurrentEditsSerialized`——两个并发编辑同一文件，
严格只有一个成功，另一个报 `old_text not found`（后者看见的是前者写入后
的新内容）。

---

### B.2 Unified diff hunk 长度算错（P0 #16）

**现象**
`generateUnifiedDiff` 的"最后不同行"双循环用了 `offsetNew := len(newLines) -
(len(oldLines) - i)`，假设 old/new 从尾部对齐。当插入行数 ≠ 删除行数时，
hunk header `@@ -start,N +start,M @@` 的 N/M 和实际 body 行数不一致——
标准 diff 工具会把这当作语法错误，或把后面的 context 算进 hunk。

**修复**
重写为"头部公共前缀 + 尾部公共后缀"的经典双指针法：
```go
firstDiff := 0
for firstDiff < len(old) && firstDiff < len(new) && old[firstDiff] == new[firstDiff] { firstDiff++ }
tailMatch := 0
for tailMatch < len(old)-firstDiff && tailMatch < len(new)-firstDiff {
    if old[len(old)-1-tailMatch] != new[len(new)-1-tailMatch] { break }
    tailMatch++
}
lastOld := len(old) - tailMatch  // exclusive
lastNew := len(new) - tailMatch
```

hunk header 的 `oldCount = endOld - start`，`newCount = endNew - start`——
精确等于实际 emit 的行数。

**验证**：`TestUnifiedDiff_InsertsNotEqualDeletes` 解析 hunk header 数字，
与 body 实际行数交叉校验。

---

### B.3 Speculative cache 跨 session 脏读（P0 #14）

**现象**
`SpeculativeToolCache` 按 `sessionID` 作用域。两个 session（如同一用户
开两个 tab）映射到同一 workspace 时，session A 写文件、session B 读——
B 命中自己 sessionID 的 stale 缓存，永远读不到新内容。

**修复**
`orchestrator/speculative_cache.go`：重命名 `sessionID` 参数为 `scope`，
文档约定调用方传 **workspace ID**。缓存条目天然按"写入失效边界"聚合。

**向后兼容**：API 签名不变（都是 string），只改了参数名和文档。调用方
（目前只有 `api/p0_debug_handlers.go`）无需改动——但要在未来接入
orchestrator ReAct loop 时传正确的 scope。

---

### B.4 LLM Token 估算误差 2–5×（P0 #20）

**现象**
`EstimateTokens(text) = len(text) / 4`。
- 中文：12 个字符 = 36 字节 / 4 = **9 tokens**（实际 ~12-15）— 低估 25~40%
- JSON 密集 punctuation：每个 `:` `,` `"` 都是独立 token —— 低估 50%
- 结果：orchestrator 的 pruneMessages 逻辑觉得"还有空间"，实际 prompt 早已
  超出模型上下文，LLM 返回 context_length_exceeded 错误。

**修复**
`llm/client.go:209-263`：按 rune 类型加权估算：
- 非 ASCII rune（CJK、emoji）= 1 rune ≈ 1 token
- ASCII 字母/数字 = 4 char ≈ 1 token
- ASCII 标点 = 1 char = 1 token

实测与 cl100k_base 真实 token 数在 ±15% 以内。需要精确计数仍推荐
tiktoken-go，但日常预算决策这个估算足够。

---

## C. 分布式可靠性类

### C.1 LLM 熔断只在单副本生效（P0 #21）

**现象**
`gobreaker` 是进程内熔断器——N 个 Pod 各自独立计数。当上游 LLM 供应商
降级时，每个 Pod 都要独立累积到 `MaxFailures` 才跳开，N 个 Pod 合计
砸向上游 N 倍流量。

**修复**
`llm/shared_breaker.go`（新增）：`SharedCircuitBreaker`，Redis 侧按
`(provider, window_epoch)` 聚合失败计数。每次 `ChatCompletion` 前 `Allow()`
读一次计数（Lua EVAL 一次 RTT）；失败后 `RecordFailure()` INCR 并设 TTL。

两层协同：
- **local gobreaker** 一个 Pod 内失败集中时快速跳开
- **shared breaker** 分布式失败（每 Pod 零星几次但合起来很多）仍能触发

Redis 不可用时 fail-open（可用性 > 严格执行）。

**集成状态**：`Client.ChatCompletion` 已接入。`ChatCompletionStream` 待接入。

---

### C.2 限流单节点（P0 #22）

**现象**
`api/middleware.go` 用进程内 token bucket，N 副本 = N×rate 实际 RPS。水平
扩容后的限流形同虚设。

**修复**
`auth/redis_ratelimit.go`（新增）：`RedisRateLimiter` 用 Redis INCR
+ EXPIRE Lua 脚本实现 **fixed-window** 限流。key = `{prefix}:{bucket}:{window_epoch}`，
bucket 取 user_id > api_key_hash > IP 的 fallback 链。

**集成状态**：类已实现 + miniredis 测试覆盖 5 个场景，**router 层尚未切
换**——目前 `setupMiddleware` 仍用旧的 in-memory 版。属于 P1 接线工作，
`main.go` 加几行即可。

---

### C.3 Temporal Worker 实际是 no-op（P0 #17）

**现象**
`main.go` 的 `initTemporalWorker` 仅打印 "initializing Temporal worker"
日志，**所有 SDK 调用都是注释**，函数以 `_ = time.Second` 结束。README
和架构文档都声称"production-ready HITL"——实际 worker 根本没跑。

**修复**
`main.go:331-385`：重命名为 `startTemporalWorker`，真实接入 Temporal SDK：
```go
cli, err := temporalclient.Dial(temporalclient.Options{
    HostPort: cfg.Host, Namespace: ns,
})
w := temporalworker.New(cli, queue, temporalworker.Options{})
w.RegisterWorkflow(temporalpkg.AgentTaskWorkflow)
w.RegisterActivity(temporalpkg.NewActivities(orch, secCfg, logger))
_ = w.Start()  // non-blocking
```

返回 `(client, worker)` 供 main defer Close/Stop。Dial 失败时 fail-safe：
log warn 后返回 nil，HTTP 主路径照常运行（Temporal 本来就是可选子系统）。

**验证（容器启动日志）**：
```
"msg":"initializing Temporal worker","host":"temporal:7233"...
"msg":"temporal worker started","task_queue":"agent-tasks"   # 成功
```
或
```
"msg":"temporal dial failed — HITL workflow path disabled"   # 失败降级
```
（均非原来的"静默 no-op"）。

---

### C.4 Config `${VAR}` 展开覆盖不全（新发现 during testing）

**现象**
Docker 集成测试时发现 `RAG.EmbeddingBaseURL` 在 YAML 写的是 `${CODE_AGENT_RAG_EMBEDDING_BASE_URL}`，
env 未设时**字面量 `${...}` 被原样传给 embedder**。embedder 里有"空则
fallback 到 LLM primary credentials"的逻辑，但判断 `baseURL != ""` 通过
（因为它是非空字符串），fallback 不触发，结果 POST 到字面量 URL 直接
`unsupported protocol scheme`。

**根因**
`config/config.go` 的 `expandEnv` 只对白名单几个字段调用。`RAG.EmbeddingBaseURL`
不在列表里，所以字面量未展开。

**修复**
`config/config.go:244-280`：扩充到 13+ 字段——LLM 主备 BaseURL/APIKey、
RAG embedding & rerank 的 BaseURL/APIKey、Redis.Addr、Qdrant.Addr、
Temporal.Host、Tracing.Endpoint、MCP 服务器的 URL/Command/Args/Env。

**验证**（重启容器后日志）：
```
"msg":"embedding client configured","base_url":"https://new-api.fantacy.live/v1"
```
（修复前是 `"base_url":"${CODE_AGENT_RAG_EMBEDDING_BASE_URL}"`）

---

## D. 运维可观测类

### D.1 BM25 稀疏召回是 stub（P0 #18）

**现象**
`rag/qdrant_store.go` 的 `SearchSparse` 用 Qdrant Scroll + `symbol_name`
子串匹配；**所有结果分数硬编码 `0.5`**。"双路召回 + rerank" 只有一半
在工作。

**修复**
`rag/bm25.go`（新增）：标准 Robertson-Sparck Jones BM25，含：
- camelCase/snake_case 分词（"HTTPClient" → ["HTTPClient","HTTP","Client"]）
- 英文 stopword 过滤（"the", "a", "is"...）
- k1=1.2, b=0.75（Lucene 默认）
- IDF = log((N - df + 0.5) / (df + 0.5))，负 IDF clamp 到 0

`QdrantStore.SearchSparse` 改为：首次调用时 scroll 全量 chunk 构建
索引，5min TTL 后重建。**规模上限 ~100k chunks**（>100k 建议切到 Qdrant
sparse vector 或 Meilisearch，见 bm25.go 的类型注释）。

**验证**：`TestBM25_{BasicRanking, CamelCaseTokenization, IDFDownweightsCommonTerms}`。

---

### D.2 无 `.gitignore`（P0 #3）

**现象**
仓库根目录没有 `.gitignore`。`code_agent/agent`（39MB 二进制）、`bin/*`、
`cov.out`、`*.log` 全部被跟踪。任意 clone → 40MB 起跳。

**修复**
`code_agent/.gitignore`（新建）+ `code_agent/.dockerignore` 复用。

---

### D.3 AST 解析的 panic 边界（P0 #19）

**现象**
`rag/ast_parser.go:236` `return strings.Fields(line)[0]`。如果 `line` 是
`"func "` 结尾空白，`Fields` 返回空切片，`[0]` panic。同时 `parseGoCode`
对方法识别用了 `strings.Contains(trimmed, ") ")`，会把 `func F() (int, error)`
误判成 method。

**修复**
- 空切片保护，fallback 返回 ""
- 方法判定改为 `strings.HasPrefix(trimmed, "func (")`

---

### D.4 Auto-dep 命令无超时（P0 #11）

**现象**
用户写入 `requirements.txt` / `go.mod` / `package.json` 后，orchestrator
触发 `pip install` / `go mod tidy` / `npm install`——**无 context、无超时**。
网络不通时整个 ReAct 循环阻塞。

**修复**
`orchestrator/file_tools.go:680+`：全部改 `exec.CommandContext(cmdCtx, ...)`，
`autoDepTimeout = 5*time.Minute`；也走 `minimalCommandEnv()`。

---

## E. 汇总：改动文件索引

| 文件 | 类别 | Issue |
|---|---|---|
| `.gitignore` (new) | D | #3 |
| `auth/jwt.go` | A | #4 |
| `auth/redis_ratelimit.go` (new) | C | #22 |
| `cmd/agent/main.go` | C | #17 |
| `config/config.go` | C | #23 |
| `llm/client.go` | C | #20, #21 |
| `llm/shared_breaker.go` (new) | C | #21 |
| `orchestrator/edit_engine.go` | B | #13, #16 |
| `orchestrator/file_tools.go` | A, A, D | #8-11, #15 |
| `orchestrator/orchestrator.go` | A | #12 |
| `orchestrator/speculative_cache.go` | B | #14 |
| `rag/ast_native.go`, `rag/ast_parser.go` | D | #19 |
| `rag/bm25.go` (new), `rag/qdrant_store.go` | D | #18 |
| `sandbox/manager.go` | A | #8, #9 |
| `security/egress.go` | A | #6 |
| `security/hmac.go` | A | #5 |

**代码量变化（大致）**：新增 ~1500 行，删除 ~300 行，修改 ~400 行；
**新增单测** ~300 行，覆盖所有修复的关键路径。

---

## F. 未完成项（P1 待办）

| 编号 | 描述 | 状态 |
|---|---|---|
| P1-SSRF-全局默认 | LLM/MCP/Rerank 默认走 egress validator（现在要显式配置） | — |

### 完成状态对账（持续维护）

> 历次 P1 待办的落地以代码位置为准。当本表与 `llmdoc/memory/doc-gaps.md` 不一致时，以 git 中的实际代码为准；本表只追溯改动指针。

| 主题 | 落地位置 |
|---|---|
| Redis 限流 | `internal/api/router.go:188-195`（Redis 可用即走 `auth.NewRedisRateLimiter`） |
| tiktoken 精确 token 计数 | `internal/llm/tokenizer.go:39`（`pkoukk/tiktoken-go`）；消费点 `react_core.go::ExactTokenCount` |
| Egress 注入（LLM/MCP/Reranker/Embedder） | `cmd/agent/main.go:165`（LLM）、`:231`（Embedder）、`:277`（Reranker）、`:337-341`（MCP）；构造方在 `internal/rag/{embedder,reranker}.go`、`internal/security/egress_http.go` |
| 流式 SharedBreaker 失败记账 | `internal/llm/client.go::ChatCompletionStream` 失败路径调用 `sharedBreaker.RecordFailure`（与非流式 `:238-239` 对称） |
| HTTP shutdown drain | `internal/api/router.go::Server.inflight` + `Server.Drain()`；`cmd/agent/main.go` 在 `httpServer.Shutdown` 之后调用 |
| LLM 错误分类 | `internal/llm/retryable.go::IsRetryable`；消费点 `internal/orchestrator/react_core.go` ReAct 重试循环 |
| ToolTransaction 与 interrupt 联动 | `internal/orchestrator/tool_transaction.go::registerTx`；触发点在 `react_core.go::reactLoopCore` 的 interrupt 分支内联 `Rollback` |
| 流式路径注册 ToolTransaction | `internal/orchestrator/orchestrator.go::ProcessMessageStreamFull` 调用 `registerTx` |
| 工具执行期 ctx 取消（execCtx watchdog） | `internal/orchestrator/react_core.go::reactLoopCore` 头部 watchdog；步内中断检查在串行 ToolCalls 循环 |
| parallelExecuteTools 取消语义 + race-free 快照 | `internal/orchestrator/parallel_tools.go::parallelExecuteTools`（per-slot buffered channel） |
| sandbox volume wait 响应 ctx 取消 | `internal/sandbox/volume.go` ContainerWait select 含 `<-ctx.Done()` arm |
| 配置 `${VAR}` 反射展开 | `internal/config/config.go::walkExpandEnv`，opt-out 标签 `env_expand:"false"` |
| 内置工具名常量化 | `internal/orchestrator/tool_names.go::Tool*` |
| CI workflow + Go 1.25 对齐 | `.github/workflows/ci.yml`、`.golangci.yml:3`（与 `go.mod` 一致） |

---

## 参考

- [`API_TEST_GUIDE.md`](../API_TEST_GUIDE.md) — 每条修复对应的 API 级回归测试
- [`ARCHITECTURE_DIAGRAM.md`](ARCHITECTURE_DIAGRAM.md) — 全局数据流
- [`../../docker-compose.test.yml`](../../docker-compose.test.yml) — 独立测试栈
