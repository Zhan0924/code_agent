# 项目基础

## 项目身份

code_agent 是一个基于 ReAct 循环的代码智能体后端，使用 Go 1.25 编写，通过 LLM 驱动的工具调用循环完成代码理解、编辑、测试和部署任务。配套前端 code_agent_ui 是 React 19 + Vite 单页应用。

## 双项目布局

两个兄弟项目，独立代码库，通过 HTTP/SSE/WS 通信：

| 项目 | 路径 | 技术栈 | 入口 |
|------|------|--------|------|
| code_agent（后端） | `code_agent/` | Go 1.25, Gin, Redis, Viper | `cmd/agent/main.go` |
| code_agent_ui（前端） | `code_agent_ui/` | React 19, TypeScript 6, Vite 8 | `src/main.tsx` |

无顶层 workspace 文件，执行命令前需 `cd` 到对应子项目。

## 必选 vs 可选子系统

启动必须满足两个硬依赖，其余全部优雅降级为 nil：

| 子系统 | 状态 | 失败行为 |
|--------|------|----------|
| **Redis** | **必选** | `Ping` 失败 → fatal 退出 |
| **LLM Client** | **必选** | `NewClient` 失败 → fatal 退出 |
| RAG（Qdrant + Embedder） | 可选 | `ragEngine = nil`，无代码检索 |
| Sandbox（Docker） | 可选 | `sandboxMgr = nil`，无代码执行 |
| MCP Gateway | 可选 | 无外部工具服务 |
| PostgreSQL Store | 可选 | 仅当 `cfg.Postgres.DSN != ""` 初始化 |
| Temporal Worker | 可选 | 仅当 `cfg.Temporal.Host != ""` 启动 |
| Tracing（OTel） | 可选 | 无分布式追踪 |
| Auth（JWT + API Key） | 可选 | 仅当 `cfg.Auth.Enabled` 开启 |

## 构建 / 运行 / 测试

### 后端

```bash
make build          # go build -> bin/code-agent
make run            # build + run（默认读 configs/config.yaml）
make test           # go test -race -cover ./...（需要 Redis）
make test-short     # -short 标志（当前实际无 testing.Short() 守卫，等价于 make test）
make lint           # golangci-lint（配置见 .golangci.yml）
make docker-up      # docker compose up -d（redis/postgres/qdrant/temporal/agent）
```

单包测试：`go test -race -run TestXxx ./internal/orchestrator`

注意：`go.mod` 声明 go 1.25.0，但 CI 和 `.golangci.yml` 使用 Go 1.22。若 `go build` 报 toolchain 问题，设置 `GOTOOLCHAIN=auto`。

### 前端

```bash
npm run dev         # Vite dev server（代理到 localhost:18080）
npm run build       # tsc -b && vite build
```

前端无测试框架，无测试文件。

## 配置

- 主配置文件：`configs/config.yaml`（Viper 加载，**已被 `.gitignore` + `.dockerignore` 排除**）
- 配置模板：`configs/config.example.yaml`（仓库内的唯一可见配置，仅含占位符）
- 环境变量覆盖前缀：`CODE_AGENT_`，分隔符 `_`
- 优先级（高到低）：环境变量 > YAML 文件 > Viper 默认值 > Go 零值
- `${VAR}` 模式在 16+ 个字符串字段上展开（含 MCP 服务器配置）
- 验证：`config.Validate()` 在启动时执行，失败 fatal
- 详见 `docs/architecture/01_config.md`
