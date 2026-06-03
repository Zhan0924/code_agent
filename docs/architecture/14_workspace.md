# 14 · Workspace 管理 `internal/workspace`

> 代码（**以代码为准**）：
>
> - `manager.go` (369 行) — Workspace 生命周期 + 路径沙箱 + 持久化 manifest + 重启恢复
> - `manager_test.go` (274 行) — 单元测试，覆盖 idempotent create / safePath 攻击向量 / 重启恢复
>
> 上层调用：
>
> - `cmd/agent/main.go:613` — `workspace.NewManager("/tmp/agent-workspaces", logger)` 唯一构造点
> - `internal/orchestrator/file_tools.go:935` — `ResolveSessionWorkspace` 把 sessionID 映射到 Workspace（**租户隔离修复在这里，不在 workspace 包里**）
> - `internal/orchestrator/edit_engine.go` — 原子多文件编辑（`.bak` 备份 + lint + rollback）通过 workspaceMgr 操作文件
> - `internal/pty/local.go` — PTY 会话默认在 `/tmp/agent-workspaces` 下分配工作目录
> - `internal/api/workspace_handler.go` — 文件浏览器/编辑器 REST 接口

---

## 1. 模块定位

**"会话的私有盘：每个 session 一个隔离目录，外加防穿越的安全文件 I/O。"**

Code agent 的所有"生成项目 / 修改文件 / 跑测试"动作必须落到磁盘上某个目录。
不能写全局共享盘——会话间相互污染、并发任务覆盖、租户跨越是真实威胁。

本包做 4 件事：

1. **目录隔离**：每个 sessionID → 单独 dir，根目录在 `/tmp/agent-workspaces/<id>`；
2. **路径沙箱**：`safePath` 三层过滤（Clean + Abs reject + EvalSymlinks + HasPrefix），防止 `../../etc/passwd` 和 symlink 穿越；
3. **manifest 持久化**：每个 workspace 写一个 `.workspace.json`，进程重启从磁盘扫描恢复内存索引；
4. **tar.gz 流式归档**：把 workspace 打包给用户下载，project name 作为 tar 顶层目录。

**不做的事**（重要）：

- ❌ **不做租户隔离决策**：workspace 包只提供"按 ID 索引到一个目录"的能力，**判断 sessionID 是否合法、能否回退到 default workspace 的逻辑在 `orchestrator/file_tools.go`**。这是历史教训：早期实现失败时回退到 `ListWorkspaces()[0]`，会跨租户泄露文件
- ❌ **不做容量限制 / 配额**：单个 workspace 写到磁盘满为止——靠上层（tools 限制单次 write 大小）和宿主机监控兜底
- ❌ **不做并发文件锁**：同一 workspace 两次并发 WriteFile 同一个文件靠 OS write 原子性兜底，没有 advisory lock

---

## 1.5 设计哲学：4 个被代码证实的抉择

### Q1 — 为什么是 `/tmp/agent-workspaces` 而不是 PV？

`main.go:613` 硬编码 `/tmp/agent-workspaces` 作为 baseDir。

| 选项 | 速度 | 持久性 | 容量 | 跨 pod | 成本 |
|---|---|---|---|---|---|
| `/tmp` (tmpfs in K8s) | 极快 | 重启丢 | RAM 受限 | 不可 | 0 |
| 容器内 OverlayFS | 快 | 容器死丢 | host disk | 不可 | 0 |
| PVC | 中 | 强 | 可大 | 可共享 | 中 |
| S3 / 对象存储 | 慢 | 强 | 无限 | 可 | 高 |

**选择 `/tmp` 的理由**：
- 代理工作流大多是**短生命周期**（一次对话内完成生成 + 跑测试 + 打包），用户取走 tar.gz 后无需保留
- Agent 容器重启时丢失 workspace = 丢失会话的 in-flight 状态——但 session.Manager 在 Redis 持久化的是**对话历史**而不是工作目录，可接受
- `manifest 恢复` 机制只能在**同一容器内重启**时生效（目录还在磁盘上），换 pod 后 `/tmp` 是新的，会丢

⚠️ **生产部署如果要跨 pod 保留 workspace，必须把 `/tmp/agent-workspaces` 挂成 PVC**——
代码本身不区分 tmpfs 和 PVC，因为它只看路径不看挂载类型。

### Q2 — 为什么 RootDir 存的是 EvalSymlinks 之后的真实路径？

`CreateForSession` L67-70：
```go
realDir, err := filepath.EvalSymlinks(dir)
if err != nil {
    return nil, fmt.Errorf("resolve workspace dir: %w", err)
}
ws := &Workspace{RootDir: realDir, ...}
```

