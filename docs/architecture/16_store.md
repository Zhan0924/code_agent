# 16 · 持久化层 `internal/store` + `internal/auth`（Redis 撤销）

> 代码（**以代码为准**）：
>
> - `internal/store/postgres.go` (535 行) — PostgreSQL Store：6 张表 + 自动 migration + Tasks / AuditLogs / ApiKeys / Approvals / DynamicTools / FileChecksums CRUD
> - `internal/store/postgres_test.go` (65 行) — 简单连通性测试，需要真实 PG
> - `internal/auth/redis_revocation.go` (97 行) — JWT 撤销黑名单的 Redis 存储 + JWTManagerWithRedis 装饰器
>
> 上层调用：
>
> - `cmd/agent/main.go:342-363` — `store.NewStoreFromDSN(cfg.Postgres.DSN, MaxOpenConns, MaxIdleConns, logger)` + `Migrate(ctx)`
> - `main.go:578-590` — `apiServer.SetStore(pgStore)` + 启动时 `LoadDynamicTools` 复原动态工具
> - `internal/indexer/indexer.go:119` — `store.GetAllChecksums(projectName)` 预热增量索引
> - `internal/auth/redis_revocation.go:71` — JWTManagerWithRedis 装饰原 JWTManager

---

## 1. 模块定位

**"让 Agent 的记忆能穿越进程重启。"**

整个系统里的状态分三层：

| 层 | 存哪里 | 失效后果 |
|---|---|---|
| **热状态**（对话滑窗、embedding 结果） | Redis（见 12_session） | 无伤大雅，对话重来一次即可 |
| **冷记忆**（Task / Audit / Approval / ApiKey / DynamicTool / Checksum） | **PostgreSQL（本模块）** | 审计丢失、授权丢失、动态工具丢失——**不可容忍** |
| **工作产物**（代码文件） | Workspace 目录（见 14_workspace） | 用户代码丢了——灾难 |

本模块负责**中间那层**。具体存什么：

| 表 | 用途 | 关键字段 |
|---|---|---|
| `tasks` | Agent 任务状态 + plan JSON | id / session_id / state / plan_json |
| `audit_logs` | 敏感操作审计链 | task_id / action / details (JSONB) / risk_level |
| `api_keys` | 长期 API Key 哈希存储 | key_hash / user_id / role |
| `approvals` | HITL 授权请求生命周期 | task_id / status / approved_by |
| `dynamic_tools` | 运行时注册的工具配置 + TTL | name / executor_type / ttl |
| `file_checksums` | indexer 增量索引的 SHA-256 缓存 | project_name + file_path → hash |

附带一个**小而关键**的姊妹组件：`internal/auth/redis_revocation.go` —— JWT 撤销黑名单。
它放在 `auth` 包里但本质是持久化逻辑，所以一起讲。

⚠️ **旧文档声称 4 张表（Tasks / AuditLogs / ApiKeys / Approvals），实际有 6 张**——新增了 `dynamic_tools` 和 `file_checksums`。

---

## 1.5 设计哲学：5 个被代码证实的抉择

### Q1 — PostgreSQL 存什么 / 不存什么？

**存**：
- 审计日志（合规 / 事后取证 / 强一致）
- API Key 哈希（服务账号凭据 / 长期）
- 长任务状态（Temporal 后端默认就是 Postgres）
- HITL 审批请求（跨重启的 pending 队列）
- 动态工具配置（运行时注册，重启需要复原）
- 文件 hash（indexer 增量索引避免冷启动 reindex）

**不存**：
- Session messages → Redis（24h TTL，丢失可接受）
- RAG embeddings → Qdrant（高维向量不适合 PG）
- 实时指标 → Prometheus
- 限流计数器 → Redis（高频读写）

**判别标准**：写入耐久要求 + 跨进程 + 不在主路径热路径上 → PostgreSQL。

### Q2 — 为什么不引入 golang-migrate / goose？

`Migrate()` 是一段硬编码 SQL 数组（L135-213）：
```go
migrations := []string{
    `CREATE TABLE IF NOT EXISTS tasks (...)`,
    `CREATE INDEX IF NOT EXISTS idx_tasks_session ON tasks(session_id)`,
    ...
    `ALTER TABLE tasks ADD COLUMN IF NOT EXISTS plan_json JSONB`,   // 兼容旧库
    ...
}
for _, m := range migrations {
    s.db.ExecContext(ctx, m)
}
```

