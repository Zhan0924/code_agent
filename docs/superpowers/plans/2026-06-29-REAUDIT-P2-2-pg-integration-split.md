# REAUDIT-P2-2 — testcontainers 集成测试拆分

## 方案
将 `TestPGCold_Integration` 单函数拆为独立用例：
- `TestPGCold_Integration_BoostScoreBatch` (§35)
- `TestPGCold_Integration_MarkDistilled` (§36)
- `TestPGCold_Integration_DeleteByUser` (§26)
- `TestPGCold_Integration_CrossTypeRetrieve` (§29)
- 保留 `TestPGCold_Integration_DedupTx`

共享 `setupPGColdIntegration` helper。

## 验收
`scripts/verify-reaudit-p2-2.sh` 运行上述 `-run TestPGCold_Integration_` 测试。
