# 反思：新增子系统未记录到 llmdoc

日期：2026-05-21

## 触发事件

最近 5 次提交引入了多个重要新子系统/增强，但 llmdoc 文档未同步更新。

## 新增能力

1. **`internal/multiagent/`** — 完整的多Agent协作包（Supervisor + SubAgent + MessageBus + AgentPool + ConflictResolver + RoleSelector），project-overview.md 完全未记录
2. **`internal/toollearn/`** — 知识蒸馏（Distiller 从成功会话提取策略，注入后续会话推荐），未记录
3. **`internal/orchestrator/metacognition.go`** — 元认知自适应反思（Confidence / StuckScore / Pivot 策略），未记录
4. **`internal/planner/evaluator.go`** — Plan 质量评估器（4 维度评分 + 自动改进），planner 包文件数从 3 增长到 8，overview 表格过期
5. **RAG 依赖分析增强** — 跨文件依赖追踪，infrastructure-subsystems.md 未更新
6. **CI 重构** — ci.yml 删除，pr-review.yml 新增

## 教训

- 多次 feat 提交之间应触发 llmdoc:update，而非积累到难以追溯
- project-overview.md 的包地图表格应包含文件数更新和新包条目

## 文档行动

- project-overview.md：添加 multiagent、toollearn 两个新包行；更新 planner 文件数 3→8
- doc-gaps.md：添加新条目（Distiller/Supervisor/Metacognition 接线状态未确认）
- architecture/ 可选：考虑新建 `architecture/multi-agent-and-learning.md`
