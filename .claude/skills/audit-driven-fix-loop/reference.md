# audit-driven-fix-loop · 参考手册

> 本文件展开 `SKILL.md` 中 Step 4 与代码生成的工程化细节。`SKILL.md` 是主索引，遇到工程细节再翻这里。

## 1. 项目预设变量表（code_agent 项目现成可用）

如果迁移到其他项目，把这张表里的"实际值"替换即可，其余流程不变。

| 变量 | 实际值（code_agent 当前） | 来源 |
|---|---|---|
| 容器运行时 | `podman`（与 docker CLI 兼容） | `deploy-p0.sh`、`scripts/verify-audit-p2-5.sh` |
| 镜像源前缀 | `docker.m.daocloud.io/` | `docker-compose.yml`、`Dockerfile` |
| agent 容器名 | `code-agent` | `docker-compose.yml` |
| agent HTTP 端口 | `18080` → 容器 `8080` | `docker-compose.yml` |
| agent WS 端口 | `18081` → 容器 `8081` | `docker-compose.yml` |
| Postgres 容器名 | `agent-postgres` | `docker-compose.yml` |
| Postgres DSN | `postgres://agent:agent_secret@localhost:15432/code_agent?sslmode=disable` | `docker-compose.yml`、`scripts/verify-audit-p2-5.sh` |
| Postgres 镜像 | `docker.m.daocloud.io/pgvector/pgvector:pg16` | `docker-compose.yml` |
| Redis 容器名 / 端口 | `agent-redis` / `16379` → `6379` | `docker-compose.yml` |
| Qdrant 容器名 / 端口 | `agent-qdrant` / `6333`(REST) `6334`(gRPC) | `docker-compose.yml` |
| Temporal 容器名 / 端口 | `agent-temporal` / `7233` | `docker-compose.yml`（profile: hitl） |
| 网络 | `agent-net`（compose）或 `p0-net`（`deploy-p0.sh`） | `docker-compose.yml` / `deploy-p0.sh` |
| Plan 文档目录 | `docs/superpowers/plans/` | 仓库约定 |
| 审计文档典型路径 | `llmdoc/<area>/reflections/*-audit.md` | 仓库约定 |
| Verify 脚本目录 | `scripts/verify-<AUDIT-ID>.sh` | 仓库约定（见 `scripts/verify-audit-p2-5.sh`） |

迁移到其它项目时，复制本表一份到目标 skill 副本里改值即可。

---

## 2. Docker 部署流程

### 2.1 依赖容器复用规则

**默认假设 pgvector / qdrant / temporal 已经在跑**。每次部署只动 agent 容器，避免数据丢失或冷启动时间。

```bash
podman ps --format '{{.Names}}\t{{.Status}}' | rg 'agent-(postgres|qdrant|temporal|redis)'
```

按结果分支：

- 全部 `Up` → 直接进 2.2 build agent
- 某个停止（`Exited`）→ `podman start <name>` 后再 `podman logs --tail 50 <name>` 确认无错
- 某个不存在 → 优先用 `docker-compose.yml` 起一份；裸 podman 起的话参考 `deploy-p0.sh` 的命令

### 2.2 构建 + 重启 agent

```bash
cd <repo-root>

podman build -t code-agent:latest .

podman rm -f code-agent 2>/dev/null || true

podman run -d --name code-agent --network agent-net \
  -p 18080:8080 -p 18081:8081 \
  -e CODE_AGENT_REDIS_ADDR=agent-redis:6379 \
  -e CODE_AGENT_POSTGRES_DSN=postgres://agent:agent_secret@agent-postgres:5432/code_agent?sslmode=disable \
  -e CODE_AGENT_QDRANT_ADDR=agent-qdrant:6334 \
  -e CODE_AGENT_TEMPORAL_HOST=agent-temporal:7233 \
  -v "$(pwd)/configs/config.yaml:/etc/code-agent/configs/config.yaml:ro" \
  code-agent:latest
```

> 优先使用 `podman compose up -d --build agent`（行为等价、env 不会漏写）；上面给出的裸命令仅用于 compose 不可用时的兜底。

### 2.3 健康检查（必跑，等 ready 才能进校验脚本）

```bash
for i in {1..30}; do
  code=$(curl -s -o /dev/null -w '%{http_code}' http://localhost:18080/healthz)
  [[ "$code" == "200" ]] && { echo "ready"; break; }
  sleep 2
done
```

