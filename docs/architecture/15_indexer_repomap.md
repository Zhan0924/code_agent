# 15 · 索引器 + 仓库地图 `internal/indexer` + `internal/repomap`

> 代码：
> - `internal/indexer/indexer.go` (261) — 全量/增量仓库索引器（RAG 入库管道）
> - `internal/repomap/generator.go` (362) — 仓库结构 + 符号地图生成器（带缓存）
> - `internal/repomap/watcher.go` (340) — 文件系统监听器（fsnotify + polling 双路径）
> - 测试：`indexer_test.go`、`generator_test.go`、`watcher_test.go`

---

## 1. 模块定位

**两个"项目感知"组件：**

| 包 | 对比类比 | 负责什么 |
|---|---|---|
| `indexer` | `docker build` 之于镜像 | 把仓库文件喂给 RAG（AST 切块 + embedding + Qdrant upsert） |
| `repomap` | `tree -L 3` + `ctags -x` | 生成一份"目录结构 + 每个文件的符号清单"给 LLM 当 system context |

**indexer 解决**：
- "新项目来了，全量扫一遍入 RAG"；
- "用户改了几个文件，只重索引它们"（基于 SHA-256 增量）；
- 并发控制（8 goroutine 限流）。

**repomap 解决**：
- LLM 不会逐个 read_file 才能知道项目长什么样；
- 一份紧凑的"目录 + 函数/类签名"文本直接注入 system prompt；
- 文件变了自动失效；用 fsnotify 实时监听。

两者**互补而非耦合**：indexer 服务语义检索；repomap 服务结构导航（类似 IDE 的"项目面板"）。

---

## 1.5 核心设计问题

### Indexer 和 Repomap 为什么是两个包？

两者都"扫仓库提取结构"，但输出的**消费者不同**：
- **Indexer** → Qdrant：每个 chunk 是文件片段（函数级），用于**向量
  检索**。chunk 粒度为"一段话 / 一个函数"。
- **Repomap** → LLM prompt：输出是**整仓库的结构摘要**（包列表、每个包
  的导出函数签名）。拼进 system prompt，给 LLM"鸟瞰图"。

**权衡**：合并会让接口混乱（有时要 chunk 有时要摘要）。拆开让每个包
自己选择粒度和存储。

### Repomap 的 token 预算管理

整仓库的完整符号列表可能 50k+ tokens，超过很多模型上下文。Repomap
按**importance** 排序（大函数 > 小函数、被调用多 > 少），按 budget 截取
top-N。目标：**repomap ≤ 5k tokens**，让出其他上下文空间。

### Watcher 的 debounce 必要

IDE 一次 save 可能触发 3-5 个 fsnotify 事件（write、chmod、.swp 删除）。
直接每个事件都重建 repomap 会让 Agent 变卡。`watcher.go` 聚合同一路径
事件，到窗口结束才重建。

### 增量索引 vs 全量索引

**策略**：
- 首次启动 / 配置变更 → 全量索引（慢但保证正确）
- 日常文件变动 → 增量（只索引变化的文件）
- 增量的失效路径：记录 content hash，没变就跳过（省 embedding API 钱）

---

## 2. 依赖架构

```
                ┌──────────────────────────────┐
                │   API / orchestrator         │
                │   POST /projects/index       │
                │   GET  /projects/:id/repomap │
                └──────┬──────────┬─────────────┘
                       │          │
                       ▼          ▼
          ┌─────────────────┐  ┌────────────────────┐
          │  indexer.Indexer│  │ repomap.Generator  │
          │   全量/增量索引  │  │   目录 + 符号地图   │
          └──────┬──────────┘  └─────┬──────────────┘
                 │                   │
                 ▼                   ▼
          ┌──────────┐       ┌────────────────────────┐
          │ rag.Engine│      │ repomap.Watcher        │
          │ (04_rag) │       │  fsnotify + polling     │
          └──────────┘       │  debounce 500ms         │
                             └────────┬───────────────┘
                                      │ OnChange(path)
                                      ▼
                               Generator.InvalidateCache
                               Indexer.ReIndexFile  (演进)
```

