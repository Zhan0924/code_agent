---
name: audit-driven-fix-loop
description: >-
  把"审计/问题分析文档 → 逐条修复 → Docker 部署 → API+日志+数据库三重校验 → safe-push 提交"
  固化为可重复的端到端动态 workflow。强制串行：当前问题未完成五步前禁止开启下一项；
  不轻信文档结论，必先用实际代码复核问题是否真实存在；无论真伪都必须 Docker 部署并以
  日志/API/DB 证据收敛。Use when the user 要求修复审计文档、闭环 AUDIT-Pn 问题清单、
  落地缺陷修复 plan、按优先级处理 doc-gaps / memory-system-audit / reflections 中的
  未修复项，or any "针对问题分析文档逐条优化" 的工作。
---

# Audit-Driven Fix Loop

把一份"按 ID 列出问题的审计/缺陷/优化文档"按工程化标准逐条闭环：每条问题独立跑完 **复核 → 方案 → 实现 → 部署校验 → 提交** 五步后，才允许进入下一条。

## 适用场景

- 输入：一份按 ID 罗列问题的清单（典型：`llmdoc/**/reflections/*-audit.md`、`llmdoc/**/doc-gaps.md`、`docs/architecture/*-audit.md`、AUDIT-Pn 清单等）
- 目标：按优先级把每一条**单独**做成一笔可回滚的修复提交
- 反场景：一次 PR 改 5 条问题、只靠跑单元测试就宣称修复完成、只引用文档结论而不读代码

## 自动触发条件（由 AGENTS.md + hook 强制）

本 skill **不需要用户显式调用**，以下任一信号到达即自动生效：

1. `Write` / `Edit` 工具写入路径匹配：
   - `**/llmdoc/**/reflections/*-audit.md`
   - `**/llmdoc/**/doc-gaps.md`
   - `**/docs/**/*-audit.md`
   - `**/docs/superpowers/audits/*.md`
2. `.cursor/hooks/post-tool-use.sh` 检测到 1 后输出 `[audit-doc-trigger]` 块（含未闭环 AUDIT-ID 列表 + 必须动作）。
3. `.cursor/hooks/stop.sh` 在任何 stop 尝试时扫描上述路径，发现未 ✅ 的 AUDIT-ID 即返回 `followup_message` 拒绝停止（受 `hooks.json::loop_limit=3` 兜底）。

合规动作（与 AGENTS.md::"Audit / Defect Doc → Auto-Trigger Fix Loop" 文案一致）：

1. 用 `.cursor/skills/planning-with-files/templates/task_plan.md` 复制出根目录 `task_plan.md`
2. 文档里每条未闭环 `AUDIT-ID` 各占一个 Phase：`### Phase N: <AUDIT-ID> — <一句话>`，`**Status:** pending`
3. 从 Phase 1 开始跑下面的"五步循环"，完成后把 Phase 标 `**Status:** complete` + 原审计文档对应行加 ✅
4. 严禁跳步、严禁一笔提交承载多条 AUDIT-ID
5. 如确需跳过：调用 `AskQuestion` 请用户显式确认 "skip audit-driven-fix-loop"，并把原因写进 `progress.md`

---

## 五条强制约束（违反任意一条都要立即停下）

1. **严格串行**：当前问题未完成五步循环前，不开启下一项。一条 PR / 一笔提交只承载一条 AUDIT-ID。
2. **代码优先于文档**：每条问题先用实际代码（`Read` / `Grep` / `codebase-memory-mcp` 的 `search_graph`、`trace_path`）复核，禁止只引用文档段落就下结论。
3. **必须 Docker 部署**：无论 Step 1 判断"真存在"还是"不存在/已修复"，都要走完 Step 4 的容器部署 + 三重校验，结论由运行时证据收敛。
4. **日志可校验**：Step 3 实现修改时，必须同时输出可被 Step 4 grep / 解析的结构化日志（见 `reference.md::"可校验日志规范"`）。代码生成时就要为 Step 4 留好观测点。
5. **safe-push 单点提交**：每条问题用 `safe-push` skill 提交一笔，commit message 必须包含 AUDIT-ID。

---

## 五步循环（每条 AUDIT-ID 都跑一遍）

复制下面这张进度卡到对话里，每条问题独立维护一份：

```
当前问题：<AUDIT-ID>  来源：<file:line of audit doc>
- [ ] Step 1  代码复核：问题真伪 + 证据 file:line
- [ ] Step 2  Plan 模式生成方案：可观测点/回滚策略已写明
- [ ] Step 3  实现修改：附带结构化日志
- [ ] Step 4  Docker 部署 + API / 日志 / DB 三重校验全绿
- [ ] Step 5  safe-push 提交 + 审计文档勾 ✅
```

### Step 1 · 代码复核

只允许通过下列证据下结论：

- 直接 `Read` 审计文档点名的 file:line
- `codebase-memory-mcp` 的 `search_graph` / `trace_path` / `query_graph`（必须先调，参见仓库 `AGENTS.md` 的"Strict MCP Usage Guardrail"）
- 必要时用 `Grep` 补充验证

输出格式：

```
判定：[真实存在 | 已修复 | 表述不准]
证据：
  - <repo-relative-path>:<line> — <一句话说明实际代码状态>
  - ...
结论：进入 Step 2 / 跳到 Step 4（用 Docker 反向验证现状）
```

### Step 2 · Plan 模式生成技术方案

调用 `SwitchMode` 切到 `plan` 模式（如果用户拒绝则继续在 agent 模式但仍按本步产出方案文档）。

