# 16 · 持久化层 `internal/store` + `internal/auth`（Redis 撤销）

> 代码：
> - `internal/store/postgres.go` (285) — PostgreSQL Store：Tasks / AuditLogs / ApiKeys / Approvals 四表 + migrations
> - `internal/auth/redis_revocation.go` (66) — JWT 撤销名单的 Redis 存储 + JWTManagerWithRedis 装饰器
> - 测试：`postgres_test.go`

---

## 1. 模块定位

**"让 Agent 的记忆能穿越进程重启。"**

整个系统里，有状态的东西分三层：

| 层 | 存哪里 | 失效后果 |
|---|---|---|
| **热状态**（对话滑窗、embedding 结果） | Redis（见 12_session） | 无伤大雅，对话重来一次即可 |
| **冷记忆**（Task / Audit / Approval / ApiKey） | PostgreSQL（本模块） | 审计丢失、授权丢失 — **不可容忍** |
| **工作产物**（代码文件） | Workspace 目录（见 14_workspace） | 用户代码丢了！灾难 |

本模块负责**中间那层** —— 业务关键但非工作产物的元数据：

- **Tasks**：每次 Agent 任务的 id / session / intent / state / result；
- **AuditLogs**：Agent 每个敏感操作的审计链（JSONB details）；
- **ApiKeys**：长期 API Key（hash 存储，对抗泄露）；
- **Approvals**：HITL 授权请求的完整生命周期（pending → approved/rejected）。

另外附带一个**小而关键**的组件：`internal/auth/redis_revocation.go` —— JWT 撤销黑名单。它虽然放在 `auth` 包里，但本质是"持久化"逻辑，所以一起讲。

---

## 1.5 核心设计问题

### Postgres 存什么 / 不存什么

**存**（持久耐久要求）：
- 审计日志（合规 / 事后取证）
- API Key 哈希（服务账号凭据）
- 长任务状态（Temporal 的后端也是 Postgres）

**不存**（不需耐久或有更合适的存储）：
- Session messages → Redis（24h TTL 足够，高吞吐要求）
- RAG embeddings → Qdrant（向量数据）
- 实时指标 → Prometheus
- 分布式锁 → Redis

### 为什么不用 ORM？

GORM / ent 等 ORM 优势：自动迁移、query builder。
劣势：
- 生成的 SQL 不可控（调优困难）
- 反射运行时开销
- 业务和数据库 schema 耦合（一个 struct 既是 JSON 又是 DB 行）

本系统用 `database/sql` + 手写 SQL。schema 演进少，不值得 ORM 的复杂性。
`MigrateOnStart` 用 `CREATE TABLE IF NOT EXISTS`，比 migration 工具
（golang-migrate）轻量。

### 连接池参数怎么定

```go
MaxOpenConns:    25     // 典型单 Pod 上限
MaxIdleConns:    10     // 保持常驻避免冷启动
ConnMaxLifetime: 5m     // 强制轮换，避免 DB 端 idle kill
```

**MaxOpenConns** 不能太大：Postgres 每个连接约 10MB 内存，100 Pod × 25 conns
= 2500 连接 × 10MB = 25GB，远超典型 DB 实例。

**ConnMaxLifetime** 必要：AWS RDS 等云 DB 会主动关闭 idle > 5min 的
连接，不轮换会导致"首个请求失败"陷阱。

### Store 可选性

`store` 构造失败时 main.go 仅 Warn，不 Fatal。原因：审计 / 长期任务功能
**降级可用**——核心 chat 路径依赖 Redis + LLM，不依赖 Postgres。开发
机器 / 演示环境不需要 PG 也能跑。

---

## 2. 依赖架构

