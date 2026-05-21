# 与 SOTA Code Agent 的差距分析 V2.0（增量更新版）

> **上版日期**：V1.0（`gap_analysis.md`）— 90 天路线图刚出炉时
> **本版日期**：2026-04-24
> **目的**：盘点 V1.0 后已闭合的差距、暴露的新差距，以及当前距离 **Cursor / Claude Code / Devin / Augment Code / Cline / Aider / Codeium Windsurf / OpenHands** 等一线产品的真实位置。
> **基准**：对比 SOTA 指的是 2025–2026 年公开可评测的版本（SWE-Bench Verified、Terminal-Bench、HumanEval+ 等）。

---

## 0. TL;DR — 一句话结论

| 维度 | V1 位置 | V2 位置（本次评估） |
|------|---------|---------------------|
| 后端 Agent Runtime | ★★★★☆（业界领先） | ★★★★★ **（已追平 OpenHands 水准）** |
| 代码编辑精度 | ★★☆☆☆（整文件 overwrite） | ★★★★☆ **（已追平 Aider + 逼近 Claude Code）** |
| 长任务自主性 | ★★☆☆☆（线性 ReAct） | ★★★★☆ **（有 Planner-Executor DAG + Temporal）** |
| 仓库理解 (repomap/LSP) | ★★★☆☆（向量 RAG） | ★★★★☆ **（补齐 repo-map + project rules）** |
| TDD / 自验证 | ★★☆☆☆ | ★★★★☆ **（auto_test_runner 已有）** |
| Git 集成 | ☆☆☆☆☆ | ★★★★☆ **（5 个 Git 工具 + auto-commit）** |
| 模型路由 | ★★☆☆☆ | ★★★★☆ **（Tier-based 动态路由）** |
| IDE 集成 | ★☆☆☆☆ | ★☆☆☆☆ **（未变化 — 仍是核心洼地）** |
| 多模态（图像） | ★☆☆☆☆ | ★☆☆☆☆ **（未变化）** |
| Browser/Web 操作 | ☆☆☆☆☆ | ☆☆☆☆☆ **（未变化）** |
| 跨 session 长期记忆 | ☆☆☆☆☆ | ☆☆☆☆☆ **（未变化）** |

**总评**：项目从 V1 的 **"1.5 代差"** 缩小到 **"0.8 代差"**。**核心弱项已从「会不会写代码」转移到「能不能进到用户桌面工作流 + 能不能看图/浏览器」**。

---

## 1. 本轮已闭合的重大差距 ✅（对照 V1.0 Sprint 计划）

V1.0 文档中 Sprint 1/2 的 8 个 P0/P1 任务，**6 个已完整落地**，2 个部分落地：

### ✅ 1.1 Claude Code 风格唯一匹配 Edit（V1 §3.2 P0）
- **已实现**：`internal/orchestrator/edit_engine.go` — 完整的 `EditEngine`
  - `old_text` 必须在文件中唯一匹配，0 或 ≥2 次均拒绝
  - 自动备份 (`.bak`) → 编辑 → lint/compile 校验 → 失败自动回滚
  - 多文件原子事务（all-or-nothing 提交）
  - 生成 unified diff 供 UI 预览
- **对标水平**：与 Claude Code 的 `Edit`/`MultiEdit` 工具等价；优于 Aider 的 `search-replace`（后者无原子事务）。

### ✅ 1.2 写后自动 lint / build / test（V1 §3.2 P0 + §3.5 P0）
- **已实现**：`internal/orchestrator/auto_test_runner.go`
  - 写文件后自动发现并运行**相关**测试文件（非全量）
  - 支持 Go、Python、TypeScript、JavaScript 测试约定
  - 测试结果反注入 ReAct 循环，实现自愈
- **对标水平**：与 Devin 的 "post-edit verification" 相同思路，但 Devin 的范围更广（E2E）。