**为什么不用 migration tool？**

| 方案 | schema 演进表达力 | 工具链复杂度 | 跨环境一致 |
|---|---|---|---|
| 硬编码 `CREATE IF NOT EXISTS` | 弱（无回滚） | 0 | 强（启动即跑） |
| golang-migrate / goose | 强（有版本表 + up/down） | 高（migration 文件 + 工具二进制） | 需要部署 step |
| ORM 自动 migration | 中 | 中 | 弱（不同 ORM 行为不同） |

**当前选硬编码**：
- schema 演进慢（4 张表→6 张表花了几个月）
- `IF NOT EXISTS` 让重复运行幂等
- 加新列用 `ALTER TABLE ADD COLUMN IF NOT EXISTS`（L189）
- 不需要 down migration（数据库 schema 倒退在生产很少见，做错就备份回滚）

**代价**：复杂迁移（如改列类型 / 重命名）做不了。**P2 风险**——schema 越变越复杂时需要切到 golang-migrate。

### Q3 — 连接池参数为什么是 25/10/5min？

```go
db.SetMaxOpenConns(cfg.MaxOpenConns)        // 默认 25
db.SetMaxIdleConns(cfg.MaxIdleConns)        // 默认 10
db.SetConnMaxLifetime(5 * time.Minute)      // 硬编码
```

| 参数 | 值 | 理由 |
|---|---|---|
| MaxOpenConns | 25 | 典型单 Pod 并发上限；PG 默认 `max_connections=100` 留 4 个 Pod 的余地 |
| MaxIdleConns | 10 | 保持少量常驻避免每次冷启动 TCP handshake |
| ConnMaxLifetime | 5 min | 强制轮换避免 DB 端 idle kill 导致首次请求失败（典型 `tcp_keepalive_time` 7200s） |

⚠️ **`ConnMaxLifetime = 5 min` 硬编码**：不响应配置。如果用户 PG 端 idle timeout 是 1 分钟，仍然会撞失败。**P2：暴露成配置**。

### Q4 — 为什么 Postgres 是可选依赖？

`main.go:344-363`：
```go
if cfg.Postgres.DSN != "" {
    pgStore, err = store.NewStoreFromDSN(...)
    if err != nil {
        logger.Warn("PostgreSQL not available, persistence disabled", zap.Error(err))
    } else {
        pgStore.Migrate(ctx)
        defer pgStore.Close()
    }
}
```

**DSN 为空 → 不创建 store；DSN 非空但连接失败 → Warn 继续启动**。
后续所有 `pgStore != nil` 的代码路径都 nil-check：
- `indexer.WithStore(pgStore)` 跳过（增量索引仍工作，但重启会全量重算）
- `apiServer.SetStore(pgStore)` 跳过（动态工具不持久化但内存仍可用）
- `toollearn.NewPGStore(pgStore.DB())` 跳过
- `pgPingFn = pgStore.Ping` 跳过（健康检查少一项）

**与 Redis 不同**：Redis 失败直接 `log.Fatal`（session 强依赖），Postgres 失败 Warn 继续。
**因为**：Redis 失败时 session 系统完全瘫痪，业务跑不动；Postgres 失败时只是审计日志和持久化缺失，agent 仍可对话。

### Q5 — JWT 撤销为什么用 Redis 不用 PG？

`auth/redis_revocation.go` 而不是 `store/postgres_jwt.go`。

| 维度 | Redis | PG |
|---|---|---|
| ValidateToken 路径 RTT | <1ms | 1-5ms（每个请求都查一次） |
| TTL 自动过期 | 原生 `EXPIRE` | 需要 cron job DELETE WHERE expires_at < now() |
| 跨 Pod | 共享（必须） | 共享 |
| 持久化 | AOF 可选 | 强 |
| 内存占用 | 撤销 entries 一般 < 1MB | 表占用 |

JWT 撤销是**每个请求都查一次**的热路径——RTT 决定一切。Redis 完胜。
TTL 自动过期也省事——撤销条目和 token 一起到期。

---

## 2. 依赖架构

