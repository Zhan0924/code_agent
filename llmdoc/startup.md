# 启动阅读顺序

每次新任务开始时，按此顺序阅读 `/must/` 文档。

## 必读文档（按顺序）

1. `llmdoc/must/project-basics.md` — 项目身份、双项目布局、必选/可选子系统、构建与测试
2. `llmdoc/must/working-agreement.md` — DI 模式、KV-cache prompt 结构、工具分发拆分、死代码清单、测试惯例
3. `llmdoc/must/doc-routing.md` — 如何找到正确的文档：architecture / guides / reference / memory 分类指引

## 升级阅读

当任务涉及特定子系统时，按需加载：

- **修改请求流程/编排器** → `llmdoc/architecture/request-flow.md`
- **修改 RAG/沙箱/MCP/Temporal/存储** → `llmdoc/architecture/infrastructure-subsystems.md`
- **修改认证/安全/可观测性** → `llmdoc/architecture/security-and-observability.md`
- **需要全局视角** → `llmdoc/overview/project-overview.md`
- **不确定某个功能是否已实现** → `llmdoc/memory/doc-gaps.md`（死代码和未接线功能清单）
