# 18. Auth & Security 安全栈

> **范围**：`internal/auth/` (JWT + APIKey + RBAC + 限流 + Redis 吊销) + `internal/security/` (HMAC webhook + Egress SSRF 防御)
> **物理路径**：
> - `internal/auth/jwt.go` (400 行) — JWTManager / APIKeyStore / AuthMiddleware / RequireRole
> - `internal/auth/ratelimit.go` (150 行) — 进程内 token-bucket
> - `internal/auth/redis_ratelimit.go` (161 行) — Redis 固定窗口（Lua 原子）
> - `internal/auth/redis_revocation.go` (97 行) — JWT 吊销黑名单
> - `internal/security/hmac.go` (220 行) — HMAC-SHA256 webhook 验签 + SigningTransport
> - `internal/security/egress.go` (398 行) — EgressPolicy / EgressValidator / 两层 SSRF
> - 装配点：`cmd/agent/main.go` L149-162 (egress) + L381-413 (auth)
> - 测试：`jwt_test.go` (274) / `redis_ratelimit_test.go` (141) / `hmac_test.go` (172) / `egress_test.go` (342)

> **注**：审计模块 `internal/audit/logger.go` 已并入 `19_observability.md`（Metrics + Tracing + Audit 一起讨论），本篇专注鉴权与南北向边界安全。

---

## 1. 模块定位

Auth & Security 是 Agent 的 **南北向边界守卫**。北向是 HTTP/WS 入口（17_api），南向是出向 HTTP/MCP/沙箱外联。两个包合起来回答四个问题：

| 问题 | 答案 | 实现 |
|---|---|---|
| 你是谁？ | JWT Bearer / X-API-Key | `auth.AuthMiddleware` |
| 你能做什么？ | RBAC 角色（admin/dev/readonly/service） | `auth.RequireRole` |
| 你能调多快？ | per-user/per-IP token bucket（双轨：进程内 + Redis 共享） | `auth.PerUserRateLimiter` / `RedisRateLimiter` |
| 沙箱/Agent 能访问哪些外部地址？ | CIDR 白/黑名单 + 强制元数据端点拦截 | `security.EgressValidator` |

加上两个补丁：
- **JWT 吊销** —— `redis_revocation.go` 解决 JWT 一旦签发无法主动失效的痛点。
- **HMAC Webhook** —— `security.HMACVerifier` 防止 MCP server / 外部回调被中间人篡改。

> **重要：本模块不负责秘密泄露过滤**。content scrub（密码/密钥脱敏）由 orchestrator 的 `containsSensitiveContent` + audit 模块负责，不在 auth/security 包内。

---

## 2. 设计哲学

### 2.1 无状态优先（JWT > Session）

Agent 是水平扩容架构，任意请求落到任意 Pod。如果用 Session，每个请求都要打一次 Redis；JWT 把身份签进 Token 里，Pod 本地验签即可，零外部依赖。代价是吊销机制要单独建（见 §5.9）。

### 2.2 双层防御（Defense in Depth）

每个边界至少两层验证：

| 层 | 作用 | 例子 |
|---|---|---|
| L1 应用层 | 快速、廉价的初筛 | `EgressTransport` 看 URL.Host |
| L2 系统层 | DNS 解析后/连接前的真正拦截 | `Dialer.Control` 看解析出的 IP |

L1 单层会被 DNS Rebinding 击穿；L2 单层会浪费 DNS RTT。两层一起才完备。
HMAC 同理：`signature` 单签不防重放，必须 `signature(timestamp + body)`。

### 2.3 Fail-Open vs Fail-Close 的明确取舍

| 组件 | 策略 | 理由 |
|---|---|---|
| `RedisRateLimiter.Allow` | **fail-open** | Redis 抖动时宁可放过流量也不要 502 整个 Agent |
| `JWTManagerWithRedis.IsRevoked` | **fail-open**（Redis 错误时不拦） | 同上 |
| `EgressValidator.IsAllowed` | **fail-close** | 误放等于 SSRF，比误拦严重得多 |
| `HMACVerifier.GinMiddleware` 缺 timestamp | **fail-close**（最近修复） | 之前的"缺则跳过"是 P0 漏洞 |

这个取舍是 Agent 安全模型的核心：**可用性优于一致性，但安全优于可用性**。

### 2.4 时间敏感的常量时间（Timing-Safe）

- `APIKeyStore.Validate`：遍历**全部**条目，**永不 break**，`subtle.ConstantTimeCompare` 比对——攻击者无法通过响应时间推断 key 是否存在或在哪。
- `HMACVerifier.VerifySignature`：`hmac.Equal` 而非 `bytes.Equal`。