`for` 循环里 30 次仍不 ready → `podman logs --tail 200 code-agent` 看启动错。常见根因：DSN 不通、migration 失败、config 文件没挂载。

---

## 3. 镜像处理

### 3.1 镜像存在性检查

```bash
podman images --format '{{.Repository}}:{{.Tag}}' | rg \
  -e 'pgvector/pgvector:pg16' \
  -e 'qdrant/qdrant' \
  -e 'temporalio/auto-setup' \
  -e 'library/redis:7-alpine' \
  -e 'code-agent:latest'
```

### 3.2 缺失镜像拉取（用项目镜像源前缀）

```bash
podman pull docker.m.daocloud.io/pgvector/pgvector:pg16
podman pull docker.m.daocloud.io/qdrant/qdrant:v1.12.4
podman pull docker.m.daocloud.io/temporalio/auto-setup:1.25
podman pull docker.m.daocloud.io/library/redis:7-alpine
```

`code-agent:latest` 是本地 build 产物，不用 pull，只跑 2.2 的 build 即可。

### 3.3 拉取失败兜底

按以下顺序换：

1. 重试一次（瞬时 5xx 常见）
2. 改镜像源前缀（如 `docker.io/`、`ghcr.io/`、`registry.cn-hangzhou.aliyuncs.com/`），但要同步改 `docker-compose.yml`
3. 如果是 agent 镜像 build 失败而非 base 镜像，看 `Dockerfile`：项目用 `goproxy.cn` + `GOSUMDB=off` 解决 go mod download；Alpine apk 走默认源

仍失败 → 用 `AskQuestion` 让用户选择"换源 / 跳过 / 终止"。

---

## 4. 可校验日志规范（Step 3 实现修改时必须遵守）

### 4.1 字段约定

每个本次修复路径的关键 log line 至少包含：

| 字段 | 说明 | 例 |
|---|---|---|
| `audit_id` | 当前 AUDIT-ID，便于 grep | `AUDIT-P1-3` |
| `op` | 操作名（建议英文 snake_case） | `gdpr_delete_user_memory` |
| `tenant.user_id` | 多租户路径必带 | `verify_p1_3_user` |
| `tenant.project_id` | 多租户路径必带 | `default` |
| `before` / `after` | 写库前后关键字段 | `before.count=12, after.count=0` |
| `result` | `ok` / `noop` / `error` | `ok` |
| `severity` | 失败时填 `warn`/`error`/`critical` | `critical` |
| `duration_ms` | 关键路径延迟 | `42` |

### 4.2 实现示例（Go zap）

```go
logger.Info("memory_delete_by_user completed",
    zap.String("audit_id", "AUDIT-P1-3"),
    zap.String("op", "gdpr_delete_user_memory"),
    zap.String("tenant.user_id", userID),
    zap.String("tenant.project_id", projectID),
    zap.Int("before.count", before),
    zap.Int("after.count", after),
    zap.Duration("duration_ms", time.Since(start)),
    zap.String("result", "ok"),
)
```

失败分支：

```go
logger.Error("memory_delete_by_user failed",
    zap.String("audit_id", "AUDIT-P1-3"),
    zap.String("severity", "critical"),
    zap.Error(err),
)
```

### 4.3 日志级别准则

- `Info`：成功执行的关键路径（Step 4 默认 grep `INFO` 拿主流证据）
- `Warn`：可降级的失败（hot tier miss、缓存失效）
- `Error`：单次操作失败但不影响整体可用性（cold tier 写失败 + DLQ）
- `Critical` severity 字段：需要告警，配合 `code_agent_memory_failures_total{severity}` metric

### 4.4 反模式（禁止）

- `logger.Debug("...")` 放在 Step 4 需要校验的路径上 → 默认级别看不到
- `fmt.Printf` / `log.Println` → 非结构化，grep 起来易误判
- 一行 log 包含上百字符的 JSON dump → grep 不到具体字段值
- 失败分支静默 `_ = err` → Step 4 完全无法发现回归

---

## 5. 三重校验（Step 4 必须三件套都跑）

### 5.1 API 校验

```bash
BASE_URL="http://localhost:18080"

resp=$(curl -sS "${BASE_URL}/api/v1/<endpoint>")
status=$(curl -s -o /dev/null -w '%{http_code}' "${BASE_URL}/api/v1/<endpoint>")
[[ "$status" == "200" ]] || fail "expected 200, got $status, body=$resp"

for field in '"id"' '"user_id"' '"<your_field>"'; do
  printf '%s' "$resp" | rg -q "$field" || fail "missing field $field in: $resp"
done
```

