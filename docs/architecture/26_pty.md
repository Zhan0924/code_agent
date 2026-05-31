# 26 · PTY 终端会话 `internal/pty`

> 代码：
> - `session.go` (40) — `ShellSession` 接口 + `ExecResult` / `SessionInfo`
> - `output.go` (44) — ANSI 转义剥离 / 输出截断 / prompt 检测
> - `manager.go` (323) — `SessionManager` 生命周期管理 + idle reaper
> - `session_local.go` (231) — `localSession` 本地 PTY 实现（creack/pty）
> - `session_docker.go` (246) — `dockerSession` Docker 容器实现
>
> 测试：`manager_test.go` (282)

---

## 1. 模块定位

**"让 `shell_exec` 工具的多次调用之间能保持 cwd / env / alias / 后台进程——也就是真正像人坐在终端前一样。"**

`internal/pty` 给 orchestrator 的 `shell_exec` 工具提供**持久化的 shell 会话**。区别于 `internal/sandbox`（每次新启容器，无状态）：

| 维度 | sandbox.Manager | **pty.SessionManager** |
|------|----------------|------------------------|
| 状态 | 每次 Run 全新容器 | 会话内 cwd / env / alias 全部保留 |
| 适合 | 单次代码执行 / 隔离运行 | 多步骤交互（`cd /src && make && ./run`） |
| 后台进程 | Run 结束即清理 | 后台进程在会话内一直跑 |
| 启动延迟 | 每次 1-3 秒 | 第一次创建后毫秒级 Execute |
| 超时 | 整个 Run | 单条 command；会话本身只在 idle 5 分钟后被回收 |

典型场景：

```
1. agent 调 shell_exec("cd /workspace/src")        # cwd 改变
2. agent 调 shell_exec("export DEBUG=1")            # env 持久
3. agent 调 shell_exec("./build.sh && ./run --test") # 上面的 cd/env 还在
```

三次调用走同一 PTY，shell 状态自然延续——这是 sandbox 做不到的。

---

## 1.5 核心设计问题

### 为什么不直接 `exec.Command` 每次跑？

`exec.Command` 是**一次性进程**：

- 没有 PTY → bash 不进入交互模式（`/etc/bash.bashrc` 等不加载，alias 不生效）；
- 跨命令状态全丢（每次新 fork 新 env）；
- 后台进程（`&`）一返回就被父进程清理。

PTY（pseudo-terminal）让 Go 进程**伪装成终端**，shell 进入交互模式后**就一直活着**——直到我们主动关掉。这是 `make / cargo / npm` 这类带进度条的工具能正常工作的前提（无 TTY 时它们检测到不是终端会切到"非交互模式"）。

### 为什么要支持 docker / local 双后端？

| 场景 | Backend | 隔离级别 |
|------|---------|----------|
| 生产 | `docker` | 完整容器（NetworkMode=none + 内存/CPU 限额）|
| 开发 / 测试 | `local` | 仅 cwd 隔离 + minimal env |
| CI | `local` | 启动快、依赖少 |

Docker 提供真隔离（用户写 `rm -rf /` 也只影响容器），但启动要 1-2 秒；local 是直接 fork bash，毫秒级启动但**信任用户工作区**。两种实现共用 `ShellSession` 接口，业务代码无感知切换。

### 为什么用 marker 而不是 PS1 探测来分隔命令输出？

PTY 的根本难题：**怎么知道一条命令的输出结束了？**

- ❌ 等 prompt 重新出现：不同 shell prompt 不同（`$` / `#` / `>>>`），且命令可能输出含 `$ ` 的内容；
- ❌ 设置固定 PS1（如 `PS1=__END__`）：用户脚本可能 `unset PS1`；
- ✅ **本实现：marker echo**

```bash
echo "actual command" \; __ec=$? \; echo "__PTY_DONE_<uuid> $__ec"
```

每次 Execute 生成新 UUID marker，shell 跑完命令后会 echo marker + exit code。读到这一行就知道命令结束，且 exit code 顺带拿到。代价是每条命令多输出一行——可接受。

### 为什么 idle 5 分钟回收而不是永久保留？

PTY 会话每个都吃一个进程（local）或容器（docker）。MacBook 上轻松上百，但服务端**每会话 100MB 内存** × **几百用户**就是几十 GB——必须有上限。

