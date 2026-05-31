# 基础设施子系统

覆盖 RAG、Sandbox、MCP、Temporal、Store、Workspace、Indexer、Repomap 八个基础设施子系统的架构、关键类型和集成点。

## RAG 管线

`internal/rag/`，18 个文件。完整的双召回检索增强生成管线。

### 整体流程

```
源代码 → AST 分块 → 嵌入(OpenAI/LocalHash) → Qdrant 存储
            ↓
查询 → 嵌入 → [稠密检索 ∥ BM25 稀疏检索] → 去重 → 可选重排 → 返回 chunks
```

### AST 感知分块

`internal/rag/ast_parser.go` (`parseWithAST`) 按语言分发：

| 语言 | 解析方式 | 粒度 |
|------|----------|------|
| Go | `go/parser` + `go/ast` 原生 AST | FuncDecl（含 receiver → "Receiver.Method"）, TypeSpec, ValueSpec |
| Python | 启发式（缩进 + 关键字） | class, def |
| Markdown | ATX 标题感知 | heading 层级，保持代码块完整，合并 <20 字符小节 |
| Shell | 函数级 | function |
| 其他 | 滑动窗口 | 固定大小 chunk |

Go AST 分块（`internal/rag/ast_native.go`）在解析失败时回退到启发式。每个 chunk 携带 `symbolName`, `symbolType`, `content`, `startLine`, `endLine`, `dependencies`（调用依赖）。

### 嵌入

两种嵌入器，自动选择：

- `OpenAIEmbedder` (`internal/rag/embedder.go`) — OpenAI 兼容 API，批量 128，默认模型 `text-embedding-3-small`。若 RAG 专用凭据缺失，回退到 LLM primary 配置
- `LocalHashEmbedder` (`internal/rag/local_embedder.go`) — FNV-64a 哈希随机投影 + bigram，L2 归一化。零成本离线回退，用于开发/测试

**嵌入缓存**：`CachedEmbedder` (`internal/rag/embedding_cache.go`) 包装任意 Embedder，内容哈希（SHA-256 前缀）→ 向量 LRU 缓存。按模型名命名空间防止跨模型污染。默认容量 10K 条目（~60MB for 1536-dim）。批量缓存未命中合并为单次 API 调用。

### 双召回

`Engine.Retrieve()` 并行启动两个 goroutine：

1. **稠密检索**：`QdrantStore.SearchDense()` — gRPC 查询 Qdrant，支持 payload 过滤
2. **稀疏检索**：`QdrantStore.SearchSparse()` — 委托本地 `BM25Index`（惰性构建，5 分钟 TTL，从 Qdrant 全量 scroll 重建）

BM25 参数：k1=1.2, b=0.75（Lucene 默认值）。`tokenizeForBM25` 拆分 camelCase/snake_case，去除停用词。设计规模 ≤100K chunks。

去重后可选重排：`APIReranker` (`internal/rag/reranker.go`) POST `/reranks`（Jina 兼容格式），30s 超时。重排失败时 graceful fallback 到原始分数排序。

### 重连

`ReconnectManager` (`internal/rag/reconnect.go`)：定期 Qdrant 重连，使用 `atomic.Pointer[Engine]` 热替换。

## Sandbox

`internal/sandbox/`，8 个文件。Docker 隔离代码执行环境。

### 安全模型（防御纵深）

`buildHostConfig()` 创建的容器配置：

| 防御层 | 配置 |
|--------|------|
| 网络隔离 | `NetworkMode: "none"`（默认，可配置） |
| 进程限制 | `PidsLimit: 256` |
| 权限剥离 | `CapDrop: ALL` + `SecurityOpt: "no-new-privileges"` |
| 文件系统 | `ReadonlyRootfs: true` + Tmpfs: `/workspace`(rw,128m) + `/tmp`(rw,noexec,nosuid,64m) |
| 输出限制 | LimitReader 2MB（每流 1MB），截断标记 |

### 执行流程

```
resolveRuntime → ensureImage → 解析内存/CPU限制 → ContainerCreate
→ copyCodeToContainer(tar) → ContainerStart → ContainerWait → collectOutput → ForceRemove
```

所有语言使用 tar 文件注入（无 shell 转义）。语言映射：python→main.py, go→main.go, node→main.js, bash→script.sh。

### 暖池

`WarmPool` (`internal/sandbox/warm_pool.go`)：按语言预热容器池。

- 每语言 buffered channel
- `Acquire()` 最大等待 50ms，超时回退到冷路径
- 容器运行 `sleep infinity`，通过 `docker exec` 使用
- 单次使用后销毁（不复用）
- `replenishLoop` 按语言独立运行，失败时指数退避
- 效果：延迟从 ~800ms 降到 ~90ms

### 卷挂载变体

`ExecuteWithVolume` (`internal/sandbox/volume.go`)：bind-mount 模式，用于项目生成器构建验证。语言→镜像映射：go→golang:1.23-alpine, python→python:3.12-slim, node→node:20-slim, rust→rust:1.78-slim, default→alpine:3.20。

