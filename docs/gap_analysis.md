# 与 SOTA Code Agent 的差距分析 (V1.0)

> 诚实的自我评估：对比 Cursor / Devin / Claude Code / Cline / Aider / GitHub Copilot Workspace / Codeium Windsurf / OpenHands 等一线产品。

本文档按「能力维度」逐项评分（★1–5★），明确**已实现 / 部分实现 / 缺失**，并给出**闭差距的优先级路径**。

---

## 1. 总体评分一览

| 维度 | 当前水平 | SOTA 水平 | 差距 |
|---|---|---|---|
| 基础设施 / 架构 | ★★★★☆ | ★★★★★ | 小 |
| ReAct 执行循环 | ★★★☆☆ | ★★★★★ | 中 |
| 代码理解深度 (RAG) | ★★★☆☆ | ★★★★★ | **大** |
| 代码编辑能力 | ★★☆☆☆ | ★★★★★ | **大** |
| 长任务自主执行 | ★★☆☆☆ | ★★★★★ | **大** |
| 测试 & 自验证 | ★★☆☆☆ | ★★★★★ | **大** |
| IDE / 编辑器集成 | ★☆☆☆☆ | ★★★★★ | **巨大** |
| 多模态（截图/UI）| ★☆☆☆☆ | ★★★★☆ | 大 |
| 模型 & Prompt 工程 | ★★★☆☆ | ★★★★★ | 中 |
| 可观测性 / 安全 | ★★★★☆ | ★★★★☆ | 无差距 |

**一句话定位**：本项目已构筑"**合格的生产级 Agent 基础设施**"，但在"**让 Agent 真正会写代码**"这件事上，与 Cursor/Devin 仍有 1–2 个代差。

---

## 2. 我们做得不错的地方（不容低估的优势）

这些是项目相对于"纯套壳的 Cline/Aider fork"的真实优势：

### ✅ 2.1 生产级基础设施（架构胜出）
- **Temporal 工作流**：HITL 挂起/恢复对长任务天然友好，Cursor/Cline 这类 IDE 产品其实没有
- **Qdrant + HNSW 向量库**：Payload 过滤 + 多租户隔离，Cline/Aider 只是做 grep
- **Docker 沙箱 + NetworkMode:none + cgroups**：比 Devin 的宿主机直跑更安全
- **MCP 网关完整实现 JSON-RPC 2.0 + SSE 重连**：与 Claude Desktop 同构
- **HMAC / JWT / 网络白名单 / 审计日志**：企业侧准入无压力
- **全栈 OTEL + Jaeger 追踪**：排障能力超过大多数开源 agent

### ✅ 2.2 工程纪律
- 50+ 个模块清晰分层（`api / orchestrator / rag / sandbox / llm / mcp / store`）
- Go 并发模型天生优于 Node/Python 的 agent（SSE 背压、goroutine 隔离）
- 单元测试覆盖核心模块（config、pruner、skill、rag/ast、sandbox、jwt、hmac）

### ✅ 2.3 All-in-One 镜像
- 单容器启动 Redis / PG / Qdrant / Temporal / Jaeger 全家桶，DX 远胜需要 docker-compose 十个服务才能跑的竞品

**结论**：如果只从「后端 Agent Runtime」角度看，本项目已经达到了 OpenHands / SWE-agent 的水平。

---

## 3. 与 SOTA 的核心差距（按重要性排序）

### 🔴 3.1【最大差距】代码理解：没有真正的「仓库级语义图谱」

**当前水平**：
- `tree-sitter` 只解析了 Go（见 `rag/ast_native.go`），Markdown 有单独 parser
- 切块粒度是函数级，存入 Qdrant 做向量 + BM25 双路召回
- **本质上是"更聪明的 grep"**

