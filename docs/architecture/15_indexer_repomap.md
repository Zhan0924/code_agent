# 15 · 索引器 + 仓库地图 `internal/indexer` + `internal/repomap`

> 代码（**以代码为准**）：
>
> - `internal/indexer/indexer.go` (405 行) — 全量/增量仓库索引器（RAG 入库管道 + 写穿缓存 + 批量持久化）
> - `internal/repomap/generator.go` (410 行) — 仓库结构 + 符号地图生成器（tree-sitter 优先 / regex 兜底 + 5 分钟 TTL 缓存）
> - `internal/repomap/watcher.go` (369 行) — 文件系统监听器（fsnotify 主路径 + polling 兜底，500ms 内部 debounce）
> - 测试：`indexer_test.go` (113) / `generator_test.go` (297) / `watcher_test.go` (208)
>
> 上层调用：
>
> - `cmd/agent/main.go:497-568` — `indexer.NewIndexer(ragEngine, &cfg.RAG, ...)` + `repomap.NewGenerator(logger)` + `repomap.NewWatcher(watchPath, gen, logger)`；watcher.SetOnChange 加**第二层 2 秒 batch debounce + 30 秒 per-file 超时**

---

## 1. 模块定位

**两个"项目感知"组件，互补而不耦合：**

| 包 | 类比 | 输出消费者 | 粒度 |
|---|---|---|---|
| `indexer` | `docker build` 之于镜像 | Qdrant 向量库 | 函数级 chunk |
| `repomap` | `tree -L 3` + `ctags -x` | LLM system prompt | 文件级符号清单 |

**indexer 解决**：
- "新项目来了，全量扫一遍入 RAG"——`IndexRepository`
- "用户改了几个文件，只重索引它们"——`IndexFile`（基于 SHA-256 + 内存缓存 + DB 写穿）
- "文件被删了，从索引中移除"——`DeleteFile`
- 并发控制（**maxConcurrency=8**）+ 跨进程 checksum 持久化（PostgreSQL）

**repomap 解决**：
- LLM 不用逐文件 `read_file` 才知道项目长什么样
- 一份紧凑的"目录 + 函数/类签名 + 行号"文本直接注入 system prompt
- 文件变了自动失效（5 分钟 TTL + fsnotify 实时失效）
- tree-sitter 优先抽符号，regex 兜底（保证 Docker 镜像 CGO_DISABLED 也能跑）

---

## 1.5 设计哲学：4 个被代码证实的抉择

### Q1 — 为什么 indexer 和 repomap 是两个包不是一个？

两者都"扫仓库、抽信息"，但**信息消费方完全不同**：

| 维度 | indexer | repomap |
|---|---|---|
| 输出 | embedding 向量 + chunk 文本 → Qdrant | 文本 (markdown-like) → LLM system prompt |
| 粒度 | 函数级 / 段落级 chunk | 文件级（仅签名行号） |
| 缓存 | Qdrant 是 source of truth；本地只缓存 SHA-256 | 内存 LRU + 5 分钟 TTL |
| 失效粒度 | 单文件 hash 比对 | 整个 rootDir 缓存（粗粒度） |
| 大小预算 | 大（百万级 chunk） | 紧（数千 token） |
| 调用频率 | 索引时 + watcher 触发时 | 每次 chat 都可能被注入 prompt |

**合并的代价**：
- 维护两种 chunk 粒度（函数级 vs 文件级）让代码极其分裂
- 缓存策略不同（持久化 vs 内存 TTL）耦合后做不到最优
- repomap 必须能在 RAG 不可用时单独工作（"无 Qdrant 也要给 LLM 项目地图"）

**两个包并存的代价**：扫仓库的逻辑（filepath.Walk + 忽略 .git/node_modules + 扩展名过滤）几乎重复两份。
当前接受这种重复——比共享一个 walker 后耦合更可控。

### Q2 — indexer 为什么是 SHA-256 而不是 mtime？

`hasContentChanged`（L281）用 `sha256.Sum256(content)`，不是文件 mtime。