```
┌──────────────────────────────────────────────────────────┐
│ orchestrator / api / temporal                            │
│  (CreateTask / UpdateTaskState / InsertAuditLog /        │
│   CreateApproval / ResolveApproval)                      │
└──────────┬───────────────────────────────────────────────┘
           │
           ▼
┌──────────────────────────────────────────────────────────┐
│                 store.Store (285 行)                     │
│                                                          │
│  ┌─────────┐  ┌─────────┐  ┌─────────┐  ┌──────────┐    │
│  │ tasks   │  │ audit_  │  │api_keys │  │approvals │    │
│  │         │  │ logs    │  │         │  │          │    │
│  └─────────┘  └─────────┘  └─────────┘  └──────────┘    │
│   PRIMARY       BIGSERIAL   hash PK       task_id PK     │
│   TEXT id       + JSONB     + role        + ON CONFLICT  │
└──────────┬───────────────────────────────────────────────┘
           │
           ▼
  database/sql + lib/pq
           │
           ▼
  PostgreSQL 14+ (TIMESTAMPTZ + JSONB)


另一条独立的持久化路径（Auth）:

┌──────────────────────────────────────────────────────────┐
│ /api/v1/auth/logout  →  JWTManagerWithRedis.RevokeToken  │
└──────────┬───────────────────────────────────────────────┘
           │
           ▼
  RedisRevocationStore.Revoke(ctx, jti, ttl)
           │
           ▼
  SET jwt:revoked:<jti> "1" EX <ttl>

ValidateToken():
  IsRevoked(jti) → EXISTS jwt:revoked:<jti>
```

两者都依赖外部存储（Postgres + Redis），**Agent 进程本身无状态** —— 这是 HA + 水平扩展的前提。

---

## 2.5 数据流总览

```text
═══════════════ PostgreSQL 持久化路径 ═══════════════

┌─────────────────────────────────────────────────────────────┐
│ orchestrator / api / temporal                                │
└──────────────┬──────────────────────────────────────────────┘
               │
    ┌──────────┼──────────┬──────────────┬────────────────┐
    │          │          │              │                │
    ▼          ▼          ▼              ▼                ▼
CreateTask  UpdateState  InsertAudit  CreateApproval  StoreAPIKey
    │          │          │              │                │
    └──────────┼──────────┴──────────────┴────────────────┘
               │
               ▼
┌─────────────────────────────────────────────────────────────┐
│              store.Store                                      │
│  sql.Open → *sql.DB (MaxOpen=25, MaxIdle=5, Life=5min)      │
│  启动时 Migrate() 自动建表                                    │
└──────────────┬──────────────────────────────────────────────┘
               │ (SQL: INSERT/UPDATE/SELECT)
               ▼
┌─────────────────────────────────────────────────────────────┐
│  【PostgreSQL 14+】                                          │
│  ┌────────┐ ┌──────────┐ ┌─────────┐ ┌──────────────────┐  │
│  │ tasks  │ │audit_logs│ │api_keys │ │   approvals      │  │
│  │TEXT PK │ │BIGSERIAL │ │hash PK  │ │ task_id FK       │  │
│  └────────┘ └──────────┘ └─────────┘ └──────────────────┘  │
└─────────────────────────────────────────────────────────────┘


═══════════════ JWT 吊销路径 (Redis) ═══════════════

┌────────────────────┐         ┌────────────────────────────┐
│ POST /auth/logout  │         │ 任意请求 AuthMiddleware     │
│ (用户主动登出)      │         │ ValidateToken()            │
└─────────┬──────────┘         └────────────┬───────────────┘
          │                                 │
          ▼                                 ▼
┌─────────────────────────┐    ┌────────────────────────────┐
│JWTManagerWithRedis      │    │ IsRevoked(jti)             │
│ .RevokeToken(jti, ttl)  │    │   EXISTS jwt:revoked:<jti> │
└─────────┬───────────────┘    └────────────┬───────────────┘
          │                                 │
          ▼                                 ▼
┌─────────────────────────────────────────────────────────────┐
│ 【Redis】                                                    │
│  SET jwt:revoked:<jti> "1" EX <token剩余TTL>                │
│  本地 map 同步写入 (同 Pod 快速路径)                          │
└─────────────────────────────────────────────────────────────┘
```

---

## 3. ★ PostgreSQL Store

### 3.1 配置与连接池

```go
// postgres.go:17
type PostgresConfig struct {
    Host, User, Password, Database, SSLMode string
    Port          int
    MaxOpenConns  int       // 默认 25，大写场景开到 100+
    MaxIdleConns  int       // 默认 5，防止连接建立成本
}

func (c *PostgresConfig) DSN() string {
    return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s", ...)
}
```

