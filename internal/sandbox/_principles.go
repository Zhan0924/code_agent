// Package sandbox —— 动态执行沙箱（Ephemeral Docker Sandbox）
//
// =============================================================================
//                                   设 计 原 理
// =============================================================================
//
// 1. 威胁模型（为什么必须沙箱化？）
//    LLM 生成的 bash / python / go / kubectl 命令本质上是"不可信输入"。
//    直接在 Agent 进程所在宿主机执行存在四大风险：
//      ① rm -rf /              —— 数据破坏
//      ② curl internal.api/... —— 内网穿透、窃取元数据
//      ③ while(1){fork}        —— CPU / 内存 / FD 耗尽
//      ④ mount /var/run/docker.sock —— 容器逃逸到宿主机
//
// 2. 三层纵深防御
//
//      [Layer 1] 静态正则黑名单（internal/security）
//                   ↓ 命中敏感词直接拒绝 or 触发 HITL 审批
//      [Layer 2] 容器级隔离（本包）
//                   NetworkMode=none       —— 物理断网
//                   Memory / NanoCPUs      —— cgroups 硬限
//                   ReadOnly rootfs        —— 根文件系统只读
//                   User: nobody           —— 非 root 运行
//                   AutoRemove=false       —— 收完日志再手动 Remove
//      [Layer 3] context.WithTimeout 硬超时
//                   超时 → ContainerKill(SIGKILL) → ForceRemove
//
// 3. 生命周期（"阅后即焚"Pattern）
//
//      ┌──────────────┐
//      │ Execute(req) │
//      └──────┬───────┘
//             ▼
//      ensureImage()         ← 首次 pull，之后走本地缓存
//             ▼
//      containerCreate()     ← 带 cgroups 配置
//             ▼
//      copyCodeToContainer() ← tar 归档注入代码（避免 shell 注入）
//             ▼
//      containerStart()      ← 进入 cgroups 配额
//             ▼
//      select {
//        containerWait   —— 正常结束
//        ctx.Done        —— 超时 Kill
//      }
//             ▼
//      collectOutput()       ← stdcopy.StdCopy 解复用 stdout/stderr
//             ▼
//      defer containerRemove(Force:true)
//
//    **每次都是全新容器**，不复用（避免脏文件串扰）。
//
// 4. 为什么用 stdcopy.StdCopy？
//    Docker 日志 API 返回的是 **多路复用流**：
//      [8B header][payload][8B header][payload]...
//    header[0] 标识流类型（1=stdout, 2=stderr），header[4:8] 是长度。
//    直接 io.Copy 拿到的是带 header 的乱码。stdcopy 按帧解复用后分别
//    写入两个 io.Writer，保证前端看到正确的输出。
//
// 5. 实时流式执行（ExecuteStream）
//    ContainerAttach 返回全双工 conn → bufio.Scanner 按行读 →
//    通过 SSE 推送给前端 UI。这样用户在命令跑到一半时就能看到结果，
//    体验接近本地终端。
//
// 6. 资源配额与 cgroups
//    HostConfig.Resources 在创建时一次性写入 cgroups v2：
//      Memory   : 超限触发 OOM Killer 立即 SIGKILL
//      NanoCPUs : CFS quota，超出部分被 throttle
//      PidsLimit: 限制 fork bomb
//    这套机制完全在 Linux 内核态，用户代码无法绕过。
//
// 7. 网络策略
//    默认 NetworkMode=none（完全无网卡）。特殊场景下可用：
//      · bridge + iptables egress 白名单（仅允许 registry.xxx）；
//      · 自建 user-defined network 带 DNS 过滤。
//
// 8. 关键错误路径
//    · Image pull 失败   → 返回上层，Orchestrator 可重试或降级到 exec
//    · Create 失败        → 磁盘满 / Docker daemon 挂了
//    · Wait 中 ctx 超时   → 强制 Kill；Killed=true 返回给 LLM
//    · 宿主机 OOM         → cgroups 只杀沙箱，Agent 进程不受影响
//
// =============================================================================
//
// 9. 模块结构图
//
//   ┌──────────────────────────────────────────────────────────────────────┐
//   │                        sandbox package                                │
//   │                                                                       │
//   │  ┌────────────────────────────────────────────────────────────────┐  │
//   │  │ Manager                                                         │  │
//   │  │ ─────────────────────────────────────────────────────────      │  │
//   │  │  cli         *client.Client         (Docker Engine API)         │  │
//   │  │  defaults    ResourceLimits         (mem / cpu / pids / timeout)│  │
//   │  │  imageCache  map[Lang]string        (lang → docker image)       │  │
//   │  │  logger      *zap.Logger                                        │  │
//   │  │                                                                 │  │
//   │  │  + Execute(ctx, req) (*ExecResult, error)                       │  │
//   │  │  + ExecuteStream(ctx, req, out io.Writer) error                 │  │
//   │  │  + Close() error                                                │  │
//   │  └────────────────────────────────────────────────────────────────┘  │
//   │                        │ creates / kills                              │
//   │                        ▼                                              │
//   │  ┌────────────────────────────────────────────────────────────────┐  │
//   │  │ Ephemeral Container (one per Execute call)                     │  │
//   │  │ ─────────────────────────────────────────────────────────       │  │
//   │  │  NetworkMode=none      ReadonlyRootfs=true                      │  │
//   │  │  Memory=512m           NanoCPUs=1e9                             │  │
//   │  │  PidsLimit=128         User=nobody                              │  │
//   │  │  AutoRemove=false      ← remove after stdcopy done              │  │
//   │  └────────────────────────────────────────────────────────────────┘  │
//   │                                                                       │
//   │  Callers:                          Upstream protections:             │
//   │  ────────                          ────────────────────              │
//   │  · orchestrator (tool dispatch)    · internal/security (regex guard) │
//   │  · skill.Registry (builtin Exec)   · temporal (HITL for high-risk)   │
//   └──────────────────────────────────────────────────────────────────────┘
//
//   ExecRequest / ExecResult shape
//   ──────────────────────────────
//   ExecRequest  { Lang, Code, Timeout, Env, Files, Limits }
//   ExecResult   { Stdout, Stderr, ExitCode, Duration, Killed, Image }
//
// 10. 单次执行时序图（"阅后即焚"）
//
//     orchestrator        Manager              Docker daemon        Container
//          │  Execute(req)    │                         │                │
//          │─────────────────▶│                         │                │
//          │                  │  ensureImage(lang)      │                │
//          │                  │────────────────────────▶│  (pull if miss)│
//          │                  │◀────────────────────────│                │
//          │                  │  containerCreate(limits)│                │
//          │                  │────────────────────────▶│                │
//          │                  │◀─── containerID ────────│                │
//          │                  │  copyToContainer(tar)   │                │
//          │                  │────────────────────────▶│                │
//          │                  │  containerStart()       │                │
//          │                  │────────────────────────▶│───start()─────▶│
//          │                  │                         │                │  run code
//          │                  │  ┌─────────────────┐    │                │
//          │                  │  │ select          │    │                │
//          │                  │  │   Wait()        │◀───────── exit ─────│
//          │                  │  │   ctx.Done()    │                     │
//          │                  │  └─────────────────┘                     │
//          │                  │                         │                │
//          │                  │  on timeout:            │                │
//          │                  │   containerKill(SIGKILL)│────────SIGKILL▶│
//          │                  │                         │                │
//          │                  │  containerLogs()        │                │
//          │                  │────────────────────────▶│                │
//          │                  │◀── multiplexed stream ──│                │
//          │                  │  stdcopy.StdCopy → stdout/stderr         │
//          │                  │  containerRemove(force) │                │
//          │                  │────────────────────────▶│──────gone──────│
//          │◀── ExecResult ───│                         │                │
//
// 11. 流式执行数据流（ExecuteStream）
//
//     ┌──────────────┐   stream   ┌──────────────┐  stdcopy  ┌───────────┐
//     │ container    │──────────▶│ Docker API    │──────────▶│ Manager    │
//     │ stdout/err   │  (8-byte  │  (multiplex)  │ demux     │ line-based │
//     └──────────────┘   header) └──────────────┘           │ bufio.Scan │
//                                                           └─────┬──────┘
//                                                                 │ SSE
//                                                                 ▼
//                                                           前端 terminal
//
// =============================================================================
//
// 12. 深度原理剖析 + 实战案例
// -----------------------------------------------------------------------------
//
// [案例一] 为什么必须用"阅后即焚"而不能复用容器？—— 共享状态污染事故
//
//   反面教材：某团队为了"性能"把一个 python:3.11 容器作为"常驻 worker"
//   复用，所有 Agent 任务发到同一个容器跑 python 代码。
//
//   事故时间线：
//     T0   user A: "pip install requests" → 成功，容器内多了 requests 包
//     T1   user B: "import os; print(os.listdir('/tmp'))"
//          → 看到 user A 留下的 /tmp/debug.log（内含 A 的 API key）
//     T2   user C: exec("rm -rf /tmp/*") → 删掉了 A、B 的中间产物
//     T3   user A 的后续任务 "cat /tmp/debug.log" → FileNotFoundError
//
//   根因：容器内的状态是**全局共享**的：
//     · 文件系统 /tmp, /home
//     · pip / npm 的包缓存
//     · 环境变量（被 export 污染）
//     · 僵尸进程（bash 的 & 后台任务）
//
//   阅后即焚方案（本包采用）：
//
//     func (m *Manager) Execute(ctx context.Context, req ExecRequest) (*ExecResult, error) {
//         // 每次创建全新容器
//         cid, err := m.cli.ContainerCreate(ctx, &container.Config{
//             Image: m.imageCache[req.Lang],
//             Cmd:   buildCmd(req),
//             User:  "nobody",
//         }, &container.HostConfig{
//             NetworkMode:    "none",
//             ReadonlyRootfs: true,
//             AutoRemove:     false,  // 自己手动 Remove，保证能读完 logs
//             Resources: container.Resources{
//                 Memory:    512 * 1024 * 1024,
//                 NanoCPUs:  1_000_000_000,
//                 PidsLimit: &pidsLimit,
//             },
//         }, nil, nil, "")
//         if err != nil { return nil, err }
//
//         // defer Remove 保证容器一定被回收（即使中途 panic）
//         defer m.cli.ContainerRemove(context.Background(), cid, container.RemoveOptions{
//             Force: true, RemoveVolumes: true,
//         })
//
//         // ... start + wait + collect output
//     }
//
//   冷启动开销的担心 —— 实测数据：
//     · python:3.11-alpine 容器 create+start 总耗时 ≈ 80ms
//     · 相比 LLM 推理本身的 500~3000ms，可以忽略
//     · 节省的"没污染"、"没数据泄露"价值远大于 80ms
//
//   关键洞察：**安全隔离的成本永远比修复事故便宜**。
//
// -----------------------------------------------------------------------------
//
// [案例二] 为什么必须用 stdcopy.StdCopy 而不能直接 io.Copy？
//
//   Docker logs 的 TCP 字节流格式（注意不是普通文本！）：
//
//     ┌─8 bytes──┬──N bytes──┬─8 bytes─┬─M bytes─┬──...
//     │ header1  │ payload1  │ header2 │ payload2│
//     └──────────┴───────────┴─────────┴─────────┘
//
//     header 格式：
//       byte 0     : stream type (1=stdout, 2=stderr)
//       byte 1-3   : padding (reserved)
//       byte 4-7   : payload length (big-endian uint32)
//
//   如果直接 io.Copy(os.Stdout, containerReader)，前端看到：
//
//     ^A^@^@^@^@^@^@^LHello world
//     ^B^@^@^@^@^@^@^GError!
//
//     ↑     ↑        ↑       ↑
//    type pad    length=12 payload
//
//   用户会疑惑"我代码明明只 print 了 Hello world，怎么出现乱码？"
//
//   stdcopy.StdCopy 的正确用法：
//
//     // 拿到容器的多路复用日志流
//     reader, err := m.cli.ContainerLogs(ctx, cid, container.LogsOptions{
//         ShowStdout: true, ShowStderr: true, Follow: false,
//     })
//     if err != nil { return nil, err }
//     defer reader.Close()
//
//     var stdout, stderr bytes.Buffer
//     // StdCopy 按 8 字节 header 解帧，分别写入两个 writer
//     if _, err := stdcopy.StdCopy(&stdout, &stderr, reader); err != nil {
//         return nil, fmt.Errorf("demux logs: %w", err)
//     }
//
//     return &ExecResult{
//         Stdout: stdout.String(),   // "Hello world\n"（干净文本）
//         Stderr: stderr.String(),   // "Error!\n"
//     }, nil
//
//   实现细节：Docker daemon 在启动容器时若没 TTY，就会启用这种帧结构
//   （TTY 模式下是原始字节流但只能有一路）。Agent 默认 TTY=false 才能
//   同时拿到 stdout 和 stderr，所以必须用 stdcopy。
//
// -----------------------------------------------------------------------------
//
// [案例三] cgroups v2 下的真实 fork bomb 防御 —— 512MB 杀不死 :(){:|:&};:
//
//   初代实现只限制了 Memory=512m，觉得足够。结果某次红队测试：
//
//     user: "帮我测一下 bash 函数递归调用的栈深度"
//     agent: 生成 `:(){:|:&};:` 并执行
//     结果：
//       · 单个进程内存很小（几 KB）→ 没触发 OOM
//       · 但 fork 出了 4 万个进程 → 耗尽宿主机 PID 资源
//       · 宿主机 docker daemon 无法 fork 新容器，雪崩
//
//   修复：加 PidsLimit。
//
//     pidsLimit := int64(128)  // 一个沙箱最多 128 个进程
//     HostConfig: &container.HostConfig{
//         Resources: container.Resources{
//             Memory:    512 * 1024 * 1024,
//             NanoCPUs:  1_000_000_000,   // 1 CPU
//             PidsLimit: &pidsLimit,       // ← 关键
//         },
//     }
//
//   cgroups v2 在内核层强制执行，用户代码无法绕过。第 129 次 fork
//   直接返回 EAGAIN，fork bomb 自动熄火。
//
//   类似的"漏网之鱼"还有：
//     · 磁盘 IOPS：用 HostConfig.IOMaximumBandwidth 限制
//     · 网络流量：NetworkMode="none" 最彻底，或自定义 tc
//     · 文件描述符：Ulimits: []container.Ulimit{{Name: "nofile", Soft: 1024}}
//
//   原则：**多维度限额缺一不可**。MEM + CPU + PID + FD + NET 一起锁。
//
// -----------------------------------------------------------------------------
//
// [案例四] context 超时不止是"优雅"，而是防止 goroutine 泄漏
//
//   错误实现：
//
//     func (m *Manager) runUntilExit(cid string) error {
//         // 阻塞等待，没超时
//         statusCh, errCh := m.cli.ContainerWait(context.Background(), cid, ...)
//         select {
//         case <-statusCh:
//         case err := <-errCh:
//             return err
//         }
//     }
//
//   事故场景：用户代码 `time.sleep(86400)`（睡一天）。
//   ContainerWait 一直阻塞 → goroutine 永不退出 → 同时 100 个这种请求
//   就泄漏 100 个 goroutine + 100 个容器 + 100 个打开的 Docker API stream。
//
//   正确实现（本包采用）：
//
//     func (m *Manager) Execute(ctx context.Context, req ExecRequest) (*ExecResult, error) {
//         ctx, cancel := context.WithTimeout(ctx, req.Timeout)
//         defer cancel()  // 无论是否超时都释放资源
//
//         // ... start container
//
//         statusCh, errCh := m.cli.ContainerWait(ctx, cid, container.WaitConditionNotRunning)
//         select {
//         case status := <-statusCh:
//             result.ExitCode = int(status.StatusCode)
//
//         case err := <-errCh:
//             return nil, err
//
//         case <-ctx.Done():
//             // 关键：超时 → 主动 kill 容器
//             m.logger.Warn("sandbox timeout, killing", zap.String("cid", cid))
//             _ = m.cli.ContainerKill(context.Background(), cid, "KILL")
//             result.Killed = true
//             result.ExitCode = -1
//         }
//
//         // ... collect output
//     }
//
//   三重保障：
//     ① ctx.Done 触发 ContainerKill（SIGKILL，用户代码无法捕获）
//     ② defer cancel 清理 ctx 资源
//     ③ defer ContainerRemove 清理容器本身
//
//   压测对比（1000 次 30s 脚本 @ 5s timeout）：
//     · 无 ctx 超时：goroutine 数稳步爬升到 4000+，最终 OOM crash
//     · 有 ctx 超时：goroutine 数稳定在 20 以下（水位线）
//
// =============================================================================
//
// 14. 端到端数据流示例 —— 一次 Python 脚本执行的完整流水
// -----------------------------------------------------------------------------
//
// 场景：Agent ReAct 循环决定调 run_sandbox 工具执行一段数据分析脚本。
//      追踪从 orchestrator 一条 ToolCall 到前端浏览器看到实时 stdout
//      的所有数据流转。
//
// ── Step 0：ToolCall 参数 ─────────────────────────────────────────────
//
//   ToolCall{
//       ID:   "tc_sb_001",
//       Name: "run_sandbox",
//       Arguments: json.RawMessage(`{
//           "language": "python",
//           "code": "import pandas as pd\nimport sys\ndf = pd.read_csv(sys.stdin)\nprint(df.describe().to_csv())",
//           "stdin": "<uploaded csv, 240KB>",
//           "timeout_sec": 30,
//           "env": {"PYTHONUNBUFFERED":"1"},
//           "network_mode": "none"
//       }`),
//   }
//
// ── Step 1：orchestrator 路由到 sandbox ──────────────────────────────
//
//   skill.Invoke("run_sandbox", args) →
//     sandboxManager.Execute(ctx, &ExecRequest{
//         Language:   "python",
//         Code:       "<code>",
//         Stdin:      "<csv>",
//         Timeout:    30 * time.Second,
//         Env:        map[string]string{"PYTHONUNBUFFERED":"1"},
//         NetworkMode:"none",
//         Mem:        512 * 1024 * 1024,   // 默认 512MB
//         CPUQuota:   100000,               // 0.1 core？改为 1 core 默认
//         StreamOut:  out chan<- StreamEvent,
//     })
//
//   ctx 已由 orchestrator 设置 deadline = now + 30s。
//
// ── Step 2：pickImage + 资源校验 ──────────────────────────────────────
//
//   image := m.imageMap["python"]   // "agent-sandbox-python:3.11-slim"
//
//   校验：
//     · image 是否已本地缓存（docker image inspect）
//     · request.Timeout ≤ maxTimeout (60s)
//     · request.Mem ≤ maxMem (1GB)
//     · tenant 当日沙箱配额（db.tenant_sandbox_usage）
//
//   通过。
//
// ── Step 3：ContainerCreate ───────────────────────────────────────────
//
//   cfg := &container.Config{
//       Image: image,
//       Cmd:   []string{"python", "-u", "-c", req.Code},
//       Env: []string{
//           "PYTHONUNBUFFERED=1",
//           "AGENT_TASK_ID=tc_sb_001",
//           "TENANT=acme",
//       },
//       User:       "1000:1000",       // 非 root
//       OpenStdin:  true,              // 要给 stdin 喂 csv
//       StdinOnce:  true,
//       Tty:        false,
//       AttachStdout: true,
//       AttachStderr: true,
//       WorkingDir: "/workspace",
//   }
//
//   hostCfg := &container.HostConfig{
//       NetworkMode: "none",                        // 完全无网
//       AutoRemove:  true,                          // 退出即清
//       ReadonlyRootfs: true,
//       Tmpfs: map[string]string{
//           "/tmp":       "rw,size=100M,exec",
//           "/workspace": "rw,size=100M,exec",
//       },
//       Resources: container.Resources{
//           Memory:     512 * 1024 * 1024,
//           MemorySwap: 512 * 1024 * 1024,      // 禁 swap
//           NanoCPUs:   1_000_000_000,          // 1 CPU
//           PidsLimit:  &pidsLimit64,           // 128
//       },
//       CapDrop: []string{"ALL"},
//       SecurityOpt: []string{"no-new-privileges:true"},
//   }
//
//   resp, err := docker.ContainerCreate(ctx, cfg, hostCfg, nil, nil, "")
//   // resp.ID = "a1b2c3d4e5f6..." (64-char container ID)
//   // 耗时：~80ms
//
// ── Step 4：Attach + Start ────────────────────────────────────────────
//
//   hijack, _ := docker.ContainerAttach(ctx, resp.ID, types.ContainerAttachOptions{
//       Stream: true, Stdin: true, Stdout: true, Stderr: true,
//   })
//   // hijack.Conn        : *net.TCPConn, 双向 I/O
//   // hijack.Reader      : *bufio.Reader 封装的响应流
//
//   docker.ContainerStart(ctx, resp.ID, types.ContainerStartOptions{})
//   // ~150ms 后容器 running
//
// ── Step 5：喂 stdin ───────────────────────────────────────────────────
//
//   go func() {
//       defer hijack.CloseWrite()
//       io.Copy(hijack.Conn, strings.NewReader(req.Stdin))  // 240KB
//   }()
//
//   Python 进程开始读 sys.stdin → pandas 解析 CSV（需 1.2s）。
//
// ── Step 6：stdcopy 解复用 stdout/stderr 流 ──────────────────────────
//
//   // Docker 多路复用协议：每帧 8 字节头
//   //   [stream][0][0][0][len_msb][len][len][len_lsb][payload...]
//   //   stream: 1=stdout, 2=stderr
//
//   stdoutR, stdoutW := io.Pipe()
//   stderrR, stderrW := io.Pipe()
//
//   // demux goroutine：从 hijack.Reader 按帧分拣
//   go func() {
//       defer stdoutW.Close()
//       defer stderrW.Close()
//       // Docker SDK 提供 stdcopy.StdCopy 做这事
//       stdcopy.StdCopy(stdoutW, stderrW, hijack.Reader)
//   }()
//
//   // stdout reader：按行读 + 推 StreamEvent
//   go func() {
//       scanner := bufio.NewScanner(stdoutR)
//       scanner.Buffer(make([]byte, 64*1024), 1024*1024)  // max 1MB/line
//       for scanner.Scan() {
//           line := scanner.Text()
//
//           select {
//           case <-ctx.Done():
//               return
//           case req.StreamOut <- StreamEvent{
//               Type:      "stdout",
//               Timestamp: time.Now(),
//               Line:      line,
//               TaskID:    "tc_sb_001",
//           }:
//           }
//
//           // 日志总量守卫
//           m.byteCounter.Add(int64(len(line)))
//           if m.byteCounter.Load() > maxOutputBytes (10MB) {
//               docker.ContainerKill(ctx, resp.ID, "SIGKILL")
//               return
//           }
//       }
//   }()
//
//   // stderr reader 同理
//
// ── Step 7：StreamEvent 穿过 orchestrator 到 SSE ─────────────────────
//
//   out 通道在 orchestrator 里被接力到前端的 SSE 响应写入：
//
//     // api/handlers/chat.go
//     w.Header().Set("Content-Type", "text/event-stream")
//     for evt := range orchestratorOut {
//         fmt.Fprintf(w, "event: sandbox.%s\ndata: %s\n\n",
//             evt.Type, evt.ToJSON())
//         flusher.Flush()
//     }
//
//   前端 EventSource 收到：
//
//     event: sandbox.stdout
//     data: {"line":"count mean std ... (pandas describe 首行)","ts":1714025930}
//
//     event: sandbox.stdout
//     data: {"line":"50%: 1234.5 ...","ts":1714025930}
//
//   总共 20 条 stdout 行，每行 ~200 字节，在 1.2s 内全部流到浏览器。
//
// ── Step 8：容器退出 + Wait ───────────────────────────────────────────
//
//   waitCh, errCh := docker.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
//
//   select {
//   case st := <-waitCh:
//       // st.StatusCode = 0 (正常退出)
//       // st.Error == nil
//   case err := <-errCh:
//       // 容器异常
//   case <-ctx.Done():
//       // 超时 or 取消
//       docker.ContainerKill(ctx, resp.ID, "SIGKILL")
//       return nil, ctx.Err()
//   }
//
// ── Step 9：收集 stats + 构造结果 ─────────────────────────────────────
//
//   stats, _ := docker.ContainerStatsOneShot(ctx, resp.ID)
//   peakMem := stats.MemoryStats.MaxUsage    // 180MB
//   cpuUsage := stats.CPUStats.CPUUsage.TotalUsage / 1e9   // ns → s
//
//   return &ExecResult{
//       TaskID:     "tc_sb_001",
//       ExitCode:   0,
//       StdoutSize: 4820,               // bytes
//       StderrSize: 0,
//       Duration:   1.38 * time.Second,
//       PeakMemMB:  180,
//       CPUSeconds: 0.9,
//       Truncated:  false,
//       // Note: 实际 stdout 不再放到结果里（已经流给前端），
//       // 这里只放 tail 5 行供 LLM 消费
//       StdoutTail: "mean 8f42...\nstd 2.31\n...",
//   }, nil
//
// ── Step 10：container AutoRemove ──────────────────────────────────────
//
//   因为 HostConfig.AutoRemove = true，Docker daemon 在容器退出时自动删：
//
//     docker events (后台):
//       container die       a1b2c3
//       container destroy   a1b2c3
//
//   fs/网络/cgroup 全部回收，zero leftover。
//
// ── Step 11：orchestrator 消费 ExecResult ─────────────────────────────
//
//   作为 tool_result 回喂 LLM：
//
//     Message{
//         Role:       "tool",
//         ToolCallID: "tc_sb_001",
//         Content: fmt.Sprintf(
//             "exit=0 duration=1.38s mem=180MB\n\nOutput tail:\n%s",
//             result.StdoutTail),
//     }
//
//   LLM 第 N+1 轮继续：
//     "The data has 1000 rows with columns [price, volume...]
//      Let me run a deeper analysis..."
//
// ── 超时分支（另一种 flow）─────────────────────────────────────────────
//
//   若脚本跑 35s（> 30s）：
//     ctx deadline → context.DeadlineExceeded
//     docker.ContainerKill(SIGKILL)
//     result = &ExecResult{
//         ExitCode: -1,
//         ErrorCode:"TIMEOUT",
//         Message:  "sandbox execution exceeded 30s",
//         StdoutTail: "... (what was produced before kill)",
//     }
//
//   AutoRemove 仍会把容器干净清掉。
//
// ── 整体数据形变 ──────────────────────────────────────────────────────
//
//   ToolCall {code, stdin(240KB), timeout=30s}
//       ↓
//   ExecRequest → Docker ContainerCreate + hijack attach
//       ↓
//   hijack.Conn ← stdin(240KB, unidirectional write)
//                 ↓ Python 执行
//   hijack.Reader → stdcopy demux
//       ├─ stdout → StreamEvent × 20 → orchestrator → SSE → 浏览器
//       └─ stderr → (empty)
//       ↓ ContainerWait exit=0
//   Stats 收集 → ExecResult {exit=0, mem=180MB, duration=1.38s}
//       ↓
//   AutoRemove 清理容器
//       ↓
//   tool_result msg 给 LLM
//
//   关键性能指标：
//     · 容器启动：Create 80ms + Start 150ms = 230ms
//     · 解复用延迟：单行 stdout 到 SSE 推送 < 10ms
//     · 总延迟：1.38s（其中 python 执行 1.15s）
//     · 资源回收：零遗留
//
// =============================================================================

package sandbox