**SOTA 水平（Cursor / Augment / Codeium）**：
1. **完整的语言服务器（LSP）集成**：Cursor 会跑 gopls/rust-analyzer/pyright，Agent 可以「跳转定义」「查找引用」「重命名符号」
2. **调用图 (Call Graph) + 类型流图**：知道 `HandleApproval` 被哪 5 处调用，参数是什么类型
3. **代码增量索引**：文件一改就 diff 式增量更新向量库，本项目目前是全量重建
4. **仓库级摘要缓存 (Aider repo map)**：让 LLM 看到一份高层结构树而非大段代码

**具体能举例的差距**：
- 用户问"`Orchestrator.reactLoop` 为什么会死循环？"
  - 我们：做 RAG 检索找到相关代码，塞给 LLM，LLM 基于字面文本猜
  - Cursor：LSP 调用链 + 类型追踪，能精确指出 `failureTracker.track()` 的状态变化导致循环未退出

**闭差距路径**（按投入产出比）：
1. ⭐️ **P0：集成 gopls/pyright 作为 MCP Server**（2 周工作量，立竿见影）
2. **P1：扩展 tree-sitter 支持 Python/TS/Rust/Java**（每种语言 2–3 天）
3. **P2：构建 repo-map 定期快照（文件树 + 每个文件的公开签名），作为 system prompt 基础上下文**（1 周）
4. **P3：文件变更监听器触发增量 re-index**（已有 indexer，补 fsnotify 即可，3 天）

---

### 🔴 3.2【最大差距】代码编辑：缺少精确 diff / 语法感知修改

**当前水平**（`file_tools.go` 中 `toolWriteFile` / `toolPatchFile`）：
- 整文件 overwrite，或 unified diff patch
- 无语法校验、无回滚、无 dry-run
- Agent 偶尔会写出语法错误的 Go 代码且不自知

**SOTA 水平**：
- **Aider**：`udiff` / `search-replace` 块，失败自动重试；编辑后立即 `go build` / `tsc --noEmit`
- **Cursor Composer**：AST-level 编辑，能只改一个函数体而不动签名；支持"预览所有变更"的 multi-file diff UI
- **Claude Code (Anthropic 官方)**：
  - `Edit` 工具要求 `old_string` 唯一匹配，否则拒绝执行，极大降低幻觉
  - `MultiEdit` 事务性批量修改
  - 改完立刻 lint + 重建索引

**具体差距**：
- 当前没有"编辑失败 → 语法报错 → 自愈回滚"的闭环
- 没有"多文件原子变更提交点"
- 前端 `WorkspacePage` 只能看到最终结果，看不到**变更 diff**

**闭差距路径**：
1. ⭐️ **P0：重写 `toolPatchFile`，采用 Claude Code 的"唯一字符串匹配"策略**（3 天）
2. **P0：每次 write 后自动触发 `go vet` / `tsc --noEmit` / `ruff`，失败回滚并反馈给 LLM**（5 天）
3. **P1：前端 Workspace 加 diff 预览 + Accept/Reject 按钮**（1 周）
4. **P2：基于 tree-sitter 的 AST 级编辑（替换函数体、插入 import）**（2 周）

---

### 🔴 3.3【最大差距】自主长任务：缺少规划 / 回溯 / 并行

**当前水平**（`orchestrator.go`）：
- ReAct 线性循环，最多 N 步（`getMaxSteps`）
- 有 `consecutiveFailureTracker` 防死循环
- `reflectionCheckpoint` 做简单自省
- **单线程串行，没有子任务分解**

**SOTA 水平**：
- **Devin**：
  - Plan → Execute → Verify → Reflect 四阶段 FSM
  - 自主创建 scratchpad、拆解成 subtask DAG
  - 任务中断后能从 checkpoint 精确恢复
- **Claude Code**：`Task` 子代理机制，一个主 agent 分派多个子 agent 并行搜索/修改
- **SWE-agent**：专门为 SWE-Bench 设计的"软件工程动作空间"（`open/scroll/find_file/edit/goto`），比通用 `execute_code` 更可控
- **Trae (字节)** / **MarsCode**：有 "Autopilot mode" 做长达数小时的自主开发

