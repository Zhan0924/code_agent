# 优化方案 V2 — 对齐 Gap Analysis V2 的工程落地规划

> 基础文档：`docs/gap_analysis_v2.md`（SOTA 对标）+ `docs/optimization_plan.md`（V1 已完成项）
> 本文只列 **V1 未完成 / V2 新识别** 的差距，给出文件级、可排期的落地清单。
> 原则：**先拿量化胜利 → 再补 IDE 入口 → 最后补多模态/长期记忆**。

---

## 0. TL;DR — 3 个 Sprint，15 件事

| Sprint | 目标 | 关键交付物 | 对应 Gap |
|---|---|---|---|
| **A（第 1 月）** | 让分数"可公开" | SWE-Bench Lite 跑分、Cost 仪表盘、Plan-Drift、精细检索工具、Workspace diff Accept/Reject | §2.8 / §2.9 / §2.1 / §2.2 / §2.3 |
| **B（第 2 月）** | 让 Agent 进 IDE | VSCode Extension MVP、gopls-MCP、Executor DAG 并行、Trajectory 沉淀 | §2.3 / §2.6 / §2.1 |
| **C（第 3 月）** | 让 Agent "看到" | Playwright 工具、Vision 透传、CLI 版、Cross-Session Memory | §2.4 / §2.5 / §2.7 / §2.10 |

---

## 1. Sprint A — 拿量化胜利（4 周）

### A1. SWE-Bench Lite Harness（**最高优先级**，3+5 天）
- **目标**：在公开榜单上打出 pass@1 分数，证明端到端可用性。
- **新增目录**：`bench/swebench/`
  - `bench/swebench/harness.go` — 读取 SWE-Bench Lite 300 个 instance
  - `bench/swebench/runner.go` — 每个 instance 走 `orchestrator.Run(task)`，隔离临时 workspace
  - `bench/swebench/grader.go` — 运行官方 `pytest` 补丁验证
  - `bench/swebench/report.go` — 产出 `pass@1 / pass@3 / cost_usd / p95_latency`
- **改动**：
  - `internal/workspace/manager.go`：新增 `CloneRepoAt(commit)`，支持 pin 到 SWE-Bench 指定 commit
  - `internal/sandbox/manager.go`：新增 `RunPython(timeout, repoMount)` 专用工厂方法
- **验收**：
  - `make bench-swe-lite` 产出 `bench/results/swe-lite-YYYYMMDD.json`
  - CI 每周一次 nightly，自动 push 到 `docs/benchmarks.md`

### A2. Cost 归因 + 仪表盘（3+3 天）
- **目标**：每次 LLM 调用可归因到 (session, task, user)，前端显示预估花费。
- **后端**：
  - `internal/metrics/cost.go`：已有 Prometheus counter，**新增**：
    - `RecordLLMCall(sessionID, taskID, model, inputTok, outputTok, priceIn, priceOut)`
  - `internal/store/postgres.go`：新增表 `llm_spend(session_id, task_id, user_id, model, in_tok, out_tok, cost_usd, ts)`
  - `internal/llm/client.go`：`Chat()` 返回后立即写入，带 OTel span attr `llm.cost_usd`
  - `internal/api/handlers.go`：新增 `GET /api/v1/spend/summary?user=&range=7d`
- **前端**：
  - `code_agent_ui/src/pages/CostPage.tsx`（新）：日/周/月折线图 + Top 5 task cost 榜
  - `Layout.tsx` 侧栏增加 💰 入口

### A3. 用户 / 项目级预算熔断（1 周）
- `internal/auth/budget.go`（新）：
  ```go
  type Budget struct{ UserID string; DailyUSD, MonthlyUSD float64 }
  func (b *Budget) Allow(ctx, costEst) error // 超限返回 ErrBudgetExceeded
  ```
- `internal/llm/client.go` 调用前拉 Redis 计数器 `spend:{user}:{YYYYMMDD}`
- `internal/api/middleware.go`：新增 `BudgetMiddleware()`，超限 HTTP 402
- 配置：`configs/config.yaml` 加 `auth.budget.default_daily_usd: 5.0`

### A4. Plan-Drift Detector（3 天）
- **痛点**：Planner 中途被 LLM 带偏，最终跑偏原始目标。
- **新增文件**：`internal/planner/drift_detector.go`
  ```go
  type DriftDetector struct{ origGoal string; embedder Embedder }
  func (d *DriftDetector) CheckStep(stepDesc string) (similarity float64, drifted bool)
  ```
- 每步骤开始前计算与 `origGoal` 的 embedding cos-sim；<0.55 → 触发 `ReplanSignal`
- **钩入**：`internal/planner/executor.go::runStep()` 前置调用
- **Metric**：`plan_drift_detected_total{severity}` Counter