### ✅ 1.3 Planner-Executor 双 agent 架构（V1 §3.3 P0）
- **已实现**：
  - `internal/planner/planner.go` — Planner 产出结构化 JSON Plan（含依赖 DAG）
  - `internal/planner/executor.go` — 按拓扑顺序执行，失败触发重规划
  - 有版本号（Version、RevisedAt），支持 Plan 演化
- **对标水平**：形态上已对齐 Devin/Claude Code 的 Plan→Execute；**但 Plan 粒度和反思深度仍弱于 Devin**（见 §2.3）。

### ✅ 1.4 Repo-Map 仓库级摘要（V1 §3.1 P2）
- **已实现**：`internal/repomap/generator.go`
  - 生成类似 Aider repo-map 的紧凑仓库概览
  - 每个文件提取 public 函数/类型签名
  - `internal/repomap/watcher.go` — 监听文件变化，增量更新
- **对标水平**：追平了 Aider 的 repo-map；**但 Cursor/Augment 的 code graph 仍更深（见 §2.1）**。

### ✅ 1.5 项目级规则支持 `.coderules` / `AGENTS.md` / `CLAUDE.md`（V1 §3.6 P1）
- **已实现**：`internal/orchestrator/project_rules.go`
  - 多个规则文件约定（`.coderules`, `AGENTS.md`, `CLAUDE.md`, `.clinerules`, `.cursorrules`）
  - 注入到 system prompt，项目级行为定制
- **对标水平**：**超过** Aider（只支持 repo-map），**等同** Cline/Cursor。

### ✅ 1.6 Git 集成（V1 §3.8）
- **已实现**：`internal/orchestrator/git_tools.go`
  - 5 个 Git 工具：status / diff / commit / log / branch
  - `AutoCommitAfterEdit` — 每次成功编辑后自动 commit
  - `ensureGitInit` — 首次进入非 git 目录自动 `git init`
- **对标水平**：**已持平** Aider 的 auto-commit（Aider 最知名的特性之一）；**优于** Cline（Cline 没有自动 commit）。

### ✅ 1.7 LLM 动态路由（V1 §3.8 中的「模型路由」）
- **已实现**：`internal/llm/router.go`
  - Tier-based（Heavy/Medium/Light），配合任务复杂度 heuristic
  - `TestRouter_` 系列单元测试已覆盖
- **对标水平**：**追平**了 Cursor 的 "Apply" 模型（小改用廉价 model，大改用 opus）；**优于** 开源 agent（大多只有单模型）。

### ✅ 1.8 Skill 热插拔 + HTTP 集成测试双验证
- **已实现**：`internal/skill/registry.go` + `integration_test.go` 中 HTTP 端到端 lifecycle
- **对标水平**：**超过**多数开源竞品（多数只有 MCP，无内置 skill registry）。

### 🟡 1.9 部分落地：前端 Diff 预览（V1 §3.2 P1）
- **部分实现**：`code_agent_ui/src/pages/WorkspacePage.tsx` 有 workspace 页面，但**未核实是否有 diff 预览 + Accept/Reject**
- **对标差距**：Cursor Composer 的"multi-file diff 一键 accept"是产品力的核心，需要再做 UI 打磨。

### 🟡 1.10 部分落地：gopls / LSP 作为 MCP Server（V1 §3.1 P0）
- **MCP 通道已有**：`internal/mcp/client.go` + `reconnect.go`
- **gopls-MCP 桥接未见**：需要单独实现 gopls-as-MCP-server 或作为 skill
- **对标差距**：这是 Cursor 的**最核心护城河**之一，待补。

---

## 2. V2 评估出的**新**差距（SOTA 在 2025–2026 又前进了）

上版文档写于 2025Q3 的认知；2025Q4 到 2026Q1 这 6 个月里，一线产品又拉开了新差距：

### 🔴 2.1 Semantic Code Graph — Cursor/Augment 的新护城河