代价：N 个 API key 时验证耗时 O(N)。N 大到性能问题前先看 §7 演进。

### 2.5 边界依赖最小化

Auth 包**不引入 Gin 之外的 HTTP 框架依赖**；Security 包**只依赖标准库 net + net/http**。这两个包要能被未来切到 echo/fiber 时极小代价迁移。

---

## 3. 依赖架构

```
                ┌────────── cmd/agent/main.go ──────────┐
                │  L149-162  egress validator + HTTPClient│
                │  L381-413  jwtMgr + apiKeyStore        │
                └──────┬─────────────────────┬───────────┘
                       │                     │
              ┌────────▼──────────┐  ┌───────▼────────┐
              │ internal/security │  │ internal/auth  │
              │ ─────────────     │  │ ─────────────  │
              │ EgressPolicy      │  │ JWTConfig      │
              │ EgressValidator   │  │ JWTManager     │
              │ EgressTransport ──┼──┤ APIKeyStore    │
              │ SigningTransport  │  │ AuthMiddleware │
              │ HMACVerifier      │  │ RequireRole    │
              └─────┬─────────────┘  │ PerUserRL      │
                    │                │ RedisRL        │
                    │                │ RedisRevocation│
                    │                └─────┬──────────┘
                    │                      │
                    ▼                      ▼
       Egress HTTP Client     Gin Middleware Chain (17_api)
         (LLM / MCP)              ↓ AuthMiddleware
                                  ↓ RequireRole
                                  ↓ RedisRateLimiter (fallback PerUserRL)
                                  → Handler
```

**外部依赖（最小集合）**：
- `github.com/golang-jwt/jwt/v5` — RFC 7519 JWT
- `github.com/redis/go-redis/v9` — 限流 + 吊销
- `github.com/gin-gonic/gin` — Middleware glue
- 标准库 `crypto/hmac` / `crypto/sha256` / `crypto/rand` / `crypto/subtle`

**反向依赖**（谁用 auth/security）：
- `internal/api/router.go` — 所有 Middleware 装配
- `cmd/agent/main.go` — 顶层 DI
- `internal/llm/client.go` — 注入 egressHTTPClient
- `internal/mcp/gateway.go` — 注入 egressHTTPClient

> **不依赖 orchestrator/rag/store**。包是叶子节点，便于单测。

---

## 4. 数据流总览

### 4.1 入向请求鉴权流（Northbound）

```
HTTP Request
    │
    ▼
1. rateLimit (Redis or in-mem)         ← router.go:187-189
    │   bucket = user_id > apikey hash > client_ip
    │   over limit → 429 + Retry-After
    ▼
2. recovery / requestID / tracing / metrics / logging / CORS
    │
    ▼  (only if route in authGroup)
3. AuthMiddleware                       ← jwt.go:312
    │   X-API-Key first → APIKeyStore.Validate (constant-time)
    │   else Authorization: Bearer <jwt> → JWTManager.ValidateToken
    │   缺失/失败 → 401
    │   成功 → c.Set("auth_claims", *Claims)
    ▼
4. RequireRole(roles...)                ← jwt.go:355
    │   claims.Role == Admin → 自动通过
    │   else claims.Role 必须在 roles 集合中
    │   不通过 → 403 + 返回 required_roles
    ▼
5. Handler (17_api)
```

### 4.2 JWT 验签内部步骤

```
ValidateToken(tokenString)              ← jwt.go:186
    │
    ▼
jwt.ParseWithClaims
    │   sign method 必须 HMAC（防 alg=none 攻击）
    │   exp 过期 → ErrTokenExpired
    │   签名不匹配 → ErrTokenInvalid
    ▼
检查内存吊销 map[jti]→time.Time         ← jwt.go:206
    │   命中 → ErrTokenInvalid
    ▼
（仅 JWTManagerWithRedis）
检查 Redis SISMEMBER jwt:revoked:<jti>  ← redis_revocation.go:79
    │   命中 → ErrTokenInvalid
    ▼
return *Claims
```

### 4.3 Webhook 入站验签（MCP callback / 外部触发）

```
POST /webhooks/...
    │
    ▼
HMACVerifier.GinMiddleware              ← hmac.go:104
    │
    ▼ Step 1: X-Signature-256 header present?
    │   missing → 401
    │
    ▼ Step 2: X-Timestamp present?      ← hmac.go:124 (REQUIRED — 最近修复)
    │   missing → 401（缺 timestamp 是协议违规）
    │   parse RFC3339 失败 → 400
    │   |age| > 5min → 401（防重放 + 防时钟伪造）
    │
    ▼ Step 3: 读 body（限 1MB，防 DoS）
    │
    ▼ Step 4: VerifySignature(body, sig, timestamp)
    │   computeHMAC = HMAC-SHA256(secret, timestamp + "\n" + body)
    │   hmac.Equal (constant-time)
    │   不匹配 → 403
    │
    ▼
Handler
```