### A5. 精细检索工具包（1 周）
- **现状**：只有粗粒度 `rag.Search()`。
- **对标 Cursor / Claude Code**：`find_symbol` / `find_references` / `grep` / `semantic_search` 四把独立工具。
- **新增目录**：`internal/tools/code/`
  - `find_symbol.go`：基于 `repomap.Generator` 反向索引，O(1) 查符号定位
  - `find_references.go`：调用 `gopls` LSP（见 B2）或 AST 粗查 fallback
  - `grep.go`：封装 `ripgrep` 在 workspace 沙箱内执行
  - `semantic_search.go`：调 `rag.Engine.Query()` + Rerank，参数 `topK, collection_filter`
- **注册**：`internal/tools/registry.go::init()` 注册 4 个新 tool schema，暴露给 Planner 的 function_call

### A6. Workspace Diff Accept/Reject UI（1 周）
- **现状**：`orchestrator.edit_engine` 写入文件后无评审环节。
- **后端**：
  - `edit_engine.go`：新增 `DryRun` 模式，返回 unified-diff 不落盘
  - `internal/api/workspace_handlers.go`：新增
    - `POST /api/v1/workspace/diff/propose` → 返回 `diff_id` + patch
    - `POST /api/v1/workspace/diff/{id}/accept|reject`
  - Pending diffs 缓存在 Redis `diff:{workspace}:{id}`，TTL 30min
- **前端**：
  - `WorkspacePage.tsx`：接入 `@monaco-editor/react` diff view，Accept / Reject 按钮回调接口
  - 全局 toast 统计待审数量

**Sprint A 验收线**：一个命令 `make sprint-a-demo` 能演示：输入 bug 描述 → 跑 SWE-Bench style 流程 → diff 待审 → 花费落表 → 预算超限阻断。

---

## 2. Sprint B — 进 IDE（4 周）

### B1. VSCode Extension MVP（6 周，优先级最高）
- **新增仓库目录**：`code_agent_vscode/`（独立 TS 项目，不污染 Go mod）
  ```
  code_agent_vscode/
    package.json                 # activationEvents: onStartupFinished
    src/extension.ts             # 入口：注册 chat webview + commands
    src/webview/ChatPanel.tsx    # 复用 code_agent_ui/ChatPage 的 API client
    src/commands/slash.ts        # /explain /fix /test /doc 四个 slash 命令
    src/diff/DiffCodeLens.ts     # 在行上显示 💡 Agent 建议
    src/api/backend.ts           # HTTP 指向 localhost:8080（或用户配置）
  ```
- **后端配合**：
  - `internal/api/router.go`：新增 CORS allow `vscode-webview://*`
  - `internal/auth/jwt.go`：新增 `IssueDeviceToken(machineID)` 免密登录
- **发布**：
  - Marketplace 发布前先用 `vsce package` 生成 `.vsix`
  - `README.md` 写 "Use from VSCode" 小节

### B2. gopls-MCP 桥接（1 周）
- **目标**：让 Agent 拥有真正的 Go 语义理解（跨文件 rename / find refs / hover）。
- **新增文件**：`internal/mcp/bridges/gopls.go`
  - 启动 `gopls serve -mode=stdio`，用已有 `mcp.Client` 包装成 MCP Tool
  - 暴露 tools：`go_find_references` / `go_rename_symbol` / `go_hover` / `go_definition`
- **注册**：`cmd/agent/main.go` 启动时探测 `$PATH` 中的 `gopls`，存在则自动挂载
- **降级**：不存在则 `log.Warn("gopls not found; Go semantic tools disabled")`

### B3. Executor DAG 并行化（1 周）
- **现状**：`planner.Executor` 串行执行 step。
- **改动**：`internal/planner/executor.go`
  - `Step` 增加字段 `DependsOn []int`
  - 调度器：拓扑排序 + `golang.org/x/sync/errgroup`，无依赖 step 并发执行
  - 输出/副作用用 `context.WithValue` 的 `stepBus` 收集，避免 race
- **验证**：多文件独立改写 case 延迟下降 >40%

### B4. Trajectory / 任务历史沉淀（1 周）
- **目标**：相似任务直接检索历史成功 trajectory，避免重复规划。
- **新增表**：`trajectories(id, user_id, goal_embedding vector(768), plan jsonb, outcome, duration_ms, ts)`
- **新增文件**：
  - `internal/store/trajectory.go`：CRUD + pgvector 查询
  - `internal/planner/trajectory_recall.go`：Planner 入口前查 top-3 相似 trajectory，注入到 system prompt
- **前端**：`DashboardPage.tsx` 新增 "成功案例复用率" 指标 

**Sprint B 验收线**：在 VSCode 里打开一个 Go 项目，选中函数 → 右键 `Agent: Find References & Refactor` → Agent 并行改完多文件 → diff 在 IDE 内审阅 → Accept。