为什么不直接存 `filepath.Join(baseDir, id)` 这个理论路径？

因为 `safePath` 的安全检查（L347）用 `strings.HasPrefix(realPath, ws.RootDir+sep)` 判断越界：
- 如果 `ws.RootDir` 是含 symlink 的逻辑路径（如 `/tmp/agent-workspaces/ws-1`）
- 而 `realPath` 是 EvalSymlinks 解析过的真实路径（如 `/private/tmp/agent-workspaces/ws-1/file.go`，macOS 下 `/tmp → /private/tmp`）
- HasPrefix 就**永远不匹配**，所有正常文件都被判为越界 → 拒绝写入

**所以必须双边都用 real path 比较**——`RootDir` 存 EvalSymlinks 结果，`safePath` 也用 EvalSymlinks，HasPrefix 才有意义。

这个细节在 `restore`（L137-139）里被显式处理：
```go
ws.RootDir = filepath.Join(m.baseDir, e.Name())   // manifest 里存的可能是旧路径
if realDir, err := filepath.EvalSymlinks(ws.RootDir); err == nil {
    ws.RootDir = realDir                          // 恢复时再做一次 EvalSymlinks
}
```
代码注释明确标 "P0-1 fix" —— 历史上 manifest 反序列化后没做这一步，导致重启后 safePath 全部拒写。

### Q3 — `sync.Map` 而非 `map + RWMutex`？

`Manager.workspaces` 是 `sync.Map`（L33）。

`sync.Map` 在 **"读多写少 + key 集合稳定"** 场景显著快于 `RWMutex + map`。
Workspace 的读写模式正好命中：
- 创建只在 session 第一次写文件时发生（低频）
- 每次 `WriteFile/ReadFile` 都要 `Get(ws.ID)` 拿到 \*Workspace（高频）
- key 集合（session ID）一旦创建就稳定，几乎不修改

**代价**：`GetBySession`（L85）需要 `Range` 全表扫，O(n) 复杂度。
在 n < 几百时无感，但**如果业务变成"按 sessionID 查询"是主路径**，应该额外维护 `sessionID → *Workspace` 的二级索引。
当前注释：业务上 `ws.ID == sessionID`（见 `ResolveSessionWorkspace` 调用 `Get(sessionID)` 而不是 `GetBySession`），所以 `GetBySession` 其实是历史遗留，**绝大多数路径不会走这里**。

### Q4 — `safePath` 为什么有 "parent dir EvalSymlinks fallback"？

L334-344：
```go
realPath, err := filepath.EvalSymlinks(absPath)
if err != nil {
    // EvalSymlinks fails if file doesn't exist (e.g., WriteFile creating new file)
    parentDir := filepath.Dir(absPath)
    realParent, err2 := filepath.EvalSymlinks(parentDir)
    if err2 != nil {
        return "", fmt.Errorf("parent directory invalid: %w", err2)
    }
    realPath = filepath.Join(realParent, filepath.Base(absPath))
}
```

为什么要这一段？因为 `WriteFile` 在新建文件时调用 `safePath`，但**新文件还不存在，EvalSymlinks 必然失败**（"no such file or directory"）。
直接 return error 会让所有 WriteFile 新建文件全部失败。

解法：解析**父目录**的真实路径，再拼上 base name。
攻击面分析：
- 攻击者写 `path = "evil.go"`，父目录 = workspace root，EvalSymlinks(root) = 真实 root → 安全
- 攻击者写 `path = "../etc/passwd"`，父目录已被 Clean + Join 规范化到 `/private/tmp/agent-workspaces/ws-1/etc`，EvalSymlinks 失败因为这个父目录不存在，**会被外层 HasPrefix 检查兜底**
- 攻击者写 `path = "evil/../../passwd"`，Clean 之后是 `passwd`，安全
- 唯一不能防住的攻击：父目录是合法的 symlink，但只要 EvalSymlinks(parent) + HasPrefix 双重检查，依然会被拒（symlink target 不在 root 下）

**完整测试见 `manager_test.go` 的 `TestSafePath_*` 系列用例**——覆盖了上述所有 case。

---

## 2. 依赖架构