正反断言都要写：

- 正向：应有的 field / value 都出现
- 反向：异常 case（如 unknown id）返回 404 而非 500、错误信息不泄露内部路径

### 5.2 日志校验

```bash
window="5m"
logs=$(podman logs --since "$window" code-agent 2>&1)

printf '%s\n' "$logs" | rg -q 'audit_id=AUDIT-P1-3' \
  || fail "audit log line missing"

printf '%s\n' "$logs" | rg -q '"op":"gdpr_delete_user_memory".*"result":"ok"' \
  || fail "expected ok result not found"

printf '%s\n' "$logs" | rg -q '"severity":"critical"' \
  && fail "unexpected critical error during verification"
```

要点：

- 用 `--since` 限定窗口，避免误命中旧 log
- 三类断言都跑：**预期出现**、**预期字段值正确**、**不应出现的错误未出现**
- 结构化日志用 JSON 字段匹配；非结构化只能用宽松 grep（这是为什么 §4 要求 JSON 输出）

### 5.3 数据库校验

参考 `scripts/verify-audit-p2-5.sh` 的 PSQL 调用方式：

```bash
PG_DSN="postgres://agent:agent_secret@localhost:15432/code_agent?sslmode=disable"

if command -v psql >/dev/null 2>&1; then
  PSQL=(psql "$PG_DSN" -v ON_ERROR_STOP=1 -A -t)
else
  PSQL=(podman exec -i agent-postgres psql -U agent -d code_agent -v ON_ERROR_STOP=1 -A -t)
fi

count=$("${PSQL[@]}" -c "SELECT count(*) FROM memories WHERE user_id='verify_user' AND deleted_at IS NULL")
[[ "$count" == "0" ]] || fail "expected 0 active rows for verify_user, got $count"
```

要点：

- 优先用宿主 `psql`，没有就用 `podman exec` 进容器跑（兼容 CI 与本地）
- 关键 metric：行数 / 字段值 / 索引使用（`EXPLAIN`）
- 涉及 vector 字段时校验 `length(embedding::float[])` 维度对得上 schema
- 校验前 seed 数据 + 校验后清理 seed，确保脚本可重复跑

### 5.4 三件套覆盖矩阵

每条 AUDIT-ID 的 verify 脚本必须勾完下表：

| 维度 | 是否必须 | 备注 |
|---|---|---|
| 健康检查 (`/healthz` 200) | ✅ | 否则后续断言无意义 |
| API 正向断言 | ✅ | 至少一个 happy path |
| API 反向断言 | ⬜ 强烈建议 | 错误码 / 异常输入 |
| 日志出现断言 | ✅ | 关键 audit_id log line 必须出现 |
| 日志不出现断言 | ✅ | 没有意外的 critical / panic |
| DB 行数 / 字段断言 | ✅（涉及持久化时） | 行数 / 字段值 / 索引 |
| 清理 seed 数据 | ✅ | 脚本可重复执行 |

---

## 6. 代码生成时的可观测性硬性要求

Step 3 写代码时，每一处状态变化（写库、写缓存、发布 blackboard、调外部 API、tools 调用）都按这张表自检：

- [ ] 关键路径已有 `Info` 级别结构化 log（带 audit_id / op / tenant / before-after / result）
- [ ] 失败分支已有 `Error` 级别 log 并带 severity
- [ ] 多租户字段（user_id / project_id）传递到 log
- [ ] 涉及数据库变更，已新增/复用 metric counter（如 `code_agent_memory_failures_total`）
- [ ] 如果新增配置项，default 值在 `config.example.yaml` 同步更新
- [ ] 如果新增 endpoint，已添加 router 注册并暴露在 `/api/v1/...`

任何一项 ✗ → 当前 Step 3 没完成，禁止进入 Step 4。

---

## 7. safe-push 提交规范

提交前 self-check：

- [ ] `git status` 仅显示当前 AUDIT-ID 相关的文件（plan 文档、源码、verify 脚本、审计文档勾选）
- [ ] 没有混入 `test_*.go` 临时调试文件、`*.log`、个人 IDE 配置
- [ ] commit message 含 AUDIT-ID 与一句话总结
- [ ] 审计文档对应行已标 ✅ 并指向 plan 文档相对路径

调用 `/safe-push`，让其按二进制过滤 + 模块分组规则处理。