```
                ┌──────────────────┐
                │  main.go DI      │
                └──────┬───────────┘
                       │
   ┌───────────────────┼─────────────────────────┐
   ▼                   ▼                         ▼
┌──────┐         ┌───────────┐            ┌──────────────┐
│ Redis│         │ pgStore   │            │ JWTManager   │
│      │         │ *sql.DB   │            │   ↓ wraps    │
│      │         │           │            │ JWTManager   │
│      │         │ 6 tables  │            │ WithRedis    │
└──┬───┘         └─────┬─────┘            └──────┬───────┘
   │                   │                         │
   │ used by:          │                         │ used by:
   │ - session.        │                         │ - api auth
   │   Manager         │ used by:                │   middleware
   │ - rate limiter    │ - orchestrator (tasks)  │
   │ - JWT revocation  │ - indexer (checksums)   │
   └─────┐             │ - toollearn (memory)    │
         │             │ - api (dynamic_tools)   │
         ▼             │ - audit logger          │
   ┌──────────────┐    └─────────────────────────┘
   │ JWT revoke   │
   │ jwt:revoked: │
   │ <jti>        │
   └──────────────┘
```

**注入点**（`cmd/agent/main.go:342-363`）：

```go
var pgStore *store.Store
if cfg.Postgres.DSN != "" {
    // ⚠️ 注意：pgCfg 的 Host/Port/User/Password 硬编码是死字段
    //     当 DSN 非空时直接走 NewStoreFromDSN，pgCfg 这些字段未使用
    pgCfg := &store.PostgresConfig{
        Host: "localhost", Port: 5432, User: "agent",
        Password: "agent_secret", Database: "code_agent", SSLMode: "disable",
        MaxOpenConns: cfg.Postgres.MaxOpenConns,
        MaxIdleConns: cfg.Postgres.MaxIdleConns,
    }
    pgStore, err = store.NewStoreFromDSN(cfg.Postgres.DSN, pgCfg.MaxOpenConns, pgCfg.MaxIdleConns, logger)
    if err != nil {
        logger.Warn("PostgreSQL not available, persistence disabled", zap.Error(err))
    } else {
        pgStore.Migrate(ctx)
        defer pgStore.Close()
    }
}
```

⚠️ **代码里 `pgCfg` 的 Host/Port/User/Password 字段是死字段**：当 DSN 非空时只用 `MaxOpenConns/MaxIdleConns`，其他都浪费——典型重构遗留。
**P2 整理**：要么用 PostgresConfig 构造（NewStore），要么完全删除 pgCfg。

---

## 2.5 数据流总览