| 检测方式 | 误判率 | 性能 | 跨平台 |
|---|---|---|---|
| mtime | 高（git checkout 不改内容也改 mtime；touch 改 mtime 不改内容） | 极快（stat） | size 与平台精度不同 |
| size | 极高（同 size 不同内容很常见） | 极快 | 一致 |
| SHA-256 | 0 | 慢（每 byte 都过一遍 hash） | 一致 |

**选 SHA-256 的理由**：embedding 重算成本很高（一个文件可能产生 10 个 chunk，每 chunk 一次 OpenAI 调用，按 token 计费）。
**误判一次的代价 >> 全文件 hash 一次的代价**——所以宁可慢也不能用 mtime。

**优化**：`OPT-17`（L203 注释）改成"读一次文件，content 复用给 hash 比对 + RAG 入库"，避免双重 I/O。

### Q3 — 为什么 indexer 有 "pending checksums" 写穿缓存？

L77-78：
```go
checksums        map[string]FileChecksum    // 完整缓存
pendingChecksums map[string]string          // 待 flush 到 DB
```

为什么不是"每次 updateChecksum 同步写 DB"？

| 方案 | 索引性能 | DB 压力 | 重启数据完整性 |
|---|---|---|---|
| 同步写每文件 | 慢（每次写一行）| 高（百万 INSERT） | 强 |
| 仅内存，重启丢 | 快 | 0 | 弱（重启后全部重新索引） |
| 写穿（pending → batch flush） | 快 | 中（一次 batch UPSERT） | 强 |

**当前选写穿**：
- 索引时 `updateChecksum` 只更新内存 + pendingChecksums（L292-305）
- `IndexRepository` 结束时一次性 `BatchUpsertChecksums`（L259）
- `IndexFile`（单文件增量）每次都立刻 `BatchUpsertChecksums` 单条（L375）——因为不知道下一次 flush 是什么时候

**风险**：`IndexRepository` 中途崩溃，pending 部分丢失——下次重启时这些文件会重新走 hash 比对，但因为已经在 Qdrant 里了，重算 embedding 是浪费但不出错。**幂等收敛**。

### Q4 — repomap 的 5 分钟 TTL 是怎么定的？

`cacheTTL = 5 * time.Minute`（L73）。

**为什么不是 1 分钟或 1 小时？**

- 1 分钟：用户用 IDE 编辑文件时，每次保存触发 watcher InvalidateCache，下一次请求会重生成——TTL 在这种场景没意义
- 1 小时：watcher 失效后兜底重建周期太长；如果 fsnotify 漏掉一个文件变更，用户会看到 stale repomap 持续 1 小时
- **5 分钟**：兜底兼性能折中——watcher 正常工作时 TTL 不起作用（直接 invalidate）；watcher 故障时最多看到 5 分钟 stale

TTL **不是关键防御机制**。真正的失效靠 watcher 的 `InvalidateCache`（L268）。TTL 只是 watcher 故障的兜底。

---

## 2. 依赖架构

```
┌─ api.handleIndex (POST /api/v1/index) ─────────────┐
│  apiServer.indexer.IndexRepository(ctx, repoPath)   │
└──────────────────┬─────────────────────────────────┘
                   │
                   ▼
        ┌───────────────────────────┐
        │ indexer.Indexer            │
        │  - ragEngine: *rag.Engine  │
        │  - store: *store.Store     │
        │  - checksums (in-mem)      │
        │  - pendingChecksums        │
        └────────┬──────────────────┘
                 │
        ┌────────┼──────────────┐
        ▼        ▼              ▼
   ┌──────┐  ┌──────────┐   ┌─────────┐
   │filew │  │ rag.     │   │ store   │
   │  alk │  │ Engine.  │   │ (PG)    │
   │      │  │ IndexCode│   │         │
   └──────┘  └──────────┘   └─────────┘

═══════════════════════════════════════════════════════

┌─ repomap.Watcher (background goroutine) ───────────┐
│   fsnotify (primary) | polling (fallback)           │
│   500ms internal debounce                           │
└──────────────────┬─────────────────────────────────┘
                   │ onChange callback
                   ▼
   ┌─────────────────────────────────────┐
   │ main.go: 2-second BATCH debounce    │
   │  collect → flush after 2s idle      │
   │  per-file 30-second timeout         │
   └──────────────────┬──────────────────┘
                      ▼
   ┌──────────────────────────────────┐
   │ indexer.IndexFile / DeleteFile    │
   │  + ragEngine.IndexCode            │
   │  + store.BatchUpsertChecksums     │
   └──────────────────────────────────┘
   ┌──────────────────────────────────┐
   │ generator.InvalidateCache         │
   │  (清空 repomap 缓存让下次重生成)   │
   └──────────────────────────────────┘
```