- **indexer** 底层依赖 `rag.Engine`（04_rag 的 AST 切块 + embedding + Qdrant 管道）；
- **repomap.Watcher** 同时驱动 **Generator 缓存失效**和**（未来）indexer 增量触发**；
- 通常 API 层注入两者，一个走索引路径、一个走 readonly 结构视图路径。

---

## 2.5 数据流总览

本模块有三条数据流：**全量索引**、**增量索引(Watcher)**、**RepoMap 生成**。

```text
══════════════════ 全量索引 IndexRepository() ══════════════════

┌──────────────┐     ┌────────────────────────────────────────┐
│  仓库根目录   │──▶  │ filepath.Walk + 过滤                   │
│  (rootDir)   │     │ 跳过: .git/ vendor/ node_modules/      │
└──────────────┘     │ 限制: 扩展名白名单 + 文件大小 <1MB      │
                     └───────────────────┬────────────────────┘
                                         │ ([]filePath)
                                         ▼
┌────────────────────────────────────────────────────────────────┐
│  并发处理 (semaphore=8)                                         │
│  per file:                                                      │
│    ┌─────────────┐    ┌────────────────┐    ┌───────────────┐  │
│    │ ReadFile    │──▶ │SHA-256 hash    │──▶ │ 变更检查       │  │
│    │ + content   │    │ vs 内存 cache  │    │ 相同→skip     │  │
│    └─────────────┘    └────────────────┘    └───────┬───────┘  │
│                                                     │ changed  │
│    ┌─────────────────────────────────────────────────┘          │
│    ▼                                                            │
│    ┌───────────────┐    ┌────────────────┐    ┌─────────────┐  │
│    │ AST Parse     │──▶ │ Build Chunks   │──▶ │ Embed batch │  │
│    │ (Go/Py/MD)   │    │ (语义切块)      │    │ 【OpenAI】  │  │
│    └───────────────┘    └────────────────┘    └──────┬──────┘  │
│                                                      │         │
│    ┌─────────────────────────────────────────────────┘          │
│    ▼                                                            │
│    ┌─────────────────────────────────────────────────────────┐  │
│    │ Qdrant Upsert (deterministic UUID from filepath+chunk#) │  │
│    │ 【Qdrant】                                               │  │
│    └─────────────────────────────────────────────────────────┘  │
└────────────────────────────────────────────────────────────────┘


═══════════════ Watcher 增量触发 ═══════════════

┌──────────────┐     ┌───────────────────────────────────────┐
│  fsnotify    │     │ polling (fallback)                    │
│  事件流      │     │ 定期 mtime 扫描                       │
└──────┬───────┘     └───────────────────┬───────────────────┘
       │                                 │
       └────────────────┬────────────────┘
                        ▼
         ┌──────────────────────────────┐
         │ debounce 500ms 窗口合并      │
         │ shouldEnqueue (新目录自动 add)│
         └──────────────┬───────────────┘
                        │ ([]changedPaths)
                        ▼
         ┌──────────────┴───────────────────────┐
         │                                      │
         ▼                                      ▼
┌──────────────────────┐          ┌─────────────────────────┐
│ Generator.           │          │ Indexer.ReIndexFile      │
│ InvalidateCache()    │          │ (同全量单文件流程)        │
└──────────────────────┘          └─────────────────────────┘


═══════════════ RepoMap 生成 ═══════════════

┌──────────────┐     ┌────────────────────────────────────────┐
│ orchestrator │     │ Generator.Generate(rootDir)            │
│ buildSystem  │──▶  │   cache TTL 检查 → 命中则直接返回       │
│ Message()    │     └───────────────────┬────────────────────┘
└──────────────┘                         │ (cache miss)
                                         ▼
                     ┌────────────────────────────────────────┐
                     │ scanDir: Walk + 过滤                    │
                     │ → extractSymbols (正则/语言)            │
                     │ → []FileEntry{Path, []Symbol}          │
                     └───────────────────┬────────────────────┘
                                         │
                                         ▼
                     ┌────────────────────────────────────────┐
                     │ formatMap: tree-style 文本              │
                     │ 例: src/                               │
                     │       api/                             │
                     │         handler.go                     │
                     │           func HandleChat              │
                     │           func HandleApproval          │
                     │ → 写入 cache + 返回 string             │
                     └────────────────────────────────────────┘
```