```text
═══════════ 启动: Migrate + LoadDynamicTools ═══════════════════════════════

main.go startup
       │
       ▼
store.NewStoreFromDSN(DSN, maxOpen, maxIdle)        [postgres.go:98]
       │
       │ sql.Open("postgres", DSN)
       │ db.SetMaxOpenConns(maxOpen)
       │ db.SetMaxIdleConns(maxIdle)
       │ db.SetConnMaxLifetime(5 * time.Minute)
       │ db.PingContext(5s timeout)
       │
       ▼ return &Store{db, logger}
pgStore.Migrate(ctx)                                [postgres.go:134]
       │
       │ for each SQL in migrations[]:
       │   db.ExecContext(ctx, m)
       │     ├ CREATE TABLE IF NOT EXISTS tasks...
       │     ├ CREATE INDEX IF NOT EXISTS idx_tasks_session...
       │     ├ ...
       │     ├ ALTER TABLE tasks ADD COLUMN IF NOT EXISTS plan_json JSONB
       │     ├ CREATE TABLE IF NOT EXISTS dynamic_tools...
       │     └ CREATE TABLE IF NOT EXISTS file_checksums...
       │
       └ log.Info("database migrations completed", count=N)

main.go L583
       ▼
pgStore.LoadDynamicTools(ctx)                       [postgres.go:416]
       │
       │ SELECT ... FROM dynamic_tools
       │ WHERE ttl IS NULL OR (created_at + (ttl||' seconds')::interval) > NOW()
       │
       │ for each rec:
       │   tools.RegisterDynamic(rec)
       │
       └ → 恢复重启前注册的动态工具

═══════════ 任务: CreateTask → SavePlan → UpdateTaskState ═════════════════

orchestrator.ProcessMessage
       │
       ▼
audit.LogTaskCreated → pgStore.CreateTask(t)        [postgres.go:243]
       │
       │ INSERT INTO tasks (id, session_id, ..., created_at, updated_at) VALUES (...)
       │
       ▼
planner.Plan → pgStore.SavePlan(taskID, planJSON)   [postgres.go:278]
       │
       │ UPDATE tasks SET plan_json=$2, updated_at=$3 WHERE id=$1
       │
       ▼
react loop → pgStore.UpdateTaskState(taskID, state, result)  [postgres.go:252]
       │
       │ UPDATE tasks SET state=$2, result=$3, updated_at=$4, completed_at=$5 WHERE id=$1
       │   completed_at 仅在 state ∈ {completed, failed, cancelled} 时填
       │
       ▼

═══════════ 审计: 敏感操作链 ═════════════════════════════════════════════

orchestrator.checkSensitive → suspendForApproval
       │
       ▼
pgStore.CreateApproval(a)                           [postgres.go:335]
       │
       │ INSERT INTO approvals (...) ON CONFLICT (task_id) DO NOTHING
       │   状态 = 'pending'
       │
       ▼ (用户审批通过/拒绝)
pgStore.ResolveApproval(taskID, status, approvedBy) [postgres.go:345]
       │
       │ UPDATE approvals SET status=$2, approved_by=$3, resolved_at=$4 WHERE task_id=$1
       │
       ▼
pgStore.InsertAuditLog(r)                           [postgres.go:311]
       │
       │ INSERT INTO audit_logs (task_id, user_id, action, details, risk_level) VALUES (...)

═══════════ JWT 撤销: Redis ═══════════════════════════════════════════════

POST /api/v1/auth/revoke (admin)
       │
       ▼
JWTManagerWithRedis.RevokeToken(ctx, jti, ttl)      [redis_revocation.go:94]
       │
       │ 1. JWTManager.RevokeToken(jti)              ← 内存黑名单
       │ 2. redisRevoke.Revoke(ctx, jti, ttl)        ← Redis SET jwt:revoked:<jti> "1" EX ttl
       │
       ▼

任何后续请求 with 该 token:
       │
       ▼
JWTManagerWithRedis.ValidateToken(token)            [redis_revocation.go:79]
       │
       │ 1. JWTManager.ValidateToken(token)          ← 签名 + exp 校验
       │ 2. redisRevoke.IsRevoked(ctx, claims.ID)    ← Redis EXISTS jwt:revoked:<jti>
       │      ├ true → return ErrTokenInvalid
       │      └ false → return claims
```

---

## 3. Schema 详解

### 3.1 `tasks`（postgres.go:136-148）

```sql
CREATE TABLE IF NOT EXISTS tasks (
    id          TEXT PRIMARY KEY,
    session_id  TEXT NOT NULL,
    user_id     TEXT NOT NULL DEFAULT '',
    intent      TEXT NOT NULL DEFAULT '',
    state       TEXT NOT NULL DEFAULT 'pending',
    user_input  TEXT NOT NULL,
    result      TEXT,
    plan_json   JSONB,                              -- planner.Plan 的完整 DAG
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ                        -- 仅 终态填写
);
CREATE INDEX idx_tasks_session ON tasks(session_id);
CREATE INDEX idx_tasks_state ON tasks(state);
CREATE INDEX idx_tasks_created ON tasks(created_at);
```

**关键点**：
- `state` 没有 enum check —— 应用层约束（"pending/running/completed/failed/cancelled"）
- `plan_json` JSONB——可用 GIN 索引加速 plan 字段过滤（**当前未建**）
- 三个索引：按 session 查所有任务 / 按 state 查 pending / 按时间排序

### 3.2 `audit_logs`（postgres.go:153-163）

```sql
CREATE TABLE IF NOT EXISTS audit_logs (
    id          BIGSERIAL PRIMARY KEY,
    task_id     TEXT NOT NULL,                      -- 外键不强制
    user_id     TEXT NOT NULL DEFAULT '',
    action      TEXT NOT NULL,                       -- "file_write" / "approval_request" / ...
    details     JSONB,                              -- 任意结构化负载
    risk_level  TEXT NOT NULL DEFAULT 'low',        -- low/medium/high/critical
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_audit_task ON audit_logs(task_id);
CREATE INDEX idx_audit_user ON audit_logs(user_id);
```

