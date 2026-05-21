# 14 · Workspace 管理 `internal/workspace`

> 代码：
> - `internal/workspace/manager.go` (343) — `Manager + Workspace`：目录隔离、路径穿越防护、持久化 manifest、tar.gz 归档
> - `internal/api/workspace_handlers.go` (354) — REST 端点：list / tree / read / write / delete / mkdir / download

---

## 1. 模块定位

**"给每个 session 划一个沙盒目录，让 Agent 在里面随便 rm -rf 都不会伤到宿主机。"**

Agent 会做的事情：

- `write_to_file: cmd/main.go` — 创建新文件；
- `execute_command: go build` — 在项目根目录编译；
- `git init / git commit` — 启动版本控制；
- `replace_in_file: config.yaml` — 原地修改。

**这些操作必须落在某个确定的目录**。本包负责：

1. **目录隔离**：每个 workspace 一个独立子目录，session 之间互不干扰；
2. **路径穿越防护**：所有 `WriteFile("../../etc/passwd")` 这类请求必须被拒绝；
3. **持久化 manifest**：进程重启后能恢复之前的 workspace 列表；
4. **可下载归档**：用户做完项目能一键 `tar.gz` 拉走；
5. **REST 管理 API**：前端 WorkspacePage 的文件树、编辑器、下载按钮都走这里。

注意：**本包不管容器沙箱** —— 沙箱执行是 `05_sandbox`；workspace 只是**宿主机侧的工作目录**，真正跑命令时会把这个目录挂载进沙箱容器（`sandbox.volume.go` 负责）。

---

## 1.5 核心设计问题

### 为什么 workspace 是一级概念而不是 session 的附属？

Chat session 短时（几分钟到几小时）；但用户可能：
- 同一 workspace 跨多个 session（新开 tab 继续）
- 一个 session 不要 workspace（纯对话）
- 多个 session 共享 workspace（协作审阅）

解耦 session / workspace 让上述场景都成立。workspace 的生命周期独立于
session，由显式的 Create/Cleanup 管理。

### 路径穿越防御为什么放在 workspace 层？

每个 I/O 调用都可能被攻击者传 `../../../etc/passwd` 逃出 workspace。
如果每个 handler / tool 都自己写路径校验，**第 N 个一定漏**。

**解决**：`safePath(wsRoot, rel) → abs` 作为所有 I/O 的**唯一入口**。
实现逻辑：
1. `filepath.Join(wsRoot, rel)`
2. `filepath.Clean` 规范化
3. 检查 result 以 `wsRoot + os.Separator` 开头

**TOCTOU 注意**：symlink 穿越需要 `EvalSymlinks` 校验——目前代码用字符串
前缀检查，对符号链接逃逸**不完全防护**（见 P1 待办）。

### 为什么 Tmpfs vs Bind Mount 的选择权给用户

- **Tmpfs**（默认）：速度快、完全隔离、容器停止即灰飞烟灭 → 适合"一次性"任务
- **Bind mount**：持久化到宿主机、可跨容器复用 → 适合"要缓存 node_modules"

bind mount 本身是沙箱逃逸面（挂错一个目录等于 root 权限泄露），所以
`volume.go` 强制：
- 默认只读
- 黑名单（docker.sock / /etc / /proc / /sys）
- 写权限必须显式要求

---

## 2. 依赖架构

```
┌────────────────────────────────────────────────────────┐
│  前端 WorkspacePage.tsx                                 │
│  (Monaco 编辑器 + 文件树 + 下载按钮)                      │
└───────────────┬────────────────────────────────────────┘
                │ HTTP
                ▼
┌────────────────────────────────────────────────────────┐
│  /api/v1/workspaces/* (workspace_handlers.go)          │
│    list/tree/read/write/delete/mkdir/download          │
└───────────────┬────────────────────────────────────────┘
                │
                ▼
┌────────────────────────────────────────────────────────┐
│            workspace.Manager                           │
│  CRUD + safePath + Archive + restore                   │
└────────────┬───────────────────────────────────────────┘
             │
   ┌─────────┼────────┬──────────────┐
   ▼         ▼        ▼              ▼
┌────────┐ ┌─────┐ ┌────────┐   ┌──────────┐
│ os.*   │ │tar  │ │sync.Map│   │ manifest │
│ WriteF │ │ .gz │ │索引 id │   │  .json   │
└────────┘ └─────┘ └────────┘   └──────────┘

   ┌────────── 上游消费者 ──────────┐
   │                              │
   ▼                              ▼
orchestrator                  sandbox.Manager
  file_tools.go                (volume mount)
  auto_test_runner.go
  edit_engine.go
  git_tools.go
```

