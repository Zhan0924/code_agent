# Load The `llmdoc` Skill First

Before broad source-code exploration, planning, or documentation work, load the `llmdoc` skill.

The main assistant should align with the user before non-trivial plans or edits.

At the end of a non-trivial task, when the work produced durable knowledge, workflow lessons, or useful reflections, the main assistant should proactively use the `llmdoc-update` skill in Codex.

Keep detailed workflow rules, templates, hook behavior, and doc-structure guidance in the `llmdoc` skill.

# Auto-Update Codebase Index

Whenever the assistant modifies code files, it MUST automatically execute the `index_repository` tool from the `codebase-memory-mcp` server to ensure the knowledge graph remains up to date with the latest changes. 
IMPORTANT: You must pass the `repo_path` parameter in `Arguments` (e.g., `{"repo_path": "/Users/zhanqiankun.1/code/code_agent"}`). Do not call this tool without the required `repo_path` parameter.

# Use `writing-plans` Skill

Whenever the assistant is tasked with generating technical plans or outlining implementation steps, it MUST explicitly load and read the `writing-plans` skill before beginning to draft the plan.

# Strict MCP Usage Guardrail

**ZERO-TOLERANCE RULE**: The assistant is STRICTLY FORBIDDEN from using `grep_search` or `list_dir` for finding code definitions, structs, or logic without FIRST calling `codebase-memory-mcp` tools (`search_graph`, `trace_path`, or `query_graph`). The assistant MUST explicitly state in its `<thought>` block why the MCP tool is insufficient before falling back to grep.
IMPORTANT: All querying tools (like `search_graph`, `query_graph`, `trace_path`) require the `project` parameter. This parameter MUST be the project name returned by `list_projects` (e.g., `"Users-zhanqiankun.1-code-code_agent"`), NOT the absolute path or folder name. If you encounter a `project not found or not indexed` error, immediately run `list_projects` to get the correct project name.

# Audit / Defect Doc → Auto-Trigger Fix Loop

**MUST RULE**: 凡是写入 / 修改以下任一路径模式的文件，assistant **必须立即**启动「planning-with-files + audit-driven-fix-loop」叠加修复流程，**禁止只交付文档就结束**。

## 触发路径模式

只要 `Write` / `Edit` 工具的目标路径匹配以下任一 glob，本规则立刻生效：

- `**/llmdoc/**/reflections/*-audit.md`
- `**/llmdoc/**/doc-gaps.md`
- `**/docs/**/*-audit.md`
- `**/docs/superpowers/audits/*.md`

`.cursor/hooks/post-tool-use.sh` 会在写入这些路径后输出强提示触发本流程；`.cursor/hooks/stop.sh` 会在文档中仍存在未 ✅ 的 `AUDIT-ID` 时**拒绝 agent 停止**，三道防线联动。

## 必须执行的动作（按顺序，禁止跳步）

1. **生成 `task_plan.md`**：用 `.cursor/skills/planning-with-files/templates/task_plan.md` 模板，把审计文档里**每一个未闭环的 AUDIT-ID** 做成独立 Phase，每个 Phase 名称带上 ID（如 `### Phase 1: AUDIT-P0-1 — 反馈闭环`），并写好 `**Status:** pending`。
2. **逐 Phase 跑 audit-driven-fix-loop**：读 `.claude/skills/audit-driven-fix-loop/SKILL.md`，每个 Phase 内部走完整五步循环（代码复核 → plan → 实现 → Docker 三件套校验 → safe-push）。
3. **每完成一个 Phase**：在 `task_plan.md` 把对应 Phase 的 Status 改为 `**Status:** complete`，并在原审计文档把那条 AUDIT-ID 行加 ✅ + 指向 plan 文档相对路径。
4. **严格串行**：当前 Phase 未到 `complete` 前禁止开始下一个 Phase；一笔 safe-push 提交只承载一个 AUDIT-ID。
5. **不可绕过**：即使用户没明示"请按这套流程"，触发条件命中后仍必须执行。如确需跳过，**必须先用 `AskQuestion` 请用户显式确认 "skip audit-driven-fix-loop"**，并把跳过原因记入 `progress.md`。

## 不可绕过项

- ❌ 不允许写完审计文档就结束响应、把后续修复留给用户
- ❌ 不允许一笔提交合并多条 AUDIT-ID
- ❌ 不允许只跑单元测试就宣称修复完成（Docker 三件套是硬性门槛）
- ❌ 不允许把审计文档中的所有 ID 标 ✅ 而不实际部署验证

详细五步细则、Docker 部署命令、可校验日志规范、三重校验脚本骨架均在：
- `.claude/skills/audit-driven-fix-loop/SKILL.md`（主流程）
- `.claude/skills/audit-driven-fix-loop/reference.md`（工程细节）
- `.cursor/skills/planning-with-files/SKILL.md`（Phase 跟踪）