**为什么 BIGSERIAL 而非 UUID**：审计场景下"按时间排序"是首要查询，自增 ID 隐含时序，索引效率高于随机 UUID。

⚠️ **无 ON DELETE CASCADE**：task 被删时 audit_logs 还在——这是**有意的**，审计日志不能因为业务实体消失而消失。

### 3.3 `api_keys`（postgres.go:165-173）

```sql
CREATE TABLE IF NOT EXISTS api_keys (
    key_hash    TEXT PRIMARY KEY,                   -- bcrypt(api_key)
    user_id     TEXT NOT NULL,
    role        TEXT NOT NULL DEFAULT 'dev',        -- admin/dev/readonly
    label       TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used   TIMESTAMPTZ
);
```

**只存 hash**：明文 key 只在创建时返回一次给用户，丢了只能 revoke 重新发。

⚠️ **`last_used` 当前不更新**：grep 整代码无 `UPDATE api_keys SET last_used` ——**死字段**。`auth/apikey.go` 校验时不更新。**P1：补 UPDATE 或删字段**。

### 3.4 `approvals`（postgres.go:175-186）

```sql
CREATE TABLE IF NOT EXISTS approvals (
    task_id     TEXT PRIMARY KEY,                   -- 一个任务一个审批
    session_id  TEXT NOT NULL,
    action      TEXT NOT NULL,
    risk_level  TEXT NOT NULL DEFAULT 'high',
    details     TEXT,                                -- ⚠️ TEXT 不是 JSONB
    status      TEXT NOT NULL DEFAULT 'pending',    -- pending/approved/rejected/timeout
    approved_by TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at TIMESTAMPTZ
);
```

⚠️ **`details` 是 TEXT 不是 JSONB**：与 `tasks.plan_json` / `audit_logs.details` 不一致。
**应用层用 string 存 JSON serialized 后的内容**——但失去了 JSONB 的查询能力。**P2：迁移到 JSONB**。

### 3.5 `dynamic_tools`（postgres.go:192-202）

```sql
CREATE TABLE IF NOT EXISTS dynamic_tools (
    name TEXT PRIMARY KEY,
    description TEXT NOT NULL,
    parameters JSONB NOT NULL,                       -- JSON Schema for tool params
    executor_type TEXT NOT NULL,                     -- "http" / "command" / "lua"
    executor_config JSONB NOT NULL,
    risk_level INTEGER NOT NULL DEFAULT 0,
    ttl BIGINT,                                       -- nullable seconds; NULL = forever
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_dynamic_tools_ttl
    ON dynamic_tools(created_at, ttl) WHERE ttl IS NOT NULL;
```

**部分索引**：只在 `ttl IS NOT NULL` 时索引——TTL 检查只关心有时效的工具。

**LoadDynamicTools 的 TTL 过滤**（L416-421）：
```sql
WHERE ttl IS NULL OR (created_at + (ttl || ' seconds')::interval) > NOW()
```

⚠️ **没有后台清理任务**：过期工具记录会**永远留在表里**，只是 LoadDynamicTools 时被过滤掉。**P2：加 cron DELETE**。

### 3.6 `file_checksums`（postgres.go:205-211）

```sql
CREATE TABLE IF NOT EXISTS file_checksums (
    project_name TEXT NOT NULL,
    file_path TEXT NOT NULL,
    hash TEXT NOT NULL,
    indexed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (project_name, file_path)
);
CREATE INDEX idx_file_checksums_project ON file_checksums(project_name);
```

**复合主键**：同一文件路径在不同项目下可以独立 hash。
**用法**：indexer.go:119 `GetAllChecksums(projectName)` 拉全表预热内存缓存。

⚠️ **没有 hash collision 检测**：理论上 SHA-256 collision 几乎不可能（2^128 量级），不需要。

---

## 4. CRUD 方法表