```
┌─ orchestrator.ProcessMessage ─────────────────────┐
│  o.ResolveSessionWorkspace(sessionID)              │  ← 租户隔离决策点
│  o.workspaceMgr.WriteFile(ws, path, content)       │
│  o.workspaceMgr.ReadFile(ws, path)                 │
│  o.workspaceMgr.Archive(ws, w)                     │
└──────────────────┬────────────────────────────────┘
                   │
                   ▼
        ┌──────────────────────────┐
        │ workspace.Manager         │
        │   - baseDir: /tmp/...     │
        │   - workspaces: sync.Map  │
        └──────────┬───────────────┘
                   │
        ┌──────────┴────────┬─────────────┐
        ▼                   ▼             ▼
   ┌─────────┐       ┌─────────────┐  ┌──────────┐
   │ os.* I/O│       │ filepath.   │  │ archive/ │
   │         │       │ EvalSymlinks│  │   tar+gz │
   └─────────┘       └─────────────┘  └──────────┘
```

**注入点**（`cmd/agent/main.go:613`）：
```go
wsMgr, err := workspace.NewManager("/tmp/agent-workspaces", logger)
if err != nil {
    logger.Warn("workspace manager init failed, file tools and generator disabled", zap.Error(err))
} else {
    orch.SetWorkspaceManager(wsMgr)
    apiServer.SetWorkspaceManager(wsMgr)
    // generator 也吃 wsMgr
}
```

**workspace 是可选依赖**：init 失败时 `wsMgr == nil`，但 main.go 不 fatal——
file_tools / generator / PTY 检测到 nil 时返回 "workspace not available" 错误，不影响其他能力（chat / RAG / MCP）。

---

## 2.5 数据流总览

```text
═══════════ 创建路径: ResolveSessionWorkspace ════════════════════════════

orchestrator.ResolveSessionWorkspace(sessionID)        [file_tools.go:943]
       │
       │ if sessionID == "" → log warn + return nil       (拒绝跨租户)
       │ if ws := workspaceMgr.Get(sessionID); ws != nil → return
       │
       ▼
workspaceMgr.Create(sessionID, label)                  [manager.go:53]
       │
       ▼
workspaceMgr.CreateForSession(id, "", projectName)     [manager.go:58]
       │
       │ if existing := Get(id); existing != nil → return existing  (idempotent)
       │ os.MkdirAll(baseDir/id, 0o755)
       │ realDir := EvalSymlinks(dir)                   ← 关键：存真实路径
       │ ws := &Workspace{ID, SessionID, RootDir: realDir, ...}
       │ workspaces.Store(id, ws)                       ← sync.Map
       │ saveManifest(ws) → write .workspace.json
       │
       ▼ return ws

═══════════ 写入路径: WriteFile ═══════════════════════════════════════════

orchestrator.handleFileWrite                           [file_tools.go:392]
       │
       ▼
workspaceMgr.WriteFile(ws, relPath, content)           [manager.go:158]
       │
       ▼
safePath(ws, relPath)                                  [manager.go:322]
       │
       │ 1. cleaned := filepath.Clean(relPath)
       │ 2. if IsAbs(cleaned) → reject
       │ 3. absPath := Join(ws.RootDir, cleaned)
       │ 4. realPath := EvalSymlinks(absPath)
       │       ├ if err (file doesn't exist):
       │       │    parentDir := Dir(absPath)
       │       │    realParent := EvalSymlinks(parentDir)
       │       │    realPath := Join(realParent, Base(absPath))
       │ 5. if !HasPrefix(realPath, ws.RootDir+sep) && realPath != ws.RootDir
       │       → reject "path traversal detected"
       │
       │ return realPath ✅
       │
       ├ os.MkdirAll(Dir(absPath), 0o755)              ← 父目录预创建
       └ os.WriteFile(absPath, []byte(content), 0o644)

═══════════ 恢复路径: NewManager → restore ═════════════════════════════════

NewManager(baseDir, logger)                            [manager.go:39]
       │
       │ os.MkdirAll(baseDir, 0o755)
       │
       ▼
m.restore()                                            [manager.go:113]
       │
       │ for each entry in baseDir:
       │   if !IsDir → skip
       │   data := os.ReadFile(entry/.workspace.json)
       │       └ if err → skip (not a managed workspace)
       │   json.Unmarshal(data, &ws)
       │   ws.RootDir = Join(baseDir, entry.Name())     ← 矫正路径
       │   if realDir, _ := EvalSymlinks(ws.RootDir); ok:
       │       ws.RootDir = realDir                     ← P0-1 fix: 真实路径
       │   workspaces.Store(ws.ID, &ws)
       │
       └ logger.Info("workspaces restored from disk", count=N)
```

---

## 3. 数据模型

### 3.1 `Workspace`（manager.go:22-28）

```go
type Workspace struct {
    ID        string    `json:"id"`
    SessionID string    `json:"session_id,omitempty"`  // 绑定 chat session
    RootDir   string    `json:"root_dir"`              // EvalSymlinks 后的真实路径
    Project   string    `json:"project_name"`          // tar.gz 顶层目录名
    CreatedAt time.Time `json:"created_at"`
}
```