---

## 3. Sprint C — 感知能力扩展（4 周）

### C1. Playwright 浏览器工具（1.5 周）
- **目的**：E2E 前端验证、截图比对、爬取文档。
- **新增目录**：`internal/tools/browser/`
  - `playwright.go`：wrap `github.com/playwright-community/playwright-go`
  - Tools：`browser_open(url)` / `browser_click(selector)` / `browser_screenshot()` / `browser_eval(js)`
- **沙箱**：Playwright 进程跑在独立 Docker `mcr.microsoft.com/playwright:focal`，`NetworkMode=bridge`（受限白名单）
- **Metric**：`browser_session_active` Gauge

### C2. Vision / 截图透传（1 周）
- **后端**：
  - `internal/models/models.go`：`Message.Content` 支持 `[]ContentPart`（text|image_url）
  - `internal/llm/openai_provider.go`：Chat payload 支持 `image_url` Base64 / URL
  - `internal/api/handlers.go`：`POST /api/v1/chat/upload` multipart，存对象存储，返回 URL
- **前端**：
  - `ChatPage.tsx`：拖拽上传图片，缩略图 inline
  - 后端收到 vision model 请求时路由到 `gpt-4o-mini` / `claude-3.5-sonnet`

### C3. CLI 版（Aider-like）（2 周）
- **新增**：`cmd/ca/main.go`（second binary）
  - 基于 `spf13/cobra`，子命令：`ca chat` / `ca run <prompt>` / `ca diff`
  - 直接复用 `internal/orchestrator`，不走 HTTP；配置读 `~/.code-agent/config.yaml`
- **Makefile**：`make cli` → `bin/ca`
- **README**：单独一节 Quick Start

### C4. Cross-Session Memory（2 周）
- **表**：
  - `user_preferences(user_id, key, value, weight, updated_at)`
  - `workspace_knowledge(workspace_id, title, content, embedding, created_at)`
- **新增模块**：`internal/memory/`
  - `memory/store.go`：pgvector 读写
  - `memory/tool.go`：暴露 `memory_remember(key, value)` / `memory_recall(query)` 两个 LLM 工具
- **注入**：`context/prompt_builder.go` 系统 prompt 追加 top-5 recall 结果

**Sprint C 验收线**：上传登录页截图 → Agent 生成对应 React 组件 → Playwright 验证像素相似度 >95% → 记忆到 `workspace_knowledge`。

---

## 4. 横切改进（贯穿三个 Sprint）

### 4.1 可观测性深化
- 所有新工具默认挂 OTel span：`tool.name` / `tool.cost_usd` / `tool.latency_ms`
- Grafana Dashboard 新面板：Tool 调用热力图、失败 Top N

### 4.2 安全
- Playwright / Browser 工具禁止出站 `169.254.*` / `10.*`（已在 `security/egress.go` 的 deny list）
- 新 MCP server 挂载必须走 `mcp_skill_handlers.go::Install` 审批流

### 4.3 测试
- 每 Sprint 至少新增 10 个集成测试，覆盖率不低于当前 `cov.out` 水平
- `test_comprehensive.sh` 追加 Sprint 对应 smoke case

---

## 5. 风险与缓解

| 风险 | 缓解 |
|---|---|
| VSCode 插件 6 周做不完 | 先发布只带 Chat + Diff 的 0.1.0，slash 命令迭代 |
| SWE-Bench 分数过低难看 | 只公布 **pass@3** + 对标 baseline（如 SWE-agent v0.1），重点叙事"企业合规场景优先" |
| Playwright 镜像大（~1.5G） | Lazy pull，只在 first use 时拉取 + 本地 volume cache |
| gopls 与 MCP 桥接延迟 | 进程常驻 + idle 5min 回收 |
| Cost 表写爆 PG | 分区表 monthly partition + 6 个月后归档 S3 |

---

## 6. 验收与发布

- **每 Sprint 末**：打 tag `vX.Y.0`，更新 `ROADMAP.md`
- **90 天末**：发布 `v1.0-public`，同步更新 `README.md` 首屏：SWE-Bench 分数 + VSCode 安装二维码 + Demo 视频
- **对外材料**：`docs/benchmarks.md` + `docs/architecture_v2.md`（补全 VSCode/CLI/Memory 层的架构图）

---

## 7. 与 V1 Optimization Plan 的衔接

`docs/optimization_plan.md` 中 V1 的 P0/P1 已基本完成（Edit Engine、Git Tools、Auto Test、Session 摘要、Qdrant 租户隔离、OTel、HMAC、JWT、Egress 白名单）。

**本文（V2）= V1 遗留 + Gap Analysis V2 新识别**，不覆盖 V1 已验收项。若需回溯 V1 具体任务，请继续查看 `optimization_plan.md` 的 "Phase 1/2 Completed" 段落。