**注入点**（`cmd/agent/main.go:497-568`）：

- L498：`if ragEngine != nil` —— indexer 强依赖 RAG，RAG 不可用就不创建 indexer
- L499-503：`indexer.WithStore(pgStore)` 是可选项，PG 不可用时仍然能用（只丢持久化）
- L504：`apiServer.SetIndexer(idx)` —— 通过 setter 注入避免 import cycle
- L516：`repomap.NewGenerator(logger)` —— 独立创建，不依赖 RAG
- L518-565：watcher 启动 goroutine，**主循环** `watcher.Start(context.Background())`

⚠️ **watcher 用 `context.Background()`**：watcher 跑到进程结束，没有显式 cancel 路径。
shutdown 时不优雅停 watcher，靠 fsnotify Close 路径兜底（main.go defer 顺序中没有 watcher 显式停止）。

---

## 2.5 数据流总览

```text
═══════ 全量索引: IndexRepository ═════════════════════════════════════════

POST /api/v1/index { "path": "/repo" }
       │
       ▼
indexer.IndexRepository(ctx, repoPath, projectName)    [L109]
       │
       ├─ 1. 预热缓存：store.GetAllChecksums(projectName) → in-mem checksums map
       │
       ├─ 2. filepath.Walk(repoPath):
       │      ├ 跳过 defaultIgnorePatterns: .git/node_modules/vendor/__pycache__/...
       │      ├ ext 必须在 supportedExtensions (19 种)
       │      ├ file size ≤ 1MB
       │      └ → filesToIndex 列表
       │
       ├─ 3. 并发处理（sem chan, maxConcurrency=8）:
       │      └ 每文件 goroutine:
       │         a. os.ReadFile(fp)                           ← 读一次
       │         b. hash := sha256(content)
       │         c. if hash == checksums[fp].Hash → skip
       │         d. lang := supportedExtensions[ext]
       │         e. ragEngine.IndexCode(ctx, relPath, lang, content, {project})
       │         f. updateChecksum: checksums[fp] = {hash, now}
       │                            pendingChecksums[fp] = hash
       │         g. stats.IndexedFiles++
       │
       ├─ 4. wg.Wait()
       │
       ├─ 5. 批量 flush: store.BatchUpsertChecksums(ctx, projectName, toFlush)
       │      └ 清空 pendingChecksums
       │
       └─ 6. return stats {TotalFiles, IndexedFiles, SkippedFiles, Duration, Errors}

═══════ 增量索引: Watcher → batch → IndexFile ═══════════════════════════════

文件变更（用户在 IDE 保存 / git checkout / build 输出）
       │
       ▼
fsnotify.Events → watcher.shouldEnqueue                [watcher.go:197]
       │
       │ 过滤：
       │   - Op ∈ {Write, Create, Remove, Rename}
       │   - ext ∈ supportedExts (8 种语言，比 indexer 少 .md/.yaml 等)
       │   - !hidden, !skipDirs
       │   - 新建目录 → 自动 fsw.Add（递归扩展监控）
       │
       │ 入队：pendingChanges[relPath] = now
       │
       ▼ 500ms timer 到期
watcher.flushPending                                   [watcher.go:252]
       │
       ├─ generator.InvalidateCache(rootDir)              ← repomap 缓存清空
       │
       └─ for each pending: onChange(relPath)
              │
              ▼ (main.go 注册的回调)
       main.go 第二层 batch:                            [main.go:526-563]
              │ batchFiles[filePath] = struct{}{}
              │ batchTimer = AfterFunc(2s, flush)
              │
              ▼ 2s idle 触发 flush
       for each file:
              │ ctx, cancel := WithTimeout(Background, 30s)
              │ if os.Stat → IsNotExist:
              │     idx.DeleteFile(ctx, watchPath, f)
              │ else:
              │     idx.IndexFile(ctx, watchPath, f)
              │ cancel()

═══════ Repomap 注入到 prompt: Generate ═════════════════════════════════════

PromptBuilder.BuildPrompt
       │
       ▼ (内部组装 system prompt 时)
generator.Generate(rootDir)                            [generator.go:84]
       │
       │ 1. cache hit (< 5min) → return cached content
       │ 2. scanDir(rootDir):
       │      filepath.Walk + 过滤 skipDirs + size ≤ 500KB + 8 种 ext
       │      每文件: extractSymbolsWithParser(path, lang)
       │         ├ tsParser != nil:
       │         │   ExtractSymbols(lang, content)
       │         │   过滤 Visibility = public/空
       │         │   → []Symbol{Kind, Name, Line}
       │         └ tsParser == nil 或失败:
       │             regex 逐行扫（goFuncRe/pyClassRe/tsClassRe/rsFnRe/javaClassRe）
       │             只匹配大写开头 (Go) / 非下划线开头 (Python) / public (Java)
       │
       │ 3. sort entries by path（保证 KV cache 命中）
       │ 4. formatMap → "# Repository Map: <name>\n## <dir>/\n  <file> (N lines)\n    func Foo (L42)\n"
       │ 5. 存 cache → return
```