---

## 2.5 数据流总览

```text
┌───────────────────────────────────────────────────────────────┐
│ orchestrator / API handler                                    │
│   CreateForSession(sessionID) 或 resolveWorkspace(reqID)      │
└─────────────────────────────┬─────────────────────────────────┘
                              │
                              ▼
┌───────────────────────────────────────────────────────────────┐
│ workspace.Manager.CreateForSession(sessionID)                 │
│   sync.Map 查重 → 已存在则幂等返回                             │
│   os.MkdirAll(/tmp/agent-workspaces/<id>/)                   │
│   saveManifest(.manifest.json)                                │
└─────────────────────────────┬─────────────────────────────────┘
                              │ (*Workspace)
                              ▼
┌───────────────────────────────────────────────────────────────┐
│               所有 I/O 操作入口                                │
│  WriteFile / ReadFile / DeleteFile / MkdirAll / ListFiles     │
└─────────────────────────────┬─────────────────────────────────┘
                              │
                              ▼
┌───────────────────────────────────────────────────────────────┐
│              ★ safePath(base, rel) ★                          │
│  ① filepath.Clean(rel)                                        │
│  ② filepath.IsAbs → reject                                   │
│  ③ HasPrefix(joined, base + separator) → reject if false     │
│  任何路径穿越尝试在此被拦截                                    │
└─────────────────────────────┬─────────────────────────────────┘
                              │ (安全绝对路径)
                              ▼
         ┌────────────────────┼────────────────────┐
         │                    │                    │
         ▼                    ▼                    ▼
┌──────────────┐    ┌──────────────┐    ┌──────────────────┐
│  os 文件操作  │    │ 【sandbox】  │    │  Archive()       │
│  Read/Write  │    │ volume mount │    │  tar.gz 打包     │
│  Delete/List │    │ → /workspace │    │  → HTTP 响应流   │
└──────────────┘    └──────────────┘    └──────────────────┘

服务重启恢复流程:
┌────────────┐     ┌──────────────────┐     ┌──────────────┐
│  启动扫描   │──▶  │ 读 .manifest.json│──▶  │ 注册到       │
│  baseDir/* │     │ 恢复 Workspace   │     │ sync.Map     │
└────────────┘     └──────────────────┘     └──────────────┘
```

---

## 3. 核心数据模型

```go
// manager.go:22
type Workspace struct {
    ID        string    // 唯一 ID，通常等于 session ID
    SessionID string    // 绑定的 session ID（可空，支持"游离 workspace"）
    RootDir   string    // 宿主机上的绝对路径，如 /var/lib/agent/ws/abc123
    Project   string    // 人类可读的项目名，如 "my-fastapi-app"
    CreatedAt time.Time
}

// manager.go:31
type Manager struct {
    baseDir    string          // 所有 workspace 的父目录
    workspaces sync.Map        // id → *Workspace (并发安全)
    logger     *zap.Logger
}
```

为什么用 `sync.Map` 而不是 `map + Mutex`？

- **读多写少**场景最优解（sync.Map 对"几乎只读"case 近乎 lock-free）；
- workspace 的创建/删除是低频事件，读（Get/List）是高频事件；
- 标准 `map+RWMutex` 在 500 QPS 下仍然会在 RLock 上有竞争开销。

---

## 4. ★ 安全核心：`safePath` (L266)

```go
safePath(ws, relPath) (absPath string, err error):
  cleaned := filepath.Clean(relPath)              // "a/../b" → "b"
  if filepath.IsAbs(cleaned): return ERROR        // 拒绝 "/etc/passwd"

  absPath := filepath.Join(ws.RootDir, cleaned)
  # 关键检查：拼接后的路径是否仍在 rootDir 下？
  if !strings.HasPrefix(absPath, ws.RootDir + sep):
      if absPath != ws.RootDir:
          return ERROR ("path traversal detected")
  return absPath, nil
```

