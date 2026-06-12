# 反思：docs/architecture 飘移面对账（C 档全面）

日期：2026-06-11

## 背景

用户请求「基于当前最新的代码更新 `docs/architecture` 中的文件」。我提供 A/B/C 三档方案：

- **A 档（最小）**：仅 `30_recent_improvements.md` 补一条时间线。
- **B 档（中度）**：A 档 + `07_tools.md` 补 `file_tools` 重构 + `19_observability.md` 补 SSE 心跳/Stream Cache 探针。
- **C 档（全面对账）**：B 档 + `09_orchestrator.md` / `12_session.md` / `16_store.md` 三篇做行号校准 + 局部增量。

用户选 C 档。任务覆盖三个上游 commit：

- `86572f9` Session PG 双层（自带文档更新，碰过 `12_session.md` §7.5、`16_store.md` §3.7）
- `7b13a3b` UI 假死链 5 层防御（未碰 docs/architecture）
- `609d99c` Temporal worker 接线（未碰 docs/architecture）

## 经过

合计 **+280 行，跨 6 个文件**，全部是文档增删，无代码改动。

| 文件 | +行 | 主改动 |
|---|---|---|
| `30_recent_improvements.md` | +144 | G 节 3 条 06-月时间线（G.1 Session PG 双层 / G.2 Verifier retry-once / G.3 UI 假死链 5 层防御） |
| `19_observability.md` | +51 | 新增 §5.12「SSE 心跳 + Stream Cache 探针」三小节（P1 ping 事件 / B3 last_event_at_ms / B1 tool_progress） |
| `09_orchestrator.md` | +42 | §9.2 verifyOutput 行号校准 + 新增测试矩阵表（9 个测试函数）+ Metadata 不变量补充 |
| `16_store.md` | +38 | CRUD 行号校准（L243→L266 等漂移 ~20 行）+ 新增 sessions 行（指向 §3.7）+ Migrate 行 + 跨包 CRUD 说明 |
| `07_tools.md` | +16 | §8.1 新增「validateWorkspaceCommand 双返契约」三态表 + 调用点说明 |
| `12_session.md` | +17 | §7.5.3 rehydrate 补 ZSET 索引刷新副作用 + 行号 |

任务过程是先打开三个 commit 的 diff 横向比对（哪些动了文档、哪些没动），然后逐个 docs/architecture 文件按 13 节模板锚点切入修改。

## 关键发现

### 1. 多 commit 时间窗回放成本不对称

`86572f9` commit 自带文档更新 → 本次 `12_session.md` / `16_store.md` 工作量极小（主要是行号漂移修正）。`7b13a3b` 与 `609d99c` 完全没碰文档 → 本次 `30_recent_improvements.md::G 节` 三条时间线全部要从 commit log + 代码现状重建。**「commit 时是否同步动了文档」是后续 audit 工作量的最大决定因子**。

### 2. 行号漂移的真实成本

`16_store.md::§4 CRUD 方法表`里 ~16 个行号引用全部漂移约 20-23 行（因为 `86572f9` 在 `postgres.go` 加了 sessions 表 schema 块）。逐个 `rg '^func '` 重新确认。**这是「同 commit 触发的二次债务」**：commit 已经更新了 §3.7 主条目，但 §4 行号表是「跨节引用」，commit 作者没注意到。说明文档表格内的 `file:line` 引用极脆弱，应优先指向**符号名**而非裸行号。

### 3. 「大重写后无小修」是错觉

B 档草案曾以为「09/12/16 在 2026-06-04 `b87d848` 大重构后已完整」，可跳过。实际上 `86572f9`（2026-06-07）的二次重构又引入了局部增量（PG 双层、verifier retry-once），所以仍有校准必要。**「最近一次大重写时间」不是免疫期**，必须按 commit 维度逐个回放。

### 4. PUT 行号 vs TAKE 行号的非对称

`09_orchestrator.md::§9.2` 我**主动**补了 9 个测试函数行号（测试矩阵表），是新增信息；`16_store.md::§4` 是**被动**校准。两类工作量都不大，但反映「文档对账」分两类：

- **校准型**：上游代码移动，文档行号失效 → 修复指向。
- **增量型**：上游代码新增，文档无对应锚点 → 新增段落。

后者更稀缺也更有价值。

### 5. 「文档对账」输出形态难评估

与「实现新功能」不同，本次产物全是文档，工程价值在于「下次新人 grep 行号不失效」。这种维护性产出常被 commit message 写成「校准多个行号」，语义模糊。**建议明确写出「校准 X 个行号 / 新增 Y 段 / 关联 commit Z」**。

### 6. 13 节模板的稳定结构降低对账成本