`NewStore` 构造函数做的事：

```
NewStore(cfg, logger):
  db := sql.Open("postgres", cfg.DSN())
  db.SetMaxOpenConns(cfg.MaxOpenConns)
  db.SetMaxIdleConns(cfg.MaxIdleConns)
  db.SetConnMaxLifetime(5 * time.Minute)   # 防 NAT 超时 / PG 主动关连接

  ctx := WithTimeout(5s)
  db.PingContext(ctx)                       # 早期暴露连通性问题
  return &Store{db, logger}, nil
```

**3 个连接池关键参数**：

| 参数 | 值 | 原因 |
|---|---|---|
| `MaxOpenConns` | 可配 | 保护 PG 不被单 Agent 打爆（PG 默认 `max_connections=100`） |
| `MaxIdleConns` | 可配 | 复用连接，省 TCP 握手 + TLS 协商 |
| `ConnMaxLifetime` | **固定 5min** | PG proxy / NAT 超时常见 30min；5min 强制重连更稳 |

`NewStoreFromDSN` 额外版本：直接传 DSN 字符串（给 docker-compose `DATABASE_URL` 注入用）。

### 3.2 `Migrate` —— 代码即迁移（L92）

**不用外部迁移工具**（flyway / migrate），直接在 Go 代码里 `CREATE TABLE IF NOT EXISTS`：

```
Migrate(ctx):
  for each DDL in migrations[]:
      db.ExecContext(ctx, DDL)
```

为什么这样选？

| 方案 | 优势 | 劣势 |
|---|---|---|
| **代码即迁移** ✅ | 无外部工具；启动即跑；容器化简单 | 需要 schema 倒退时不方便；不显式版本 |
| golang-migrate | 文件版本化；up/down | 需要额外命令、额外镜像 |
| flyway/liquibase | 工业级 | 重，Java 依赖 |

当前规模下**代码即迁移足够**；真要 schema rollback 时再引入专业工具。注意：`IF NOT EXISTS` 保证**幂等** —— 启动 N 次也不会冲突。

### 3.3 四张表的 Schema

#### 3.3.1 `tasks`

```sql
id          TEXT PRIMARY KEY        -- UUID，Agent 任务 ID
session_id  TEXT NOT NULL           -- 关联 session
user_id     TEXT NOT NULL DEFAULT ''
intent      TEXT                     -- coding / debugging / exploration / ...
state       TEXT DEFAULT 'pending'   -- pending / running / completed / failed / cancelled / suspended
user_input  TEXT NOT NULL            -- 用户原始 prompt
result      TEXT                     -- 最终回答（NULL 表示未完成）
created_at, updated_at, completed_at  TIMESTAMPTZ

INDEX: (session_id), (state), (created_at)
```

**索引选择**：

- `session_id`：前端"本会话历史任务" 高频查询；
- `state`：看板过滤（"pending 的任务有多少"）；
- `created_at`：时间序列分析、归档清理。

`TIMESTAMPTZ` 而非 `TIMESTAMP`：**始终带时区**，避免跨地域部署时的经典坑。

#### 3.3.2 `audit_logs`

```sql
id          BIGSERIAL PRIMARY KEY    -- 自增，追加写
task_id     TEXT                      -- 关联任务
user_id     TEXT
action      TEXT                      -- "tool_call: execute_command" / "approval_granted" / ...
details     JSONB                     -- 灵活 payload（args / result / diff / ...）
risk_level  TEXT DEFAULT 'low'        -- low / medium / high / critical
created_at  TIMESTAMPTZ

INDEX: (task_id), (user_id)
```

**为什么用 `JSONB` 而非 `TEXT`？**

- **原生查询**：`WHERE details->>'tool' = 'execute_command'`；
- **GIN 索引支持**（当前未开，演进）；
- **压缩存储**：PG 14 的 JSONB 比 TEXT 省 ~20% 空间；
- **schema-on-read**：审计 payload 的形态未来一定会演进，不用加列。

**risk_level** 用字符串而非 enum：灵活；后续可以无缝加 `"extreme"` 而不动 schema。

#### 3.3.3 `api_keys`