---

## 3. ★ Indexer 核心

### 3.1 数据模型

```go
// indexer.go:51
type FileChecksum struct {
    Path      string
    Hash      string    // SHA-256 hex of file content
    IndexedAt time.Time
}

// indexer.go:58
type IndexStats struct {
    TotalFiles   int
    IndexedFiles int
    SkippedFiles int
    TotalChunks  int
    Duration     time.Duration
    Errors       []string
}

// indexer.go:68
type Indexer struct {
    ragEngine  *rag.Engine
    cfg        *config.RAGConfig
    logger     *zap.Logger
    checksumMu sync.RWMutex
    checksums  map[string]FileChecksum    // 内存保存，重启会丢（见 §7 演进）
}
```

### 3.2 `IndexRepository` 全景（L92）

```
IndexRepository(ctx, repoPath, projectName):

  # Phase 1: 收集候选文件 (filepath.Walk)
  for each file in repoPath:
      if dir in {.git, node_modules, vendor, __pycache__, ...}: SkipDir
      if ext not in {.go, .py, .ts, .js, .rs, .md, ...}: SkipFile
      if size > 1 MB: SkipFile
      stats.TotalFiles++
      filesToIndex.append(path)

  # Phase 2: 并发处理 (sem = bounded 8)
  for each filePath:
      goroutine:
          sem <- {}                         # acquire
          content := os.ReadFile(filePath)  # 只读一次
          if !hasContentChanged(filePath, content):
              stats.SkippedFiles++; return  # 增量跳过
          chunks := rag.ProcessFile(content, lang, ...)
          rag.Upsert(chunks, project=projectName)
          updateChecksum(filePath, content)
          stats.IndexedFiles++
          stats.TotalChunks += len(chunks)
          <-sem                             # release

  wg.Wait()
  stats.Duration = time.Since(start)
  return stats
```

### 3.3 关键设计决策

| 设计点 | 选择 | 动机 |
|---|---|---|
| **文件过滤（ext）** | 白名单 19 种扩展（Go/Py/TS/JS/Java/Rust/C/C++/MD/Sh/YAML/JSON/TOML/...）| 不同语言用不同 AST parser；未知格式直接跳 |
| **大文件阈值** | 1 MB | 超大文件多为生成代码/minified；AST 切块收益 → 成本比极低 |
| **忽略目录** | `.git`、`node_modules`、`vendor`、`__pycache__` 等 11 项 | 都是"构建产物 / 依赖缓存"，无索引价值且量巨大 |
| **并发数** | 硬编码 8 | I/O 主导场景下 4-16 都差不多；8 是一个保守平衡点 |
| **hash 算法** | SHA-256 | 碰撞概率可忽略；标准库原生；256 bit 的代价可接受 |
| **增量策略** | Hash 比对 | 不依赖文件 mtime（CI 的 touch 会骗过 mtime） |
| **[OPT-17]** `ReadFile` + `hasContentChanged(path, content)` 合并 | 减少一次 I/O（之前是先单独读 hash、再重新读 body） | 大仓库下是可观的 QPS 提升 |

### 3.4 `hasContentChanged` (L225)

```go
hasContentChanged(path, content) bool:
  newHash := sha256(content)
  checksumMu.RLock()
  old, ok := checksums[path]
  checksumMu.RUnlock()
  if !ok: return true                  # 首次 → 必须入库
  return old.Hash != newHash
```

注意：**只在内存里存** —— 进程重启后所有文件都"视为新"，会做一次全量重索引。这在启动时间可接受（大仓 30-60s），但生产环境应落盘，见 §7。

### 3.5 `ctx.Done()` 只做**提交 gate**

```go
for _, filePath := range filesToIndex {
    select {
    case <-ctx.Done():
        break                  // 注意：break 只跳 select，实际外层 for 还会继续！
    default:
    }
    wg.Add(1); ...
}
```

**已知改进点**：这里的 `break` 语义不对（只 break select），应该改成 `goto out` 或者用 `continue/return`。当前在 `ctx` 已取消后仍会把剩余文件的 goroutine 启起来，只是这些 goroutine 里的 rag.Engine 调用会在 ctx 取消后尽快返回。收益：不影响正确性，但会浪费少量 goroutine 创建开销。**列为后续演进项**。