### 4.1 为什么这样做？

攻击样本：

| 恶意输入 | `filepath.Clean` 后 | `filepath.Join(rootDir, ...)` 后 | 落哪 |
|---|---|---|---|
| `../secret.txt` | `../secret.txt` | `/var/lib/agent/ws/secret.txt` | **跳出 ws** |
| `/etc/passwd` | `/etc/passwd` | Go 在 Unix 下会拼成 `/etc/passwd` | **跳出 ws** |
| `a/../../b` | `../b` | `/var/lib/agent/b` | **跳出 ws** |
| `./safe.go` | `safe.go` | `/var/lib/agent/ws/abc/safe.go` | ✅ 正常 |

前三种都会被 `HasPrefix(absPath, rootDir+"/")` 拦下。**第二种靠 `IsAbs` 早期判断** 直接报错。

### 4.2 为什么要 `+ string(filepath.Separator)`？

不加分隔符会被 `/var/lib/agent/ws/abcdef` 这类"rootDir + 任意后缀"骗过：

```
rootDir = "/var/lib/agent/ws/abc"
absPath = "/var/lib/agent/ws/abcdef-attack"   # 不在 abc/ 下
HasPrefix(absPath, rootDir) → true            # ★ Bug!
```

加了分隔符：

```
HasPrefix(absPath, rootDir + "/") → false     # ✅ 正确拦截
```

这是 Go 代码审计里**经典漏洞**，务必注意。

### 4.3 所有 I/O 方法都先调 `safePath`

`WriteFile / ReadFile / DeleteFile / MkdirAll` 的第一行都是：

```go
absPath, err := m.safePath(ws, relPath)
if err != nil { return err }
```

**单点拦截 + 强制**，避免各处重复实现防护。

---

## 5. 生命周期：Create / Restore / Cleanup

### 5.1 `CreateForSession` (L58)

```text
CreateForSession(id, sessionID, projectName):
  if exists := m.Get(id): return exists         # 幂等

  rootDir := baseDir + "/" + id
  os.MkdirAll(rootDir, 0755)

  ws := &Workspace{ID: id, SessionID: sessionID, RootDir: rootDir, Project: projectName, CreatedAt: now}
  m.workspaces.Store(id, ws)
  m.saveManifest(ws)                            # 写 .manifest.json
  return ws
```

**幂等**是关键：同一 session 多次调 `CreateForSession` 要返回同一个 workspace，不能创建第二个（会导致历史文件"丢失"）。

### 5.1.1 ★ 租户隔离：`ResolveSessionWorkspace` 的 fallback 陷阱（P0 #15 修复）

> ⚠️ **修复前的 bug**：`orchestrator.ResolveSessionWorkspace(sessionID)`
> 当 `Get(sessionID)` 没命中、`Create` 又失败时，fallback 到
> `resolveWorkspace("")` → `ListWorkspaces()[0]`——**返回另一个租户的
> workspace**！
>
> ```
> session-A 创建 workspace A（存在）
> session-B 调 ResolveSessionWorkspace("session-B")
>   → Get 失败（没创建过）
>   → Create 失败（例如磁盘满、路径冲突）
>   → fallback ListWorkspaces()[0] → 返回 workspace A
> session-B 开始读写 session-A 的文件！  ← 跨租户数据泄露
> ```

**修复**：`orchestrator/file_tools.go:749-782` 去掉 fallback。创建失败返回
`nil` + log error，由上层 handler 翻成 500 并拒绝服务。绝**不**用一个不
属于请求者的 workspace 继续工作。