- `MaxSessionsPerWorkspace=3`：单 workspace 最多 3 个会话（一个 default + 2 个用户命名），避免 LLM 失控狂建；
- `IdleTimeout=5min`：5 分钟没活动就回收，比 LLM 思考一次（< 30 秒）大一个量级，不会误杀正在用的会话；
- `reaper goroutine` 每 30 秒扫一次，按 `LastActive` 决定回收。

---

## 2. 依赖架构

```
                    ┌──────────────────────────────────────────┐
                    │  shell_exec tool (pty_tools.go)          │
                    │  args = {command, timeout?}              │
                    │  workspace 来自 getCurrentWorkspaceID()  │
                    │  ← 当前实现：始终返回 "default"（占位）   │
                    └────────────────┬─────────────────────────┘
                                     │
                                     ▼
                    ┌──────────────────────────────────────────┐
                    │  pty.SessionManager                      │
                    │  ┌────────────────────────────────────┐  │
                    │  │ GetOrCreate(ctx, workspaceID)       │  │
                    │  │  ├─ 查 workspaceSessions[ws] 缓存   │  │
                    │  │  ├─ 不存在 → 检查 maxSessions       │  │
                    │  │  ├─ docker → createDockerSession    │  │
                    │  │  └─ local  → createLocalSession     │  │
                    │  └────────────────────────────────────┘  │
                    │  ┌────────────────────────────────────┐  │
                    │  │ reaper goroutine (30s tick)         │  │
                    │  │  → 扫所有 session                    │  │
                    │  │  → LastActive > 5min → Destroy      │  │
                    │  └────────────────────────────────────┘  │
                    └──────┬─────────────────────┬─────────────┘
                           │                     │
                           ▼                     ▼
              ┌────────────────────────┐  ┌──────────────────────────┐
              │ localSession           │  │ dockerSession            │
              │  ├─ cmd: *exec.Cmd     │  │  ├─ containerID          │
              │  ├─ ptmx: *os.File     │  │  ├─ conn: net.Conn       │
              │  ├─ lines: chan string │  │  ├─ reader: bufio.Reader │
              │  └─ readLoop goroutine │  │  └─ (no goroutine: sync) │
              └────────────────────────┘  └──────────────────────────┘
                          │                            │
                          ▼                            ▼
                  ┌──────────────┐            ┌─────────────────┐
                  │ creack/pty   │            │ docker.Client   │
                  │ pty.Start    │            │ ContainerCreate │
                  │ pty.Setsize  │            │ ContainerAttach │
                  └──────────────┘            └─────────────────┘
```

---

## 2.5 数据流总览

```text
═══════════════ Execute("ls /workspace") 全流程 ═══════════════

1. shell_exec tool 收到 LLM 调用
   args = {command: "ls /workspace", timeout: 120}
   # workspace_id 由 getCurrentWorkspaceID() 决定——当前实现是占位：
   # 只要 o.workspaceMgr != nil 就返回字符串 "default"（pty_tools.go:106）。
   # ctx 上没真正解析任何 session/tenant 标识——这是已知短板（见 §10）
   
2. manager.GetOrCreate(ctx, "default") → ShellSession sess  # ← 由 getCurrentWorkspaceID 返回的占位值
   ├─ 缓存命中：直接返回已有 session
   └─ 未命中：createLocalSession / createDockerSession
      ├─ pty.Start(exec.Command("/bin/bash"))
      ├─ pty.Setsize(24, 80)
      ├─ go readLoop()  # 持续读 PTY，每行扔进 channel
      ├─ Sleep 100ms 让 shell 启动
      └─ drainPending()  # 丢弃 shell 启动横幅
   
3. sess.Execute(ctx, "ls /workspace")
   ├─ marker = "__PTY_DONE_a1b2c3d4__"
   ├─ fullCmd = "ls /workspace; __ec=$?; echo "__PTY_DONE_a1b2c3d4__ $__ec"
   ├─ io.WriteString(ptmx, fullCmd)
   │
   ├─ select loop:
   │   case line := <-s.lines:
   │     if line == marker prefix:
   │       parse "__PTY_DONE_a1b2c3d4__ 0" → exitCode=0
   │       return ExecResult{Output, ExitCode: 0, Duration, Truncated}
   │     if line.contains(marker):  # 命令自身的 echo
   │       skip
   │     if output.Len() + len(line) > 1MB:
   │       truncated = true; skip
   │     output += line + "\n"
   │   
   │   case <-timer.C (120s):
   │     return Truncated=true, ExitCode=-1
   │   
   │   case <-ctx.Done():
   │     return Truncated=true, ExitCode=-1
   │
   ├─ stripANSI(output)  # 去 \x1b[xxxm 转义
   └─ return &ExecResult{...}

4. tool 把 ExecResult 包装成 ToolResult 返回给 LLM
```