`09/12/16/19` 都是 13 节模板，可直接跳到 §9.2 / §3.7 / §5.x 改。而 `30_recent_improvements.md` 是编年体，需要在文件末尾找到正确锚点（最近一节后追加），编辑成本反而更高。**模板化的稳定结构是文档对账的最大杠杆**。

## 教训与模式

- **「行号引用 = 维护税」**：每写一个 `file:line` 就背一份维护负担。但 `file:line` 也是不可替代的可验证锚点（`file:symbol` 漂移更隐蔽）。策略：**只在「设计点 / 不变量 / 测试矩阵」三类高价值场景写行号**，简单介绍段（如调用链概述）只写 `file` 不写 `:line`。
- **「commit 时同步更新关联文档」效率最高**：`86572f9` 自己更新了 12/16 → 本次工作量极小。`7b13a3b` / `609d99c` 没碰 → 本次全推一遍 `30_recent_improvements`。**最便宜兜底：commit 至少在 `30_recent_improvements` 加一行时间线**。
- **「编年体文档比模板文档难维护」**：13 节模板可按节点替换，编年体只能追加，且锚点漂移。**新增类似 `30_recent_improvements` 形态文档前应三思**。
- **「文档对账输出表格化」让 review 容易**：6 行表格（文件 / +行 / 主改动）就是可 review 单元，比散文式 changelog 友好。

## 推广候选

| 内容 | 目标位置 | 理由 |
|---|---|---|
| 「commit 至少更新 `30_recent_improvements` 时间线」commit hygiene 约定 | `must/working-agreement.md` 新增段，或新建 `guides/commit-hygiene.md` | 本次 G 节 3 条全部是「commit 时未更新文档」的回补，模式重复 |
| 「行号引用的三类高价值场景」准则 | 新建 `guides/doc-line-anchor-policy.md` | docs/architecture 行号引用密度大，维护税逐步显现；统一策略可避免下次大规模漂移 |
| 「文档校准 vs 增量」二分类 + commit message 约定 | `must/working-agreement.md` 或 guides 内一段 | 让未来「校准」型 commit 不再失语，便于历史追溯 |

## 已发现的 doc gap

- **不开新 `memory/doc-gaps.md` 条目**：G 节回补后该 gap 已闭环，重新开条目反而制造双源。
- **`must/doc-routing.md` 建议增补**：当前提到「修改特定包前先读对应 `NN_*.md`」，可补一句「**也应**读 `30_recent_improvements.md` 看是否已有最近时间线」，避免重复 audit。
- **`docs/architecture/16_store.md::§4` 表格**：已校准本次，但格式（裸 `file:line`）仍易漂。可考虑下次重构时改为 `file::FuncName` + 折叠行号到尾注。

## 后续行动

供 recorder / 后续 reflector 跟进：

1. **决定**是否落地「commit 至少更新 `30_recent_improvements` 时间线」约定 — 进入 `must/working-agreement.md` 还是新建 `guides/commit-hygiene.md`。
2. **若推广行号策略**：起草 `guides/doc-line-anchor-policy.md`，明确「设计点 / 不变量 / 测试矩阵」三场景规则，并把现有违反点列入 follow-up。
3. **轻量动作**：在 `must/doc-routing.md` 补一句「修改包前并读 `30_recent_improvements.md` 最近 3 条时间线」。
4. **不动作**：不开 `memory/doc-gaps.md` 新条目（G 节已闭环）。

## 关联文件

本次修改的 6 个 docs/architecture 文件（绝对路径）：

- `/Users/qiankun/code/agent/code_agent/docs/architecture/07_tools.md`
- `/Users/qiankun/code/agent/code_agent/docs/architecture/09_orchestrator.md`
- `/Users/qiankun/code/agent/code_agent/docs/architecture/12_session.md`
- `/Users/qiankun/code/agent/code_agent/docs/architecture/16_store.md`
- `/Users/qiankun/code/agent/code_agent/docs/architecture/19_observability.md`
- `/Users/qiankun/code/agent/code_agent/docs/architecture/30_recent_improvements.md`

关联上游 commit：

- `86572f9` feat(session,store): Session 持久化到 PG 双层架构
- `7b13a3b` UI 假死链 5 层防御（详见 `memory/reflections/2026-06-07-ui-freeze-chain-defense-in-depth.md`）
- `609d99c` chore(infra): Temporal worker 接线

相关反思：

- `/Users/qiankun/code/agent/code_agent/llmdoc/memory/reflections/2026-06-01-architecture-docs-rewrite.md`（13 节模板初次重写）
- `/Users/qiankun/code/agent/code_agent/llmdoc/memory/reflections/2026-06-07-verifier-retry-and-process-as-artifact.md`
- `/Users/qiankun/code/agent/code_agent/llmdoc/memory/reflections/2026-06-07-ui-freeze-chain-defense-in-depth.md`