### 4.4 出向 HTTP 请求 SSRF 防御（Southbound）

```
LLM/MCP HTTP client.Do(req)
    │
    ▼ EgressTransport.RoundTrip          ← egress.go:309 (L1 URL 层)
    │   解析 req.URL.Host + port
    │   IsAllowed(host, port)？
    │   ├─ host 是主机名（非 IP）+ deny-default → 拒绝
    │   └─ host 是 IP → 检查黑/白 CIDR
    │   不允许 → ErrEgressDenied
    │
    ▼ Base transport.RoundTrip
    │
    ▼ Dialer.DialContext
    │
    ▼ DNS resolver → IP
    │
    ▼ Dialer.Control(network, "<IP>:port")  ← egress.go:363 (L2 IP 层)
    │   net.SplitHostPort + strconv.Atoi
    │   IsAllowed(IP, port)？
    │   ├─ 命中 BlockedCIDRs（169.254.169.254 等） → 拒绝
    │   └─ deny-default 且不在 AllowedCIDRs → 拒绝
    │   不允许 → ErrEgressDenied
    │
    ▼ syscall.connect(2)
```

L1 拦截 90% 攻击，但对手控制 DNS 时仍能让 `safe.com → 169.254.169.254`。L2 在 connect(2) 之前看实际 IP，是真正的 SSRF 防线。

### 4.5 出向 HMAC 签名（Agent → MCP server）

```
SigningTransport.RoundTrip               ← hmac.go:201
    │
    ▼ 读取 req.Body 全量到内存
    │
    ▼ timestamp = time.Now().UTC().Format(RFC3339)
    │
    ▼ signature = "sha256=" + HMAC(secret, ts + "\n" + body)
    │
    ▼ Header.Set("X-Signature-256", signature)
    ▼ Header.Set("X-Timestamp", timestamp)
    │
    ▼ Base.RoundTrip
```

注意 `SigningTransport` 不强制要求外层叠 EgressTransport——但生产环境装配时**应当**叠加。

---

## 5. 实现细节

### 5.1 Claims 结构 vs `_principles.go` 文档的偏离

`_principles.go:18` 注释写：

```
· roles : []string (admin / dev / readonly / service)
```

但 `jwt.go:79-85` 的实际定义：

```go
type Claims struct {
    jwt.RegisteredClaims
    UserID   string `json:"user_id"`
    TenantID string `json:"tenant_id,omitempty"`
    Role     Role   `json:"role"`     // ← 单字段，非切片
    Email    string `json:"email,omitempty"`
}
```

**结论**：代码是 `Role` 单字段，文档是 `roles[]`。`GenerateToken(userID, tenantID, role Role, email)` 签名也是单角色。
**修复方向**（任选其一）：
- A) 改代码：`Role []Role` + JSON `roles`，迁移老 token。
- B) 改文档：把 `_principles.go` 注释改成 `role: string`。

**当前建议**：保持代码现状（B 方案），多角色用"组合角色"模式（如 `admin-dev`）或上线时再迁移。**不要**为了对齐文档强行改 Claims，会破坏所有已签发 token。

### 5.2 JWT Secret 默认值的 P0 风险

`jwt.go:96-103` + `cmd/agent/main.go:407-409`:

```go
if jwtCfg.SecretKey == "" {
    jwtCfg = auth.DefaultJWTConfig()           // ← 调用 mustGenerateSecret()
    logger.Warn("using auto-generated JWT secret ...")
}
```

`mustGenerateSecret()` 每次启动随机生成 32 字节 hex。

**后果**：
- 多 Pod 部署时每个 Pod 各自有不同 secret → Pod A 签发的 token 在 Pod B 无效。
- Pod 重启 → 所有现存 token 立刻失效。
- 仅 Warn 日志提示，未走 Fatal。

**修复建议**：生产部署必须设 `CODE_AGENT_AUTH_JWT_SECRET` 环境变量。可选改进：当 `cfg.Auth.Enabled=true && jwtCfg.SecretKey==""` 时 fatal 而非 warn。

### 5.3 Refresh Token 是声明而未实现

`JWTConfig.RefreshExpiry: 7 * 24 * time.Hour` 字段存在，main.go L393-399 也解析了 `cfg.Auth.RefreshExpiry`，但：

```
$ rg -n "refresh|Refresh" internal/api/*.go | rg -v test
（无任何路由或 handler 匹配）
```