---

## 3. 接口定义

### 3.1 `ShellSession`

```go
type ShellSession interface {
    ID() SessionID                                          // uuid
    Execute(ctx, command string) (*ExecResult, error)
    Resize(rows, cols uint16) error
    IsAlive() bool
    Close() error
}

type ExecResult struct {
    Output    string         // 已 strip ANSI、已限长
    ExitCode  int            // 命令 exit code；超时/上下文取消时为 -1
    Duration  time.Duration  // 命令执行耗时
    Truncated bool           // 输出 > OutputLimit 或超时
}
```

**两种 `ExitCode=-1` 的语义**：
- `Truncated=true` + ExitCode=-1：超时或 ctx 取消（命令可能还在跑）；
- `Truncated=false` + ExitCode=-1：PTY 读出错且已有部分输出。

调用方（shell_exec tool）需要根据 Truncated 字段决定怎么向 LLM 描述结果（"超时了"vs"返回了"）。

### 3.2 `SessionManager`

```go
type SessionManager interface {
    GetOrCreate(ctx, workspaceID) (ShellSession, error)     // 拿到/创建 default 会话
    Create(ctx, workspaceID, name) (ShellSession, error)   // 显式创建命名会话
    Get(sessionID) (ShellSession, bool)
    Destroy(sessionID) error
    DestroyAll(workspaceID) error
    ActiveSessions(workspaceID) []SessionInfo
    Close() error
}
```

`GetOrCreate` 走"default"命名会话——这是 80% 用例的快捷入口。`Create` 用来开命名 session（如 `Create(ws, "build-watcher")` 让某个 PTY 专门跑 watch 任务，不被普通 shell_exec 打扰）。

---

## 4. `manager.go` —— 生命周期管理

### 4.1 配置

```go
type ManagerConfig struct {
    Backend       string         // "docker" or "local"
    DockerClient  *client.Client // docker backend required
    WorkspaceBase string         // 本地路径前缀: /tmp/agent-workspaces
    Image         string         // docker backend: 使用的镜像
    MaxSessions   int            // 默认 3
    IdleTimeout   time.Duration  // 默认 5min
    MemoryLimit   int64          // docker: bytes
    CPUQuota      int64          // docker: 1/100000 of CPU
    OutputLimit   int            // 默认 1MB
    Shell         string         // 默认 /bin/bash
    Timeout       time.Duration  // 单条命令默认 120s
}
```

启动时验证 `Backend ∈ {docker, local}` 不通过直接 return err——`local` 后端不需要 `DockerClient`。

### 4.2 双索引

```go
sessions          map[SessionID]ShellSession      // 全局 ID 查询
workspaceSessions map[string][]SessionID           // 按 workspace 列表 + maxSessions 校验
```

写两份索引（Destroy 时双删）是为了：
- `Get(sid)` O(1)；
- `ActiveSessions(ws)` 不需要扫全表；
- `MaxSessions` 校验在 `len(workspaceSessions[ws])` 里做。

代价是 Destroy 要扫一遍 workspaceSessions 找到目标 sid 删除。会话数小（< 几百），可接受。

### 4.3 Reaper goroutine

```go
go m.reaper():
    ticker := time.NewTicker(30 * time.Second)
    for {
        select {
        case <-ticker.C: m.reapIdleSessions()
        case <-m.stopReaper: return
        }
    }
```

每 30 秒扫一次——这个粒度是 idle timeout (5min) 的 1/10。换句话说会话在 idle 第 4:30 到 5:30 之间被回收，精度足够。

reapIdleSessions 持锁全程：
1. 收集 `LastActive < now - IdleTimeout` 的 session IDs；
2. 对每个 `Close()`、`delete sessions`、`从 workspaceSessions 移除`；
3. 日志记 idle 时长（"reaped after 6m12s idle"）。