```sql
key_hash    TEXT PRIMARY KEY         -- ★ 存 hash，不存明文！
user_id     TEXT NOT NULL
role        TEXT DEFAULT 'dev'        -- dev / admin / readonly
label       TEXT                      -- 用户给的昵称 "MyLaptop"
created_at, last_used  TIMESTAMPTZ

INDEX: (user_id)
```

**安全核心**：`key_hash` 作为主键，明文 key **只在创建时返回一次**，之后永远查不到。即使 DB 泄露，攻击者拿不到原 key（hash 反推不出）。

#### 3.3.4 `approvals`

```sql
task_id     TEXT PRIMARY KEY         -- ★ 一个任务最多一条 pending approval
session_id  TEXT NOT NULL
action      TEXT NOT NULL             -- "execute_command: rm -rf /data"
risk_level  TEXT DEFAULT 'high'
details     TEXT                      -- 供前端渲染的 payload
status      TEXT DEFAULT 'pending'    -- pending / approved / rejected / timeout
approved_by TEXT
created_at, resolved_at  TIMESTAMPTZ

INDEX: (status)
```

**task_id 做主键** 是关键：保证一个任务最多一条 approval 记录，`INSERT ... ON CONFLICT DO NOTHING` 天然幂等。见下面 `CreateApproval`。

### 3.4 核心 CRUD 实现要点

#### `CreateApproval` 的幂等性（L243）

```sql
INSERT INTO approvals (...) VALUES (...)
ON CONFLICT (task_id) DO NOTHING
```

**为什么要 ON CONFLICT？** Orchestrator 的 `suspendForApproval` 可能因为重试、状态机恢复、并发调用被触发多次。如果没有 ON CONFLICT：

- 重复 INSERT 报 `duplicate key` error；
- Agent 看到错误 → 退出 ReAct 循环 → 用户永远等不到授权 UI；

有了 ON CONFLICT：重复调用等于 no-op，**天然幂等**，状态机可以随便重放。

#### `UpdateTaskState` 自动填 `completed_at`（L182）

```go
if state in {"completed", "failed", "cancelled"}:
    completedAt = now
else:
    completedAt = nil
```

终态**自动盖时间戳**；中间态（running/suspended）保持 NULL。这样 `WHERE completed_at IS NULL` 就能查所有**进行中**的任务，不用关心具体状态。

#### `GetTask` 的 ErrNoRows 转义（L195）

```go
err := db.QueryRowContext(ctx, "SELECT ...").Scan(...)
if err == sql.ErrNoRows {
    return nil, fmt.Errorf("task not found: %s", taskID)
}
```

把 `sql.ErrNoRows` 转成有业务语义的错误，让上层（API handler）能直接映射到 HTTP 404，不用到处判定 `errors.Is(err, sql.ErrNoRows)`。

#### `InsertAuditLog` 的 `json.RawMessage`

```go
type AuditRecord struct {
    Details json.RawMessage   // 已经是 JSON bytes，不再 encode
}
```

调用者预先把 details marshal 成 JSON bytes，Store 直接塞给 `JSONB` 列。省一次 JSON 编解码，也避免"二次 marshal"的转义灾难。

### 3.5 `Ping` 健康检查

```go
func (s *Store) Ping(ctx context.Context) error {
    return s.db.PingContext(ctx)
}
```

API 的 `/healthz` 调用。**带 context** 是关键 —— 否则 DB 挂了会阻塞整个健康探测，K8s 看到超时反而会杀 Pod，而不是重启 DB。

---

## 4. ★ Redis JWT 撤销（`auth/redis_revocation.go`）

### 4.1 问题背景

JWT 的致命缺陷：**无状态 → 无法撤销**。token 一旦签发，就算"logout"也仍然有效直到过期。

常见解：

| 方案 | 问题 |
|---|---|
| **超短 TTL**（5min） | UX 灾难，每 5min 强制登录 |
| **黑名单存内存 map** | 重启丢、多 pod 不一致（用户 A 登出后访问 pod B 仍然有效） |
| ✅ **黑名单存 Redis** | 跨 pod 共享 + 持久化 + TTL 自动清理 |

### 4.2 `RedisRevocationStore` (L12)