方案保存到 `docs/superpowers/plans/<YYYY-MM-DD>-<AUDIT-ID>-<slug>.md`，使用 `templates/plan-template.md` 模板。

**方案必须包含且不可省略**：

- 受影响 file 与接口/数据流变更（精确到函数级）
- **可观测性增强清单**：本次修复需要新增/增强哪些 log line、metric label、DB 字段，使 Step 4 的 grep / curl / psql 能拿到证据
- 回滚策略（feature flag / config 默认值 / db migration 是否可逆）
- 不在本次范围内（unscoped）的相邻问题（防止 Step 3 拖泥带水）

方案没写到"可观测性增强清单" → 不允许进入 Step 3。

### Step 3 · 实现修改（含可校验日志）

边写代码边补观测，严格遵守 `reference.md::"可校验日志规范"`。最低要求：

- 关键路径输出包含 `audit_id=<AUDIT-ID>` 的结构化字段（zap / slog / logrus 任一）
- 写库前后输出 `before / after` 关键字段值
- 多租户路径必带 `user_id`、`project_id`
- 失败分支输出 severity 与下游影响

任何 Step 4 的 API 响应 / 日志 / DB 行数变化无法证明的状态，都必须由本步骤补 log 把它暴露出来。

### Step 4 · Docker 部署 + 三重校验

详细命令见 `reference.md::"Docker 部署流程"`。骨架：

1. **镜像存在性检查**：`podman images` / `docker images` 列出依赖镜像；缺失则按 `reference.md::"镜像处理"` 拉取（项目用 `docker.m.daocloud.io/*` 镜像源）
2. **依赖容器复用**：pgvector / qdrant / temporal 已在跑就**不重启**，只 `podman start <name>` 兜底
3. **重 build agent**：`podman build -t code-agent:latest .`
4. **重启 agent**：`podman rm -f code-agent && podman run -d --name code-agent ...`（参考 `docker-compose.yml`）
5. **健康检查**：`curl http://localhost:18080/healthz` 等到 200
6. **跑校验脚本** `scripts/verify-<AUDIT-ID>.sh`（基于 `templates/verify-template.sh` 改写），必须同时覆盖三件套：
   - **API 校验**：curl 相关 endpoint，断言 status code + body 关键字段
   - **日志校验**：`podman logs code-agent --since 5m | grep "audit_id=<AUDIT-ID>"` 断言关键 log line 出现 / 不出现
   - **数据库校验**：`psql $PG_DSN -c "SELECT ..."` 断言行数 / 字段变更 / 索引使用

**任一断言失败 → 回到 Step 3 修代码 → 重新部署 → 重跑脚本**，直到三件套全绿。

如果 Step 1 判定"已修复 / 不存在"，Step 4 用反向断言：跑脚本证明现网行为已是预期。

### Step 5 · 提交并进入下一项

调用 `safe-push` skill（`/safe-push`）：

- commit message 格式：`fix(audit): <AUDIT-ID> <一句话总结>` 或 `refactor(audit): <AUDIT-ID> ...` / `docs(audit): ...`
- **本次提交只能包含当前 AUDIT-ID 相关的修改**（含 plan 文档、verify 脚本、源码、审计文档勾选）
- 同时在审计文档（如 `llmdoc/memory/reflections/*-audit.md`）把对应行 ID 加 ✅ 并指向 plan 文档路径

提交完成后**回到 Step 1 处理下一条 ID**。

---

## 例外处理

- **镜像拉不下来**：切到 `docker.m.daocloud.io/*` 镜像源；如果仍失败，记录失败日志后向用户报告，使用 `AskQuestion` 让用户选择"换源 / 跳过 / 终止"
- **依赖容器异常**：不重建（重建会丢数据），先 `podman logs <name>` 看根因；只在必要时 `podman restart <name>`
- **Step 4 连续 3 次校验失败**：暂停循环，向用户报告：当前 AUDIT-ID + 失败的具体断言 + 候选修复路径，调用 `AskQuestion` 让用户决策
- **审计文档结论与实际代码矛盾**：以代码为准，在 Step 1 的"判定"里写 `表述不准`，并在 Step 5 同步修订审计文档原文（注明修订原因）

---

## 配套文件

- [reference.md](reference.md) — Docker/Podman 部署流程、镜像处理、可校验日志规范、三重校验细则、代码生成时的可观测性硬性要求、**code_agent 项目预设变量表**
- [templates/plan-template.md](templates/plan-template.md) — Step 2 plan 文档模板
- [templates/verify-template.sh](templates/verify-template.sh) — Step 4 校验脚本骨架（含 API / 日志 / DB 三块占位）
- [examples.md](examples.md) — 用 AUDIT-P2-5（memory explain 接口）走一遍完整五步循环

---

## 启动检查表（开始一份新审计文档前过一遍）

- [ ] 审计文档路径已读取并枚举出全部待修复 ID 与优先级
- [ ] `reference.md::"项目预设变量表"` 已对照当前项目填好（容器名 / 端口 / DSN / 镜像源）
- [ ] 依赖容器已 `podman ps` 检查；缺失或停止已按 `reference.md::"依赖容器复用规则"` 处理
- [ ] `safe-push` skill 可用（`ls ~/.cursor/skills/safe-push/SKILL.md`）
- [ ] 已就"按什么顺序处理 ID"与用户对齐（默认按文档自身的 P0 → P1 → P2 顺序）

全部勾完 → 开始第 1 个 AUDIT-ID 的 Step 1。