**没有按 workspace 限流的逻辑**——若一个 workspace 同时被攻击式 3 个 session 占满，新请求直接 `return err`，由 shell_exec tool 把错误反馈给 LLM（LLM 会读到"max sessions limit"并知道该 destroy 老的）。

### 4.4 启动 / 关闭顺序

启动（`main.go:600-625`）：
```go
mgr := pty.NewManager(cfg, logger)  // 立即起 reaper goroutine
orch.SetPTYManager(mgr)             // 注入到 orchestrator (shell_exec tool 用)
```

关闭（defer in `main.go`）：
```go
defer mgr.Close()
  → close(stopReaper)              # 终止 reaper goroutine
  → for sess in sessions:           # 遍历所有会话
      if err := sess.Close():       # 单个失败不中断
          errs = append(errs, err)
  → if len(errs) > 0:               # 聚合错误返回
      return fmt.Errorf("errors closing sessions: %v", errs)
```

`Close()` **返回聚合错误**——所有 session.Close 都会尝试执行，单个失败不中断后续；最终把累积的 errors 一次性返回给调用方。这样调用方可以决定 log warn 还是 fatal，但**关闭流程已经走完**（不会因为某个 sess 卡住而漏关其他）。

---

## 5. `localSession` —— 本地 PTY

### 5.1 启动

```go
cmd := exec.CommandContext(ctx, "/bin/bash")
cmd.Dir = "/tmp/agent-workspaces/default"  // 当前 workspace 占位符
cmd.Env = minimalEnv(wsPath)         // 只暴露 PATH/HOME/TERM/LANG/SHELL

ptmx, _ := pty.Start(cmd)            // creack/pty 启动 PTY
pty.Setsize(ptmx, 24x80)            // 标准终端尺寸
```

`minimalEnv` **故意不继承当前进程的 env**——避免泄露 `OPENAI_API_KEY` 等敏感变量到子 shell。代价是子 shell 看不到 host 的 PATH 扩展（用户 zshrc 加的 PATH 元素全无）——对 agent 场景这是 feature 不是 bug。

### 5.2 readLoop goroutine

```go
go sess.readLoop():
    reader := bufio.NewReader(s.ptmx)
    for {
        line, err := reader.ReadString('\n')
        if err != nil:
            s.readErr <- err
            return
        s.lines <- strings.TrimRight(line, "\r\n")
    }
```

**为什么必须有这个 goroutine**：bufio.Reader 不能被多个 goroutine 并发读——`Execute` 拉的同时 readLoop 也读会数据错乱。所以 readLoop **独占**Reader，Execute 通过 channel 拿数据。

**channel 容量 1000 行** —— 一条命令的输出超过 1000 行时 channel 会塞满，readLoop 阻塞在 `s.lines <-` 上，但因为 Execute 在另一端 `<-s.lines` 持续消费，**实际中不会满**。极端长输出场景 `OutputLimit` 截断会先生效。

### 5.3 Execute 的 select 多路复用

```go
for {
    select {
    case <-timer.C:            return Truncated=true        // 120s 超时
    case <-ctx.Done():         return Truncated=true        // 上下文取消
    case err := <-s.readErr:   return partial or err        // PTY 关了
    case line := <-s.lines:    
        if startswith marker: parse exit code, return       // 命令结束
        if contains marker:   skip (命令自身的 echo)
        if output too long:   truncated=true; skip
        output += line
    }
}
```

四路 select 涵盖了所有可能的终止条件——这是 Go concurrency 的经典模式。

### 5.4 stripANSI

```go
ansiEscapeRegex := regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)
```

匹配 `\x1b[` 开头 + 数字/分号 + 字母结束的 ANSI 控制序列（颜色、光标移动、清屏）。LLM 看到这些会乱码——必须剥掉。

**已知不完整**：不处理 `\x1b]...\x07` 形式的 OSC（操作系统命令，如 iTerm2 的窗口标题）。实际中 LLM 看到这些会忽略，影响小。

---

## 6. `dockerSession` —— 容器 PTY

### 6.1 启动

