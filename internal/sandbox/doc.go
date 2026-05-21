// Package sandbox 提供基于 Docker 的"阅后即焚"式安全执行沙箱。
//
// # 威胁模型
//
// 用户或 LLM 生成的脚本可能包含：恶意代码、死循环、fork 炸弹、网络横向攻击、
// 宿主机逃逸尝试。本包通过 5 层隔离将风险控制在单一短暂容器内：
//
//  1. 镜像隔离：只允许使用预先拉取的白名单镜像（python-runner:3.11 / go-runner / node-runner）
//  2. 网络隔离：NetworkMode=none，杜绝外连；或接入隔离 bridge 配合 Egress Policy
//  3. 文件系统隔离：ReadonlyRootfs + tmpfs /tmp
//  4. 资源隔离：cgroups v2 限制 memory=512MB / cpu=1 core / pids=64
//  5. 能力隔离：CapDrop=[ALL] + SeccompProfile=default + User=1000:1000 + NoNewPrivileges
//
// # 执行流程
//
//	 ┌─────────────┐
//	 │ Execute Req │
//	 └──────┬──────┘
//	        ▼
//	┌──────────────────┐
//	│ 1. Validate Lang │  ← 语言白名单
//	├──────────────────┤
//	│ 2. Tar Archive   │  ← 用户代码+依赖打包 (tar.go)
//	├──────────────────┤
//	│ 3. Container     │  ← ContainerCreate + Update (cgroups)
//	│    Create/Start  │
//	├──────────────────┤
//	│ 4. Attach I/O    │  ← io.Pipe → SSE flush 到客户端
//	├──────────────────┤
//	│ 5. Watchdog      │  ← 独立 goroutine 每 500ms 查 Stats
//	├──────────────────┤
//	│ 6. Remove        │  ← 容器销毁 (ContainerRemove force=true)
//	└──────────────────┘
//
// # 流式输出
//
// 不等容器跑完再返回全量日志。Manager.ExecuteStream 启动两个 Goroutine：
//   - 主 goroutine：持续从 docker attach 的 io.Reader 逐行读，经 pool.SmallBytePool 分配 buf 后 flush 到 chan；
//   - 看门狗 goroutine：ContainerStats 每 500ms 采样，内存超限主动 kill，超时 SIGTERM 后 SIGKILL。
//
// # 关键类型
//
//	Manager        —— 对外入口，Execute / ExecuteStream
//	Volume         —— 可选的持久化工作区挂载（workspace.Workspace 注入）
//	tar.go         —— 代码+依赖 → tar 归档的 builder
//
// # 可观测
//
// 每次执行发射：
//   - sandbox_execution_total{lang, status} —— 成功/失败/超时/OOM 计数
//   - sandbox_execution_duration_seconds —— p50/p95/p99 直方图
//   - sandbox_oom_total / sandbox_timeout_total
//
// 审计日志同时写入 audit.Logger（SIEM 合规要求）。
//
// 详见 docs/architecture/05_sandbox.md。
package sandbox