```go
func (o *Orchestrator) ResolveSessionWorkspace(sessionID string) *workspace.Workspace {
    if o.workspaceMgr == nil { return nil }
    if sessionID == "" {
        o.logger.Warn("ResolveSessionWorkspace called with empty sessionID")
        return nil   // 空 session 不会 fallback 到默认
    }
    if ws, ok := o.workspaceMgr.Get(sessionID); ok { return ws }

    // 防止 sessionID < 8 字符导致切片 panic
    label := "session-" + sessionID
    if len(sessionID) > 8 { label = "session-" + sessionID[:8] }

    ws, err := o.workspaceMgr.Create(sessionID, label)
    if err != nil {
        o.logger.Error(
            "failed to create session workspace — refusing to fall back (would cross tenants)",
            zap.String("session_id", sessionID), zap.Error(err))
        return nil   // ← 绝不返回别人的 workspace
    }
    return ws
}
```

**相关 `resolveWorkspace("")` 收紧**：原来在没有 `default` workspace 时
也会 `return list[0]`；现改为**只**匹配 `project=="default"` 的 workspace，
否则创建新的。见 `file_tools.go:774-803`。


### 5.2 `saveManifest` (L95)

```go
saveManifest(ws):
  manifestPath := rootDir + "/.manifest.json"
  data, _ := json.Marshal(ws)
  os.WriteFile(manifestPath, data, 0o644)
```

把 `Workspace` 元数据（ID / SessionID / Project / CreatedAt）以 JSON 落到**每个 workspace 根目录下的 `.manifest.json`**。好处：

- 单个 workspace 完全自描述（目录 + manifest 即可重建）；
- 不依赖中心化 DB，重启不需要查 PG；
- 用户要"把 workspace 整包迁移"时一起打包即可。

### 5.3 `restore` (L109)

```
restore():
  entries := os.ReadDir(baseDir)
  for each entry:
    if not dir: skip
    manifestPath := baseDir/entry.Name()/.manifest.json
    data := os.ReadFile(manifestPath)
    var ws Workspace
    json.Unmarshal(data, &ws)
    m.workspaces.Store(ws.ID, &ws)
```

服务启动时扫 `baseDir` 下所有子目录，找 `.manifest.json` 读回内存。**自动自愈**：即使宿主机重启、pod rescheduled，只要 PV 还在，workspace 全自动恢复。

### 5.4 `Cleanup` (L255)

```
Cleanup(id):
  ws := Get(id)
  m.workspaces.Delete(id)
  os.RemoveAll(ws.RootDir)                      # 物理删除整个目录
```

**硬删除**。调用时机：

- 用户主动调 DELETE /workspaces/:id（暴露在 API 层但默认需要授权）；
- 定时清理任务（当前无；后续演进）；
- E2E 测试结束 teardown。

---

## 6. I/O 操作集

```go
WriteFile(ws, relPath, content)     // 自动 MkdirAll 父目录
ReadFile(ws, relPath)               // 返回 string
DeleteFile(ws, relPath)             // idempotent (NotExist 忽略)
MkdirAll(ws, relPath)               // 递归建目录
ListFiles(ws)                       // 返回所有文件相对路径（filepath.Walk 递归）
ListDir(ws, relPath)                // 返回单层目录内容
TreeString(ws)                      // 返回可读的 tree 结构字符串
```

### 6.1 默认权限

- 目录：`0755`
- 文件：`0644`

没有设计成可配。因为 workspace 本身就在容器 / K8s Pod 内，不涉及多用户 Linux 权限。

### 6.2 `DeleteFile` 的幂等性

```go
if err := os.Remove(absPath); err != nil && !os.IsNotExist(err) {
    return err
}
```

**删除不存在的文件不算错误**。因为 Agent 可能在一次循环里反复调用"清理目录"，用户不该看到 "file not found" 的脏错误。

---

## 7. `Archive` —— tar.gz 打包下载（L219）

```
Archive(ws, w io.Writer):
  gw := gzip.NewWriter(w)
  tw := tar.NewWriter(gw)

  filepath.Walk(ws.RootDir):
    for each file:
      rel := relative to ws.RootDir
      header := tar.FileInfoHeader(info)
      header.Name = project + "/" + rel          # 解压后带项目名顶层目录
      tw.WriteHeader(header)
      if not dir: io.Copy(tw, file)
```

**流式写入**：不用先 archive 到磁盘再发出，直接写到 `io.Writer`（通常是 gin 的 `c.Writer`）。对大项目（几百 MB）无需中间缓存。

### 7.1 头部项目名前缀

解压后形如：