```go
containerCfg := &container.Config{
    Image:        cfg.Image,
    Cmd:          []string{cfg.Shell},
    WorkingDir:   "/workspace",
    Tty:          true,          // ★ 关键：开 TTY
    OpenStdin:    true,          // ★ stdin 可写
    AttachStdin:  true, AttachStdout: true, AttachStderr: true,
}

hostCfg := &container.HostConfig{
    NetworkMode: "none",         // ★ 完全断网
    Binds:       ["/host/ws:/workspace"],
    Resources: {Memory, NanoCPUs},
}

ContainerCreate + ContainerAttach + ContainerStart
```

**与 sandbox.Manager 的区别**：
- sandbox 每次 `Run` 完即 Remove；PTY 容器 **直到 idle 5 分钟才回收**；
- sandbox 默认 NetworkMode=none + readonly rootfs；PTY 一样 NetworkMode=none，但 rootfs 可写（agent 需要 install 软件包等）；
- 都 bind mount workspace 目录——这是 agent 写入文件的去处。

### 6.2 与 localSession 的实现差异

| 操作 | localSession | dockerSession |
|------|--------------|---------------|
| 数据通道 | `*os.File` PTY | `net.Conn` (TCP-like attach) |
| 读模式 | goroutine + channel | 同步 `reader.ReadString` |
| 超时 | timer + select | `conn.SetReadDeadline` |
| Resize | `pty.Setsize` | `docker.ContainerResize` |
| Close | kill process + close ptmx | ContainerStop + Remove |
| IsAlive | `cmd.ProcessState == nil` | `docker.ContainerInspect` |

**为什么 docker 不用 readLoop goroutine**？docker attach 用的是 `net.Conn`，**支持 SetReadDeadline**——不需要异步读循环，直接 `ReadString` + 推进 deadline 就能轮询。

local 的 `*os.File` 在 macOS/Linux 上**不支持 SetReadDeadline**（PTY 设备文件没有 net.Conn 的 deadline 语义），所以只能起 goroutine 配合 channel 多路复用。这是两种实现走不同代码路径的根本原因。

### 6.3 drainInitialOutput

```go
for {
    SetReadDeadline(now + 500ms)
    _, err := reader.ReadString('\n')
    if err != nil: break       // deadline 触发 EOF
}
```

容器刚启动时 bash 会输出欢迎信息（`bash-5.1$` 之类）——用短 deadline 轮询读直到没新内容就停。这比 `time.Sleep(100ms)` 更可靠：慢启动的容器（如 alpine 没装 bash 需要 fallback 到 sh）能拿到完整启动输出再开始接受命令。

---

## 7. 与 orchestrator 的集成

### 7.1 启动接入

```go
// cmd/agent/main.go:600
if cfg.PTY.Enabled {
    ptyCfg := pty.ManagerConfig{
        Backend:     cfg.PTY.Backend,            // "local" by default
        MaxSessions: cfg.PTY.MaxSessionsPerWorkspace,
        OutputLimit: cfg.PTY.OutputLimit,
        Shell:       cfg.PTY.Shell,
    }
    // 默认值兜底
    if ptyCfg.Backend == "" { ptyCfg.Backend = "local" }
    if ptyCfg.MaxSessions == 0 { ptyCfg.MaxSessions = 3 }
    ...
    mgr, _ := pty.NewManager(ptyCfg, logger)
    orch.SetPTYManager(mgr)
    logger.Info("PTY session manager initialized", zap.String("backend", ptyCfg.Backend))
}
```

`docker` backend 需要再传 `DockerClient`——当前默认 `local` 是因为 docker-in-docker 在容器化部署下并不总可用（需要挂 `/var/run/docker.sock`）。

### 7.2 shell_exec tool 适配

shell_exec tool 定义在 `internal/orchestrator/pty_tools.go`（`RegisterPTYTools`），通过 `SetPTYManager` 时挂入工具注册表。args schema **故意收窄**——LLM 不应能跨 workspace 借 PTY 拿别人的 session：

```json
{
  "type": "object",
  "properties": {
    "command": {"type": "string", "description": "Shell command to execute"},
    "timeout": {"type": "integer", "description": "Timeout in seconds", "default": 120}
  },
  "required": ["command"]
}
```