**ID 约定**：业务上 `ws.ID == sessionID`（见 `ResolveSessionWorkspace` 调 `workspaceMgr.Get(sessionID)`）。
`SessionID` 字段历史遗留，与 `ID` 重复——`GetBySession` 用它做反查，但当前调用路径几乎不走这条。

### 3.2 `Manager`（manager.go:31-35）

```go
type Manager struct {
    baseDir    string
    workspaces sync.Map // id → *Workspace
    logger     *zap.Logger
}
```

⚠️ **没有 mutex**——所有并发安全靠 `sync.Map` 提供。
单字段读（`baseDir`）天然 race-free（只在构造时赋值）。

### 3.3 manifest 文件：`.workspace.json`

**实际文件名**：`/tmp/agent-workspaces/<id>/.workspace.json`（manager.go:100、124）。

⚠️ 旧文档曾写作 `.manifest.json`——**与代码不符**，本次重写已修正。

manifest 内容就是 `json.Marshal(ws)`，即上面 `Workspace` 结构体的字段。
重启时 `restore` 扫描 baseDir 下每个子目录，能读到 `.workspace.json` 的就恢复，否则跳过。

**没有 manifest 的目录会变成孤儿**：手动 `mkdir /tmp/agent-workspaces/orphan-1` 之后，重启 agent 不会认领它，**也不会清理**。这是有意保守——避免误删用户数据。

---

## 4. ★ `safePath` —— 三层路径沙箱（manager.go:322-352）

### 4.1 完整算法

```go
func (m *Manager) safePath(ws *Workspace, relPath string) (string, error) {
    // L1: 规范化
    cleaned := filepath.Clean(relPath)
    if filepath.IsAbs(cleaned) {
        return "", fmt.Errorf("absolute paths not allowed: %s", relPath)
    }

    // L2: 拼接 + 解析 symlink
    absPath := filepath.Join(ws.RootDir, cleaned)
    realPath, err := filepath.EvalSymlinks(absPath)
    if err != nil {
        // 文件不存在（如新建文件）的兜底：解析父目录
        parentDir := filepath.Dir(absPath)
        realParent, err2 := filepath.EvalSymlinks(parentDir)
        if err2 != nil {
            return "", fmt.Errorf("parent directory invalid: %w", err2)
        }
        realPath = filepath.Join(realParent, filepath.Base(absPath))
    }

    // L3: 边界检查
    if !strings.HasPrefix(realPath, ws.RootDir+string(filepath.Separator)) && realPath != ws.RootDir {
        return "", fmt.Errorf("path traversal detected (symlink resolved to %s)", realPath)
    }

    return realPath, nil
}
```

### 4.2 攻击向量覆盖

| 攻击 | 输入 | 防御层 | 结果 |
|---|---|---|---|
| 绝对路径 | `/etc/passwd` | L1 IsAbs | reject |
| 父级穿越 | `../../etc/passwd` | L1 Clean → `etc/passwd`, L3 HasPrefix | reject |
| 隐式当前目录 | `./../../etc/passwd` | L1 Clean 归一 → `../../etc/passwd` → L3 | reject |
| 绝对 symlink | 提前 `ln -s /etc/passwd ws/evil`，然后 `path=evil` | L2 EvalSymlinks(evil) = `/etc/passwd`, L3 HasPrefix 失败 | reject |
| 相对 symlink | `ln -s ../../etc/passwd ws/evil` | 同上 | reject |
| dir 是 symlink | `ln -s /etc ws/etc`, path=`etc/passwd` | L2 EvalSymlinks 解出真实 `/etc/passwd`, L3 失败 | reject |
| 新文件 | path=`new.go`（不存在） | L2 fallback: 解析父目录 = ws root, 拼回 → 真实路径 | accept |
| 嵌套新文件 | path=`a/b/new.go`（a 已存在，b 是新目录） | L2 fallback: parentDir=`ws/a/b`, EvalSymlinks 失败 | **reject** — 走 WriteFile 时 L164 已先 MkdirAll；走 MkdirAll 自身则 accept（见下） |
| 嵌套新目录 | path=`internal/shortcode`（两层都不存在） | MkdirAll 走逐段 lstat：existing 段做 EvalSymlinks 边界校验，missing 段交给 os.MkdirAll | accept |
| symlink 父级 | `ln -s /etc ws/foo` 已存在，path=`foo/bar` | MkdirAll 第一段 lstat `foo` 是 symlink → EvalSymlinks → realCurr=/etc，HasPrefix(RootDir) 失败 | **reject** |