```
my-fastapi-app/
├── main.py
├── requirements.txt
└── tests/
    └── test_main.py
```

不是直接散落在当前目录里 —— 用户体验标配。

---

## 8. REST API 层 `workspace_handlers.go`

### 8.1 端点一览

| 方法 | 路径 | 功能 |
|---|---|---|
| GET | `/workspaces` | list：返回所有 workspace（id/session/project/createdAt） |
| GET | `/workspaces/:id/tree?path=...` | 文件树（递归，返回 `FileTreeNode` 数组） |
| GET | `/workspaces/:id/file?path=...` | 读文件内容（带语言检测） |
| PUT | `/workspaces/:id/file` | 写文件（body 含 path + content） |
| DELETE | `/workspaces/:id/file?path=...` | 删文件 |
| POST | `/workspaces/:id/mkdir` | 建目录 |
| GET | `/workspaces/:id/download` | 下 tar.gz（见 §7） |

### 8.2 `FileTreeNode` 与 `buildFileTree` (L35/L65)

```go
type FileTreeNode struct {
    Name     string
    Path     string          // relative to workspace root
    IsDir    bool
    Size     int64
    Children []*FileTreeNode // 递归
    Language string          // 根据后缀推断，前端 Monaco 用来决定 syntax
}
```

`buildFileTree` 递归遍历，**自动跳过常见 junk**：`node_modules / .git / __pycache__ / .DS_Store`（见源码实际列表）。好处：前端侧边栏不会被 10k 个 node_modules 文件撑爆。

### 8.3 `detectLanguage(path)` (L292)

```
.go → "go"
.py → "python"
.ts / .tsx → "typescript"
.js / .jsx → "javascript"
.rs → "rust"
.yaml / .yml → "yaml"
.json → "json"
.md → "markdown"
...
```

前端 Monaco `language` prop 直接用这个值，渲染正确的高亮。

### 8.4 `resolveWorkspace(c)` (L259)

```
resolveWorkspace(c):
  id := c.Param("id")
  ws := wm.Get(id)
  if not found: c.JSON(404, error); return nil
  return ws
```

所有 handler 的第一步，**单点鉴权**的挂钩点（后续可以在这里注入 "这个用户是否能访问 workspace X" 的校验）。

---

## 9. 与其他模块的协作

### 9.1 Session → Workspace 绑定

`CreateForSession(id, sessionID, projectName)` 把两者联起。Orchestrator 在处理第一条 user message 时：

```go
ws := workspaceMgr.GetBySession(sessionID)
if ws == nil:
    ws = workspaceMgr.CreateForSession(newID, sessionID, projectName)
```

### 9.2 Workspace → Sandbox 挂载

Sandbox 启容器时会把 `ws.RootDir` 作为 volume 挂载到容器内（通常挂到 `/workspace`）：

```go
sandbox.Run(cmd, &Options{
    VolumeBinds: []string{ws.RootDir + ":/workspace"},
    WorkDir:     "/workspace",
})
```

让 `go build` 的编译产物自动出现在宿主机 workspace 目录里，用户可以立刻在前端看到。

### 9.3 Workspace → Git

`orchestrator/git_tools.go` 的 `ensureGitInit(ws)` 在第一次 git 操作前自动 `git init` 这个目录。这样用户无需手动初始化就能有版本控制。

### 9.4 Workspace → RAG Indexer

当用户说"索引当前项目"时，indexer 直接读 `ws.RootDir` 下所有文件走 RAG 入库（见 `15_indexer_repomap`）。

---

## 10. 设计权衡