---

## 4. ★ RepoMap Generator

### 4.1 数据模型

```go
// generator.go:23
type Generator struct {
    logger   *zap.Logger
    cache    map[string]*CachedMap
    cacheTTL time.Duration
    mu       sync.RWMutex
}

// generator.go:32
type CachedMap struct {
    Content   string        // 已格式化的 map 文本
    Entries   []FileEntry   // 结构化数据（供外部再解析）
    CreatedAt time.Time
}

// generator.go:39
type FileEntry struct {
    RelPath   string
    Language  string
    Symbols   []Symbol     // 函数/类/常量
    SizeBytes int
}

// generator.go:47
type Symbol struct {
    Kind  string    // "func" / "class" / "method" / "const" / "var"
    Name  string
    Line  int
}
```

### 4.2 `Generate` 带缓存（L64）

```
Generate(rootDir):
  # 1. 命中缓存直接返回
  if cached := cache[rootDir]; cached && Age < TTL:
      return cached.Content

  # 2. 重新扫
  entries := scanDir(rootDir)    // walk + 过滤 + extractSymbols
  content := formatMap(rootDir, entries)

  # 3. 入缓存
  cache[rootDir] = &CachedMap{content, entries, now}
  return content
```

- **TTL 缓存**：默认几分钟（见构造器）；
- **Watcher 主动失效**：文件变化立刻 `InvalidateCache(rootDir)` 而非傻等 TTL；
- **双锁语义**：读缓存走 `RLock`，miss 后写走 `Lock` —— 经典"缓存模式"。

### 4.3 `extractSymbols` (L231)

按语言从**正则**或**启发式行扫**抽签名：

```
Go:    ^func [(recv)] Name(        / ^type Name struct|interface
Python: ^def Name(                 / ^class Name[:( ]
TS/JS: ^(export )?(async )?function Name| ^(export )?class Name
```

**注意**：这里是**正则**而非 tree-sitter AST（RAG 那套用的是 AST）。原因：

- repomap 只要符号**签名**，不要语法树；
- 正则快、无外部依赖，对一两万文件瞬间出结果；
- 个别漏捕获（匿名函数、动态方法）可接受；
- AST 精确度留给 rag 的 ast_parser 做。

### 4.4 `formatMap` 的输出样式

示例（生产环境类似 `tree -P`）：

```
project_root/
├── cmd/
│   └── main.go        (package main; func main)
├── internal/
│   ├── auth/
│   │   ├── jwt.go     (func NewService; func (s *Service) Sign; func Verify)
│   │   └── ratelimit.go (type Limiter; func NewLimiter; func (l *Limiter) Allow)
│   └── orchestrator/
│       └── ...
└── go.mod
```

`FormatCompact` 提供更紧凑的版本（无缩进层级，一行一文件），给 token 敏感的 LLM 用。

---

## 5. ★ Watcher：fsnotify + polling 双路径

### 5.1 核心结构

```go
// watcher.go:26
type Watcher struct {
    rootDir         string
    gen             *Generator         // 失效目标
    onChange        func(path string)  // 用户回调
    debounceWindow  time.Duration      // 默认 500ms
    forcePolling    bool               // 强制轮询（跨平台容器 bind-mount 时）
    pending         map[string]bool    // 缓冲区（debounce 期间累积）
    pendingMu       sync.Mutex
    snapshot        map[string]int64   // polling 用：path → mtime
    logger          *zap.Logger
}
```

### 5.2 `Start` 启动策略（L77）

```
Start(ctx):
  snapshot()                          # 首次快照（polling 基线）
  if !forcePolling:
      if runFsnotify(ctx) == nil: return
      logger.Warn("fsnotify unavailable, falling back to polling")
  runPolling(ctx)
```

**降级策略**：
- 优先用 fsnotify（OS 内核事件，零轮询开销）；
- 某些环境不行（**NFS、SMB、Docker bind-mount 跨平台、inotify limit 爆**）立刻切轮询；
- 用户也可以强制 polling（`SetPollingFallback(true)`）。

### 5.3 `runFsnotify` 的 debounce 技巧（L98）