**WriteFile 的 nested 限制**：WriteFile 的实际顺序是 `safePath` **先**跑（L159），然后才是 `os.MkdirAll(Dir(absPath))`（L164）—— 也就是说 nested-and-missing parents 在 `safePath` 阶段就会 reject（父目录与祖父都不存在时，L2 fallback 的 `EvalSymlinks(parentDir)` 会失败）。换句话说 **WriteFile 不会自动递归创建多层 missing parents**，调用方必须先 `MkdirAll` 把父链建出来，再 WriteFile。
`safePath` 单独被调时（如 ListDir 一个不存在的子目录），同样可能在父目录链不完整时报错——见 ListDir L369-L375 的 fallback 处理（root 用 `ws.RootDir`，其他相对路径直接 return err）。

**MkdirAll 的 symlink 防御**（2026-06-03 引入）：`MkdirAll` 早期版本直接走 `safePath`，对 `internal/shortcode` 这种 missing-ancestor 直接 reject；后来去掉 safePath 改成"path 字符串校验 + `os.MkdirAll`"——但这样如果 `internal/` 是已存在的 symlink 指向 `/etc`，`os.MkdirAll` 会跟随 symlink 在 `/etc/shortcode` 创建。所以现在的实现是：
1. 字符串层防 `..` 与 RootDir prefix
2. 逐段 `os.Lstat`，遇到 symlink 就 `EvalSymlinks` 校验 realCurr 仍在 RootDir 内
3. 遇到第一个 `IsNotExist` 段 break，剩余路径交给 `os.MkdirAll`（只会创建真实目录，不会创新 symlink）

### 4.3 与旧文档的差异（**重要纠正**）

⚠️ **旧 doc 在改进建议里列 "P1: `safePath` 缺少 symlink 防护"——这是错误**：
- 代码 L333 明确调用 `filepath.EvalSymlinks(absPath)`
- 代码 L338 在父目录 EvalSymlinks fallback 路径也做了 symlink 解析
- 代码 L299 用 `HasPrefix(realPath, ws.RootDir+sep)` 边界检查（`ws.RootDir` 本身就是 EvalSymlinks 之后的真实路径，见 §1.5 Q2）

**实际状态**：symlink 防护已完整实现。如果有遗留怀疑，跑 `manager_test.go` 看 `TestSafePath_SymlinkAttack` 系列即可验证。

---

## 5. 持久化与恢复

### 5.1 `saveManifest`（manager.go:99-109）

每次 `CreateForSession` 后立即写：
```go
manifestPath := filepath.Join(ws.RootDir, ".workspace.json")
data, _ := json.Marshal(ws)
os.WriteFile(manifestPath, data, 0o644)
```

写失败不 propagate——只记 error 日志。
**原因**：manifest 是"恢复友好"特性，不是关键路径。manifest 丢了下次重启时这个 workspace 不会被恢复，但文件还在磁盘上——下次 `Create(sameID, ...)` 会重新走一遍，可能创建出和之前不同的 workspace 对象（但 RootDir 仍指向同一目录），文件不丢。

### 5.2 `restore`（manager.go:113-146）

**何时触发**：`NewManager` 构造时一次性扫描。

```go
entries := os.ReadDir(baseDir)
for each entry:
    if !IsDir: skip
    data := ReadFile(entry/.workspace.json)
    if err: continue                              ← 静默忽略孤儿目录
    json.Unmarshal(data, &ws)
    ws.RootDir = Join(baseDir, entry.Name())       ← 矫正：手动覆盖 manifest 里的旧值
    if realDir := EvalSymlinks(ws.RootDir); ok:
        ws.RootDir = realDir                       ← 关键：必须再 EvalSymlinks
    workspaces.Store(ws.ID, &ws)
```

**注意 L135-139 的两步**：
1. 先用 `Join(baseDir, entry.Name())` 覆盖 manifest 里存的 `RootDir`——防止挂载点变了之后 manifest 里写的是 `/old/path/ws-1`，新部署在 `/new/path` 时直接拿来用会越界
2. 再 EvalSymlinks 一次——保证内存 `RootDir` 是真实路径（与 CreateForSession 行为一致），否则 safePath 全部失败

**P0-1 fix 注释**（L136）指的就是历史 bug：早期实现只做了第一步，没做第二步，导致 macOS 上重启后 safePath HasPrefix 全部不匹配，所有写入失败。

### 5.3 manifest 字段演化