| 表 | 方法 | 行号 | 备注 |
|---|---|---|---|
| tasks | `CreateTask` | L243 | INSERT |
| tasks | `UpdateTaskState` | L252 | UPDATE，终态填 completed_at |
| tasks | `GetTask` | L265 | SELECT 全字段 |
| tasks | `SavePlan` | L278 | UPDATE plan_json |
| tasks | `LoadPlan` | L286 | SELECT plan_json |
| audit_logs | `InsertAuditLog` | L311 | INSERT，append-only |
| approvals | `CreateApproval` | L335 | INSERT ON CONFLICT DO NOTHING（幂等） |
| approvals | `ResolveApproval` | L345 | UPDATE status + resolved_at |
| approvals | `GetPendingApprovals` | L354 | SELECT 全部 pending，按时间升序 |
| dynamic_tools | `SaveDynamicTool` | L394 | INSERT ON CONFLICT DO UPDATE（upsert） |
| dynamic_tools | `LoadDynamicTools` | L416 | SELECT 非过期 |
| dynamic_tools | `DeleteDynamicTool` | L444 | DELETE |
| file_checksums | `BatchUpsertChecksums` | L458 | tx + prepared stmt + 循环 ExecContext |
| file_checksums | `GetAllChecksums` | L489 | SELECT 全表 → map |
| file_checksums | `DeleteChecksums` | L510 | tx + prepared stmt + 循环 DELETE |
| 通用 | `Ping` | L375 | db.PingContext |
| 通用 | `DB()` | L129 | 暴露原始 \*sql.DB（toollearn / memory 用） |
| 通用 | `Close()` | L123 | defer 调用 |

⚠️ **没有 `DeleteTask` / `GetTasksBySession` / `ListAuditLogs`**：grep 整文件不存在——目前任务和审计**只能写，不能查**（除单条 GetTask）。前端要看任务列表必须自己绕过 store 或加方法。**P1**。

### 4.1 `BatchUpsertChecksums` 的事务模式（L458-486）

```go
tx, _ := s.db.BeginTx(ctx, nil)
defer tx.Rollback()                                  // 失败时回滚

stmt, _ := tx.PrepareContext(ctx, `INSERT ... ON CONFLICT ... DO UPDATE ...`)
defer stmt.Close()

for filePath, hash := range checksums {
    stmt.ExecContext(ctx, projectName, filePath, hash, now)
}

return tx.Commit()
```

**Prepared statement 复用**：N 个文件只 prepare 一次，Exec N 次——性能远高于每次都 Prepare+Exec。
**事务**：要么全部成功要么全部回滚——保证 checksums 一致性。
**defer Rollback**：Commit 成功后 Rollback 是 no-op（sql/database 库行为）。

---

## 5. JWT 撤销（auth/redis_revocation.go）

### 5.1 RedisRevocationStore（L43-62）

```go
type RedisRevocationStore struct {
    rdb    *redis.Client
    prefix string                                    // "jwt:revoked:"
}

func (s *RedisRevocationStore) Revoke(ctx, jti, ttl) error {
    return s.rdb.Set(ctx, s.prefix+jti, "1", ttl).Err()
}

func (s *RedisRevocationStore) IsRevoked(ctx, jti) bool {
    val, err := s.rdb.Exists(ctx, s.prefix+jti).Result()
    return err == nil && val > 0
}
```

**关键细节**：
- `Set` 带 TTL = token 剩余有效期 → Redis 自动过期 → 不需要手动 GC
- `Exists` 而非 `Get` —— 只看键存不存在，不读 value（更快）
- `err == nil && val > 0` 双重检查 —— Redis 错误时**不当作撤销**（fail-open）

⚠️ **fail-open 是有意的**：Redis 临时不可用时与其阻断所有用户登录，不如让 token 暂时通过验证。攻击窗口在 Redis 恢复时关闭。**这是安全权衡**。

### 5.2 JWTManagerWithRedis 装饰器（L65-97）

```go
type JWTManagerWithRedis struct {
    *JWTManager                                      // 内嵌：复用所有原方法
    redisRevoke *RedisRevocationStore
}

func (m *JWTManagerWithRedis) ValidateToken(token) (*Claims, error) {
    claims, err := m.JWTManager.ValidateToken(token)
    if err != nil { return nil, err }
    if m.redisRevoke.IsRevoked(context.Background(), claims.ID) {
        return nil, ErrTokenInvalid
    }
    return claims, nil
}

func (m *JWTManagerWithRedis) RevokeToken(ctx, jti, ttl) error {
    m.JWTManager.RevokeToken(jti)                    // 内存黑名单（L1）
    return m.redisRevoke.Revoke(ctx, jti, ttl)       // Redis（L2）
}
```