```
timer := nil
loop:
  if timer == nil:
      block on Events / Errors / Ctx     # 完全 idle
      if shouldEnqueue(ev):
          ensureTimer()                   # 起 500ms 定时器
  else:
      select:
          ev:  pending[ev.Path]=true; ensureTimer()  # 累积
          err: log warn
          timer.C: flushPending(); timer=nil         # 500ms 无新事件 → 批量回调
          ctx.Done: stop
```

**为什么要 debounce？**

- IDE 保存一个文件 = fsnotify 触发 5-10 次事件（CHMOD + WRITE + CLOSE_WRITE + ...）；
- 用户"git checkout"切分支 = 几百到几千事件 1 秒内风暴；
- 500ms 内"连续事件"视作**一批**，只调一次 `onChange + InvalidateCache`。

### 5.4 `shouldEnqueue` 自动**跟踪新目录**（L168）

```
shouldEnqueue(ev, fsw):
  if ev.Op & Create != 0 && isDirectory(ev.Name):
      fsw.Add(ev.Name)                   # 新目录进入 watch list
      # 递归也要加：用户 mkdir -p a/b/c 时只有 a 的事件，b/c 事件错过
      ...
  if ev.Name in ignored OR ext not supported: return false
  return true
```

**新目录自动 watch** 是 fsnotify 模式的关键补丁 —— 默认 fsnotify 只监听**当前目录**，不追子目录（inotify 语义）。

### 5.5 `runPolling` 后备方案（L250）

```
runPolling(ctx):
  ticker := NewTicker(pollInterval)     # 默认 2s
  for:
      case ctx.Done: return
      case ticker.C: poll()

poll():
  current := snapshot of all files (path → mtime)
  for path, mtime in current:
      if old := previous[path]; old != mtime: onChange(path)
  for path in previous: if not in current: onChange(path)  # 删除
  previous = current
```

朴素轮询，每 2s 对比 mtime。**CPU/IO 代价比 fsnotify 高得多**，只作 fallback。

---

## 6. 与其他模块的协作

### 6.1 Indexer ← Orchestrator / Planner

- 用户首次 "索引当前项目"：API handler → `Indexer.IndexRepository(wsRootDir, projectName)`；
- Planner 的 "探索型任务" 步骤 1 通常是 "`index_repository` 再检索"。

### 6.2 RepoMap → Orchestrator.buildSystemMessage

```go
repoMap, _ := generator.Generate(ws.RootDir)
systemMsg += "\n[Project Structure]\n" + repoMap
```

让 LLM 一眼看到项目全景，不至于乱写路径。

### 6.3 Watcher → 三路下游

```
Watcher.onChange(path)
    │
    ├──▶ Generator.InvalidateCache(rootDir)     # repomap 下次重生成
    ├──▶ (演进) Indexer.ReIndexFile(path)       # 增量喂 RAG
    └──▶ (演进) 前端 WebSocket 推送（热更新）
```

当前 `onChange` 回调**用户自己注入**，是否触发 indexer/WebSocket 由 caller 决定 —— 解耦。

---

## 7. 设计权衡

| 抉择 | 动机 |
|---|---|
| **indexer + repomap 拆成两个包** | 职责不同（语义 vs 结构）；缓存策略不同；演进速度不同 |
| Indexer **基于 SHA-256 增量**而非 mtime | mtime 可被 `touch` 骗；hash 绝对可靠 |
| Indexer checksums **内存存**（非落盘） | 简单；进程重启全量重刷在可接受范围；真要落盘用 Redis/SQLite 见演进 |
| **[OPT-17] 合并 `ReadFile` 与 hash 检查** | 省一次 I/O；大仓库下 QPS 显著提升 |
| 并发硬编码 **8** | I/O 主导场景下 4-16 差异不大；8 防打爆 FD |
| 大文件阈值 **1 MB** | 超过基本是 minified / 生成文件；切块收益低 |
| Repomap 用 **正则而非 tree-sitter** | 签名级别够用；快；无新依赖；AST 精确留给 rag |
| Repomap **TTL 缓存 + Watcher 主动失效** | 两层保险：没 watcher 时 TTL 兜底；有 watcher 时瞬时失效 |
| Watcher 先 **fsnotify**，失败再 **polling** | fsnotify 快但不通用；polling 慢但万能；双路径覆盖 Docker bind-mount 等边角 |
| Watcher debounce **500ms** | 应对"保存 1 次 → 内核多事件"+"git checkout 风暴" |
| Watcher 自动**跟踪新目录** | 默认 inotify 只监听当前目录，必须补递归 add |
| `onChange` 回调**用户注入** | 解耦：watcher 只发事件，下游各自消费 |
| Repomap `FormatCompact` 额外格式 | 某些 token-敏感场景要 LLM 一眼过的紧凑版 |
| IndexStats 返回 **错误列表**而非整体失败 | 单文件失败不挡后续；调用者自己决定容忍度 |
| Context cancel 语义不完整（`break` bug） | 已识别，后续修为 `goto out`；当前功能正确 |