manifest 是 JSON，向后兼容：
- 加新字段：旧 manifest 反序列化时新字段为零值——OK
- 改字段类型：会反序列化失败 → `restore` L131 记 warn 并 skip ——这个 workspace 丢失内存索引，但目录文件还在

**没有 schema version 字段**——是隐患但不致命。如果未来要重大改 Workspace 结构，要么加 `Version int` 字段，要么用 manifest 文件名后缀（`.workspace.v2.json`）做版本路由。

---

## 6. 其他 CRUD

| 方法 | 行号 | 行为 |
|---|---|---|
| `Create(id, projectName)` | L53 | 转发到 `CreateForSession(id, "", projectName)` |
| `CreateForSession(id, sessionID, projectName)` | L58 | idempotent；存在直接 return；写 manifest；EvalSymlinks RootDir |
| `Get(id)` | L149 | sync.Map.Load |
| `GetBySession(sessionID)` | L85 | sync.Map.Range 线性扫描——O(n)，**低频路径** |
| `WriteFile(ws, relPath, content)` | L158 | safePath + MkdirAll 父目录 + os.WriteFile |
| `DeleteFile(ws, relPath)` | L175 | safePath + os.Remove（IsNotExist 不报错） |
| `ReadFile(ws, relPath)` | L188 | safePath + os.ReadFile |
| `ListFiles(ws)` | L201 | Walk 收集相对路径（不过滤 hidden / `.workspace.json`） |
| `MkdirAll(ws, relPath)` | L227 | 逐段 lstat（已存在祖先做 EvalSymlinks 校验仍在 RootDir 内）+ os.MkdirAll(0o755)。**不再走 safePath**，2026-06-03 起 |
| `Archive(ws, w io.Writer)` | L275 | tar+gzip 流式写；header.Name = `<project>/<rel>` |
| `Cleanup(id)` | L311 | workspaces.Delete + os.RemoveAll —— **不写 manifest 删除日志** |
| `ListWorkspaces()` | L355 | Range → []*Workspace |
| `ListDir(ws, relPath)` | L365 | safePath + os.ReadDir + 给目录加 `/` 后缀 |
| `TreeString(ws)` | L394 | Walk + 缩进字符串（用于 prompt 注入） |

⚠️ **`Cleanup` 不删除 manifest**：实际上 `os.RemoveAll(ws.RootDir)` 把整个目录连同 `.workspace.json` 一起删了，所以 manifest 跟着没了。代码注释没明说这点，但语义是对的。

⚠️ **`ListFiles` / `TreeString` 不过滤 `.workspace.json`**：扫描结果里会出现 manifest 文件，下游使用方（LLM prompt 注入）要意识到这个伪文件。生产场景下可能想 filter 掉——但目前没做。

---

## 7. tar.gz 归档（manager.go:275-308）

```go
gw := gzip.NewWriter(w)
defer gw.Close()
tw := tar.NewWriter(gw)
defer tw.Close()

filepath.Walk(ws.RootDir, func(path, info, err) error {
    rel, _ := filepath.Rel(ws.RootDir, path)
    if rel == "." { return nil }                  ← 跳过根目录自身
    header, _ := tar.FileInfoHeader(info, "")
    header.Name = filepath.Join(ws.Project, rel)   ← project name 作为 tar 顶层目录
    tw.WriteHeader(header)
    if info.IsDir() { return nil }
    f, _ := os.Open(path)
    defer f.Close()
    io.Copy(tw, f)
})
```

**关键点**：
- **流式写**：直接写 `io.Writer`，不缓冲到内存；HTTP 下载端直接 chunked transfer
- **顶层目录**：tar 里所有文件路径都是 `<project>/<rel>`——用户解压时不会污染当前目录
- **包含 `.workspace.json`**：manifest 会被打包进去（与 ListFiles 同理）。用户拿到 tar 后看到这个隐藏文件可能困惑——**P2 应该过滤**

---

## 8. 与 orchestrator 的契约

### 8.1 租户隔离决策点：`ResolveSessionWorkspace`（**不在 workspace 包**）

`orchestrator/file_tools.go:935-971` 才是把 sessionID 映射到 workspace 的入口：

```go
func (o *Orchestrator) ResolveSessionWorkspace(sessionID string) *workspace.Workspace {
    if o.workspaceMgr == nil { return nil }
    if sessionID == "" {
        o.logger.Warn("ResolveSessionWorkspace called with empty sessionID")
        return nil                                  ← 拒绝兜底到 default
    }
    if ws, ok := o.workspaceMgr.Get(sessionID); ok { return ws }
    label := "session-" + sessionID
    if len(sessionID) > 8 {
        label = "session-" + sessionID[:8]
    }
    ws, err := o.workspaceMgr.Create(sessionID, label)
    if err != nil {
        o.logger.Error("failed to create session workspace — refusing to fall back to a shared workspace (would cross tenants)", ...)
        return nil                                  ← 失败也不兜底
    }
    return ws
}
```