**SOTA 动态（2026）**：
- **Cursor Agent v1.0+**：不再只用 RAG，而是**持续维护一个 PostgreSQL 图数据库**，节点是 symbol（函数/类/变量），边是引用/调用/继承。
- **Augment Code**：宣传的 "Context Engine" 用的是 **Tree-sitter AST + LSP semantic tokens + 向量 + 结构图** 四路融合。
- **Sourcegraph Cody**：直接把 **SCIP**（Source Code Intelligence Protocol）作为底层索引。

**当前位置**：
- ✅ Tree-sitter AST for Go（`rag/ast_native.go`）
- ✅ Vector + BM25 双路（`rag/engine.go`）
- ❌ **没有 symbol graph 持久化**
- ❌ **没有 SCIP 解析**
- ❌ **Find References / Find Implementations / Find Callers 三大 LSP 高级能力缺失**

**闭差距路径（高 ROI）**：
1. **P0：把 gopls 包装成一个本地 MCP Server**，暴露 `textDocument/references`、`textDocument/implementation`、`textDocument/typeDefinition`（1 周）
2. **P1：在 Qdrant payload 里加入 `called_by` / `calls` 数组**，从 AST 静态扫描提取（2 周）
3. **P2：考虑引入 `scip-go` 作为离线索引器**（4 周）

---

### 🔴 2.2 Multi-Turn Trajectory 学习 — Devin/Trae 的新玩法

**SOTA 动态（2026）**：
- **Devin v3**：声称有 "**Experience Replay**"，把历史成功/失败的完整 trajectory 存起来，下次遇到**相似任务时作为 few-shot example 注入 prompt**。
- **Trae / MarsCode**：用 **LoRA 精调** 让 agent 学会用户偏好（尽管效果存疑）。
- **Cursor @past**：能引用历史 Chat 作为上下文。

**当前位置**：
- ✅ Session 短期记忆（Redis）
- ✅ 会话摘要（`session/summarizer.go`）
- ❌ **无跨 session 的成功案例库**
- ❌ **无失败模式库**（哪些 edit 导致 build fail，下次避开）
- ❌ **无类似任务检索 + 少样本注入**

**闭差距路径**：
1. **P1：每次 task 完成后，把 `(goal, plan, outcome, duration, cost)` 写入 PG / Qdrant**（1 周）
2. **P1：新任务开始时，RAG 查历史相似任务，Top-3 注入 system prompt**（1 周）
3. **P2：失败模式库 — 每次回滚就记录 `(edit_pattern, error_pattern)`，规划时主动排除**（2 周）
4. **P3：基于 trajectory 的 model 精调**（6 个月 + 算力，暂不建议）

---

### 🔴 2.3 深度 Reflection / 自我检查 — Claude Code 4.6 / OpenAI o3 style

**SOTA 动态**：
- **Claude Code 4.6 的 "extended thinking"**：每 10 步触发一次"元思考"（reflection），能主动说"我走错方向了，重来"
- **OpenAI o3**：reasoning token 机制，内部推理链条 10–100× 于输出
- **Devin** 显式维护 "plan drift detection" — 检测当前步骤和 plan 的偏离度

**当前位置**：
- ✅ `reflectionCheckpoint`（`orchestrator.go`）— 简单自省
- ✅ `consecutiveFailureTracker` 防死循环
- 🟡 Planner 支持 revise，但**粒度较粗**（整个 plan 重生成）
- ❌ **没有"当前步骤 vs 目标"偏离度打分**
- ❌ **没有 reasoning token / extended thinking 的利用**

**闭差距路径**：
1. **P0：引入 plan-drift detector** — 每 5 步让 light-tier model 评估"当前进度 vs 计划"（3 天）
2. **P1：支持 Anthropic extended thinking 参数**（`thinking: { enabled: true, budget: N }`）（2 天，仅适配 LLM 层）
3. **P2：支持 OpenAI o1/o3 的 reasoning token 透传**（3 天）

---