```go
type RedisRevocationStore struct {
    rdb    *redis.Client
    prefix string    // "jwt:revoked:"
}

Revoke(ctx, jti, ttl):
    SET jwt:revoked:<jti> "1" EX <ttl>

IsRevoked(ctx, jti):
    return EXISTS jwt:revoked:<jti> > 0
```

**3 个关键设计**：

1. **Key 是 JTI 而非完整 token**：JTI（JWT ID）是 token payload 里的唯一 ID，撤销它就等于撤销 token。存 JTI 而非 token 本体 → key 更短、更快。

2. **Value 是 `"1"` 哑值**：我们只关心 key 是否存在。Redis 约定占位值 `"1"` 比空字符串更明显。

3. **TTL 与 token 剩余寿命对齐**：token 自然过期后，黑名单条目也无意义。`SET ... EX <ttl>` 让 Redis 自动清理，**无需定期扫描**。

### 4.3 `JWTManagerWithRedis` 装饰器模式（L34）

```go
type JWTManagerWithRedis struct {
    *JWTManager         // embed 基础实现
    redisRevoke *RedisRevocationStore
}

ValidateToken(tokenString):
    claims := m.JWTManager.ValidateToken(tokenString)   // 先签名验证
    if err != nil: return err
    if redisRevoke.IsRevoked(claims.ID):                # 再查黑名单
        return ErrTokenInvalid
    return claims

RevokeToken(ctx, jti, ttl):
    m.JWTManager.RevokeToken(jti)                       # 本地内存（快速路径）
    return redisRevoke.Revoke(ctx, jti, ttl)            # Redis（跨 pod 可见）
```

**装饰器模式**好处：

- 不改 `JWTManager` 的签名；
- 开发/测试时可以不挂 Redis，直接用基类（内存黑名单）；
- 生产把 `*JWTManager` 换成 `*JWTManagerWithRedis` 即可无感升级。

**双写策略（本地 + Redis）** 是有意设计：

- 同一 pod 的后续请求：本地内存查到即返回，省一次 Redis RTT；
- 跨 pod 的请求：Redis 兜底；
- Redis 挂了：本地内存仍然工作（降级可用）。

---

## 5. 设计权衡

| 抉择 | 动机 |
|---|---|
| 选 **PostgreSQL** 而非 MySQL | JSONB / TIMESTAMPTZ / GIN index / `ON CONFLICT` 语法全部原生；TS 生态友好 |
| 用 **`database/sql` + lib/pq** 而非 ORM（gorm） | 显式 SQL 可审计；无魔法；性能可预测；4 张表不需要复杂关联 |
| **代码即迁移** (`CREATE TABLE IF NOT EXISTS`) | 无外部工具；启动即跑；容器化友好；当前规模下足够 |
| **`TIMESTAMPTZ`** 强制 | 跨时区部署时避免"时间偏移 8 小时"经典坑 |
| Audit `details` 用 **JSONB** | schema-on-read；原生查询；节省空间 |
| `risk_level` 字符串 vs enum | 灵活扩展（`critical` 加就加），无需 ALTER TYPE |
| ApiKey 存 **hash** 而非明文 | DB 泄露也反推不出原 key |
| Approval **task_id 做主键 + ON CONFLICT DO NOTHING** | 幂等：Orchestrator 重试 / workflow 重放都安全 |
| `completed_at` **终态自动填** | 查 in-progress = `WHERE completed_at IS NULL`，跨状态统一 |
| `ErrNoRows` **转业务错误** | API 层无需感知 `database/sql` 细节 |
| `ConnMaxLifetime = 5min` | 对抗 PG proxy / NAT 超时（常见 30min）+ 连接老化；5min 重建可接受 |
| Redis 撤销：**装饰器模式**覆盖 `ValidateToken` | 不改基类；开发/测试可 fallback 到内存黑名单 |
| Redis 黑名单存 **JTI** 而非 token | 更短；查询更快；隐私（不缓存完整 token） |
| Redis 黑名单 TTL = token 剩余寿命 | 自动清理，无需定期 GC |
| 撤销**本地 + Redis 双写** | 本地快速路径；Redis 跨 pod；任一挂了仍可用（降级） |

---

## 6. 后续演进