**为什么这逻辑在 orchestrator 不在 workspace 包**：
- workspace 包是"目录管理"原语，对租户、隔离、回退策略一无所知
- orchestrator 才知道 "什么是 session"、"empty sessionID 意味着什么"
- 历史 P0 bug：早期 `resolveWorkspace`（L982）在 default 不存在时 fallback 到 `ListWorkspaces()[0]`，跨租户泄露文件
- 修复后 **两个函数都明确不兜底**：失败就返回 nil，调用方必须处理"无 workspace 可用"

### 8.2 调用方处理 nil 的责任

`file_tools.go` 里的 tool handler（handleFileRead/handleFileWrite/...）拿到 `nil workspace` 时：
- 返回 `{"error": "workspace not available"}` 给 LLM
- LLM 看到错误后会改 plan（要求用户提供 sessionID / 走别的工具）
- **不会 panic**，**不会 fallback 到任意 workspace**

---

## 9. 实现剖析与改进方向

### 9.1 当前实现的真实利弊

**优势（验证过的）**
- ✅ 三层 safePath 完整覆盖路径穿越 + symlink 攻击
- ✅ `RootDir` 双边 EvalSymlinks 保证 HasPrefix 比较正确
- ✅ manifest 持久化 + 重启自动恢复（同 pod 内）
- ✅ tar.gz 流式归档不吃内存
- ✅ `sync.Map` 读多写少场景接近 lock-free
- ✅ idempotent Create：重复调用不创建新目录
- ✅ 失败不兜底原则（在 orchestrator 层）杜绝跨租户泄露

**已知风险**

| 严重度 | 问题 | 位置 | 建议 |
|---|---|---|---|
| P2 | `.workspace.json` 出现在 ListFiles / TreeString / tar 包里 | manager.go:201/346/227 | filter 掉 `.workspace.json` |
| P2 | manifest 无 schema version 字段 | manager.go:22 | 加 `Version int` 或路径分版本 |
| P2 | `GetBySession` 是 O(n) 线性扫描 | manager.go:85 | 维护 `sessionID → *Workspace` 二级索引（或确认调用路径已废弃） |
| P2 | 没有容量限制 | 全局 | 加 quota 检查 + per-workspace 大小统计 |
| P2 | 跨 pod 不持久（`/tmp` 是 tmpfs） | main.go:613 硬编码 | 文档化 PVC 挂载要求 / 改可配置 baseDir |
| P3 | `Cleanup` 不发清理事件 | manager.go:263 | 加 metrics + audit log |
| P3 | `restore` 静默丢弃 unmarshal 失败的 manifest | manager.go:131 | 改为重命名为 `.workspace.json.broken` |

### 9.2 优先级修复建议

**P1（生产质量）**
1. ListFiles / Archive 过滤 `.workspace.json`（避免泄漏内部 metadata 给用户）
2. baseDir 可配置（`config.workspace.base_dir`），文档化 PVC 挂载

**P2（设计完善）**
3. 加 manifest schema version 字段
4. 加 metrics：`workspace_create_total / workspace_cleanup_total / workspace_disk_usage_bytes`
5. Cleanup 加 audit log
6. 把 `GetBySession` 改成 O(1) 索引或标 Deprecated

**P3（未来扩展）**
7. workspace quota（per-session 文件大小 / 文件数上限）
8. 增量 archive（只打包 diff，给 git 集成用）
9. workspace 加密 at-rest（KMS 包 mount）

---

## 10. 设计权衡

| 抉择 | 动机 |
|---|---|
| **`/tmp/agent-workspaces` 硬编码 baseDir** | 简化部署；短生命周期工作流不需要持久化；PVC 挂载是部署侧的事 |
| **`RootDir` 存 EvalSymlinks 真实路径** | safePath HasPrefix 比较必须双边都是真实路径，否则全部误判越界 |
| **safePath 三层防御**（Clean + Abs reject + EvalSymlinks + HasPrefix） | 任何单层都有绕过，组合才能拒掉 symlink + parent escape + abs path 三类攻击 |
| **safePath 父目录 EvalSymlinks fallback** | 新建文件场景必须支持，否则 WriteFile 全失败 |
| **manifest 持久化 + restore** | 进程重启不丢索引；同 pod 内的崩溃恢复 |
| **`sync.Map` 而非 RWMutex+map** | 读多写少 + key 集合稳定；本场景命中 sync.Map 优势区 |
| **失败不 fatal（main.go Warn）** | workspace 是可选能力；缺它仍可跑 chat / RAG / MCP |
| **租户隔离决策放 orchestrator 不放 workspace** | workspace 是无状态目录原语；session/tenant 概念属于 orchestrator |
| **失败不兜底（ResolveSessionWorkspace 返回 nil）** | 历史 P0：`ListWorkspaces()[0]` fallback 导致跨租户泄露；宁可拒绝服务也不混租户 |
| **tar.gz 流式不缓冲** | 大项目（10K+ 文件）打包不吃内存；HTTP chunked transfer 友好 |
| **idempotent Create** | LLM 多次调用 generate_project 时不会爆裂创建多个 workspace |