`workspace_id` 不在 LLM 参数里——由 `o.getCurrentWorkspaceID(ctx)` 决定。**当前实现是占位**：方法签名带 `ctx`，但内部根本没读 ctx 任何 value——只要 `o.workspaceMgr != nil` 就返回字符串 `"default"`（`pty_tools.go:106-114` 注释也承认这是 placeholder）。这意味着：

- **所有调用共用一个名为 "default" 的会话**——同一 process 内所有 LLM tenants 共用一个 PTY；
- **真正的多租户隔离需要补完**——往 ctx 注入 workspaceID + 在 `getCurrentWorkspaceID` 里读取（见 §10 演进项）；
- **P9 原则当前未真正生效**——文档承认这是已知短板。

执行流程（pty_tools.go:toolShellExec）：
```go
1. json.Unmarshal(args, &{Command, Timeout})
2. validateWorkspaceCommand(params.Command)        # ★ 黑名单 + 白名单校验
3. workspaceID := o.getCurrentWorkspaceID(ctx)     # 占位：返回 "default"
4. sess, _ := o.ptyManager.GetOrCreate(ctx, workspaceID)
5. execCtx, cancel := context.WithTimeout(ctx, params.Timeout*time.Second)
6. result, _ := sess.Execute(execCtx, params.Command)
7. content := result.Output
   if result.ExitCode != 0: content = "[exit code: N]\n" + content
   if result.Truncated:     content += "\n... (output truncated)"
8. return ToolResult{Content: content, IsError: result.ExitCode != 0}
```

注意 `ToolResult` **没有** Metadata 字段——exit_code / duration / truncated 三个信息分别通过：
- **exit_code**：失败时拼到 Content 头部（`"[exit code: N]\n"`），LLM 直接能读；
- **truncated**：当 `result.Truncated == true` 时拼到 Content 尾部（`"... (output truncated)"`，与 ExitCode 无关——超 1MB 的成功命令同样会被标记）；
- **duration**：只进 `o.logger.Info("tool:shell_exec", zap.Duration(...))` 不进 ToolResult。

这是 "metadata 通过 LLM 可读的文本字段透出" 的设计——避免引入结构化元数据后还要专门解析。

`IsError: ExitCode != 0` 让 LLM 自然看出"命令失败"，配合 [21_agentloop](21_agentloop.md) 的 `failure_tracker` 在连续失败时触发 step-back。

### 7.3 multiagent allowlist

`shell_exec` 在 `multiagent.allowedTools` 里属于 `AgentCode` 和 `AgentTest`（见 [22_multiagent](22_multiagent.md) §6.3）——`AgentReview` 不能执行 shell 命令（只读审查）。

---

## 8. 安全考量

### 8.1 命令注入

`shell_exec("foo; rm -rf /")` 会被 bash 解释为两条命令——这是 PTY 设计的**预期行为**，不是漏洞。安全边界由**外层**保证：

| 后端 | 边界 |
|------|------|
| docker | NetworkMode=none + bind 限定 + 资源限额 + 容器 root 仍是宿主 unprivileged user |
| local | OS user 权限 + minimalEnv（无敏感变量） |

local 后端**信任用户工作区**——不适合公网部署。生产必须用 docker backend。

### 8.2 危险命令拦截

shell_exec **复用 file_tools 的双层校验**（`validateWorkspaceCommand` in `internal/orchestrator/file_tools.go:104`）：

1. **bannedCommandPatterns**（regex 黑名单）：匹配即拒绝 + 返回 `"matches banned pattern: ..."`；
2. **allowedCommandPrefixes**（白名单）：base command 必须命中允许前缀（如 `go test`、`npm test`、`ls`、`cat` 等），否则拒绝。

→ **改进项**：当前黑名单 / 白名单写死在 `file_tools.go`，没做配置化；不同 multiagent 角色（Code/Test/Review）应支持差异化白名单——目前只能在工具注册层做粗粒度过滤（见 [22 §6.3](22_multiagent.md)）。

### 8.3 输出泄露

- ANSI 已剥；但**环境变量泄露**没拦：若用户的脚本 `env | grep KEY` 输出会原样返回给 LLM；
- Marker 是随机 UUID——攻击者无法预测下一个 marker 来污染输出；
- OutputLimit 1MB 防内存爆炸。

### 8.4 资源耗尽