---

## 3. Indexer 数据结构（indexer.go:69-79）

```go
type Indexer struct {
    ragEngine *rag.Engine
    cfg       *config.RAGConfig
    logger    *zap.Logger
    store     *store.Store          // 可选

    checksumMu       sync.RWMutex
    checksums        map[string]FileChecksum   // 写穿缓存（key = absolute path）
    pendingChecksums map[string]string         // 待 flush 到 DB
}

type FileChecksum struct {
    Path      string    `json:"path"`
    Hash      string    `json:"hash"`         // hex of sha256
    IndexedAt time.Time `json:"indexed_at"`
}

type IndexStats struct {
    TotalFiles   int
    IndexedFiles int
    SkippedFiles int
    TotalChunks  int                          // ⚠️ 当前不被 IndexRepository 填充
    Duration     time.Duration
    Errors       []string
}
```

⚠️ **`TotalChunks` 字段是死字段**：IndexRepository 全程不写它（grep 整文件无写入点）。
要拿真实 chunk 数得从 RAG 侧统计。**P2 待修**。

⚠️ **`checksums` 的 key 是 absolute path**（`fp` 来自 `filepath.Walk(repoPath, ...)`，是绝对路径）。
跨进程恢复（`GetAllChecksums`）时也存的是绝对路径——所以**换部署目录后所有 checksum 失效**。`projectName` 作为 namespace 在 PG 表里隔离不同项目，但同一项目换 mount path 不行。

---

## 4. 索引规则

### 4.1 文件扩展名（indexer.go:22-43）

```
.go .py .js .ts .java .rs .rb .c .cpp .h .hpp     ← 程序源码
.md .sh .bash                                       ← 文档 + 脚本
.yaml .yml .json .toml .txt                         ← 配置 + 文本
```

**注意 `.go` 但**没有 `.tsx` `.jsx`**——TypeScript/JavaScript 的 React 组件不会被索引到 RAG。
这与 `repomap.supportedExts`（含 `.tsx` `.jsx`）**不一致**——是历史遗留。**P2 待统一**。

### 4.2 忽略目录（indexer.go:46-49）

```
.git node_modules vendor __pycache__ .venv
dist build target bin .idea .vscode
```

⚠️ **`.next` 不在列表**（Next.js 构建产物）——会被索引一遍。**P2 加入忽略**。

### 4.3 文件大小限制（indexer.go:163）

```go
if info.Size() > 1024*1024 {  // > 1MB
    stats.SkippedFiles++
    return nil
}
```

**1MB** 是硬限。生成的 SQL dump、长 JSON、minified bundle 都会被丢。
没有 stats 报告"跳过了哪个大文件" → 用户可能不知道为什么搜索找不到某个内容。**P3 改进可观测性**。

---

## 5. ★ 增量索引（indexer.go:281-305）

### 5.1 hasContentChanged