**结论**：没有 `POST /auth/refresh` endpoint。Token 15 分钟过期后必须重新登录，没法续签。
**影响**：UI 长会话需要每 15min 弹一次登录。
**修复方向**：
- 实现 `POST /auth/refresh`：验旧 token（即使过期但仍在 RefreshExpiry 内）→ 签新 token。
- 或者把 `TokenExpiry` 调到 1 小时（仍短于 RefreshExpiry）。

### 5.4 APIKeyStore 的常量时间遍历

`jwt.go:281-301`：

```go
for i := range s.entries {
    rec := &s.entries[i]
    if subtle.ConstantTimeCompare(rec.hash[:], want[:]) == 1 {
        e := rec.entry
        matched = &e
        // 注意：不 break
    }
}
return matched, matched != nil
```

**关键细节**：
- 计算量 O(N)，N 是 API key 总数。
- 不论命中位置，每次调用耗时恒定（取决于 N）。
- 即使重复命中（理论上不可能，hash collision 才会），后命中覆盖前面。

**性能考量**：N < 1000 时单次验证 < 100μs，可接受。N 大到 5000+ 应改用预排序 + Bloom filter 加常量时间分支模板，但目前没必要。

### 5.5 RequireRole 的 Admin 自动绕过

`jwt.go:368-372`:

```go
if claims.Role == RoleAdmin {
    c.Next()
    return
}
```

**含义**：`RequireRole(RoleDev)` 让 `admin` 和 `dev` 都通过，而**不是**只让 `dev`。
这是符合预期的（admin 应当能做 dev 能做的），但下游 handler 如果按 `Role == RoleDev` 做分支判断，会和这里的语义不一致。

**修复方向**：handler 内部判定权限时不要直接比对 `claims.Role`，而是用 `claims.HasRole(...)` 辅助方法（当前未实现，可加）。

### 5.6 X-API-Key 优先于 JWT 的设计

`jwt.go:315-326`：

```go
if apiKey := c.GetHeader("X-API-Key"); apiKey != "" {
    entry, ok := apiKeys.Validate(apiKey)
    if !ok {
        c.AbortWithStatusJSON(http.StatusUnauthorized, ...)
        return        // ← X-API-Key 存在但无效 → 直接拒绝，不 fallback 到 JWT
    }
    ...
    c.Next()
    return            // ← 命中 API Key 后不再检查 JWT
}
```

**关键约束**：客户端**不能**同时发 X-API-Key 和 Authorization。如果 X-API-Key 存在但错误，请求被拒绝，即使 Authorization 是合法 JWT。
**这是有意为之**：避免靠"碰运气"试出有效凭证。

### 5.7 限流双轨：进程内 vs Redis

`router.go:187-189` 的逻辑（参考 17_api §5.1）：

```go
if rdb != nil {
    rateLimiter = auth.NewRedisRateLimiter(rdb, ...)
} else {
    rateLimiter = auth.NewPerUserRateLimiter(...)
}
```

| 维度 | 进程内 `PerUserRateLimiter` | Redis `RedisRateLimiter` |
|---|---|---|
| 算法 | Token bucket（突发友好） | Fixed window（INCR + EXPIRE） |
| 跨副本共享 | 否（N 副本 = N × rate） | 是（Lua 原子） |
| Redis 故障 | 不可用（Redis 故障 = 整个 Agent 故障） | fail-open（继续放行） |
| 突发处理 | burst 字段控制 | 跨窗口边界可达 2× 配置速率 |
| CPU 开销 | 极低 | 每请求一次 EVAL RTT |
| 内存 | 每 key 一个 bucket，5min cleanup | Redis 内 TTL 自动清理 |

**为什么 Redis 路径不 fallback 到进程内**：fallback 意味着 Redis 抖动时所有副本的限流被重置成各自计数，反而放过更多流量。fail-open 是经过权衡的选择。

### 5.8 Redis 限流 Lua 脚本的原子性

`redis_ratelimit.go:63-72`：

```lua
local count = redis.call('INCR', KEYS[1])
if count == 1 then
  redis.call('EXPIRE', KEYS[1], ARGV[1])
end
if count > tonumber(ARGV[2]) then
  return 0
end
return 1
```

**为什么必须 Lua**：单独 `INCR` + `EXPIRE` 两步在两个 Pod 同时打到 `count=0` 的 key 时，可能两个 Pod 都 INCR 到 1，但只有一个执行 EXPIRE，另一个没 EXPIRE → 这个 key 永远不过期 → 此 bucket 用户永久被拒。Lua 把 INCR 和 EXPIRE 包进单线程原子段。

### 5.9 JWTManagerWithRedis 装饰器模式

`redis_revocation.go:65-91`：

