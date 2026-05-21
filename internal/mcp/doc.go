// Package mcp 实现 Model Context Protocol（MCP）客户端，用于与外部 MCP Server
// 进行 JSON-RPC 2.0 双向通信，是 Agent 能力插拔的核心。
//
// # MCP 是什么
//
// MCP 由 Anthropic 提出，是 LLM Agent 与外部工具/资源通信的标准协议。Agent 以
// "宿主"身份启动或连接多个 "MCP Server"（可以是 Python/TS/Go 等语言写的独立进程），
// 通过 stdio 或 HTTP SSE 传输 JSON-RPC 2.0 消息，实现：
//
//   - tools/list   —— 发现远端提供的工具（含 JSON Schema）
//   - tools/call   —— 调用远端工具并接收结果
//   - resources/*  —— 读取远端只读资源（文档、数据库 schema 等）
//   - prompts/*    —— 获取预定义的提示词模板
//
// # 在本系统中的角色
//
// MCP 使 Agent 能力"活"起来——无需改 Agent 代码就能挂载新工具。典型场景：
//
//   - GitHub MCP Server：create_issue / list_prs / review_code
//   - Database MCP Server：run_sql / describe_table (只读)
//   - Jira MCP Server：create_ticket / update_status
//   - 自定义 MCP Server：企业内部 CMDB / Wiki / 自研系统
//
// # 架构
//
//	┌─────────────────────────────┐
//	│  Orchestrator               │
//	└────────────┬────────────────┘
//	             │ Call(server, tool, args)
//	             ▼
//	┌─────────────────────────────┐
//	│  Gateway                    │   ← 聚合多 server，维护 tool→server 映射
//	└────────────┬────────────────┘
//	             │
//	        ┌────┼─────┬──────────┐
//	        ▼    ▼     ▼          ▼
//	    Client1 Client2 ClientN  (reconnect.go 包裹所有 client)
//	       │     │      │
//	    stdio  stdio   HTTP
//	       │     │      │
//	       ▼     ▼      ▼
//	 [GitHub MCP] [DB MCP] [Custom MCP]
//
// # 关键机制
//
//  1. 启动批量发现：配置的每个 server 启动时调 initialize + tools/list，把 schema 注册到内存 Map
//  2. 运行时 O(1) 查询：tool FQN（如 "github.create_issue"）→ Client + 原生 tool name
//  3. 并发 pending 表：单 stdio 流支持并发请求——atomic.Int64 分配 id，map[id]chan 回发
//  4. 断线自愈：reconnect.go 指数退避重连（1s→2s→4s→...→60s 上限）
//  5. 心跳保活：每 30s ping 一次，失败 3 次即标记 offline
//
// # 关键类型
//
//	Gateway          —— 多 server 聚合器（供 Orchestrator 使用）
//	Client           —— 单 server JSON-RPC 客户端
//	ReconnectClient  —— 带自愈的 Client wrapper
//	RPCRequest/Response —— JSON-RPC 2.0 报文
//
// # 软降级
//
// 单个 MCP server 崩溃不会影响其他 server 或 Agent 主流程。Orchestrator 通过
// Registry 查询时，该 server 下所有工具标记 ⚠️ offline，不会被 LLM 选中。
// 重连成功后自动恢复。
//
// 详见 docs/architecture/06_mcp.md。
package mcp