```go
func (idx *Indexer) hasContentChanged(filePath string, content []byte) bool {
    hash := fmt.Sprintf("%x", sha256.Sum256(content))
    idx.checksumMu.RLock()
    existing, ok := idx.checksums[filePath]
    idx.checksumMu.RUnlock()
    return !ok || existing.Hash != hash
}
```

简单一行：内存有记录 + hash 相同 → 不变；否则视为变化。

### 5.2 updateChecksum

```go
func (idx *Indexer) updateChecksum(filePath string, content []byte) {
    hash := fmt.Sprintf("%x", sha256.Sum256(content))
    idx.checksumMu.Lock()
    idx.checksums[filePath] = FileChecksum{Path: filePath, Hash: hash, IndexedAt: time.Now()}
    if idx.store != nil {
        idx.pendingChecksums[filePath] = hash
    }
    idx.checksumMu.Unlock()
}
```

**双写**：内存 map + pending map（如果有 PG）。
**没有 PG 时**：每次重启全部从头算 hash → 全量重索引（昂贵）。

### 5.3 跨进程恢复

`IndexRepository` 开头（L119-136）从 DB 预热缓存：
```go
records, _ := store.GetAllChecksums(ctx, projectName)
for filePath, rec := range records {
    if _, exists := idx.checksums[filePath]; !exists {
        idx.checksums[filePath] = FileChecksum{...}
    }
}
```

`if _, exists` 检查保证**已有内存值不被 DB 覆盖**——本进程刚 IndexFile 过的值优先。

⚠️ **`IndexFile`（单文件入口）不做预热**：单文件路径假设 in-mem 缓存已经是 source of truth。
如果进程刚启动，watcher 触发 `IndexFile` 时缓存是空的——会被判定为"changed"，重新索引（浪费但不错）。

---

## 6. Repomap Generator（generator.go）

### 6.1 5 分钟 TTL 缓存（L83-110）

```go
func (g *Generator) Generate(rootDir string) (string, error) {
    g.mu.RLock()
    if cached, ok := g.cache[rootDir]; ok && time.Since(cached.CreatedAt) < g.cacheTTL {
        g.mu.RUnlock()
        return cached.Content, nil
    }
    g.mu.RUnlock()
    // ... scan + format + store cache
}
```

**key = rootDir**：不同项目独立缓存。同一项目 5 分钟内重复 Generate 返回同一字符串——KV cache 命中率最大化。

### 6.2 tree-sitter 优先 + regex 兜底（L252-277）

```go
func (g *Generator) extractSymbolsWithParser(filePath, lang string) ([]Symbol, int, error) {
    if g.tsParser != nil {
        content, _ := os.ReadFile(filePath)
        tsSymbols, err := g.tsParser.ExtractSymbols(lang, string(content))
        if err == nil && len(tsSymbols) > 0 {
            // 过滤 Visibility == "public" 或 ""
            // 转换为 []Symbol
            return symbols, lines, nil
        }
    }
    // fallback
    return extractSymbols(filePath, lang)
}
```

**为什么这种 fallback 策略**：
- Docker 镜像（Dockerfile 中 `CGO_ENABLED=0`）不能链接 tree-sitter C 库 → `tsParser == nil` → 走 regex
- tree-sitter 解析失败（语法错误的文件）→ 走 regex
- 用户本地编译时 `CGO_ENABLED=1` 可启用 tree-sitter → 拿到更精确的符号信息（visibility/scope）

### 6.3 regex 支持的语言（L227-249）

| 语言 | func/method | type/class | interface/trait | enum/struct | const |
|---|---|---|---|---|---|
| Go | `goFuncRe`（仅大写） | `goTypeRe` | `goInterfaceRe` | — | `goConstRe` |
| Python | `pyDefRe` + `pyAsyncRe`（仅非下划线） | `pyClassRe` | — | — | — |
| TS/JS | `tsFuncRe` | `tsClassRe` + `tsTypeRe` | `tsInterfaceRe` | — | `tsConstRe` |
| Rust | `rsFnRe`（仅 pub） | `rsStructRe`（仅 pub） | `rsTraitRe`（仅 pub） | `rsEnumRe`（仅 pub） | — |
| Java | `javaMethodRe`（仅 public/protected） | `javaClassRe` | (合并到 class) | — | — |