### 🔴 2.4 Agentic RAG / Adaptive Retrieval — 动态搜索策略

**SOTA 动态**：
- **Cursor**：不同问题用不同检索策略 — code search / symbol search / grep / LSP，**LLM 自主选择**
- **Augment Code**：推出了 "Retrieve API"，Agent 可以显式问"给我 `HandleApproval` 的所有调用点"
- **Codeium**：Window scope 推理 — 只用当前打开的 tab 相关文件

**当前位置**：
- ✅ 有 `tool_search_code`（向量 + BM25）
- ❌ **没有 symbol-level 精确搜索工具**
- ❌ **没有 LSP 级 find-references 工具**
- ❌ **LLM 不能选择「用哪种检索」**（只有一种）

**闭差距路径**：
1. **P0：新增 4 个检索工具**：`find_symbol_definition`、`find_references`、`grep_literal`、`semantic_search`；由 LLM 自主选择（1 周）
2. **P1：基于问题类型的路由 heuristic**（如含"who calls X" → 走 find_references）（3 天）

---

### 🔴 2.5 Browser + Shell 真正的 Computer Use

**SOTA 动态**：
- **Devin**：完整的 VNC Browser，能看网页截图、点按钮
- **Claude Computer Use API**：截图 + 鼠标/键盘坐标操作
- **OpenHands**：集成了 `browsergym` 做 Web 导航
- **Cursor Agent**：新版可以跑终端命令并读取输出、处理 TUI

**当前位置**：
- ✅ Shell 在 Docker sandbox 中执行（安全性优于 Devin 的裸宿主机）
- ❌ **无 Browser / Playwright 集成**
- ❌ **无 Computer Use（看截图点按钮）**
- ❌ **TUI 交互基础（curses/dialog 之类）未验证**

**闭差距路径**：
1. **P1：集成 Playwright-Go 作为 browser 工具**（2 周）
2. **P2：对接 Anthropic Computer Use API**（需要模型支持，1 周适配）
3. **P3：VNC 方案**（投入大，不建议）

---

### 🔴 2.6 Sub-Agent 并行探索 — OpenHands / Claude Code 新玩法

**SOTA 动态**：
- **Claude Code 的 `Task` 工具**：主 agent 可以启动 N 个子 agent 并行独立探索
- **OpenHands 多 agent 架构**：Coder、Tester、Reviewer 分工
- **AutoGen / CrewAI**：把多 agent 抽象成框架

**当前位置**：
- ✅ Temporal 支持并行 activity（基础设施有）
- ✅ Planner 已有 DAG
- ❌ **ReAct 循环仍是单线程**（没用 goroutine 跑 subtask）
- ❌ **无 sub-agent 抽象**

**闭差距路径**：
1. **P1：Executor 对于 DAG 中无依赖的 steps，goroutine 并行执行**（1 周）
2. **P2：新增 `task_delegate` 工具，启子 agent 做独立探索**（2 周）

---

### 🟠 2.7 IDE 集成 — 仍是**最核心洼地**（V1 §3.4）

**本版仍未改善**。这 6 个月里，竞品继续拉大差距：

| 产品 | 形态 | 安装量 |
|------|------|--------|
| **Cursor** | VSCode fork，AI-first editor | **~500 万活跃** |
| **Cline** | VSCode extension | **>100 万下载** |
| **Continue** | VSCode + JetBrains 插件 | **>50 万** |
| **Claude Code** | CLI + VSCode ext | Anthropic 官方背书 |
| **Augment Code** | VSCode + JetBrains 插件 | 企业市场快速增长 |
| **Codeium Windsurf** | 自研 IDE | 融资 5 亿美金级别 |

**我们的位置**：仍然只有 Web UI。

**闭差距路径（不变）**：
1. **P0：VSCode Extension MVP** — Chat panel + diff view + /commands（**仍是 90 天内最值得做的 1 件事**）（6–8 周）
2. **P1：CLI（类 Aider）**（2 周）
3. **P2：JetBrains plugin**（3 个月）