**装饰器模式而非继承**：Go 没有继承——靠**结构体内嵌**复用 `*JWTManager` 所有方法，同时通过定义同名方法**重写** ValidateToken 和 RevokeToken。

⚠️ **`ValidateToken` 用 `context.Background()`**：调用方的 request ctx 没传进来——意味着 Redis 调用不带超时，可能阻塞主请求。**P2：把 ctx 透传进 ValidateToken 签名**。

---

## 6. 实现剖析与改进方向

### 6.1 当前实现的真实利弊

**优势（验证过的）**
- ✅ 6 张表 schema 清晰且 IF NOT EXISTS 幂等
- ✅ `BatchUpsertChecksums` 用事务 + prepared stmt 高效插入
- ✅ `ON CONFLICT DO NOTHING/UPDATE` 双语义清晰（approvals 幂等创建 vs dynamic_tools upsert）
- ✅ 部分索引 `WHERE ttl IS NOT NULL` 节省索引空间
- ✅ JWT 撤销内存+Redis 双层，Redis 失败 fail-open 不影响可用性
- ✅ store 可选依赖原则 + 所有 nil-check 全 wire 链路一致

**已知风险**

| 严重度 | 问题 | 位置 | 建议 |
|---|---|---|---|
| P1 | `api_keys.last_used` 永不更新（死字段） | postgres.go:165 | 加 UPDATE 或删字段 |
| P1 | 缺 `DeleteTask / GetTasksBySession / ListAuditLogs` 等查询方法 | 全表 | 补 CRUD |
| P1 | `pgCfg.Host/Port/User/Password` 硬编码且 DSN 模式下未使用 | main.go:346 | 整理为 NewStore 或删 |
| P2 | `approvals.details` 是 TEXT 不是 JSONB | postgres.go:180 | 迁移到 JSONB（破坏性变更） |
| P2 | `ConnMaxLifetime = 5 min` 硬编码 | postgres.go:85 | 暴露配置 |
| P2 | 过期 dynamic_tools 记录永远留在表里 | postgres.go:416 | 加 cron DELETE WHERE ttl 过期 |
| P2 | 无 schema migration 工具，加列只能 ALTER IF NOT EXISTS | postgres.go:215 | schema 变复杂时切 golang-migrate |
| P2 | `JWTManagerWithRedis.ValidateToken` 用 Background ctx | redis_revocation.go:86 | 透传调用方 ctx |
| P3 | `tasks.plan_json` JSONB 无 GIN 索引 | postgres.go:144 | 业务需要按 plan 字段过滤时再加 |
| P3 | audit_logs 无归档策略（无限增长） | postgres.go:153 | 加月度分区 / 归档到对象存储 |

### 6.2 优先级修复建议

**P0（生产风险）**
- 当前没有 P0

**P1（生产质量）**
1. 补齐 tasks CRUD（DeleteTask、GetTasksBySession、ListByUser）
2. `last_used` 真实更新（在 auth middleware 校验 api_key 后异步 UPDATE）
3. 整理 main.go 的 pgCfg 死字段
4. audit_logs 列表查询方法（前端审计面板需要）

**P2（设计完善）**
5. approvals.details 迁移到 JSONB（双写期 + cutover）
6. ConnMaxLifetime 配置化
7. dynamic_tools 过期清理 cron job
8. ValidateToken ctx 透传

**P3（长期）**
9. 引入 golang-migrate（schema 变复杂后）
10. audit_logs 月度分区或归档
11. plan_json GIN 索引（按需）

---

## 7. 设计权衡