## MCP（Model Context Protocol）

`internal/mcp/`，7 个文件。JSON-RPC 2.0 客户端，管理外部工具服务器。

### 单连接

`ServerConnection` (`internal/mcp/client.go`)：单个 MCP 服务器进程。

- stdin/stdout 管道通信（NDJSON）
- `reqID` 原子递增并发请求 ID
- `pending map[int64]chan *JSONRPCResponse` reactor 模式响应路由
- `writeMu` 序列化 stdin 写入（与 pending 锁分离避免死锁）
- `readResponses()` goroutine 处理 3 种帧：响应（按 ID）、`notifications/progress`（按 progressToken）、其他通知（丢弃）

### Gateway

`Gateway` (`internal/mcp/client.go`)：多服务器聚合器。**每个服务器背后是一个 ConnPool**，不再是裸 `*ServerConnection`。

- `servers map[string]*ConnPool`
- 初始化：`NewGateway` → `pool := NewConnPool(serverCfg) → pool.Start(ctx, gw.initializeServer)`，handshake 闭包封装 `initialize` → `notifications/initialized` → `tools/list` 三步
- `toolIndex map[string]string` O(1) 查询 toolName → serverName
- `CallTool` 委托 `pool.CallTool`（least-pending + atomic CAS 选择 slot）
- 运行时动态管理：`AddServer`, `RemoveServer`, `ListServers` 均以 pool 为单位

### 连接池

`ConnPool` (`internal/mcp/pool.go`)：多子进程连接池。

- 由 `MCPServerConfig.PoolSize` 控制；`PoolSize<=1` 等价单连接（向后兼容，无需配置迁移）
- 最少挂起负载均衡（O(N) 扫描，N≤8）
- `Start()` 并行 fork N 个进程并完成 handshake；要求 minAlive（默认 size/2）
- `CallTool` 选择连接发送阻塞请求；内部 `parseToolResult` 合并 text content（O(n) 一次性 buf）
- `CallToolStream` 注入 `_meta.progressToken`，订阅进度通道，合并进度块与最终结果
- 测试辅助：`newSingletonPool(cfg, conn, logger)` 把已构造的 ServerConnection 包成 1-slot pool

### 健康检查

`healthChecker` (`internal/mcp/reconnect.go`)：

- `connAlive(conn)` 委托 `conn.transport.Alive()`，两套传输各自实现存活语义：
  - **stdio** (`transport.go::stdioTransport`): 两层组合 `conn.exited` 原子位 + `Process.Signal(syscall.Signal(0))`
    1. `conn.exited`——reaper goroutine 在 `cmd.Wait()` 返回时置 true；Wait 同时**回收僵尸**，这是检出"已 exit 但 PID 仍占着进程表"的唯一可靠手段
    2. `Signal(0)`——处理 exit 与 reaper 回归之间的短暂竞态：`ESRCH` 视为死，其他错误（罕见 EPERM）假定活，避免瞬时 syscall 抖动误杀健康池
    - 单独看 `Signal(0)` 不够：僵尸进程的 PID 仍可接收信号，探针会误报存活；必须组合 `exited` 位
    - **reaper 生命周期约束**：`os/exec` 文档要求 `cmd.Wait()` 不得在 StdoutPipe 还在被读取时调用。reaper 先 `readerWg.Wait()`，等 `readResponses` 因 stdout EOF 自然结束后再 Wait
  - **sse** (`transport_sse.go`): `now - lastRecv < keepaliveTimeout`（默认 90s）。任何 SSE 事件（含 endpoint/message）都会刷新 lastRecv；连接断流则下次检查死
- `processAlive(pool)` 逐 slot 调 `connAlive`；遇到死连接 CAS 清空 slot（避免 Pick 派发到死连接）。pool 实现尚无自动 replaceLoop，所以这步必须显式做
- `ConnPool.Alive()` 只查 slot 指针非空；**健康检查必须用 `processAlive`，不能只看 `Alive()`**
- 整池零活时触发 `reconnectServer`：关闭旧池 → 新建并 Start
- 指数退避：Initial=1s, Max=30s, MaxRetries=5
- **已接线**（2026-06）：`cmd/agent/main.go` MCP 初始化尾部调 `mcpGateway.StartHealthCheck(30 * time.Second)`，自愈循环以 30s tick 运转

### 传输

stdio + sse 双传输（2026-06）。差异封装在 `Transport` 接口（`internal/mcp/transport.go`）下，pool/health/reconnect 路径完全复用：

- **stdio**: fork 子进程 + JSON-RPC over stdin/stdout，由命令白名单（`config.IsAllowedMCPCommand`）守门
- **sse**: HTTP+SSE（`event: endpoint` 第一事件提供 POST URL，后续 `event: message` 承载 JSON-RPC 响应）。配置层仅要求 `url`；egress HTTP client 在 dial 时做 host 级安全 (CIDR ACL)
- 配置/网关/API 三层校验都按 transport 分支：stdio 走命令白名单，sse 仅校验 url 非空