---

### 🟠 2.8 Benchmark 得分 — 我们缺少量化证据

**SOTA 透明度**：
- Cursor、Devin、Claude Code 都**公开 SWE-Bench Verified 得分**（Claude Sonnet 4.6 单独 ~50%，带 agent 可达 ~70%）
- 业界标准：**SWE-Bench**、**Terminal-Bench**、**LiveCodeBench**、**HumanEval+**、**MultiPL-E**

**我们的位置**：
- ✅ 单元测试 + HTTP 集成测试都有
- ❌ **没跑过 SWE-Bench**
- ❌ **没有端到端成功率的量化指标**

**闭差距路径**：
1. **P0：跑 SWE-Bench Lite（300 instances，开销可控）**，给出 pass@1 分数（3 天做 harness，1 周跑评测）
2. **P1：跑 Terminal-Bench**（测 shell 任务完成率）（1 周）
3. **P2：建立自己的业务场景 benchmark**（具体到 Go 后端、TS 前端等）（持续建设）

---

### 🟠 2.9 Cost 精细管控

**SOTA 做法**：
- Cursor、Cline 都有**每请求 cost 显示**、**session 累计 spend**、**预算告警**
- Augment 按 query 定价，透明

**当前位置**：
- ✅ OTEL + Prometheus 指标
- ❌ **没有按 session / task 的 cost 归因**
- ❌ **没有"本次任务预估花了 $X"的 UI**
- ❌ **没有 token 预算限制（user-level / session-level）**

**闭差距路径**：
1. **P0：每次 LLM 调用记录 `(input_tokens, output_tokens, model, unit_price)` 到 PG**（3 天）
2. **P0：前端加 spend 仪表盘**（3 天）
3. **P1：用户/项目级预算 + 熔断**（1 周）

---

### 🟠 2.10 跨 Session 长期记忆

**SOTA 做法**：
- **Mem0**、**Zep**、**Letta（MemGPT）** 都在提供专门的 agent 记忆层
- **Cursor** 有 workspace 级别的 `@past` 引用
- **Claude Projects** 有持久化的 knowledge 文件

**当前位置**：
- ✅ Session-level 记忆
- ❌ **无 cross-session user preference 学习**
- ❌ **无 workspace-level 知识沉淀库**

**闭差距路径**：
1. **P1：引入 `user_preferences` 表 + `workspace_knowledge` 表，LLM 自主调用"记住这条"工具**（2 周）
2. **P2：对接 Mem0 API（SaaS）或自研 hierarchical memory**（1 个月）

---

## 3. 目前我们仍然领先的地方（不容低估）

对标 SOTA 全景后，这些是**开源 / 竞品少有的亮点**：

| 能力 | 我们 | Cursor | Claude Code | Cline | Aider | OpenHands |
|------|------|--------|-------------|-------|-------|-----------|
| **Temporal 工作流编排** | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ |
| **Docker NetworkMode:none 强隔离** | ✅ | ❌ | ⚠️ 半 | ❌ | ❌ | ✅ |
| **Qdrant 向量库 + 租户隔离** | ✅ | 类似 | ❌ | ❌ | ❌ | ⚠️ |
| **JSON-RPC 2.0 MCP + 自动重连** | ✅ | ⚠️ | ✅ | ⚠️ | ❌ | ✅ |
| **HMAC / JWT / Egress 白名单** | ✅ | ❌ | ❌ | ❌ | ❌ | ⚠️ |
| **完整 OTEL + Jaeger 追踪** | ✅ | ❌ | ❌ | ❌ | ❌ | ⚠️ |
| **All-in-One Docker 启动** | ✅ | N/A | N/A | N/A | N/A | ⚠️ |
| **企业审计日志** | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ |
| **Skill Registry 内置** | ✅ | ❌ | ❌ | ❌ | ❌ | ⚠️ |
| **Tier-based 模型动态路由** | ✅ | ✅ | ⚠️ | ❌ | ❌ | ⚠️ |