⚠️ **regex 是简陋的**：
- Go func 用 `^func\s+(?:\(.*?\)\s+)?([A-Z]\w*)\s*\(` —— receiver 多行会漏；泛型 `func Foo[T any](...)` 仍然能匹配，但带 type param 复杂场景未必准
- Python `pyDefRe` 不识别 decorator 上的函数
- Java method regex 起始有 `\s+` 限制——顶级（无缩进）方法漏匹配
- 全部不识别**注释里的**伪 func 关键字（如 `// func Foo(...)`）会误判

这些是已知限制——**tree-sitter 启用后这些都解决**。

### 6.4 输出格式（L359-395）

```
# Repository Map: <project>
# Files: 42 | Generated: 2026-06-01 14:30

## internal/orchestrator/
  orchestrator.go (567 lines)
    func ProcessMessage (L102)
    func processStep (L245)
    type Orchestrator (L62)

## internal/session/
  manager.go (633 lines)
    func AddMessage (L239)
    ...
```

**有序输出**：`sort.Slice(entries, ...)` 按路径排序保证**确定性** → KV cache 复用最大化。

### 6.5 `FormatCompact`（L399-410）— 紧约束预算的备用格式

```
internal/orchestrator/orchestrator.go [12 symbols, 567 lines]
internal/session/manager.go [9 symbols, 633 lines]
...
```

只输出"路径 + 符号数 + 行数"，约 80% token 节省。
**当前没有调用方在 main.go / prompt_builder.go 里使用**——是预留的低预算分支。**P2：接到 PromptBuilder 的"压缩级别"参数上**。

---

## 7. Watcher（watcher.go）

### 7.1 双路径策略（L106-123）

```go
func (w *Watcher) Start(ctx context.Context) {
    w.snapshot()                                     // 初始 modTimes 快照
    if !w.forcePolling {
        if err := w.runFsnotify(ctx); err == nil {
            return                                   // 正常退出
        } else {
            w.logger.Warn("fsnotify unavailable, falling back to polling", ...)
        }
    }
    w.runPolling(ctx)
}
```

**fsnotify 失败场景**：
- Linux 上 `/proc/sys/fs/inotify/max_user_watches` 用尽 → `fsnotify.NewWatcher` 失败
- 某些 Docker volume / NFS 挂载不支持 inotify
- macOS FSEvents 偶发问题

**polling fallback**：3 秒一次全量 walk + 比对 mtime。CPU 占用比 fsnotify 高得多，但保证最终一致。

### 7.2 fsnotify 路径关键点

**递归监听**：`addRecursive`（L233-250）一次性把所有非忽略子目录都 `fsw.Add`。
**新目录自动观察**：`shouldEnqueue`（L199-207）检测到 `Create + IsDir` 时立刻 `fsw.Add(ev.Name)` —— 否则在新建目录里改文件不会触发事件。

### 7.3 500ms 内部 debounce（L138-191）

```go
var timer *time.Timer
ensureTimer := func() {
    if timer == nil {
        timer = time.NewTimer(w.debounceWindow)  // 500ms
    }
}
for {
    select {
    case ev := <-fsw.Events:
        if w.shouldEnqueue(ev, fsw) {
            ensureTimer()
        }
    case <-timer.C:
        w.flushPending()
        timer = nil
    }
}
```

**关键**：500ms 窗口期内来的事件**追加到 pendingChanges map**（去重）；窗口结束统一 `flushPending` 调用 `onChange(p)` for each p。

为什么是 500ms？
- IDE 保存触发多次 Write + Rename 事件（atomic save = write tmp + rename）
- `git checkout` 一次性修改几百个文件
- 太短：每文件回调；太长：用户改完文件等不到更新

### 7.4 第二层 batch debounce（在 main.go）

main.go L526-563 给 `onChange` 包了**第二层 2 秒 debounce**：
```go
watcher.SetOnChange(func(filePath string) {
    batchMu.Lock()
    batchFiles[filePath] = struct{}{}
    if batchTimer != nil { batchTimer.Stop() }
    batchTimer = time.AfterFunc(2*time.Second, func() {
        // flush batchFiles
    })
})
```

**为什么两层 debounce**：
- 第一层（watcher 内 500ms）：合并 fsnotify 风暴事件
- 第二层（main.go 2s）：合并 watcher 多次 flushPending 调用——比如用户连续保存多次