## Temporal（HITL 工作流）

`internal/temporal/`，4 个文件。生产就绪但可选。

### 工作流

`AgentTaskWorkflow` (`internal/temporal/workflows.go`)，4 步：

1. `ParseIntentActivity` — 意图解析（当前为 stub，返回 IntentConversation）
2. `SecurityCheckActivity` — 正则 + 部署意图检查
3. **HITL 审批** — `Signal("approval-signal")` + `Timer(30min)` + `Selector` 多路等待。Timer 收到信号后取消
4. `ExecuteTaskActivity` — 委托 `orch.ProcessMessage`

Activity 配置：StartToCloseTimeout=5min, RetryPolicy{Initial=1s, Backoff=2.0, MaxInterval=1min, MaxAttempts=3}。

### 接线状态

`cmd/agent/main.go` 的 `startTemporalWorker` 完整实现——连接 Temporal 服务器，注册 workflow + activities，启动 worker。条件：`cfg.Temporal.Host != ""`。失败非 fatal（Warn 继续）。

## Store（PostgreSQL）

`internal/store/`，2 个文件。可选持久化层。

### 表结构

`Migrate()` 自动创建 4 张表：

| 表 | 主要字段 | 用途 |
|---|---------|------|
| `tasks` | id, session_id, user_id, intent, state, user_input, result | 任务记录 |
| `audit_logs` | task_id, user_id, action, details(JSONB), risk_level | 审计日志 |
| `api_keys` | key_hash, user_id, role, label | API Key 存储 |
| `approvals` | task_id, session_id, action, risk_level, status, approved_by | HITL 审批记录 |

连接池：MaxOpenConns / MaxIdleConns 可配置，ConnMaxLifetime=5min。

**不变量**：Store 故障不影响核心功能——Session 状态在 Redis，Store 仅用于持久化/审计。

## Workspace

`internal/workspace/`，1 个文件。本地文件系统管理器。

- 基础目录 + `sync.Map`（id → *Workspace）
- 路径遍历防护：`filepath.Clean` + `filepath.EvalSymlinks` + 前缀检查
- `.workspace.json` manifest 持久化，重启时 `restore()` 扫描恢复
- `CreateForSession`：幂等（相同 ID 返回已有）
- `Archive`：tar.gz 导出
- `GetBySession`：线性扫描 sync.Map
- 文件操作：WriteFile, ReadFile, DeleteFile, ListFiles, ListDir, MkdirAll

**EvalSymlinks 规范化**：`CreateForSession` 和 `restore` 在存储 `ws.RootDir` 前调用 `filepath.EvalSymlinks` 解析符号链接。这解决了 macOS 上 `/tmp` → `/private/tmp` 导致的路径遍历误报——`isPathSafe` 的 `strings.HasPrefix` 检查要求 workspace root 和目标路径都是规范化后的真实路径。修复前，macOS 集成测试会因 symlink 差异触发安全拒绝。

## Indexer

`internal/indexer/`，2 个文件。仓库增量索引。

- 遍历目录，按扩展名过滤（Go/Python/JS/TS/Java/Rust/Ruby/C/C++/Markdown/Shell/YAML/JSON/TOML/text）
- 跳过 >1MB 文件和默认忽略模式（.git, node_modules, vendor 等）
- **增量策略**：内存中维护 sha256 校验和/文件，仅重新索引内容变化的文件
- 调用 `ragEngine.IndexCode(ctx, relPath, lang, content, metadata)`
- 有界并发：信号量限制 8 个 goroutine
- `IndexRepositoryAny` 满足 `api.Indexer` 接口

**限制**：校验和存储仅在内存中——重启后需从头重新索引。

## Repomap

`internal/repomap/`，4 个文件。类似 Aider 的 repo-map，展示文件路径 + 公开符号。

### 符号提取

`Generator` (`internal/repomap/generator.go`)：正则（非完整 AST）提取符号。

| 语言 | 提取目标 |
|------|----------|
| Go | 导出 func/type/interface/const |
| Python | class/def |
| TS/JS | export |
| Rust | pub fn/struct/enum/trait |
| Java | public class/method |

- 按 rootDir 缓存，5 分钟 TTL
- 输出确定性：文件按路径排序，按目录分组
- `FormatCompact` 变体用于紧凑 token 预算

### 文件监听

`Watcher` (`internal/repomap/watcher.go`)：

- **主策略**：fsnotify 事件驱动，递归监听所有子目录，自动添加新目录
- **回退**：轮询（3s 间隔），fsnotify 不可用时使用
- **去抖**：500ms 窗口合并突发事件（如 IDE 保存）
- 变化时：使目标 rootDir 的 Generator 缓存失效，调用可选 `onChange` 回调
