# 反思：`docs/architecture/` 全面重写 + 8 篇新增（2026-06-01）

## 任务

把 `docs/architecture/` **00–20 重写 + 21–28 新增**（共 29 篇）统一到 13 节模板：

> 模块定位 / 设计哲学 / 依赖架构 / 数据流总览 / 实现细节 / 设计权衡 / 后续演进 / 已知缺陷一览 / 测试矩阵 / 配置示例 / 跨文档引用 / 下一篇导引

**29 结语 + 30 近期改进** 暂未纳入本次重写：两篇文件名已重命名为新顺序（29/30），但内部 H1 标题仍为 `# 21 · 架构回顾` / `# 22 · Recent Improvements`，且结构本身就是收尾性文档（全景回顾 / 改进时间线），不适合 13 节模板。属遗留 TODO。

并同步 `llmdoc/index.md` / `startup.md` / `must/doc-routing.md` / `memory/doc-gaps.md`。

## 核心原则

- **诚实文档**：当代码与设计文档不一致时，以代码为准。任何"未接线 / 占位 / 已知 bug"都必须显式标注，每个标注带 file:line 锚点。
- **不发明数据**：所有数字、行号、字段值来自实际 Read/Grep 观察。引用代码片段时附 file:line。
- **每篇含"已知缺陷一览"**：每包 5–10 个 P0/P1/P2 条目，把死代码与未接线问题分散到对应包文档，而非集中在一个 doc-gaps。

## 关键发现（投射到 llmdoc）

1. **DEP-1 P0**：`Dockerfile.allinone:79 COPY configs/config.allinone.yaml` 与 `.dockerignore:23 configs/config.allinone.yaml` 互斥 → 构建失败。这是部署链路的硬阻塞。
2. **DEP-4 P0**：`deployments/k8s/deployment.yaml` 未注入 `CODE_AGENT_AUTH_JWT_SECRET` env，多副本时 JWT 签名密钥独立随机生成（与 `18_auth_security AS-1` 联动）。
3. **`internal/audit` / `internal/errors` 为孤儿包**：零生产 importer。`internal/pool` 仅被 `session/manager.go` 一处使用。已在 `19_observability.md` 显式标注。
4. **`hmac.go` timestamp 已强制必填**（recent fix at L124-136）—— 旧 `_principles.go` 描述的"可选 timestamp"语义已不准确。
5. **ToolLearn `PGStore` 已接线**：`cmd/agent/main.go` 中 pgStore 可用时自动 `Migrate` 并 `orch.SetToolLearnStore`，反馈跨重启保留在 `tool_feedback` 表。doc-gaps 中"未接线确认"已过时。

## 教训

1. **架构文档与代码同步是周期性工作**：本次重写发现至少 6 处文档与代码不一致（HMAC timestamp、PGStore 接线、orphan packages、Dockerfile/.dockerignore 冲突、JWT secret 多副本约束、tree-sitter CGO 回退）。建议季度审计。
2. **每包独立"已知缺陷一览"优于集中式 doc-gaps**：单包 5–10 个条目带 file:line，远比集中清单更易于在修改前查阅。
3. **跨文档引用是核心机制**：13 节模板的最后两节"跨文档引用 / 下一篇导引"形成阅读骨架，避免文档孤岛。
4. **`docs/architecture/` 与 `llmdoc/` 的分工**：单包深度文档放 `docs/architecture/`；跨包流程汇总放 `llmdoc/architecture/`；历史决策与孤立缺口放 `llmdoc/memory/`。

## 后续建议

- 把 `docs/architecture/NN_*.md` 中的"已知缺陷一览"汇总成机读 JSON / YAML，CI 中扫描代码做 stale 检测（条目对应代码已修复时报警）。
- 为常见工作流补 `llmdoc/guides/`：「如何添加内置工具」「如何接线一个死代码包」「如何添加新 MCP 服务器」。
- 在 `cmd/agent/main.go` 顶部加 link comment 指向 `docs/architecture/00_overview.md`，让代码读者能反向找到文档。