实际上**第二层意义不大**：watcher 内的 500ms 窗口已经合并了大部分风暴；第二层只在编辑速度 < 500ms 时才合并。
**P2 评估能否删掉第二层**——简化 main.go 逻辑。

### 7.5 per-file 30 秒超时（main.go L543）

```go
fCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
if _, statErr := os.Stat(fullPath); os.IsNotExist(statErr) {
    idx.DeleteFile(fCtx, watchPath, f)
} else {
    idx.IndexFile(fCtx, watchPath, f)
}
cancel()
```

**30 秒**：单文件 RAG embedding 最多 30 秒（OpenAI API 调用 + chunking）。
超时不取消其他文件——下一个 file 用新的 ctx。

---

## 8. 实现剖析与改进方向

### 8.1 当前实现的真实利弊

**优势（验证过的）**
- ✅ SHA-256 增量索引：mtime / size 误判都被规避
- ✅ 写穿缓存（in-mem + pending → batch flush）：性能与持久化兼得
- ✅ 跨进程恢复（store.GetAllChecksums 预热）：重启不需要重算 hash
- ✅ 双层 debounce（500ms + 2s）+ per-file 30s 超时：风暴抗压 + 单点慢不阻塞其他
- ✅ tree-sitter 优先 + regex 兜底：CGO 与 non-CGO 都能跑
- ✅ Generator 5 分钟 TTL + watcher 主动 invalidate：缓存命中率高且不 stale
- ✅ fsnotify 主路径 + polling 兜底：跨平台健壮

**已知风险**

| 严重度 | 问题 | 位置 | 建议 |
|---|---|---|---|
| P1 | indexer/repomap supportedExts 不一致（`.tsx/.jsx` 在 repomap 有 indexer 没） | indexer.go:22 / generator.go:157 | 抽出共享列表 |
| P1 | `IndexStats.TotalChunks` 死字段未填 | indexer.go:63 | RAG 侧返回 chunk count 给 indexer |
| P1 | `.next` 不在 indexer 忽略列表 | indexer.go:46 | 加入 |
| P1 | watcher 用 `context.Background()` 跑到死 | main.go:565 | shutdown 时 cancel ctx |
| P2 | checksum key 是绝对路径，换 mount 点全失效 | indexer.go:296 | 用 repoPath-relative 作为 key |
| P2 | regex symbol 抽取漏匹配（Java 顶级方法 / 泛型 / 装饰器） | generator.go:227 | 强制启用 tree-sitter |
| P2 | `FormatCompact` 写了不被调用 | generator.go:399 | 接到 PromptBuilder 压缩档位 |
| P2 | 第二层 2s batch debounce 可能可删 | main.go:532 | A/B 测试性能影响 |
| P3 | 跳过大文件无可观测性 | indexer.go:163 | 加 stats 字段 |
| P3 | repomap 没有 import 关系图 | generator.go 全文 | 加 import graph for navigation |

### 8.2 优先级修复建议

**P1（生产质量）**
1. 把 indexer 和 repomap 的 supportedExts 抽到 `internal/config` 共享 + 加 `.tsx/.jsx`
2. `IndexStats.TotalChunks` 真填上数据
3. `.next` 加入忽略
4. watcher 接受 shutdown ctx

**P2（设计完善）**
5. checksum key 改 relative path
6. Docker 启用 tree-sitter（CGO_ENABLED=1）或文档化 fallback 是次要质量
7. PromptBuilder 接 `FormatCompact` 做"低预算 repomap"

**P3（未来扩展）**
8. import 关系图（function-level）
9. webhook 增量索引（GitHub push → 自动 IndexFile）
10. embedding 模型版本号入 checksum（embed 模型升级时强制重建）

---

## 9. 设计权衡