| 抉择 | 动机 |
|---|---|
| **6 张表 / `IF NOT EXISTS` 硬编码 migration** | schema 演进慢 + 工具链零依赖；坏处是复杂迁移做不了 |
| **PostgreSQL 可选**（main.go Warn 继续） | 核心能力依赖 Redis；PG 只做审计 + 跨重启状态 |
| **JWT 撤销用 Redis 不用 PG** | 每请求查一次，RTT 比一致性优先 |
| **撤销 fail-open**（Redis 错误时不阻断） | 可用性 > 安全严密性；攻击窗口随 Redis 恢复关闭 |
| **装饰器模式 JWTManagerWithRedis** | 复用原 JWTManager 的内存黑名单作为 L1 缓存 |
| **`approvals` task_id PRIMARY KEY** | 一个任务一个审批，ON CONFLICT DO NOTHING 保证幂等 |
| **`dynamic_tools` ON CONFLICT DO UPDATE** | 用户重新注册同名工具 = 更新；不是错误 |
| **`audit_logs` 无 ON DELETE CASCADE** | 审计是合规材料，不能因业务实体删除而消失 |
| **`BatchUpsertChecksums` 用 prepared stmt + tx** | 单次索引可能 thousands of files，性能与一致性必须兼顾 |
| **`tasks.plan_json` JSONB 而非 TEXT** | 未来按 plan 字段查询可以建 GIN |
| **`api_keys` 存 hash 不存明文** | 数据库泄露不导致 key 泄露 |
| **5min ConnMaxLifetime** | 经验值；防 PG 端 idle kill |

---

## 8. 后续演进

- [ ] 补齐 tasks CRUD（list / delete）
- [ ] `last_used` 实际写入
- [ ] 整理 main.go 的 pgCfg 死字段
- [ ] `approvals.details` 迁移 JSONB
- [ ] ConnMaxLifetime 配置化
- [ ] dynamic_tools 过期清理 cron
- [ ] ValidateToken ctx 透传
- [ ] golang-migrate 替换硬编码 migrations（schema 变复杂时）
- [ ] audit_logs 月度分区 / 归档对象存储
- [ ] `plan_json` GIN 索引（按需）
- [ ] PG 故障双写降级（写 disk-backed queue → 后补回放）
- [ ] PG 换 CockroachDB（多区域 HA）—— 谨慎评估，多数场景不需要

---

## 9. 设计教训

1. **持久化分层是设计第一步**：Redis 存热的、丢得起的（session、限流计数）；PG 存冷的、丢不起的（审计、状态、key）；磁盘存大的（embedding、文件）。三层混淆是最容易让人后悔的设计错误。

2. **可选依赖 + nil check**：PG 失败不让整个 agent 死掉，因为核心能力（chat / RAG）不依赖 PG。这是 graceful degradation 的样板——但代价是每个调用方都要 `if pgStore != nil`。值得，因为它把生产可用性提高了一个数量级。

3. **硬编码 migration 在 schema 演进慢时合理**：6 张表花了几个月长出来，每次 `IF NOT EXISTS` 都幂等，不需要 down migration。但**schema 复杂度增长就该切换工具**——硬编码的甜蜜期约 10 张表。

4. **prepared statement + tx 是批量操作的标配**：BatchUpsertChecksums 一次性插 1000 行，prepared stmt 节省了 999 次 plan + parse 开销。事务保证原子。**别裸 INSERT 在循环里**——慢且不一致。

5. **JSONB 比 TEXT 强但要选对**：`audit_logs.details` 是 JSONB（查询能力）；`approvals.details` 是 TEXT（历史遗留，限制了将来按字段过滤）。一开始就用 JSONB 是更好的默认值——TEXT 只在确定永远不会按字段过滤时才用。

6. **装饰器模式比继承友好**：Go 没继承，但**结构体内嵌 + 同名方法重写**实现等价能力。JWTManagerWithRedis 把"内存撤销名单"作为 L1 复用，"Redis 撤销名单"作为 L2 装饰——干净且可测试。

7. **fail-open vs fail-closed 是安全设计抉择**：Redis 撤销失败时 `IsRevoked` 返回 false（fail-open）——可用性优先。这种选择是有意的，但**必须在文档和测试里说清楚**——否则下个工程师可能在不知情情况下改成 fail-closed 引发可用性事故。

8. **死字段 / 死表 / 死配置必须定期清理**：`api_keys.last_used` 永不更新、`pgCfg.Host` 硬编码但 DSN 模式下不用——这些会让新维护者反复怀疑"是不是漏接了什么"。**所有声明未读的字段都该 TODO 或删除**。

---

下一篇：[`17_api.md`](17_api.md) —— Gin HTTP/WS 服务端：路由树、中间件链、SSE 流、WebSocket 升级、JWT/APIKey 双轨认证、`cmd/agent/main.go` 的依赖注入全景。