---

## 8. 后续演进

- [ ] **Checksums 持久化**：落 Redis / SQLite，重启后增量依然有效；
- [ ] **Context cancel 修正**：`break` → `goto out` / `return`；
- [ ] **并发度自适应**：按 CPU 核数 / 磁盘类型动态调（SSD 可开到 16+，HDD 降到 4）；
- [ ] **大文件智能切**：1 MB 不是简单跳，而是切成多段各自 embed；
- [ ] **更多语言 AST**：扩展 supportedExtensions（Kotlin/Swift/Scala/...）；
- [ ] **Watcher → Indexer 级联**：onChange 自动触发单文件 re-index；
- [ ] **符号深度**：Repomap 支持"展开指定文件到类/函数内部签名"的第二级视图；
- [ ] **Repomap diff 输出**：用户只想知道"和上次相比哪些文件变了"；
- [ ] **.gitignore 兼容**：忽略规则读取项目 `.gitignore` 而非硬编码列表；
- [ ] **.aiignore 自定义**：让用户显式告诉 Agent 哪些目录不要索引；
- [ ] **IDE 打开状态感知**：LSP 集成，优先索引/展示用户正在编辑的文件；
- [ ] **前端 WebSocket 热推**：文件变化实时推到前端文件树；
- [ ] **Repomap 分页**：大仓库 10k 文件时一次性字符串过大，改成"按子目录分块"；
- [ ] **Metrics**：`indexer_files_indexed_total / indexer_duration_seconds / repomap_cache_hit_ratio / watcher_events_total{type}`。

---

## 11. 实现剖析与改进方向

### Indexer 的增量策略

```text
indexRepository(repoPath):
  1. walk(repoPath) 收集所有 source files（过滤 .git / node_modules 等）
  2. 对每个 file:
     hash := sha256(file content)
     stored := lookupHashInMetadata(file.path)
     if hash == stored:
         skip  # 文件没变，省 embedding API 钱
     if hash != stored:
         chunks := parseWithAST(content)
         embeddings := embedder.Embed(chunks)
         store.Upsert(chunks with embeddings)
         updateMetadata(file.path, hash)
  3. InvalidateSparseIndex (下次 SearchSparse 会重建 BM25)
```

**冷启动**：10k files 全新索引 ~30 min（embedding API 是瓶颈）。
**热路径**：每次 IDE save 触发 1 file 重新索引 ~500ms。

### Pros
- ✅ 内容 hash 去重，避免重复 embedding 调用（省钱）
- ✅ Watcher debounce 避免 IDE 保存风暴
- ✅ gitignore-aware，不索引 node_modules

### Cons
- ⚠️ 索引失败无重试（文件读失败就跳过）
- ⚠️ 并发索引粒度太粗（单 goroutine 处理）
- ⚠️ Repomap 按 line count 排 importance，不看 "被依赖次数"
- ⚠️ 没有索引进度展示（长任务看不到卡在哪）

### 改进方向
- **P0** — 索引失败入死信队列，定期重试
- **P1** — 并行索引（多 goroutine 同时跑不同文件）
- **P1** — Repomap importance 用 "被 import 次数" 更合理
- **P2** — Webhook 触发（GitHub push → 自动增量索引）

---

下一篇：`16_store.md` —— 持久化层：PostgreSQL 存储（tasks / audit / workspaces）+ Redis JWT 撤销黑名单。