**具体差距**：
- 我们没有「任务规划器」—— LLM 直接 ReAct，不画 DAG
- 没有 subtask 并行执行（goroutine 基础是有了但没用于并行 subagent）
- Temporal workflow 虽然接入了，但**还是跑单线程 ReAct**，完全没发挥编排引擎的价值

**闭差距路径**：
1. ⭐️ **P0：引入 Planner-Executor 双 agent 架构**，Planner 先出 JSON plan（含步骤依赖图），Executor 按拓扑顺序执行（1 个月工作量）
2. **P1：Temporal 用起来做真正的「任务持久化」—— 每个 subtask 一个 activity，支持崩溃恢复**（2 周）
3. **P1：加入 SWE-agent 风格的"文件浏览动作集"（open/scroll/find_def）**（2 周）
4. **P2：子 agent 并行探索 + 主 agent 合并决策**（1 个月）

---

### 🔴 3.4【巨大差距】IDE 集成：只有 Web UI

**当前水平**：
- 一个 React Web UI（`code_agent_ui/`），有 Chat、Workspace、Skills、MCP 等页面
- 纯浏览器端，**无法接入用户已有工作流**

**SOTA 水平**：
- **Cursor** = fork 的 VSCode，内嵌 agent
- **Claude Code** = Anthropic 官方 CLI + VSCode extension
- **Cline** = VSCode extension (Marketplace 数十万安装)
- **Aider** = CLI，无缝接入 git workflow

**差距后果**：
- 用户必须离开熟悉的编辑器进入 Web UI，**体验上就输了**
- 无法获取编辑器当前光标位置 / 选中内容 / 打开的标签页等富上下文

**闭差距路径**（投入大、价值大）：
1. ⭐️ **P0：做一个 VSCode Extension**，通过 WebSocket 连本项目 backend（1–2 个月）
2. **P1：CLI 版本（类 Aider）**，作为轻量 entry point（2 周）
3. **P2：JetBrains 插件**（3 个月）

---

### 🟠 3.5 测试与自验证：缺少 TDD 闭环

**当前水平**：
- 有 `toolRunTests`（执行 `go test` / `npm test`），但 **被动调用**
- 没有"先写测试，再改代码"的 TDD 模式
- 没有 benchmark / 回归检测

**SOTA 水平**：
- **Devin** 每次完成功能后自动写 unit test + e2e test，跑通后才 commit
- **Cursor Bug Finder**：专门的 agent 跑 SWE-Bench-style 自动调试
- **AutoCodeRover**：缺陷定位 + 修复 + 验证的完整闭环

**闭差距路径**：
1. ⭐️ **P0：每个 write_file 后自动检测并运行相关测试文件**（当前只有全量 test）（1 周）
2. **P1：加入 TDD 模式：让 LLM 先产出 failing test，再改代码直到 test 通过**（2 周）
3. **P2：集成 coverage 工具（`go cover`/`c8`），把未覆盖区域作为信号反馈给 Agent**（1 周）

---

### 🟠 3.6 代码生成质量：Prompt 工程与模型选择

**当前水平**（`orchestrator.go:buildSystemMessage`）：
- System prompt 约 100 行，中文 + 工具说明
- 默认模型 claude-opus-4.6 / Qwen 系列
- 没有针对语言/框架定制的 prompt

**SOTA 水平**：
- **Cursor** 有**数千行精调**的 system prompt，针对不同文件类型切换
- **Aider** 的 prompt 经过 SWE-Bench 大量实验迭代
- **Cline** 有成熟的 "tools/rules" 系统，用户可以自定义 `.clinerules`

**闭差距路径**：
1. **P1：按语言定制 prompt（Go/Python/TS/Rust 各一份），根据检测到的文件扩展名切换**（1 周）
2. **P1：支持 `.coderules` / `AGENTS.md` 作为项目级规则**（3 天）
3. **P2：引入 prompt 版本化 + A/B 测试框架**（2 周）