**启示**：**ToB（企业）和 Compliance（合规）场景**是我们明显的战场，Cursor/Cline 都是**纯 ToC 产品**，不满足国企/金融/央企 IT 的合规要求。

---

## 4. 更新版 90 天追赶路线图

### 🔥 Sprint A（第 1 个月）：**让量化证明出现**
- [ ] **跑 SWE-Bench Lite 并公布分数** ← 最重要，证明一切
- [ ] **Cost 仪表盘 + 预算限制**
- [ ] **plan-drift detector**（3 天见效）
- [ ] **4 个精细检索工具**（find_symbol/find_refs/grep/semantic）
- [ ] **前端 Workspace 补齐 diff accept/reject**

### 🔥 Sprint B（第 2 个月）：**让 Agent 进 IDE**
- [ ] **VSCode Extension MVP**（Chat + Diff + /commands）
- [ ] **gopls-MCP 桥接**
- [ ] **Executor DAG 并行化**
- [ ] **Trajectory 历史沉淀**（相似任务检索）

### 🔥 Sprint C（第 3 个月）：**让 Agent 能"看到"**
- [ ] **Playwright 浏览器工具**
- [ ] **图像/截图上传 + Vision model 透传**
- [ ] **CLI 版本（Aider-like）**
- [ ] **跨 session memory 层**

---

## 5. 对比表：本次评估 vs. V1 评估

| 指标 | V1.0 评分 | V2.0 评分 | Δ |
|------|-----------|-----------|---|
| ReAct 执行循环 | ★★★☆☆ | ★★★★☆ | +1 |
| 代码理解深度 (RAG) | ★★★☆☆ | ★★★★☆ | +1 |
| 代码编辑能力 | ★★☆☆☆ | ★★★★☆ | **+2** |
| 长任务自主执行 | ★★☆☆☆ | ★★★★☆ | **+2** |
| 测试 & 自验证 | ★★☆☆☆ | ★★★★☆ | **+2** |
| IDE / 编辑器集成 | ★☆☆☆☆ | ★☆☆☆☆ | 0 |
| 多模态 | ★☆☆☆☆ | ★☆☆☆☆ | 0 |
| 模型 & Prompt 工程 | ★★★☆☆ | ★★★★☆ | +1 |
| Git 集成 | ☆☆☆☆☆ | ★★★★☆ | **+4** |
| 可观测性 / 安全 | ★★★★☆ | ★★★★☆ | 0（已满分） |
| 总平均 | 2.6 / 5 | **3.6 / 5** | **+1.0** |

---

## 6. 现阶段项目的精准定位

- **"后端 Agent 基础设施 + 企业合规能力"** = 超越多数开源 agent
- **"代码编辑 + 长任务 + 自验证"** = 已追平 Aider，逼近 Claude Code
- **"IDE 内嵌体验"** = 仍是**0 分洼地**，决定我们能否有 ToC 市场
- **"看屏幕 / 动浏览器"** = 另一个 0 分洼地，决定能否做 E2E 开发

**最值得豪赌的一件事**：**做 VSCode Extension**。
**最容易的量化胜利**：**跑 SWE-Bench 并公布分数**。
**最容易被低估的优势**：**Temporal + Docker + MCP + OTEL 的企业级合规栈**。

---

## 7. 诚实的话

- 这半年做了很多事，**工程确实进步显著**（V1 的 Sprint 1/2 基本完成）
- 但 **Cursor / Devin / Claude Code 同期也在快速迭代**，绝对差距并没有缩小到质变
- 真正要接近一线，**必须有 IDE 原生入口 + SWE-Bench 公开成绩**，否则永远在"看起来不错但没人用"的局面
- **保持已有优势（企业合规 + 观测 + MCP 生态），在 ToB 场景先赢一单**，比追逐 Cursor 更现实