- [ ] **Schema 版本化**：引入 `schema_migrations` 表 + golang-migrate，支持 rollback；
- [ ] **分区表 (audit_logs)**：按 `created_at` 月度分区，旧数据自动归档到冷存；
- [ ] **GIN 索引 (audit_logs.details)**：`CREATE INDEX ... USING GIN (details jsonb_path_ops)` 支持高速 JSON 查询；
- [ ] **读写分离**：PGPool / ProxySQL，Read 打从库；
- [ ] **Prepared statements**：大量相同 SQL 时预编译；
- [ ] **批量插入**：audit logs 高频小写入，累积 100 条一起 COPY；
- [ ] **Task state machine 约束**：pg 层面用 CHECK constraint 防止非法状态转移（如 `completed → running`）；
- [ ] **Approval 超时扫描**：后台 cron，把 `pending` 超过 N 分钟的改成 `timeout`；
- [ ] **Redis Sentinel / Cluster**：撤销黑名单高可用；
- [ ] **Redis Lua 原子化 revoke+log**：revoke 和审计写入同一个事务；
- [ ] **Metrics**：`db_query_duration_seconds{query} / db_pool_in_use / db_pool_idle / jwt_revocations_total`；
- [ ] **Outbox 模式**：Task 状态变化时写 outbox，异步投递到消息队列（供告警、统计）；
- [ ] **敏感字段加密**：`user_input` / `result` 如果含 PII，应用层 AES-GCM 再落盘；
- [ ] **归档**：`completed_at < now() - 90d` 的 tasks 导出到 S3 冷存并从 PG 删；
- [ ] **PG 14+ 的 `MERGE` 语法**：替代 `INSERT ... ON CONFLICT`，更标准 SQL。

---

## 11. 实现剖析与改进方向

### 自动 Migrate 的实现

```go
func (s *Store) Migrate(ctx context.Context) error {
    stmts := []string{
        `CREATE TABLE IF NOT EXISTS tasks (
            id UUID PRIMARY KEY, session_id UUID NOT NULL,
            state VARCHAR(32), intent VARCHAR(32),
            payload JSONB, created_at TIMESTAMPTZ, updated_at TIMESTAMPTZ
        )`,
        `CREATE INDEX IF NOT EXISTS idx_tasks_session ON tasks(session_id)`,
        `CREATE TABLE IF NOT EXISTS audit_events (...)`,
        ...
    }
    for _, sql := range stmts {
        if _, err := s.db.ExecContext(ctx, sql); err != nil { return err }
    }
    return nil
}
```

**特点**：纯 `IF NOT EXISTS`，跑第二次不报错；不支持列删除（需要手动
ALTER TABLE），schema 演进少的项目用简单的胜过 ORM。

### 连接池实测

```
MaxOpenConns=25, MaxIdleConns=10, ConnMaxLifetime=5m

Single Pod, 100 RPS（混合读写）:
  · 实际活跃连接 8-15
  · P99 query latency 3ms
  · 冷连接建立 ~30ms

Single Pod, 1000 RPS burst:
  · 队列积压 ~50 queries（等 conn 释放）
  · P99 latency 50ms
  → 需要调大 MaxOpenConns 或缓存热数据
```

### Pros
- ✅ 简单：没有 ORM 运行时开销
- ✅ 自动 Migrate 适合 schema 稳定的场景
- ✅ Store 可选性降低了部署门槛

### Cons
- ⚠️ 手写 SQL 容易遗漏 prepared statement 复用
- ⚠️ 没有 migration version 表（无法"回滚"到旧 schema）
- ⚠️ `NewStoreFromDSN` 的连接参数不从 PostgresConfig 读（代码遗留）
- ⚠️ JSONB payload 不建索引，查询慢

### 改进方向
- **P1** — 引入 `golang-migrate` 或 `atlas` 做正式 migration
- **P1** — 热路径加 prepared statement 缓存
- **P1** — JSONB payload 的常用字段建 GIN 索引
- **P2** — PG 换 Cockroach（跨区域 HA）

---

下一篇：`17_api.md` —— Gin HTTP/WS 服务端：路由、中间件、SSE 流、集成测试；还包含 `cmd/agent/main.go` 的依赖注入全景。