| 抉择 | 动机 |
|---|---|
| **每个 session 独立目录** 而非共享 | 安全隔离 + 并发互不干扰 + 可独立归档 |
| `safePath` **三重校验**（Clean + IsAbs + HasPrefix+sep） | 路径穿越是经典 OWASP Top-10，必须多层防御 |
| `sync.Map` 索引 | 读多写少最优解；避免 RWMutex 竞争 |
| 每个 workspace **一个 `.manifest.json`** | 自描述 + 不依赖中心 DB + 可整体迁移 |
| 进程启动时 `restore()` 扫盘 | 重启自愈；无需外部存储；天然 idempotent |
| `CreateForSession` **幂等** | 同 session 多次调用不会丢文件；防御性好 |
| `DeleteFile` **NotExist 不算错** | Agent 会多次重试清理；用户不该看到脏错误 |
| `Archive` **流式 tar.gz** | 大项目不占中间内存 |
| `buildFileTree` **跳 junk 目录** | 前端体验；node_modules 没人看 |
| REST 层单独文件 `workspace_handlers.go` | 分离"核心能力"与"HTTP 胶水" |
| 默认 `0644/0755` 权限不可配 | 容器内场景下调权限无价值；避免噪音配置 |
| 不做 **磁盘配额** | 当前在 K8s Pod 里由 ephemeral storage limit 兜底；未来可加 |
| 不做 **Workspace 生命周期自动回收** | 当前显式清理；防止误删重要上下文；TTL 留到后续 |

---

## 11. 后续演进

- [ ] **磁盘配额**：单 workspace 上限 1 GB，超了拒绝写入；可用 Linux quota 或用户层统计；
- [ ] **生命周期 TTL**：闲置 N 天未访问自动归档 → S3 / 冷存；
- [ ] **软删除**：DELETE 不立即 rm -rf，先标记 `deleted_at`；N 天后真删；
- [ ] **用户鉴权**：`resolveWorkspace` 里校验 `userID == ws.OwnerID`；
- [ ] **Git-based 版本管理**：workspace 自动 commit 每次编辑，WSL 级别的时光回溯；
- [ ] **多 workspace link**：允许 workspace A 软链接引用 workspace B 的部分目录（monorepo 场景）；
- [ ] **Workspace 模板**：prebuilt templates（FastAPI / Next.js / Go-gin），一键创建即可用；
- [ ] **文件 watcher**：监听 workspace 变化实时推送给前端（WebSocket），多人协作；
- [ ] **Workspace 镜像化**：把 workspace 打包成 Docker image，用户随时拉起与当时一致的环境；
- [ ] **压缩存储**：不常访问的文件自动 zstd 压缩，节省磁盘；
- [ ] **Metrics**：`workspace_created_total / workspace_disk_usage_bytes{id} / workspace_file_ops_total`。

---

## 11. 实现剖析与改进方向

### safePath 的实际校验

```go
func (m *Manager) safePath(ws *Workspace, rel string) (string, error) {
    if strings.Contains(rel, "..") {
        return "", ErrPathTraversal   // 快速拒绝常见攻击
    }
    abs := filepath.Join(ws.RootDir, rel)
    abs = filepath.Clean(abs)          // 规范化 ../ 和 ./
    if !strings.HasPrefix(abs, ws.RootDir + string(filepath.Separator)) &&
       abs != ws.RootDir {
        return "", ErrPathTraversal   // 逃出 workspace 根
    }
    return abs, nil
}
```

**TOCTOU 未防护**：symlink `ws/evil → /etc/passwd` 通过字符串检查但
读取时跟随 symlink。需要 `os.Lstat + EvalSymlinks` 才能彻底防。

### Pros
- ✅ 所有 I/O 走 safePath 单入口，不会漏校验
- ✅ ResolveSessionWorkspace 严格拒绝 fallback（P0 #15）
- ✅ Tmpfs + Bind Mount 二选一，按需选
- ✅ Cleanup 幂等，重复调不报错

### Cons
- ⚠️ Symlink 逃逸未完全防护（TOCTOU）
- ⚠️ 文件数 / 总大小无配额（用户可以把 workspace 撑到几 GB）
- ⚠️ Manifest 是 JSON 文件无 checksum，能被篡改
- ⚠️ Windows 路径分隔符兼容未测试

### 改进方向
- **P0** — safePath 加 `filepath.EvalSymlinks` 防 symlink 逃逸
- **P0** — workspace 配额：`ws.MaxBytes / ws.MaxFiles`
- **P1** — Manifest 加 HMAC 签名（被手改就失效）
- **P2** — 支持远程 workspace（S3 / git clone 到本地）

---

下一篇：`15_indexer_repomap.md` —— Indexer + RepoMap：启动索引器 + 文件监视器，增量维护 RAG 向量库与项目地图。