```go
type JWTManagerWithRedis struct {
    *JWTManager      // 内嵌
    redisRevoke *RedisRevocationStore
}

func (m *JWTManagerWithRedis) ValidateToken(tokenString string) (*Claims, error) {
    claims, err := m.JWTManager.ValidateToken(tokenString)  // 先走内存
    if err != nil {
        return nil, err
    }
    if m.redisRevoke.IsRevoked(context.Background(), claims.ID) {  // 再查 Redis
        return nil, ErrTokenInvalid
    }
    return claims, nil
}
```

**特点**：
- 通过**结构体嵌入** + **方法重写**实现装饰。`*JWTManager` 上的其他方法（GenerateToken 等）自动继承。
- `IsRevoked` 用 `context.Background()`——**请求 context 未透传**，超时和 trace span 都丢了。

**风险**：Redis 慢响应 → 整个 HTTP 请求挂起 30s（go-redis 默认超时）而非按 gin request ctx 取消。
**修复方向**：把 `ValidateToken(ctx, tokenString)` 改成接受 context 的签名，AuthMiddleware 传 `c.Request.Context()`。需要修改接口契约，是 P2 重构。

### 5.10 HMAC：timestamp 必填（最近 P0 修复）

`hmac.go:124-136`：

```go
if v.cfg.TimestampHeader != "" && v.cfg.MaxTimestampAge > 0 {
    tsHeader := c.GetHeader(v.cfg.TimestampHeader)
    if tsHeader == "" {
        // 拒绝（之前是 silently skip — 重放保护被绕过）
        c.AbortWithStatusJSON(http.StatusUnauthorized, ...)
        return
    }
    ...
}
```

**修复前的漏洞**：攻击者抓一个合法签名的请求，删除 X-Timestamp 头然后重放——middleware 直接跳过时间检查，签名仍有效（因为旧实现的 HMAC 没把 timestamp 计入），整个重放保护失效。
**修复后**：缺 timestamp 直接 401；signature 计算把 timestamp 拼到 body 前缀，篡改 timestamp 即破坏签名。

### 5.11 HMAC：computeHMAC 的 "timestamp + \n + body"

`hmac.go:86-92`：

```go
mac.Write([]byte(timestamp))
mac.Write([]byte("\n"))
mac.Write(payload)
```

**为什么用 `\n` 分隔**：避免 length-extension 风格的歧义。例如 `ts="123" body="456"` vs `ts="12" body="3456"` 如果不加分隔符，两者 HMAC 输入相同——攻击者可以伪造任意 timestamp/body 拼接。`\n` 是简单且 RFC-safe 的分隔符。

### 5.12 Egress：DefaultEgressPolicy 的黑名单清单

`egress.go:103-110`:

```
169.254.169.254/32   ← AWS / GCP 元数据
100.100.100.200/32   ← Alibaba Cloud 元数据
10.0.0.0/8           ← 私网 A
172.16.0.0/12        ← 私网 B
192.168.0.0/16       ← 私网 C
```

**遗漏**（建议补充）：
- `fd00::/8` — IPv6 ULA
- `fc00::/7` — IPv6 私网
- `::1/128` 和 `127.0.0.0/8`（loopback）—— 当前依赖 deny-default 拦下
- OCI / 华为云的元数据端点（与 AWS 不同 IP，需调研）

### 5.13 Egress：DockerNetworkMode 与沙箱集成

`egress.go:232-244`：

```go
if v.policy.DefaultAction == "deny" && len(AllowedHosts)==0 && len(AllowedCIDRs)==0 {
    return "none"             // ← 沙箱完全隔离网络
}
return "code-agent-sandbox"   // ← 自定义网络名（外部 iptables 应用规则）
```

**当前状态**：沙箱 (05_sandbox) 默认 `NetworkMode: "none"`，与此返回值一致。**但 `code-agent-sandbox` 网络在 docker 中并未自动创建**——`GenerateIptablesRules()` 只生成字符串，**没有自动注入** iptables 规则的代码路径。

### 5.14 Egress 只接 LLM/MCP，未接沙箱

`cmd/agent/main.go:148-165` 的装配：

```go
if cfg.Security.EgressEnabled {
    ...
    egressHTTPClient = security.NewEgressHTTPClient(...)
}
llmClient, _ := llm.NewClientWithOptions(&cfg.LLM, nil, egressHTTPClient, logger)   // ← LLM
...
mcpGateway, _ := mcp.NewGateway(&cfg.MCP, egressHTTPClient, logger)                  // ← MCP
```

**重要**：沙箱 (sandbox.Manager) 不使用 egressHTTPClient。沙箱的网络隔离靠 Docker `NetworkMode: "none"`，**不**靠 Go 层 SSRF 防御。
**如果沙箱将来允许网络访问**（比如允许 LLM 工具调用外部 API），必须给沙箱也装配 egressHTTPClient。