---

### 🟠 3.7 多模态：视觉理解缺失

**当前水平**：纯文本
**SOTA**：
- Cursor / Claude Code 都支持截图输入（"看这张 UI mock，照着实现"）
- Devin 能看 browser 自截图做 E2E 调试
- 对前端开发者至关重要

**闭差距路径**：
- **P1：API 层支持 multipart image upload，透传给 LLM provider**（1 周，前提是模型支持 vision）

---

### 🟡 3.8 其他较小差距

| 能力 | 当前 | SOTA | 差距 |
|---|---|---|---|
| Git 集成 | 无 | 自动 commit/branch/PR | 中 |
| Browser/Web 动作 | 无 | Playwright/Devin-browser | 大 |
| Memory / 长期记忆 | 只有 session | 跨 session 知识沉淀（Mem0 / Zep） | 中 |
| 团队协作 | 单用户 | 多人共享 session（Cursor Multiplayer） | 中 |
| Cost / Token 监控 | 基本 OTEL | 精细到每步的成本/Token 看板 | 小 |
| 模型路由 | 单主 + fallback | 按任务复杂度动态路由到不同模型 | 中 |

---

## 4. 90 天追赶路线图（Quick Wins 优先）

### Sprint 1（第 1 月）：让 Agent "真的会写代码"
- [ ] Claude Code 风格的**唯一匹配 Edit 工具**
- [ ] **写后自动 lint / build / test**，失败自动回滚反馈
- [ ] 前端 Workspace **diff 预览 + Accept/Reject**
- [ ] 集成 **gopls 作为 MCP 工具**，提供 goto_def / find_refs

### Sprint 2（第 2 月）：让 Agent "能做长任务"
- [ ] **Planner-Executor 双 agent 架构**
- [ ] Temporal activity 级的 **checkpoint / 恢复**
- [ ] **`.coderules` + `AGENTS.md`** 项目规则支持
- [ ] **增量 RAG re-index**（fsnotify）

### Sprint 3（第 3 月）：让 Agent "进到用户工作流"
- [ ] **VSCode Extension MVP**（Chat 面板 + 内嵌 diff）
- [ ] **CLI 版本**（aider 风格）
- [ ] **Image/screenshot 支持**
- [ ] **Tree-sitter 支持 Python / TS**

---

## 5. 我们**不应该追求**的方向（反面清单）

诚实地说，这些是不该浪费时间的"看起来很美但 ROI 低"的方向：

1. ❌ **自研 LLM**：算力不够，不如专心做 agent framework
2. ❌ **做 SWE-Bench 刷榜**：短期没有资源也没有意义
3. ❌ **搞 AutoML / AgenticRL**：技术不成熟，过早优化
4. ❌ **对标 Cursor 做 IDE fork**：工程量巨大，不如做 VSCode extension
5. ❌ **追求支持 50 种语言**：前 5 种做到位已经足够

---

## 6. 终极结论

- **我们的项目 = 一套优秀的 Agent 后端基础设施**（Temporal/Qdrant/Docker sandbox/MCP/OTEL）
- **SOTA 产品的护城河 = 代码理解 + 编辑体验 + IDE 集成**（我们三项都弱）
- **最值得投入的是 Sprint 1 的 4 个任务**，做完差距可以从 "1.5 代差" 缩小到 "0.5 代差"
- **90 天可以做到的目标**：对标 **Cline / Aider 的完整度**，未尝不可
- **12 个月可以做到的目标**：国产开源 **OpenHands 水准**，社区可接受
- **想追上 Cursor/Devin**：需要产品方向转型（IDE 集成）+ 10 倍工程投入，短期不现实

**真正的优势**应保持：**生产级可观测性、企业级安全、HITL 可控性、MCP 生态兼容性** —— 这是 ToB 场景下我们能赢过 Cursor 的战场。
