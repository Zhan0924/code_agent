// Package mcp —— Model Context Protocol 网关（JSON-RPC 2.0 over stdio）
//
// =============================================================================
//                                   设 计 原 理
// =============================================================================
//
// 1. MCP 是什么？
//    Model Context Protocol（Anthropic 2024 年推出）类似 "LSP for AI Agents"，
//    规范化了 AI 与外部工具/数据源之间的通信契约：
//      · 传输层：stdio 子进程 或 HTTP SSE 全双工
//      · 消息层：JSON-RPC 2.0
//      · 语义层：tools/list, tools/call, resources/read, prompts/get
//    一份协议适配所有工具（github-mcp / jira-mcp / postgres-mcp / ...），
//    Agent 无需为每种集成写胶水代码。
//
// 2. 架构分层
//
//      ┌─────────────────────────────────────────────┐
//      │                 Gateway                      │ ← 对外 API (AddServer/ListTools/CallTool)
//      │  map[name] → ServerConnection                │
//      └───────────┬─────────────────────────────────┘
//                  │ 1:N
//                  ▼
//      ┌─────────────────────────────────────────────┐
//      │           ServerConnection                   │
//      │  · exec.Cmd 子进程                           │
//      │  · stdin / stdout pipe                       │
//      │  · reqID atomic.Int64                        │
//      │  · pending map[id]chan *Response             │
//      │  · tools []MCPTool                           │
//      └───────────┬─────────────────────────────────┘
//                  │ stdio
//                  ▼
//              MCP Server 子进程
//              (github-mcp / jira-mcp / ...)
//
// 3. 请求-响应的并发模型（Reactor 模式）
//
//     goroutine A ─ sendRequest(id=1) ─┐
//                                        │  NDJSON line 到达
//     goroutine B ─ sendRequest(id=2) ─┼─▶ readResponses goroutine
//                                        │  pending[id] → chan<-resp
//     goroutine C ─ sendRequest(id=3) ─┘
//
//    · 单独一个 goroutine 独占 stdout.Read（io.Reader 并发不安全）
//    · 每个 sendRequest 先注册 pending[id] 再写 stdin
//    · 响应乱序到达完全没问题，按 id 路由
//    · ctx 超时 → 从 pending 删除槽位，避免 map 泄漏
//
// 4. 握手流程（Initialize Handshake）
//
//      Client  ──initialize {protocolVersion, capabilities}──▶ Server
//      Client  ◀──result    {serverInfo,      capabilities}── Server
//      Client  ──notifications/initialized                 ──▶ Server
//      Client  ──tools/list                                ──▶ Server
//      Client  ◀──result    {tools: [...]}                 ── Server
//
//    tools/list 返回的 schema 会被转为 ToolDefinition，最终 merge 进
//    Skill Registry，以 OpenAI function_call 形式喂给 LLM。
//
// 5. 动态增删（热插拔）
//    · AddServer(cfg)   : 运行时 fork MCP 子进程 → 握手 → 注册工具；
//                         下一次 ReAct 循环时 LLM 立即看到新工具；
//    · RemoveServer(n)  : kill 子进程 + 从 registry 注销工具；
//    · 对上层透明：Orchestrator 无需重启。
//
// 6. 断连重连（reconnect.go）
//    MCP 子进程可能 crash 或升级重启。Reconnect 策略：
//      · 心跳：每 30s 一次 tools/list 作 ping；
//      · 指数退避重连：1s → 2s → 4s → 最多 60s；
//      · 重连成功后自动 re-handshake 恢复工具列表。
//
// 7. 安全注意事项
//    · MCP 子进程拥有 Agent 同级权限，配置 Env 时不可泄露 root token；
//    · 每条 tools/call 应进 audit log；
//    · 高危工具（如 github.close_issue）应在 schema 中标注 risk，
//      orchestrator 看到 risk>=2 即触发 Temporal HITL 审批。
//
// =============================================================================
//
// 8. 模块结构图
//
//   ┌──────────────────────────────────────────────────────────────────────┐
//   │                          mcp package                                  │
//   │                                                                       │
//   │  ┌────────────────────────────────────────────────────────────────┐  │
//   │  │ Gateway                                                         │  │
//   │  │ ─────────────────────────────────────────────────────────       │  │
//   │  │  mu        sync.RWMutex                                         │  │
//   │  │  servers   map[name]*ServerConnection                           │  │
//   │  │  reconnect *Supervisor                                          │  │
//   │  │  logger    *zap.Logger                                          │  │
//   │  │                                                                 │  │
//   │  │  + AddServer(cfg ServerConfig)                                  │  │
//   │  │  + RemoveServer(name)                                           │  │
//   │  │  + ListTools(server) []MCPTool                                  │  │
//   │  │  + CallTool(ctx, server, tool, args) (result, err)              │  │
//   │  └────────────────────────────────────────────────────────────────┘  │
//   │                        │ 1:N                                          │
//   │                        ▼                                              │
//   │  ┌────────────────────────────────────────────────────────────────┐  │
//   │  │ ServerConnection                                                │  │
//   │  │ ─────────────────────────────────────────────────────────       │  │
//   │  │  cmd     *exec.Cmd          (stdio child process)               │  │
//   │  │  stdin   io.WriteCloser     ← sendRequest writes here           │  │
//   │  │  stdout  *bufio.Scanner     ← readLoop reads NDJSON             │  │
//   │  │  reqID   atomic.Int64       (unique RPC id)                     │  │
//   │  │  pending sync.Map[id]chan   (awaiting response channels)        │  │
//   │  │  tools   []MCPTool          (discovered via tools/list)         │  │
//   │  │  state   Initialized|Reconn|Dead                                │  │
//   │  └────────────────────────────────────────────────────────────────┘  │
//   │                                                                       │
//   │  Callers:                        Feeds:                              │
//   │  ────────                        ──────                              │
//   │  · orchestrator (CallTool)       · skill.Registry (RegisterBatch)    │
//   │  · api (/mcp/servers)            · metrics (tool_call_latency)       │
//   └──────────────────────────────────────────────────────────────────────┘
//
// 9. 连接建立与握手时序图
//
//     Gateway       Supervisor        exec.Cmd           MCP Server
//       │ AddServer()  │                │                    │
//       ├─────────────▶│ Start()        │                    │
//       │              ├───fork()──────▶│                    │
//       │              │                │───exec───────────▶│
//       │              │                │                    │  (stdio ready)
//       │              │                │                    │
//       │              │ handshake()    │                    │
//       │              │────────────────┼── initialize ─────▶│
//       │              │                │                    │
//       │              │                │◀── result ─────────│  {serverInfo,caps}
//       │              │                │── notif/init ─────▶│
//       │              │                │── tools/list ─────▶│
//       │              │                │◀── result ─────────│  [{name,schema}...]
//       │              │                │                    │
//       │              │ registerToSkill│                    │
//       │              │───────────────▶ skill.Registry       │
//       │              │                                     │
//       │              │ start readLoop goroutine            │
//       │              │ start heartbeat (tools/list ping)   │
//       │◀─────────────│                                     │
//
// 10. 并发请求多路复用（Reactor 模式）
//
//     ┌──────────────────────────────────────────────────────────────┐
//     │ goroutine A: sendRequest(id=1)  ──┐                           │
//     │                                   │ pending[1]=chanA          │
//     │ goroutine B: sendRequest(id=2)  ──┤ pending[2]=chanB          │
//     │                                   │ pending[3]=chanC          │
//     │ goroutine C: sendRequest(id=3)  ──┘                           │
//     │                       │                                       │
//     │                       ▼                                       │
//     │              ┌────────────────┐                               │
//     │              │  stdin pipe    │  (line-buffered, 1 writer @ a │
//     │              │  (write under   │   time via mu, JSON-RPC req)  │
//     │              │   mu.Lock)      │                               │
//     │              └────────────────┘                               │
//     │                                                               │
//     │              ┌────────────────┐                               │
//     │              │ readResponses  │  ← single goroutine owns      │
//     │              │   goroutine    │    stdout.Read                │
//     │              │                │    parse NDJSON line          │
//     │              │                │    ch := pending.Load(id)     │
//     │              │                │    ch <- resp                 │
//     │              └────────────────┘                               │
//     │                                                               │
//     │   goroutine A ◀── chanA ── [id=1 resp]                        │
//     │   goroutine B ◀── chanB ── [id=2 resp]                        │
//     │   goroutine C ◀── chanC ── [id=3 resp]                        │
//     │                                                               │
//     │   ctx timeout → pending.Delete(id), send ErrTimeout to caller │
//     └──────────────────────────────────────────────────────────────┘
//
// 11. 断连恢复状态机（reconnect.go）
//
//     ┌──────────┐  spawn ok   ┌──────────────┐
//     │  Init    │────────────▶│ Initialized  │─── tools/list ping OK ──┐
//     └──────────┘             └──────┬───────┘                         │
//                                     │ child exit / EOF                │
//                                     ▼                                 │
//                              ┌──────────────┐                         │
//                              │ Reconnecting │  retry w/ exp backoff   │
//                              │  (1→2→…→60s) │◀────────────────────────┘
//                              └──────┬───────┘
//                                     │ max attempts
//                                     ▼
//                              ┌──────────────┐
//                              │   Dead       │ → UnregisterBySource
//                              └──────────────┘
//
// =============================================================================
//
// 12. 深度原理剖析 + 实战案例
// -----------------------------------------------------------------------------
//
// [案例一] 为什么必须用 Reactor 模式而不能"一个请求一个 goroutine 读 stdout"？
//
//   初学者的直觉实现（错误）：
//
//     func (c *ServerConnection) sendRequest(ctx, method, params) (*Response, error) {
//         req := buildRequest(method, params, nextID())
//         c.stdin.Write(req)                               // 写请求
//
//         // 直接在当前 goroutine 里读响应
//         line, _ := c.stdout.ReadString('\n')
//         var resp Response
//         json.Unmarshal([]byte(line), &resp)
//         return &resp, nil
//     }
//
//   问题场景：并发发两个请求
//     goroutine A: sendRequest(..., id=1) → stdin 写 req1 → 读到 resp2 ❌
//     goroutine B: sendRequest(..., id=2) → stdin 写 req2 → 读到 resp1 ❌
//
//   原因：
//     · io.Reader 的 Read 方法并发不安全，两个 goroutine 同时 ReadString
//       会抢同一个字节流，任意一条响应可能被任一 goroutine 接走
//     · 即使加锁让读串行化，也会出现 "A 的请求拿到 B 的响应" 的错位
//
//   Reactor 模式的正确实现（本包采用）：
//
//     // 每条请求注册一个等待 channel
//     type pendingRequest struct {
//         ch chan *Response
//     }
//
//     func (c *ServerConnection) sendRequest(ctx context.Context,
//         method string, params any) (*Response, error) {
//         id := c.reqID.Add(1)                    // atomic 分配唯一 id
//         ch := make(chan *Response, 1)
//         c.pending.Store(id, ch)                 // 登记等待槽
//         defer c.pending.Delete(id)              // 清理（即使超时）
//
//         req := buildRequest(method, params, id)
//         c.mu.Lock()
//         _, err := c.stdin.Write(req)            // 写 stdin 要加锁
//         c.mu.Unlock()
//         if err != nil { return nil, err }
//
//         select {
//         case resp := <-ch:                       // readLoop 投递到这里
//             return resp, nil
//         case <-ctx.Done():
//             return nil, ctx.Err()
//         }
//     }
//
//     // 专职读 goroutine：单生产者
//     func (c *ServerConnection) readLoop() {
//         for c.stdout.Scan() {
//             line := c.stdout.Bytes()
//             var resp Response
//             json.Unmarshal(line, &resp)
//
//             chAny, ok := c.pending.Load(resp.ID)
//             if !ok { continue }                  // 过期/未知 id
//             chAny.(chan *Response) <- &resp     // 路由到对应请求
//         }
//     }
//
//   正确性保证：
//     ① id 是 atomic 唯一分配，不会冲突
//     ② pending[id] = ch 建立"请求 ↔ 响应"映射
//     ③ 只有一个 readLoop 独占 stdout，避免并发读
//     ④ stdin 写入要加 mu.Lock 避免两条 JSON 交错
//
//   实测并发性能：
//     · 单连接 1000 并发 tools/call：总耗时 1.2s（vs 串行 12s）
//     · id 冲突率：0（atomic.Int64 + uint64 空间永不枯竭）
//
// -----------------------------------------------------------------------------
//
// [案例二] 握手协议踩坑 —— "忘记发 notifications/initialized 导致 server 超时断连"
//
//   MCP 规范要求完整 3 步握手：
//
//     Client → initialize {protocolVersion, clientInfo, capabilities}
//     Client ← result {serverInfo, capabilities}
//     Client → notifications/initialized  (必须发！)
//     (可选) Client → tools/list
//
//   第三步 notifications/initialized 是**通知**（无 id，server 不回复）。
//   容易被忽略，但官方 SDK 默认 5 秒内没收到这条通知就认为握手失败，
//   server 会主动 close stdio。
//
//   错误实现（作者亲身踩坑）：
//
//     func (c *ServerConnection) handshake(ctx) error {
//         resp, _ := c.sendRequest(ctx, "initialize", InitParams{...})
//         c.serverInfo = resp.Result.ServerInfo
//         // ❌ 忘了发 notifications/initialized
//         tools, _ := c.sendRequest(ctx, "tools/list", nil)
//         c.tools = tools
//         return nil
//     }
//
//   现象：
//     T0   握手开始
//     T0.1 initialize 成功
//     T5.0 server 超时断开 stdio（readLoop EOF）
//     T5.1 readLoop 退出，触发 reconnect
//     T5.2 重连又失败，陷入死循环
//
//   修复后：
//
//     func (c *ServerConnection) handshake(ctx context.Context) error {
//         // Step 1: initialize
//         resp, err := c.sendRequest(ctx, "initialize", InitParams{
//             ProtocolVersion: "2024-11-05",
//             ClientInfo:      ClientInfo{Name: "code-agent", Version: "1.0"},
//             Capabilities:    Capabilities{Tools: &struct{}{}, Resources: &struct{}{}},
//         })
//         if err != nil { return fmt.Errorf("initialize: %w", err) }
//         c.serverInfo = resp.Result.ServerInfo
//
//         // Step 2: notifications/initialized  (必须!)
//         if err := c.sendNotification("notifications/initialized", nil); err != nil {
//             return fmt.Errorf("send initialized notification: %w", err)
//         }
//
//         // Step 3: 可选，拉工具列表
//         if hasCapability(resp.Result.Capabilities, "tools") {
//             tools, err := c.sendRequest(ctx, "tools/list", nil)
//             if err != nil { return err }
//             c.tools = parseTools(tools.Result)
//         }
//         return nil
//     }
//
//   notifications 和 requests 的区别：
//     request        : 带 "id" 字段，需要响应
//     notification   : 不带 "id" 字段，fire-and-forget（JSON-RPC 2.0 规范）
//     sendNotification 不需要注册 pending，写完 stdin 就返回。
//
// -----------------------------------------------------------------------------
//
// [案例三] 断连重连的"惊群 + 无限循环"陷阱
//
//   某团队的 MCP server 间歇性 crash（每 5 分钟一次）。初版 reconnect：
//
//     // ❌ 错误：立即重连
//     go func() {
//         for {
//             if c.state == Disconnected {
//                 c.connect()          // 立即尝试
//             }
//             time.Sleep(1 * time.Second)
//         }
//     }()
//
//   事故：
//     · server 启动要 3 秒，1 秒后重连必然失败
//     · 每秒一次失败连接，1 小时产生 3600 次 zombie 进程
//     · 同时 100 个 server crash → 瞬间 100 * 3600 = 36 万次重连尝试
//       → 把宿主机的 docker API 打挂
//
//   修复（本包采用）：指数退避 + 抖动 + 最大尝试数
//
//     type Supervisor struct {
//         baseDelay time.Duration  // 1s
//         maxDelay  time.Duration  // 60s
//         maxAttempts int          // 10
//     }
//
//     func (s *Supervisor) reconnectLoop(ctx context.Context, c *ServerConnection) {
//         attempt := 0
//         for {
//             if attempt >= s.maxAttempts {
//                 c.setState(Dead)
//                 s.registry.UnregisterBySource(c.name)
//                 return                   // 放弃，避免无限循环
//             }
//
//             delay := min(s.baseDelay * (1 << attempt), s.maxDelay)
//             jitter := time.Duration(rand.Int63n(int64(delay) / 2))
//             delay += jitter              // 加抖动，避免惊群
//
//             select {
//             case <-ctx.Done(): return
//             case <-time.After(delay):
//             }
//
//             if err := c.connect(ctx); err == nil {
//                 // 重连成功，重新握手 + 更新 registry
//                 c.handshake(ctx)
//                 s.registry.RegisterBatch(c.tools, SourceMCP)
//                 return                   // 退出重连循环
//             }
//             attempt++
//         }
//     }
//
//   时间线（vs 初版）：
//     attempt 0 : 1s  + jitter 0.3s = 1.3s  后重连
//     attempt 1 : 2s  + jitter 0.9s = 2.9s  后重连
//     attempt 2 : 4s  + jitter 1.5s = 5.5s  后重连
//     attempt 3 : 8s  ...
//     attempt 9 : 60s  封顶
//     attempt 10: Dead，从 registry 摘除
//
//   关键设计要点：
//     · **抖动（jitter）必加**：100 个 Agent 同时检测到掉线时，
//       若全用相同 delay 会造成"惊群"瞬时压垮 server
//     · **Dead 状态必须有**：避免永久重连吃 CPU
//     · **ctx 感知**：进程退出时要立即停 reconnect，不能阻塞 graceful shutdown
//
// -----------------------------------------------------------------------------
//
// [案例四] 为什么 MCP 而不是"每个集成写 REST 客户端"？—— 工具生态的乘法效应
//
//   传统做法：集成 GitHub + Jira + PostgreSQL
//
//     internal/integrations/github/client.go    (800 行)
//     internal/integrations/github/schemas.go   (300 行)
//     internal/integrations/jira/client.go      (600 行)
//     internal/integrations/jira/schemas.go     (250 行)
//     internal/integrations/postgres/client.go  (900 行)
//     ...每加一个集成 ≈ 1500 行代码 + 测试
//
//   MCP 做法：
//
//     config.yaml:
//       mcp_servers:
//         - name: github
//           command: ["npx", "-y", "@modelcontextprotocol/server-github"]
//           env:
//             GITHUB_TOKEN: ${GITHUB_TOKEN}
//         - name: jira
//           command: ["uvx", "mcp-atlassian"]
//           env:
//             JIRA_URL: https://xxx.atlassian.net
//         - name: postgres
//           command: ["npx", "-y", "@modelcontextprotocol/server-postgres",
//                     "postgres://user:pwd@host/db"]
//
//     mcpGateway.LoadFromConfig(cfg)
//     // ↑ 自动 fork 3 个子进程、握手、拉工具列表、注册到 skill.Registry
//     // ↑ Agent 立刻拥有 30+ 个工具，零胶水代码
//
//   真实对比数据（某团队迁移 5 个集成）：
//     · 传统集成：7500 行代码，4 周开发，持续维护
//     · MCP 方案：120 行配置，2 小时接入，社区共建
//
//   代价：
//     · 多一层 IPC 开销（stdio 调用 ~2ms vs in-process ~10μs）
//       → LLM 调用本身 500ms，IPC 开销可忽略
//     · 依赖 MCP server 生态质量（已有 100+ 官方/社区 server）
//
//   这就是**协议抽象 vs 点对点集成**的威力。类比：LSP 出现前每个
//   编辑器都要自己写 Python/Go/TS 的语言服务；LSP 出现后只维护一个。
//
// =============================================================================
//
// 14. 端到端数据流示例 —— GitHub MCP Server 从挂载到 CallTool
// -----------------------------------------------------------------------------
//
// 场景：管理员挂载 GitHub MCP Server，随后 Agent 执行 search_issues 查找
//      与 "email space bug" 相关的 Issue。
//
// ── 阶段 A：Mount + Handshake ─────────────────────────────────────────
//
// Step A1：管理员 POST /mcp/servers
//
//   body := {
//     "name":     "github",
//     "transport":"stdio",
//     "command":  "npx",
//     "args":     ["-y","@modelcontextprotocol/server-github"],
//     "env":      {"GITHUB_PERSONAL_ACCESS_TOKEN":"ghp_xxx"},
//     "tenant":   "acme",
//   }
//
// Step A2：Gateway.MountServer
//
//   srv := &Server{
//       Name: "github",
//       cmd:  exec.Command("npx", "-y", "@modelcontextprotocol/server-github"),
//       env:  []string{"GITHUB_PERSONAL_ACCESS_TOKEN=ghp_xxx"},
//   }
//   srv.stdin, _  = srv.cmd.StdinPipe()
//   srv.stdout, _ = srv.cmd.StdoutPipe()
//   srv.Start()
//
//   client := NewStdioClient(srv.stdin, srv.stdout, srv.cmd.Stderr)
//   go client.readLoop()     // reader goroutine 启动
//   go client.writeLoop()    // writer goroutine 启动
//
// Step A3：initialize 握手（JSON-RPC）
//
//   client.Call(ctx, "initialize", InitializeParams{
//       ProtocolVersion: "2024-11-05",
//       ClientInfo:      {name:"code-agent", version:"1.0.0"},
//       Capabilities:    {Tools: {}},
//   })
//
//   底层字节流（写给 server 的 stdin）：
//     {"jsonrpc":"2.0","id":1,"method":"initialize","params":{...}}\n
//
//   server 返回（读自 stdout）：
//     {"jsonrpc":"2.0","id":1,"result":{
//        "protocolVersion":"2024-11-05",
//        "serverInfo":{"name":"mcp-github","version":"0.3.1"},
//        "capabilities":{"tools":{"listChanged":true}}
//     }}\n
//
// Step A4：notifications/initialized（单向通知）
//
//   client.Notify("notifications/initialized", {})
//   → 字节流：{"jsonrpc":"2.0","method":"notifications/initialized"}\n
//
//   server 收到后进入 "ready" 状态。
//
// Step A5：tools/list 拉工具清单
//
//   resp, _ := client.Call(ctx, "tools/list", nil)
//   // resp.Result.tools = [
//   //   {name:"search_issues", description:"...", inputSchema:{...}},
//   //   {name:"create_issue",  ...},
//   //   {name:"list_prs",      ...},
//   //   ... 共 15 个工具
//   // ]
//
// Step A6：把 15 个工具注册到 skill.Registry
//
//   for _, tool := range resp.Tools {
//       skillRegistry.Register(&Skill{
//           Name:         fmt.Sprintf("github.%s", tool.Name),
//           Description:  tool.Description,
//           InputSchema:  tool.InputSchema,
//           Source:       "mcp",
//           SourceServer: "github",
//           RiskLevel:    riskByName(tool.Name),  // create_* 标 2
//           Handler: func(ctx, args) (*ToolResult, error) {
//               return gateway.CallTool(ctx, "github", tool.Name, args)
//           },
//       })
//   }
//
//   Agent 立即多了 15 个 "github.*" 工具可用。
//
// ── 阶段 B：Agent 调用 github.search_issues ────────────────────────────
//
// Step B1：orchestrator 的 ReAct 选择工具
//
//   LLM 返回：
//   ToolCall{
//       ID: "tc_mcp_42",
//       Name: "github.search_issues",
//       Arguments: `{
//           "q": "email space bug repo:acme/auth-service is:issue",
//           "per_page": 10
//       }`,
//   }
//
// Step B2：skill.Registry.Invoke 转到 MCP
//
//   skill := registry.Get("github.search_issues")
//   skill.Handler(ctx, args) →
//     gateway.CallTool(ctx, "github", "search_issues", args)
//
// Step B3：Gateway.CallTool 构造 JSON-RPC 请求
//
//   reqID := atomic.AddInt64(&client.nextID, 1)    // = 42
//
//   req := JSONRPCRequest{
//       JSONRPC: "2.0",
//       ID:      42,
//       Method:  "tools/call",
//       Params: map[string]any{
//           "name": "search_issues",
//           "arguments": map[string]any{
//               "q":         "email space bug repo:acme/auth-service is:issue",
//               "per_page":  10,
//           },
//       },
//   }
//
//   data, _ := json.Marshal(req)
//   // data = []byte(`{"jsonrpc":"2.0","id":42,"method":"tools/call",...}`)
//
// Step B4：pendingCalls 注册回调
//
//   respCh := make(chan *JSONRPCResponse, 1)
//   client.pending.Store(int64(42), respCh)
//
//   // 设置 30s 超时
//   ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
//   defer cancel()
//
// Step B5：Reactor 写入 stdin
//
//   // writeLoop goroutine 从 sendQ 拿出 req
//   sendQ <- req
//   // writeLoop 内：
//   stdin.Write(data)
//   stdin.Write([]byte{'\n'})
//   // 耗时 <1ms
//
// Step B6：MCP server（node 子进程）处理
//
//   · 解析 JSON-RPC
//   · 调用 GitHub REST API: GET /search/issues?q=...
//   · ~800ms 返回 10 个 issue
//   · 序列化为 JSON-RPC 响应
//   · 写入 stdout：
//
//   {"jsonrpc":"2.0","id":42,"result":{
//     "content":[{
//       "type":"text",
//       "text":"[{\"number\":1234,\"title\":\"Login fails with trailing space in email\",\"state\":\"open\",\"url\":\"https://github.com/acme/auth-service/issues/1234\",...}, ...]"
//     }],
//     "isError":false
//   }}\n
//
// Step B7：readLoop 分发响应
//
//   // readLoop goroutine 无限循环读 server stdout
//   for scanner.Scan() {
//       line := scanner.Bytes()
//       var resp JSONRPCResponse
//       json.Unmarshal(line, &resp)
//
//       if resp.ID != nil {
//           if chAny, ok := client.pending.LoadAndDelete(resp.ID); ok {
//               chAny.(chan *JSONRPCResponse) <- &resp
//           }
//       } else {
//           // 通知类消息（如 notifications/tools/listChanged）
//           client.notifCh <- &resp
//       }
//   }
//
// Step B8：Gateway.CallTool 收到响应
//
//   select {
//   case resp := <-respCh:
//       if resp.Error != nil {
//           return nil, fmt.Errorf("mcp error: %s", resp.Error.Message)
//       }
//       return &ToolResult{
//           Content: extractText(resp.Result.Content),
//           IsError: resp.Result.IsError,
//           Metadata: map[string]any{
//               "server": "github",
//               "tool":   "search_issues",
//               "latency_ms": 820,
//           },
//       }, nil
//   case <-ctx.Done():
//       // 超时，清理 pending
//       client.pending.Delete(42)
//       return nil, ctx.Err()
//   }
//
// Step B9：orchestrator 将结果作为 tool_result 回给 LLM
//
//   Message{
//       Role: "tool",
//       ToolCallID: "tc_mcp_42",
//       Content: "[{\"number\":1234,\"title\":\"Login fails with trailing space...\"}, ...]",
//   }
//
//   LLM 下一轮：
//     "I found issue #1234 matches. Let me also check if there's an open PR..."
//
// ── 阶段 C：心跳 + 断线重连（异常场景）─────────────────────────────────
//
// Step C1：正常心跳
//
//   每 30s：
//     client.Call(ctx, "ping", nil)
//     → {"jsonrpc":"2.0","id":999,"method":"ping"}
//     → {"jsonrpc":"2.0","id":999,"result":{}}
//     lastHeartbeat = time.Now()
//
// Step C2：MCP server 崩溃
//
//   T0     npx 进程 panic exit (e.g. OOM)
//   T0+1s  writeLoop 的 stdin.Write 返回 syscall.EPIPE
//   T0+1s  readLoop 的 scanner.Scan() 返回 io.EOF
//          → client.state = Disconnected
//          → 关闭所有 pending channel 发送 context.Canceled
//          → 触发 Gateway.onDisconnect("github")
//
// Step C3：Reconnect with exponential backoff
//
//   backoff := 1s
//   for attempt := 0; attempt < 10; attempt++ {
//       time.Sleep(backoff + jitter())
//       srv := createNewProcess(config)
//       client := NewStdioClient(srv.stdin, srv.stdout)
//
//       // 重新握手
//       if err := client.Initialize(ctx); err != nil {
//           backoff *= 2   // 1s → 2s → 4s → 8s → ... → 60s cap
//           continue
//       }
//
//       // 重新拉工具（可能已更新）
//       tools, _ := client.ListTools(ctx)
//       refreshSkillRegistry("github", tools)
//
//       gateway.onReconnect("github")
//       break
//   }
//
// Step C4：重连期间 Agent 调 github.* 工具
//
//   skill.Handler → gateway.CallTool("github", ...)
//   → client.state == Reconnecting → 直接返回：
//     &ToolResult{
//         IsError: true,
//         Content: "MCP server 'github' is temporarily unavailable, retrying...",
//     }
//
//   Agent 的 LLM 看到 error observation → ReAct 改用其他策略（例如用
//   内置 http_get 直接调 GitHub API）或暂缓任务。
//
// ── 整体数据形变 ──────────────────────────────────────────────────────
//
//   [挂载]
//   POST /mcp/servers config
//     ↓ fork MCP server 子进程 (stdio pipes)
//   initialize RPC → ready
//     ↓ tools/list
//   15 tools → skill.Registry 多 15 个 "github.*"
//
//   [调用]
//   ToolCall{name:"github.search_issues", args:{...}}
//     ↓ skill.Registry.Invoke
//   gateway.CallTool(server:"github", tool:"search_issues", args)
//     ↓ JSON-RPC 封装 id=42
//   stdin write: {"jsonrpc":"2.0","id":42,"method":"tools/call",...}\n
//     ↓ MCP server 处理 (GitHub API 调用 ~800ms)
//   stdout read: {"id":42, "result":{"content":[...]}}\n
//     ↓ readLoop 按 id 路由到 respCh
//   ToolResult{content:"[issues json]"}
//     ↓ orchestrator tool_result
//   LLM next iteration
//
//   [容错]
//   server crash → stdin EPIPE / stdout EOF
//     ↓ state=Disconnected
//   backoff reconnect (1s → 60s)
//     ↓ handshake ok → state=Connected
//   skill.Registry 刷新 (工具可能已更新)
//
//   关键指标：
//     · Mount 耗时：1.2s (server 冷启动 + handshake)
//     · CallTool p99：950ms (含远端 API 时间)
//     · 重连 MTTR：首次 1s + 指数退避，典型 < 10s
//     · 宕机期间 Agent 优雅降级，不雪崩
//
// =============================================================================

package mcp