- `MaxSessionsPerWorkspace=3`：单 workspace 最多 3 会话；
- `IdleTimeout=5min`：自动回收；
- Docker backend 的 `Memory` / `CPUQuota` 在 host 层强制；
- local backend **无 cgroup 限制**——子 bash fork 出 chrome 之类的会吃光内存。

---

## 9. 设计权衡

| 抉择 | 动机 |
|------|------|
| 双后端共用接口 | 开发/CI 用 local（快、无 docker 依赖），生产用 docker（隔离） |
| Marker 探测命令结束 | shell-agnostic，比 PS1 配置更可靠 |
| local 用 goroutine + channel | `*os.File` 不支持 SetReadDeadline，只能异步读 |
| docker 直接同步读 + deadline | `net.Conn` 支持 deadline，更简单 |
| MaxSessions=3 / Idle=5min | 平衡资源消耗与用户体验 |
| reaper 30s tick | 比 idle timeout 小一个量级，精度够 |
| minimalEnv 不继承 host env | 防 API key 泄露到 LLM 视野 |
| stripANSI 用单一 regex | OSC 等少数转义遗漏，LLM 容忍度高 |
| ExitCode=-1 兼表"超时"和"读错误" | 由 Truncated 字段消歧；调用方需配合判断 |
| Output 1MB 上限 | 防输出爆炸；超出时 `truncated=true` 但保留已读部分 |
| Close 聚合错误返回（不中断） | 单会话关闭失败不阻塞其余 session.Close，但错误会一并返回给调用方 |
| 没有自动重连 | session 死了就死了；LLM 拿到 error 后会自然重 create |

---

## 10. 后续演进

- [ ] **真实多租户隔离（高优先）**：`getCurrentWorkspaceID` 当前返回 `"default"`，全 process 共用一个 PTY；需要在 orchestrator 入口把 workspaceID 注入 ctx + 在此读取；触及 P9 硬隔离原则
- [ ] **黑/白名单配置化 + 角色差异化**：当前 `validateWorkspaceCommand` 走硬编码 pattern；Code/Test/Review 角色应支持差异化白名单（如 Review 禁 `shell_exec`、Test 准许 `go test` 但禁 `git push`）
- [ ] **流式输出**：当前 `Execute` 阻塞等命令结束；改成 channel 输出能让 SSE 实时 push（前端看到 `make` 进度）
- [ ] **跨进程持久化**：用 `unix domain socket` 让重启后的 agent 进程能重新 attach 到老的 PTY 会话（当前 manager 重启 = 所有会话丢失）
- [ ] **资源指标**：暴露 `pty_active_sessions{backend,workspace}` / `pty_command_duration_seconds` / `pty_truncated_total`
- [ ] **审计**：每条 Execute 调用写 audit log，便于追溯"谁让 agent 跑了什么命令"
- [ ] **local 后端 cgroup**：用 `golang.org/x/sys/unix.Cgroup` 给 local session 限内存/CPU
- [ ] **shell 历史**：在 session 内导出 `HISTFILE`，关闭时写到 workspace 目录便于审计
- [ ] **多 shell 支持**：当前只支持 bash；zsh / fish 的 prompt 探测和 marker 写法略不同
- [ ] **stdin 交互**：当前 Execute 一次发命令 + 等结果；类似 `python` REPL 的多轮交互未支持
- [ ] **会话压缩 snapshot**：长会话的 cwd / env / 历史命令导出为可恢复 snapshot

---

## 11. 与人类终端使用的类比

| 人类用 tmux | 本模块 |
|-------------|--------|
| tmux session 持久化 | PTY session 持久化 (5min idle) |
| `tmux new -s work` | `Create(ws, "work")` |
| `tmux ls` | `ActiveSessions(ws)` |
| `tmux kill-session` | `Destroy(sid)` |
| 窗口大小同步 | `Resize(rows, cols)` |
| 自动重连 | ❌ (未实现，进程重启即丢) |

agent 比人类多一个约束：**5 分钟没活动就回收**——人类用 tmux 是为了"我离开 30 分钟回来还能接着干"，agent 不需要那么长持久化，5 分钟够覆盖一次复杂任务的连续 shell 调用即可。

---

下一篇：[`27_lsp.md`](27_lsp.md) —— LSP 客户端：goto_definition / find_references 工具的底层。