| 抉择 | 动机 |
|---|---|
| **indexer / repomap 分包** | 输出消费者不同（Qdrant vs LLM prompt），合并增加耦合不减少代码 |
| **SHA-256 而非 mtime** | embedding 重算成本远高于 hash 成本；误判一次就浪费几十次 LLM 调用 |
| **写穿缓存 + batch flush** | 索引时不卡 DB；崩溃时部分 pending 丢失但语义幂等 |
| **maxConcurrency=8** | 限制 OpenAI 并发调用，避免触发 rate limit |
| **1MB / 500KB 文件大小硬限** | 防止单个 SQL dump / minified bundle 把 indexer 拖死 |
| **tree-sitter 优先 + regex 兜底** | 兼容 CGO 关闭场景；regex 总能跑通 |
| **5 分钟 cache TTL** | watcher 失效兜底；不参与正常路径 |
| **500ms watcher debounce + 2s main batch** | 双层防风暴；前者合并 fsnotify burst，后者合并连续保存 |
| **fsnotify 主 + polling 副** | 跨平台健壮；polling 不优雅但保证最终一致 |
| **watcher.Start 用 Background ctx** | 简化代码但牺牲优雅 shutdown |
| **支持的文件扩展名硬编码** | 简单、可审计；代价是新语言要改源码 |

---

## 10. 后续演进

- [ ] 共享 supportedExts 列表
- [ ] `TotalChunks` 真实统计
- [ ] watcher 优雅 shutdown
- [ ] checksum key 用 relative path
- [ ] Docker 启用 tree-sitter（CGO_ENABLED=1）
- [ ] `FormatCompact` 接入 PromptBuilder 的低预算分支
- [ ] import 关系图（generator 增加 `Imports []string` 字段）
- [ ] webhook 增量索引（GitHub push event → IndexFile）
- [ ] embedding 模型版本入 checksum 元数据
- [ ] 评估能否删掉 main.go 的第二层 2s batch debounce
- [ ] indexer skip 大文件 + 不支持扩展时记 stats（让用户知道为什么搜不到）

---

## 11. 设计教训

1. **检测变更用内容 hash 不要用 mtime**：mtime 在 `git checkout` / `touch` / 编辑器原子保存（write+rename）下不可信；size 在 minified 文件场景下高重复。SHA-256 唯一可靠且语义清晰——代价（每 byte 过一遍 hash）远低于误判触发的 embedding 重算成本。

2. **批量 flush 优于同步写**：单条 UPSERT 在 N 万次时是数据库杀手。indexer 的"内存写穿 + pending map + 末尾 batch UPSERT"是 Go 项目常见 pattern，能用就用。

3. **跨进程恢复要预热缓存**：indexer 在 `IndexRepository` 开头一次性 `GetAllChecksums` 拉全表预热，避免每个文件都查一次 DB。**`if _, exists` 检查不覆盖本地值**是细节但重要——本进程刚算过的 hash 比 DB 里的可能更新。

4. **fsnotify + polling 双路径是跨平台库的标配**：fsnotify 在容器 / NFS / inotify limit 用尽场景下随时可能失败；polling 慢但万能。生产代码必须有 fallback。

5. **debounce 要分层**：watcher 内 500ms 合并 fsnotify burst；上层调用方再加 2s batch 合并业务级风暴（连续保存、git checkout）。两层是不同时间尺度的合并，不冗余但需要 A/B 验证收益。

6. **tree-sitter 与 regex 双策略**：一手抓质量（tree-sitter 精确）、一手保兼容（regex CGO-less 也能跑）。代价是维护两套抽符号代码，但收益（在 Docker 镜像里也能给 LLM repomap）巨大。

7. **缓存 TTL 不是主要失效机制**：repomap 5 分钟 TTL 是兜底；真正的失效靠 watcher 主动 invalidate。**别把 TTL 当核心防御**——TTL 只能保证"最迟 X 秒后看到新值"，不能保证"立刻看到新值"。

8. **配置共享 vs 重复**：indexer 和 repomap 各有 supportedExts 和 skipDirs 列表，几乎重叠但不完全相同（repomap 多了 `.tsx/.jsx`，indexer 多了 `.md/.yaml`）。看似该共享，但**消费场景不同**——LLM prompt 不需要 `.yaml` 的 repomap，但 RAG 需要 `.yaml` 的语义检索。**两份列表是有意的，而非疏漏**。共享会把两个需求拖在一起。

---

下一篇：[`16_store.md`](16_store.md) —— 持久化层：PostgreSQL（tasks/audit/checksums/workspaces/dynamic tools）+ Redis JWT 撤销黑名单 + auto-migrate schema。