---

## 6. 设计权衡

### 6.1 JWT 黑名单 vs 短 TTL

| 方案 | 优点 | 缺点 |
|---|---|---|
| **短 TTL（15min）** | 无中心化吊销表 | 撤销窗口最长 15min |
| **黑名单（Redis）** | 即时撤销 | 每请求一次 Redis RTT |
| **当前实现：两者都用** | 即时撤销 + 自动清理 | Redis 慢响应影响所有 API |

理由：15min 已够短，但管理员强制下线场景仍需要"立刻"生效，所以叠加 Redis 黑名单。代价是单点依赖。

### 6.2 限流策略：Token bucket vs Fixed window

为什么进程内用 token bucket、Redis 用 fixed window？

- Token bucket 在内存里维护 `lastReset` 时间戳，每次请求 lock-free 计算补充——CPU 廉价。
- Fixed window Lua 脚本只用一次 INCR + 一次 EXPIRE，网络往返一次。如果改用 token bucket 需要 GET + SET + EXPIRE 多步原子，或者用 Redis sorted set 模拟 sliding window——两个都更贵。
- Fixed window 的缺陷"边界突发可达 2×"对 HTTP 限流来说**完全可以接受**。

### 6.3 API Key vs JWT 的并存

**为什么不统一**：
- JWT：交互式用户登录，有过期、续签需求。
- API Key：CI/服务账号，长寿命、不能续签、不能交互式登录。

强行用 JWT 给 CI 意味着要给 CI 写续签逻辑或者用 100 年 TTL（等于没 TTL）。两套机制并存是 industry 标准（GitHub / Stripe 都这么干）。

### 6.4 HMAC 而不是 mTLS

HMAC 优点：
- 无需 CA / 证书轮换。
- 配置一个 secret 即可，秘钥分发用现有秘密管理（Vault / env）。

mTLS 优点：
- TLS 握手层就拒绝，对端连握手都过不了，更早。
- 不用每请求验签 CPU。

**当前选 HMAC 的理由**：MCP server 是异构生态（Python / Node / Go），mTLS 配置在每个 server 都要做一遍；HMAC 一个 secret 全搞定。生产规模上去后可以叠加 mTLS。

### 6.5 Egress 两层 vs 容器网络隔离

**理论上**：Docker `NetworkMode: "none"` + iptables 已经能挡 SSRF。
**为什么还要 Go 层两层**：
- 沙箱外的代码（如 LLM 客户端、MCP gateway）跑在 Agent 主进程里，**不受沙箱网络隔离影响**。
- 主进程的 LLM 调用经过 LLM 提供商（OpenAI / Anthropic）的 API 域名，理论上是安全的，但万一 LLM 提供商被劫持或 DNS 中毒，Egress 层是最后防线。

### 6.6 Fail-Open 的明确成本

**Redis 失效时**：
- 限流：放行所有流量。如果 Redis 失效持续 10min，Agent 在被 DDoS 时无法保护。
- JWT 吊销：被吊销的 token 在 Redis 故障期间仍能用。

**为什么仍选 fail-open**：Redis 失效本身是 SEV1 事件（session 全失效），相比之下限流和吊销退化是次要影响。如果改成 fail-close，Redis 抖动时 100% 请求 503，对用户体验是灾难性的。

监控告警可以填补这个空隙：Redis 失效 → 立即触发告警 → 人工介入。

---

## 7. 后续演进

### 7.1 短期（1-2 sprint）

| 项 | 优先级 | 描述 |
|---|---|---|
| JWT secret 启动 fatal | P0 | `auth.Enabled=true && JWTSecret==""` 时 fatal 而非 warn |
| POST /auth/refresh | P1 | 实现 refresh token endpoint |
| Egress 黑名单补 IPv6 | P1 | 补 `fd00::/8` `fc00::/7` `::1/128` `127.0.0.0/8` |
| `ValidateToken(ctx, token)` | P2 | 把 context 透传，让 Redis 调用可以被请求超时取消 |
| HMAC body size 校验前置 | P3 | 当前 1MB 限是边界，应该做 Content-Length 预检 |

### 7.2 中期

| 项 | 描述 |
|---|---|
| OIDC 集成 | 支持企业 SSO（Okta / Azure AD），用户态从外部 IdP 来 |
| Token Introspection | 替代 Redis 黑名单，用 RFC 7662 active 检查 |
| 多 secret 轮换 | 同时持有 2 个 secret，新签用 v2，旧 token 仍能 v1 验签 |
| 沙箱接 egressHTTPClient | 如果沙箱允许网络访问，必须先把 egress 接上 |
| Cilium NetworkPolicy 注入 | `GenerateIptablesRules` 转成 Cilium policy yaml |