---

## 11. 后续演进

- [ ] **可配置 baseDir**：`config.workspace.base_dir`，部署时映射 PVC
- [ ] **ListFiles / Archive 过滤内部文件**：`.workspace.json` 等
- [ ] **manifest schema version**：未来字段变更的迁移路径
- [ ] **workspace quota**：per-session 总大小 / 文件数 / 单文件大小上限
- [ ] **metrics**：create / cleanup / disk usage / safePath reject 计数
- [ ] **`GetBySession` 重构**：二级索引或标 deprecated 后移除
- [ ] **增量 archive**：和 git 集成；只打包 diff
- [ ] **at-rest 加密**：KMS / 透明加密 mount
- [ ] **跨 pod 共享**：S3 backend + 本地 cache（如果业务需要跨节点）
- [ ] **TTL 自动清理**：长期未访问的 workspace 自动归档到对象存储 + 删本地

---

## 12. 设计教训

1. **租户隔离决策必须在能感知 tenant 概念的层做**：把 "失败时怎么 fallback" 留在 workspace 包是错的——它只看到 ID 字符串，不知道哪些 ID 来自哪个用户。决策应该在能区分 session/user 的层（orchestrator）做。历史 P0 跨租户泄露就是因为决策点选错了。

2. **safePath 必须是三层防御**：只做 `filepath.Clean` 防不住 symlink 攻击；只做 `HasPrefix` 防不住相对路径穿越；只做 `EvalSymlinks` 防不住 abs path。三层组合 + 双边 real path 才完整。本包测试用例覆盖了所有已知绕过模式。

3. **`RootDir` 存什么路径是隐性 ABI**：macOS 的 `/tmp → /private/tmp` 让逻辑路径和真实路径不一致；如果 manager 一边存逻辑路径、一边用真实路径比较，HasPrefix 永远不匹配。**所有边界检查的两边都用 real path** 是隐含契约——`CreateForSession` 和 `restore` 各有一处 EvalSymlinks 就是为了维护这个契约。新代码加任何 path 字段时必须意识到这点。

4. **`filepath.EvalSymlinks` 在新文件场景会失败**：这是 Go 标准库行为而非 bug。任何"创建前先校验路径"的逻辑必须在 EvalSymlinks 失败时 fallback 到父目录解析。这是非常常见的踩坑点。

5. **manifest 持久化的两步式恢复**：`restore` 不能直接信 manifest 里的 `RootDir`——挂载点变化会让旧值无效。必须先用 baseDir + entry name 重建路径，再 EvalSymlinks 解析。manifest 里的 RootDir 只是 informational，不是 source of truth。

6. **sync.Map 是 sharp tool 不是 general replacement**：读多写少 + key 稳定时是 lock-free 巨大胜利；但 Range 永远是 O(n)，不能做"按 sessionID 反查"这种 secondary index 操作。如果业务需求变了，应该立刻退回 RWMutex+map + 显式二级索引。

7. **可选依赖用 Warn 不用 Fatal**：workspace 是可选——`main.go` 失败时 logger.Warn 后继续启动，让 chat/RAG/MCP 仍然能用。这是 Go 项目 graceful degradation 的常见手法。代价：每个调用方都必须 nil-check（`if o.workspaceMgr == nil`），样板代码增多。

8. **文档与代码必须每次审查同步**：旧 doc 把 `.manifest.json` 当文件名（实际是 `.workspace.json`）、宣称 `safePath` 没 symlink 防护（实际三层都做了）——新维护者读旧 doc 后会浪费时间排查"为啥代码不像 doc 说的那样"。文档不准比缺文档更糟。

---

下一篇：[`15_indexer_repomap.md`](15_indexer_repomap.md) —— 仓库索引与符号地图：`internal/indexer` 与 `internal/repomap` 怎么把代码切分、建符号表、喂给 RAG 和 agent 的工作。