### 7.3 长期

| 项 | 描述 |
|---|---|
| Zero Trust 网格 | mTLS + SPIFFE Workload Identity |
| 行为画像限流 | 不只是固定窗口，按用户 LLM tool 调用模式建模异常 |
| HSM / KMS 集成 | JWT signing key 放 KMS，每次签名走 KMS API |

---

## 8. 设计教训

### 8.1 缺失 timestamp 的 HMAC 是装饰品

之前实现中，若 `TimestampHeader` 配了但请求没带，代码 silently skip。攻击者抓一个合法签名包，扔掉 timestamp 头重放——签名仍 valid（旧实现 HMAC 输入也没含 timestamp），整个重放保护失效。

**教训**：**安全配置项一旦启用，就必须 fail-close**。"配置存在但缺值" 不能等同于 "未配置"。这是 hmac.go 最近修复的核心思想。

### 8.2 `Role` 单字段 vs `roles[]`：文档先行的风险

`_principles.go` 提前写了"roles 是数组"，但实际实现因为简单和 backward-compat 用了单字段。结果是：
- 新读者看文档以为支持多角色。
- 代码评审走不到这层细节。
- 任何"对齐文档"的修改会破坏现存 token。

**教训**：**`_principles.go` 这种文档源头必须和代码强一致**。审计时要发现并修正，而不是让两者长期漂移。

### 8.3 `mustGenerateSecret()` 是开发便利的反模式

设计目标：开发者本地启动 Agent 不用配 secret。
副作用：生产部署忘了配 secret 也不会失败，只在日志里 warn 一行，多 Pod 部署立刻数据面崩溃。

**教训**：开发便利的 fallback **必须在生产配置 explicit 时失败**。`cfg.Auth.Enabled=true` 已经是生产意图的信号，此时 secret 缺失应当 fatal。

### 8.4 装饰器模式 + 内嵌结构体的隐式契约

`JWTManagerWithRedis` 内嵌 `*JWTManager`，重写 `ValidateToken`。但**仍保留** `JWTManager.RevokeToken(jti)` 方法可被外部调用——这个方法只更新内存 map，**不写 Redis**。Decorator 提供的 `RevokeToken(ctx, jti, ttl)` 才同时写两边。

如果 handler 写 `m.RevokeToken("...")` 而 `m` 类型是 `*JWTManagerWithRedis`，Go 方法分发选最具体的——但**如果方法签名不同（一个有 ctx 一个没有），编译器允许两者共存**。这种重载二义性容易写错。

**教训**：装饰器模式必须**强制**重写方法签名一致或者 hide 老方法。当前实现两者签名故意不同（一个有 ctx），但这反而让调用方易错。应当：
- 方案 A：`JWTManagerWithRedis` 实现 `interface` 而不内嵌。
- 方案 B：内嵌但额外提供 `RevokeTokenSync(jti)` panic-on-call 警告。

### 8.5 SSRF 单层防御 = 没防

DNS Rebinding 是真实存在的攻击。L1 URL 层只看 `req.URL.Host`，攻击者用 allow-listed `safe.com` 但配置 DNS 返回 169.254.169.254，请求直接到元数据端点。Go 标准库的 `Dialer.Control` 是 L2 拦截的唯一干净 hook 点——connect(2) 之前能看到实际 IP。

**教训**：边界防御**默认是单层不够**。设计时先问"这一层被绕过会怎样"，再确定第二层在哪。

### 8.6 限流 fail-open 是有意识的选择，不是疏漏

Redis 故障时 fail-open 在审计时常被批评为安全漏洞。实际上是**可用性优先于安全**的明确取舍：限流的核心目的是防 DoS，但限流系统本身故障变成 DoS 自身是更严重的问题。

**教训**：每条 fail-open 路径都要在代码注释 + 文档明确说明"这是有意的，因为 X"。否则下次审计来人会把它"修复成" fail-close，业务可用性直接掉。

---

## 9. 已知缺陷一览（含来源行号）

| 编号 | 级别 | 文件 | 行 | 现象 | 修复建议 |
|---|---|---|---|---|---|
| AS-1 | P0 | `cmd/agent/main.go` | 407-409 | JWT secret 空时只 warn，多 Pod 直接崩 | 改 fatal |
| AS-2 | P1 | `auth/redis_revocation.go` | 79-91 | `ValidateToken` 内部用 `context.Background()` | 改成接受 ctx 参数 |
| AS-3 | P1 | 全包 | — | RefreshExpiry 配了但无 `/auth/refresh` 路由 | 实现 refresh endpoint |
| AS-4 | P1 | `internal/auth/_principles.go` | 18 | 注释 `roles[]` 与代码 `Role` 单字段冲突 | 改注释 |
| AS-5 | P2 | `security/egress.go` | 103-110 | DefaultEgressPolicy 缺 IPv6 私网 | 补 `fd00::/8` `fc00::/7` |
| AS-6 | P2 | `security/egress.go` | 249-278 | `GenerateIptablesRules` 仅返回字符串，无自动应用 | 接入 Cilium 或 docker network plugin |
| AS-7 | P2 | `auth/redis_ratelimit.go` | 79-95 | fail-open 缺少 metrics 暴露 | 加 `ratelimit_redis_fail_open_total` 计数 |
| AS-8 | P3 | `auth/jwt.go` | 368-372 | Admin 自动绕过 RequireRole 未在文档凸显 | 在 `_principles.go` 补一段 |

---

## 10. 测试矩阵

| 测试文件 | 覆盖范围 | 关键用例 |
|---|---|---|
| `jwt_test.go` (274 行) | JWT 签发 / 验签 / 吊销 / APIKeyStore / RequireRole | `TestJWTLifecycle` / `TestRequireRoleAdminBypass` / `TestAPIKeyConstantTime` |
| `redis_ratelimit_test.go` (141 行) | Redis 固定窗口 + Lua 原子性 | `TestConcurrentIncrement` / `TestFailOpenOnRedisError` |
| `egress_test.go` (342 行) | EgressPolicy / EgressValidator / 两层 SSRF | `TestDNSRebindingBlocked` / `TestMetadataEndpointAlwaysBlocked` |
| `hmac_test.go` (172 行) | HMAC 验签 + timestamp + body limit | `TestMissingTimestampRejected` / `TestSignatureMismatchReturns403` |

**未覆盖**：
- `JWTManagerWithRedis` 在 Redis 网络抖动时的行为（需要 chaos 测试）。
- `SigningTransport` 与 `EgressTransport` 叠加时的双层签名（手动验证过，无自动化测试）。

---

## 11. 配置示例

`configs/config.yaml` 中相关段落：

```yaml
auth:
  enabled: true
  jwt_secret: ${CODE_AGENT_AUTH_JWT_SECRET}    # 32 字节 hex，必填生产
  jwt_issuer: "code-agent"
  token_expiry: 15m
  refresh_expiry: 168h                          # 7 天（refresh 未实现，但已解析）

  ratelimit:
    requests_per_second: 10
    burst_size: 20

security:
  hmac_enabled: true
  hmac_secret: ${CODE_AGENT_SECURITY_HMAC_SECRET}
  hmac_header: "X-Signature-256"
  hmac_timestamp_header: "X-Timestamp"
  hmac_max_age: 5m

  egress_enabled: true                          # 生产强烈建议 true
  egress_policy:
    default_action: "deny"
    allowed_hosts:
      - "api.openai.com:443"
      - "api.anthropic.com:443"
    blocked_cidrs:
      - "169.254.169.254/32"
      - "100.100.100.200/32"
      - "10.0.0.0/8"
      - "172.16.0.0/12"
      - "192.168.0.0/16"
```

环境变量覆盖：

```bash
export CODE_AGENT_AUTH_JWT_SECRET=$(openssl rand -hex 32)
export CODE_AGENT_SECURITY_HMAC_SECRET=$(openssl rand -hex 32)
```

---

## 12. 与 17_api 的交互点

| API 路由组 | 中间件链 |
|---|---|
| `/healthz` `/readyz` `/metrics` | （仅基础链，无 auth） |
| `/api/v1/chat*` | `AuthMiddleware` |
| `/api/v1/mcp/servers` `/skills` `/index` `/projects` `/tasks` `/approve` `/tools` `/dynamic` | `AuthMiddleware` + `RequireRole(admin, dev)` |
| `/api/v1/debug/p0/*` | `AuthMiddleware`（**未**叠加 RequireRole — 17_api 已记 P0） |
| `/webhooks/*`（未实现，预留） | `HMACVerifier.GinMiddleware` |

---

## 13. 跨文档引用

- `17_api.md` §5 中间件链 — `AuthMiddleware` / `RequireRole` 的具体装配
- `05_sandbox.md` §4 网络隔离 — 与 `DockerNetworkMode()` 的关系
- `03_llm.md` §3 HTTP 客户端 — `egressHTTPClient` 注入点
- `06_mcp.md` §6 安全 — `SigningTransport` 装配
- `19_observability.md`（下一篇） — 限流 fail-open 的 metrics 暴露 + audit logger

---

下一篇：[`19_observability.md`](19_observability.md) —— Metrics / Tracing / Audit / Logging 全栈可观测性。
