# 18 · 安全与审计 `internal/auth` + `internal/security` + `internal/audit`

> 代码：
> - `internal/auth/jwt.go` (284) — JWT / Claims / Roles / APIKeyStore / AuthMiddleware / RequireRole
> - `internal/auth/ratelimit.go` (105) — `PerUserRateLimiter`：按用户/角色细粒度限流
> - `internal/auth/redis_revocation.go` (66) — Redis 黑名单（已在 16_store 详述）
> - `internal/security/hmac.go` (198) — `HMACVerifier` + `SigningTransport`（Webhook / MCP 通信）
> - `internal/security/egress.go` (223) — `EgressPolicy` + `EgressValidator`（容器出网策略）
> - `internal/audit/logger.go` (132) — 结构化审计日志（9 种事件类型）
> - 测试：jwt_test / hmac_test / egress_test / logger_test

---

## 1. 模块定位

**"让 Agent 在可信任的边界内工作"** —— 本章合订四个安全支柱：

| 维度 | 组件 | 解决什么 |
|---|---|---|
| **身份识别** | `auth/jwt` + `APIKeyStore` | "谁在请求？" |
| **权限控制** | `auth/RequireRole` + `PerUserRateLimiter` | "他能做什么？做多少？" |
| **数据可信** | `security/hmac` | "机器之间的请求是真的吗？" |
| **网络边界** | `security/egress` | "容器想访问的地方被允许吗？" |
| **行为取证** | `audit/Logger` | "发生了什么？谁做的？" |

这些模块**互相独立**，但在 API 入口（middleware chain）和关键业务（orchestrator / sandbox / mcp）上被**组合使用**。

---

## 1.5 设计哲学：安全栈的"7 层防御"

Agent 的攻击面比普通 Web 服务大得多——LLM 生成的代码要执行、外部 MCP
工具要联网、管理员要能审批重写数据库的操作。任何一道防线都不能假设
"肯定拦住"，必须多层。

### 攻击-防御矩阵

| 攻击类型 | 典型载荷 | 防御层（由外到内） |
|---|---|---|
| 未授权调 API | 匿名 POST /chat | 1. Rate Limit → 2. Auth JWT/APIKey → 3. RBAC |
| 窃听 / 中间人 | 抓 HTTP 流量 | TLS 终止（网关层，在 Agent 之外） |
| 重放攻击 | 重发抓到的合法签名 | HMAC + **timestamp 必填**（P0 #5） |
| 时序侧信道 | 通过毫秒差推测 key | constant-time compare（P0 #4） |
| Prompt injection | 诱导 LLM 做危险操作 | sensitive patterns + intent classifier + HITL |
| 敏感命令执行 | `DROP DATABASE`、`kubectl delete` | HITL 人工审批 |
| SSRF | 诱导 Agent 访问 169.254 | Egress L1（URL）+ L2（IP）+ L3（容器 net none） |
| 代码容器逃逸 | 提权 + 挂 docker.sock | CapDrop + no-new-priv + Readonly + PidsLimit |
| 数据外泄 | Agent 读密钥 + 写外网 | 环境变量白名单 + Egress ACL |
| 跨租户访问 | session A 读 B 文件 | workspace 硬隔离 + 绝无 fallback |

### 7 层防御的映射

```
┌─── L0 网络边界（反代 / WAF / TLS） ──────── 不在代码库 ────┐
│                                                          │
├─── L1 Rate Limit（api/middleware + redis_ratelimit） ────┤
│       │  阻挡匿名 / NAT DDoS                              │
│       ▼                                                   │
├─── L2 Auth（jwt/APIKey） ────────────────────────────────┤
│       │  识别"你是谁"                                     │
│       │  P0 #4: SHA-256 存储 + constant time             │
│       ▼                                                   │
├─── L3 RBAC（RequireRole） ───────────────────────────────┤
│       │  判断"你能做什么"                                 │
│       ▼                                                   │
├─── L4 Input Validation（handler） ───────────────────────┤
│       │  Body size / Content-Type / JSON schema          │
│       ▼                                                   │
├─── L5 Semantic Guard（orchestrator.containsSensitive） ──┤
│       │  匹配敏感 pattern → HITL                         │
│       │  防 prompt injection 漂移到危险命令               │
│       ▼                                                   │
├─── L6 HITL（人工审批） ──────────────────────────────────┤
│       │  signal + temporal workflow                      │
│       │  部署、生产数据操作必须人工点批准                  │
│       ▼                                                   │
├─── L7 Execution Sandbox（sandbox/manager） ─────────────┤
│       │  即便指令放行，也在隔离容器里跑                   │
│       │  CapDrop + Readonly + no-new-priv                │
│       ▼                                                   │
│    Egress（HTTP 出站 + Docker network） ───── 外部 ──→   │
│       │  URL 层 + DNS 层两级拦截                          │
└──────────────────────────────────────────────────────────┘

审计横切：每层都写 audit log → 事后取证
```

### "最小权限" 在每层的体现

| 层 | 最小权限举例 |
|---|---|
| Auth | 默认无 role → 401；要求 role 才能进业务组 |
| RBAC | admin / dev / readonly 递减；readonly 不能 execute_code |
| Env | sandboxed 命令只看到白名单 env var（P0 #10） |
| Capability | 容器默认 CAP_DROP ALL，用啥加啥（至今不需要加） |
| Network | NetworkMode=none 默认；用到网络再开白名单 |
| Egress | DefaultAction=deny；不在 AllowList 一律拒 |

### 失败优先策略：Fail-Close vs Fail-Open

不同组件的失败处理必须刻意选择：

| 组件 | 默认 | 原因 |
|---|---|---|
| HMAC verifier | **Fail-Close** | 签名校验挂了宁错勿漏 |
| JWT 撤销检查 | Fail-Close | 无法查 Redis → 拒绝请求（安全 > 可用） |
| Rate Limiter（Redis） | **Fail-Open** | Redis 挂 → 放行（可用性 > 严格限速） |
| Egress ACL | Fail-Close（policy.Enabled=true 时） | 不能"policy 挂了就让所有出向通过" |
| Circuit Breaker | Fail-Open | 熔断挂了 → 让调用过，由下游自己处理 |
| Audit Log | Fail-Open | 日志挂了业务不该中断 |

规则：**安全决策组件 fail-close，可用性组件 fail-open**。

---

## 2. 依赖架构

```
请求进入
    │
    ▼
┌────────────────────────┐
│ auth.AuthMiddleware    │ ← 从 Authorization header 解析 JWT / X-API-Key
│   ├─ JWTManager.Validate│   解析签名 → 黑名单 check → 填 Claims 到 ctx
│   └─ APIKeyStore.Validate│   长 token 别名（脚本 / 服务账号）
└──────────┬─────────────┘
           │
           ▼  (分组路由)
┌────────────────────────┐
│ auth.RequireRole(admin)│ ← RBAC 门禁
└──────────┬─────────────┘
           │
           ▼
┌────────────────────────┐
│ PerUserRateLimiter     │ ← 按 user_id（而非 IP）限流
└──────────┬─────────────┘
           │
           ▼
        业务 handler ──┐
                      │ 敏感操作发生
                      ▼
               ┌──────────────┐
               │ audit.Logger │ ← 结构化落盘（可走 SIEM）
               └──────────────┘

Webhook 独立路径:
    外部请求 → hmac.GinMiddleware → 验签通过 → handler

容器出网独立路径:
    sandbox 启动前 → EgressValidator.IsAllowed → 生成 iptables 规则
```

---

## 2.5 数据流总览

```text
═══════════════ 请求认证主链路 ═══════════════

┌───────────────┐
│ HTTP Request  │
│ Authorization:│
│  Bearer <jwt> │
│  或 X-API-Key │
└───────┬───────┘
        │
        ▼
┌─────────────────────────────────────────────────────────────┐
│ auth.AuthMiddleware                                          │
│  ┌────────────────────────────────────────────────────┐     │
│  │ 路径1: Bearer Token (JWT)                          │     │
│  │  parse → verify signature → check exp             │     │
│  │  → IsRevoked(jti)? 【Redis EXISTS】               │     │
│  │  → 注入 Claims 到 gin.Context                     │     │
│  ├────────────────────────────────────────────────────┤     │
│  │ 路径2: X-API-Key (长 token)                       │     │
│  │  SHA-256(key) → constant-time compare             │     │
│  │  遍历所有 hash (不早退 → 防时序攻击)               │     │
│  │  → 注入 Claims 到 gin.Context                     │     │
│  └────────────────────────────────────────────────────┘     │
└──────────────────────────┬──────────────────────────────────┘
                           │ (认证通过)
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ RequireRole(admin / user)                                    │
│  Claims.Role ∈ allowed? → pass : → 403 Forbidden           │
└──────────────────────────┬──────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ PerUserRateLimiter                                           │
│  key = user_id (非 IP → 防 NAT 误杀)                        │
│  token bucket → 放行 / 429 Too Many Requests                │
└──────────────────────────┬──────────────────────────────────┘
                           │
                           ▼
                      业务 Handler
                           │
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ audit.Logger.Log(Event{UserID, Action, Resource, Detail})    │
│  → zap structured log + optional PG insert                  │
└─────────────────────────────────────────────────────────────┘


═══════════════ 出网控制 (Egress) ═══════════════

┌───────────────────────┐
│ sandbox / MCP 出网请求 │
└───────────┬───────────┘
            │
            ▼
┌─────────────────────────────────────────────────────────────┐
│ L1: EgressTransport (http.RoundTripper)                      │
│  URL 级别: 检查 host + path 是否在白名单                     │
│  拒绝 → 返回 ErrEgressBlocked                               │
└──────────────────────────┬──────────────────────────────────┘
                           │ (URL 通过)
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ L2: Dialer.Control (net.Dialer)                              │
│  DNS 解析后得到 IP → 检查是否为私有地址 / 禁止段              │
│  防止 DNS rebinding 绕过 L1                                  │
│  拒绝 → syscall.EACCES                                      │
└──────────────────────────┬──────────────────────────────────┘
                           │ (IP 通过)
                           ▼
                    正常建立 TCP 连接


═══════════════ Webhook HMAC 验签 ═══════════════

┌────────────────────┐
│ 外部系统 Webhook   │
│ X-Signature: hmac  │
│ X-Timestamp: unix  │
└─────────┬──────────┘
          │
          ▼
┌─────────────────────────────────────────────────────────────┐
│ hmac.GinMiddleware                                           │
│  ① 提取 X-Timestamp → 检查 |now-ts| < 5min (防重放)        │
│  ② LimitReader(body, 10MB) → 读取 body                     │
│  ③ HMAC-SHA256(secret, timestamp+body) → expected           │
│  ④ hmac.Equal(signature, expected) → constant-time          │
│  通过 → 放行; 失败 → 401                                    │
└─────────────────────────────────────────────────────────────┘
```

---

## 3. ★ JWT + API Key 双轨认证

### 3.1 `Claims` 结构（jwt.go:40）

```go
type Role string
const (
    RoleAdmin    Role = "admin"    // 可授权部署、管理用户
    RoleDev      Role = "dev"      // 标准开发：chat / exec / search
    RoleReadOnly Role = "readonly" // 只读：search / chat（不可 exec）
    RoleService  Role = "service"  // 服务账号：webhook 回调
)

type Claims struct {
    jwt.RegisteredClaims        // 标准：iss/sub/iat/exp/jti
    UserID string
    Role   Role
    Email  string
}
```

### 3.2 `JWTManager` (jwt.go:74)

```
GenerateToken(userID, role, email):
    claims := Claims{ RegisteredClaims{
        Issuer: cfg.Issuer,           // "code-agent"
        Subject: userID,
        IssuedAt: now,
        ExpiresAt: now + 24h,
        ID: uuid()                    // JTI → 撤销黑名单 key
    }, userID, role, email }
    token := jwt.NewWithClaims(HS256, claims)
    return token.SignedString(cfg.SecretKey)

ValidateToken(tokenString):
    parsed := jwt.ParseWithClaims(tokenString, &Claims{}, keyFunc)
    if !parsed.Valid: return ErrTokenInvalid
    if revoked[claims.ID]: return ErrTokenInvalid   // 黑名单命中
    return claims

RevokeToken(jti):
    revokedMu.Lock()
    revoked[jti] = now
```

**关键决策**：

| 点 | 选择 | 原因 |
|---|---|---|
| 签名算法 | **HS256**（对称） | 单服务部署够用；多服务可升 RS256/ES256 |
| Token 有效期 | **24h** | 平衡 UX 和风险 |
| Refresh token | 独立 7d 过期 | 标准做法 |
| **JTI 必填** | 每 token 唯一 ID | 撤销名单的 key |
| 黑名单位置 | 内存 map + Redis（见 16_store） | 双写降级 |
| Secret 默认值 | **mustGenerateSecret()** → 32 字节随机 hex | 忘配也不会用弱 key |

### 3.3 `APIKeyStore` (jwt.go:189) —— 长 token 旁路

> ⚠️ **2026-05 更新（P0 #4 修复）**：此前 Store 内部是 `map[plaintext]*Entry`，
> 既泄漏 key 又是非常量时间查找。修复后的实现如下：

```go
type APIKeyEntry struct {
    Key     string  // 仅 Register 输入，不存储
    UserID, Role, Label string
    Created time.Time
}

type apiKeyRecord struct {
    hash  [32]byte     // SHA-256，持久存储
    entry APIKeyEntry  // entry.Key 在这里被强制清空
}

func (s *APIKeyStore) Register(entry *APIKeyEntry) {
    stored := *entry
    stored.Key = ""                    // ← 绝不保留 plaintext
    rec := apiKeyRecord{
        hash:  sha256.Sum256([]byte(entry.Key)),
        entry: stored,
    }
    s.entries = append(s.entries, rec)
}

func (s *APIKeyStore) Validate(key string) (*APIKeyEntry, bool) {
    want := sha256.Sum256([]byte(key))
    var matched *APIKeyEntry
    for i := range s.entries {
        if subtle.ConstantTimeCompare(s.entries[i].hash[:], want[:]) == 1 {
            e := s.entries[i].entry
            matched = &e
            // ← 不 break，保持总运算量恒定 → 时序侧信道不可观测
        }
    }
    return matched, matched != nil
}
```

**两个关键设计点**：

1. **永不存 plaintext** — `Register` 拷贝 `*entry` 后把 `Key` 置空再入 store。
   即使攻击者能 dump 内存或 `%+v` 打 log，也拿不到原始 API Key。
2. **常量时间比较** — `subtle.ConstantTimeCompare`；遍历到匹配也**不提前
   return**。时间无论 key 存在与否、匹配在第几位都一样。
   （说明：Go map lookup 不是常量时间，所以不能用 hash 做 map key 直接查
   —— 一旦攻击者能控制查询方式，可以通过时间测信息。虽然现实噪声大，但
   defense-in-depth 本就是消除理论漏洞。）

API Key 通过 `X-API-Key` header 传入。**验证用例**：
`TestAPIKeyStore_NoPlaintextStorage`（断言 `rec.entry.Key == ""`）。

### 3.4 `AuthMiddleware` 双轨处理（jwt.go:196）

```
AuthMiddleware(c):
    // 优先 API Key
    if apiKey := c.GetHeader("X-API-Key"); apiKey != "":
        entry, ok := apiKeys.Validate(apiKey)
        if ok:
            c.Set("auth_claims", fakeClaims(entry))
            c.Next(); return

    // 再 JWT
    auth := c.GetHeader("Authorization")
    tokenString := strings.TrimPrefix(auth, "Bearer ")
    claims, err := jwtMgr.ValidateToken(tokenString)
    if err: return 401

    c.Set("auth_claims", claims)
    c.Next()
```

业务 handler 通过 `auth.GetClaims(c)` 拿用户身份。

### 3.5 `RequireRole(...)` RBAC 门禁（jwt.go:239）

```
RequireRole(allowed ...Role):
    return func(c):
        claims := GetClaims(c)
        for _, r := range allowed:
            if claims.Role == r: c.Next(); return
        c.AbortWithStatusJSON(403, ...)
```

在路由里声明式使用：`approvalGroup.Use(auth.RequireRole(RoleAdmin, RoleDev))`。

---

## 4. `PerUserRateLimiter` —— 更细的限流

和 17_api 中的 "Per-IP" 限流互补：

| 对比 | middleware.rateLimiter | auth.PerUserRateLimiter |
|---|---|---|
| 粒度 | 按 Client IP | 按 `claims.UserID` |
| 位置 | 全局中间件 | 路由组 opt-in |
| 配额 | 10 rps / 20 burst | 由 role 决定（Admin 无限、Dev 宽松、ReadOnly 严） |

数据结构和算法同 17_api 的 tokenBucket，**差在 key 是 user_id** —— NAT 后面 100 个人共用一个 IP 时，per-user 才能公平。

---

## 5. ★ HMAC：机器到机器的可信通道

### 5.1 场景

- **入站**：`/webhooks/*` 接收外部 MCP / CI 回调；
- **出站**：Agent 调用 MCP server 时自动签名（`SigningTransport`）。

### 5.2 `HMACVerifier.GinMiddleware` 四步走（hmac.go:100）

```text
Step 1  取签名 header（X-Signature-256）
        缺失 → 401

Step 2  时间戳防重放（P0 #5 修复后为"必填"）
        X-Timestamp 缺失      → 401 "missing timestamp header"   ⚠ 修复前：跳过
        X-Timestamp 格式错    → 400 "invalid timestamp format"
        |age| > MaxTimestampAge → 401 "timestamp expired or skewed"
           （同时拒绝过去 / 未来两端，防时钟欺骗）

Step 3  读请求体（LimitReader 防 OOM）
        读完后 reset c.Request.Body = io.NopCloser(...)   ← 关键！后续 handler 还要读

Step 4  VerifySignature(body, signature)
        hmac.Equal(computed, provided)   ← 常量时间比对（防 timing attack）
        失败 → 403
```

> ⚠️ **2026-05 更新（P0 #5 修复）**：Step 2 以前写的是 `if tsHeader != ""
> { check } else { pass }`——这等于宣告"攻击者只要省略时间戳就可以重放
> 签名"。修复后，当 `TimestampHeader` 和 `MaxTimestampAge` 同时配置非
> 默认值，timestamp 是**强制字段**，缺失直接 401。

**4 个易错点**：

1. **body 必须重置**：middleware 读完 body 后 handler 再读就是空的，必须 `NopCloser`；
2. **常量时间比对**：用 `hmac.Equal` 而非 `==`，防 timing oracle；
3. **LimitReader**：大 body 攻击防护；
4. **timestamp 防重放必须真拒绝**：仅签名不够，必须 pin 时间窗口；**缺 header 不能跳过校验**。

**完整测试矩阵**（见 [API_TEST_GUIDE § 11](../API_TEST_GUIDE.md#11-hmac-webhook)）：

| 用例 | Signature | Timestamp | 期望 |
|---|---|---|---|
| A 完全无头 | ❌ | ❌ | 401 missing signature |
| B 有签名无 ts | ✅ | ❌ | 401 missing timestamp  ← P0 #5 关键回归 |
| C 过期 ts | ✅ | 过去 1h | 401 expired or skewed |
| D 未来 ts | ✅ | 未来 1h | 401 expired or skewed（新加） |
| E 格式错 | ✅ | 非 RFC3339 | 400 invalid format |
| F 错签名 | 错 | ✅ | 403 invalid signature |
| G 全对 | ✅ | ✅ | 200 业务响应 |

### 5.3 `SigningTransport`（hmac.go:174）

`http.RoundTripper` 包装器，出站自动签：

```go
type SigningTransport struct {
    Base    http.RoundTripper
    Secret  []byte
    Header  string
}

RoundTrip(req):
    body := readAndReplace(req.Body)
    sig := hmac-sha256(body, secret)
    req.Header.Set(Header, "sha256="+hex(sig))
    return Base.RoundTrip(req)
```

给 MCP client 的 `http.Client.Transport` 注入后，所有出站自动带签名。

---

## 6. ★ Egress Policy —— 容器出网黑/白名单

### 6.1 `EgressPolicy` (egress.go:18)

```go
type EgressPolicy struct {
    Enabled       bool
    DefaultAction string   // "deny" | "allow"  (生产 MUST "deny")
    AllowedHosts  []string // "api.openai.com:443"
    AllowedCIDRs  []string // "10.0.0.0/8"
    BlockedCIDRs  []string // 生效优先级 > allowed
    DNSAllowed    bool
}
```

### 6.2 两个预置策略

#### `DefaultEgressPolicy()` —— **最严**（给不可信代码）

```
Enabled: true, DefaultAction: deny
BlockedCIDRs:
  - 169.254.169.254/32  ← AWS/GCP 元数据端点
  - 100.100.100.200/32  ← 阿里云元数据
  - 10.0.0.0/8          ← 私有网 A
  - 172.16.0.0/12       ← 私有网 B
  - 192.168.0.0/16      ← 私有网 C
DNSAllowed: false
```

**防范的威胁**：

- **SSRF 攻 metadata endpoint**：AWS/GCP/阿里云的 `169.254.169.254` 能读到 instance role credentials —— 一旦代码 curl 它，整个账号可能沦陷；
- **内网穿透**：即使沙箱逃逸，也打不到业务内网的 DB / Redis / k8s API；
- **DNS tunnel exfil**：禁 DNS 防止用 DNS 查询偷数据。

#### `InternalServiceEgressPolicy()` —— 可信 Agent 容器

```
AllowedCIDRs: ["10.0.0.0/8"]     ← 允许访问内网
BlockedCIDRs: ["169.254.169.254/32"]   ← 仍禁 metadata
DNSAllowed: true
```

用于 Agent 自己的服务容器（需要访问 Qdrant / Redis / MCP）。

### 6.3 `EgressValidator` (egress.go:76)

```go
IsAllowed(host, port):
    1. 先检查 BlockedCIDRs（否决优先）
    2. 精确 host:port 白名单
    3. CIDR 白名单 (allowedNets)
    4. fallthrough: return DefaultAction == "allow"
```

**整合到 Docker 沙箱**：

```go
DockerNetworkMode() string:
    if !policy.Enabled: return "bridge"
    if len(AllowedHosts) == 0 && len(AllowedCIDRs) == 0: return "none"   // 完全断网
    return "bridge"    // + 后续 iptables 过滤

GenerateIptablesRules() []string:
    // 可选：输出能直接 `iptables -A DOCKER-USER` 的规则串
```

Sandbox Manager（见 05_sandbox）在 ContainerCreate 时根据 `DockerNetworkMode()` 决定 network；对于 bridge 模式，额外用 iptables 规则精确控制。

### 6.4 Go 层的 Egress 强制执行（新增 P0 #6）

> ⚠️ **2026-05 更新**：此前 `EgressValidator.IsAllowed` **定义了但没有任何
> 调用方**——它只出现在 `GenerateIptablesRules` 和 `_test.go` 里，本质是
> "纸面文档"。现在新增了两个适配器，让策略在 Go HTTP client 层**真正生效**。

**两层防御**：

```text
应用代码 http.Client.Do(req)
            │
            ▼
┌───────────────────────────────┐
│ L1: EgressTransport.RoundTrip │  ← URL 层
│   if !IsAllowed(req.URL.Host) │
│       return ErrEgressDenied  │    拒绝 allow-list 外的主机名
└───────────────────────────────┘
            │
            ▼  （允许则委托给 http.Transport）
┌───────────────────────────────┐
│ L2: Dialer.Control            │  ← DNS 解析后，connect(2) 前
│   解析出 IP xxx.xxx.xxx.xxx    │
│   if !IsAllowed(ip)           │    挡住 DNS rebinding 到内网
│       return ErrEgressDenied  │
└───────────────────────────────┘
            │
            ▼
        connect(2) 真正发起
```

**为什么需要两层**：
- L1 快但粗：不消耗网络资源，但攻击者可以把 `allow.example.com` 的 DNS
  指向内网 IP（DNS rebinding），然后用合法主机名绕过。
- L2 精但晚：已经 DNS 解析完，此时看到的就是 kernel 即将 connect 的 IP。
  所以不管 DNS 怎么玩，L2 总能拦住。

**API**：
```go
// 仅 URL 层
http.Client{Transport: &security.EgressTransport{Validator: v, Base: ...}}

// 两层一次装配（推荐）
client := security.NewEgressHTTPClient(v, 30*time.Second)
```

**Fail-open 策略**：`policy.Enabled=false` 时完全放行（开发环境友好）。
`Enabled=true` 但 validator 构造失败时报 error 上抛——不静默 fallback。

**集成状态**：类库已实现并通过单测（`TestNewEgressHTTPClient_BlocksLoopbackUnderDenyPolicy`
模拟 `allow localhost` + `block 127/8` 场景，验证 L2 在 L1 放行后依然拦下）。
**LLM / MCP / rerank 客户端的注入仍是 P1 待办**。

---

## 7. 审计日志 `audit.Logger` (132 行)

### 7.1 事件类型（10 种）

```go
const (
    EventApprovalRequested
    EventApprovalGranted
    EventApprovalDenied
    EventApprovalTimeout
    EventSandboxExecution
    EventMCPToolCall
    EventSensitiveBlocked
    EventSessionCreated
    EventSessionDeleted
    EventIndexingStarted
)
```

### 7.2 `Event` 结构（audit/logger.go:31）

```go
type Event struct {
    Timestamp time.Time
    Type      EventType
    SessionID, TaskID, UserID string
    Action    string
    Details   map[string]string   // flat KV，适合日志系统
    IP        string
    Success   bool
    Error     string
}
```

`Details` **是 flat map**（而非 `any`）—— 理由：zap 结构化日志、ES / Loki / Splunk 等 SIEM 系统最友好的是 `k:v` 扁平结构。不存嵌套。

### 7.3 `Logger.Log` (logger.go:59)

```
Log(ctx, event):
    if event.Timestamp.IsZero(): event.Timestamp = now
    fields := [event_time, event_type, action, success]
    if SessionID/TaskID/UserID/IP/Error: append
    for k,v := range Details: append("detail_"+k, v)
    zap.Info("audit", fields...)   // Level 故意是 INFO，不是 WARN
```

**设计点**：

- **Named logger**：`baseLogger.Named("audit").With("log_type", "audit")`  
  → 输出里都有 `"logger":"audit"` 字段 → 下游 SIEM 可用 `log_type=audit` 精确过滤，独立归档；
- **不失败**：审计写入永远成功（zap 内部失败会丢，不 return error）—— 审计不能阻塞业务；
- **无 DB 写**：审计走 zap，可通过 zap 的 sink（log shipper / Kafka / PostgreSQL store）灵活配置。实际也会写到 `audit_logs` 表（见 16_store）。

### 7.4 便捷方法（专类事件）

```go
LogApproval(ctx, type, taskID, sessionID, action, success)
LogSandboxExec(ctx, sessionID, language, exitCode, duration)
LogMCPCall(ctx, serverName, toolName, success, errMsg)
```

帮调用者省去手写 Event 结构的样板代码。

---

## 8. 合力：典型请求的安全链路

```
POST /api/v1/tasks/123/approve
    │
    ▼
[recovery] → [requestID] → [tracing] → [metrics] → [IP rate limit]
    │
    ▼
[auth.AuthMiddleware]
    └─ JWT.Validate(Bearer xxx) → claims{UserID=alice, Role=admin}
    │
    ▼
[auth.RequireRole(admin,dev)]   ← ✅ alice 是 admin
    │
    ▼
[PerUserRateLimiter]            ← ✅ 配额充足
    │
    ▼
handleApproval:
    orchestrator.HandleApproval(taskID, true, "alice")
        └─ store.ResolveApproval(taskID, "approved", "alice")
        └─ audit.LogApproval(EventApprovalGranted, taskID, sessionID, action, true)

outbound → external MCP server
    └─ http.Client{Transport: SigningTransport{secret}}
        └─ 自动添加 X-Signature: sha256=<hex>
```

sandbox 执行时：

```
sandbox.Execute
    └─ EgressValidator.IsAllowed("api.openai.com", 443) → true  (白名单)
    └─ EgressValidator.IsAllowed("169.254.169.254", 80) → false (BlockedCIDRs)
    └─ ContainerCreate(NetworkMode=none)
    └─ audit.LogSandboxExec(sessionID, "python", 0, 2.3s)
```

---

## 9. 设计权衡

| 抉择 | 动机 |
|---|---|
| **JWT + API Key 双轨** | JWT 给人用（有 UI）；API Key 给机器用（脚本无 UI 无法交互登录） |
| HS256 vs RS256 | HS256 简单 + 对称；单服务够用，多服务再升级 |
| JWT **24h 有效期** | UX 和风险的平衡；撤销靠黑名单而非短 TTL |
| **JTI 必填** + 黑名单 | 唯一方案能撤销 JWT |
| **4 种 Role 而非 ACL** | RBAC 够用 + 简单；未来业务复杂时可过渡到 Casbin |
| `RequireRole` **分组声明** | 路由声明式；handler 不写权限判定 |
| **Per-IP + Per-User 双层限流** | IP 防 bot；User 防单人滥用；NAT 场景下仅 IP 不公平 |
| HMAC **常量时间比对** | 防 timing oracle 攻击 |
| HMAC **timestamp 防重放** | 仅签名不够，拦截 24h 前的 replay |
| HMAC **body 重置** | 必须 `io.NopCloser`，否则 handler 读不到 body |
| `SigningTransport` 透明化出站签名 | 业务代码无感；不用每个 mcp client 手动签 |
| Egress 默认**全拒绝** | 安全需要"默认禁止，显式允许"；漏洞暴露面小 |
| 明确屏蔽 **169.254.169.254** 等 metadata | SSRF + 云元数据窃取是真实攻击向量 |
| Egress 默认**禁 DNS** | 防 DNS tunnel 数据泄露 |
| 黑名单 CIDR **优先级高**于白名单 | 避免"白名单写大了，黑名单被无视"的配置失误 |
| 审计事件 **zap.Named("audit")** | 下游 SIEM 独立提取；不与应用日志混 |
| 审计 Details = **flat map** | ES/Loki 等系统最友好；避免嵌套难查询 |
| 审计**不返回 error** | 审计失败不阻塞业务；loss 走 zap sink 监控 |
| 审计**双写 zap + pg** | zap 快、pg 可查 |

---

## 10. 后续演进

- [ ] **JWT 升 RS256/ES256**：多服务部署时公钥验签；
- [ ] **OIDC / OAuth2 接入**：对接 Okta / Azure AD / Keycloak；
- [ ] **SPIFFE/SPIRE**：工作负载身份 + mTLS；
- [ ] **Casbin ACL**：RBAC 遇到 "某用户可以 approve 自己创建的 task 但不能 approve 别人" 等复杂规则时切换；
- [ ] **HMAC 密钥轮换**：`KID` header + 多 secret 同时生效 → 无停机轮换；
- [ ] **Nonce 缓存**：HMAC 防重放加 nonce 去重（timestamp 粒度有限）；
- [ ] **Egress 运行时拦截**（eBPF / CNI plugin）：不靠 iptables，按进程 + SNI 更精细；
- [ ] **审计→Kafka→ES**：高写入量下 zap file sink 顶不住；
- [ ] **审计区块链化（WORM）**：合规场景要求不可篡改；
- [ ] **Rate limit 分布式**：当前内存实现在多 pod 下每个 pod 独立；Redis-based 全局限流；
- [ ] **User bans**：黑名单 by UserID；
- [ ] **IP reputation**：接入 AbuseIPDB 等；
- [ ] **2FA / WebAuthn**：敏感操作二次验证；
- [ ] **同态加密敏感字段**：审计日志里的 prompt / result 如含 PII 需加密；
- [ ] **Policy-as-Code**（OPA / Rego）：egress / rbac 全部策略化；
- [ ] **Metrics**：`auth_success_total / auth_failed_total{reason} / hmac_verify_failed_total / egress_denied_total{host} / audit_events_total{type}`。

---

## 13. 实现剖析与改进方向

### JWT 验证的完整时序

```text
client → Authorization: Bearer <token>
           │
           ▼
AuthMiddleware
    1. extract "Bearer <token>"
    2. jwt.ParseWithClaims(token, claims, keyFunc)
       ├── keyFunc: 校验 alg == HS256 (拒绝 alg=none)  ← 防 algorithm confusion
       │           返回 []byte(secret)
       └── 验签 + 解析
    3. 如果 err contains "expired" → 401 ErrTokenExpired
    4. 否则 err → 401 ErrTokenInvalid
    5. 检查 revoked map（内存） → revoked: 401
    6. c.Set("auth_claims", claims)
           │
           ▼
RequireRole("admin")（可选的 RBAC 组）
    - claims.Role == "admin"? 通过 / 否则 403
           │
           ▼
Handler
    - 从 c.Get("auth_claims") 拿 claims
    - 业务逻辑
```

### HMAC 验证的 7 步（Webhook 场景）

```text
incoming POST /webhooks/mcp-callback
  │
  │ Step 1. extract X-Signature-256       → 缺失 401
  │
  │ Step 2. timestamp check (P0 #5 修复)
  │   - X-Timestamp 缺失 → 401 missing   ← 以前会跳过
  │   - parse RFC3339 失败 → 400
  │   - |age| > MaxTimestampAge → 401 expired/skewed
  │
  │ Step 3. read body under LimitReader (1 MiB)
  │   - 读失败 → 400
  │   - 重置 c.Request.Body = NopCloser(body)   ← 关键，handler 还要读
  │
  │ Step 4. compute HMAC-SHA256(body, secret)
  │
  │ Step 5. hmac.Equal(computed, provided)     ← 常量时间比较
  │   - 不等 → 403 invalid signature
  │
  │ Step 6. c.Next() → handler
  │
  │ Step 7. handler 从 body 读取 payload 处理
```

### API Key 查找的常量时间性质

```go
// ❌ 反例（如果我们用 map）
entry, ok := s.keys[plaintextKey]  // Go map 是 probabilistic，
                                   // 不命中时时间比命中短（没遍历 bucket）
if !ok { return nil, false }
return entry, true

// ✅ 现在的实现
want := sha256(plaintextKey)
var matched *APIKeyEntry
for i := range s.entries {
    if subtle.ConstantTimeCompare(s.entries[i].hash[:], want[:]) == 1 {
        e := s.entries[i].entry
        matched = &e
        // 注意：不 break，继续遍历保持运算量恒定
    }
}
return matched, matched != nil
```

**关键点**：
1. `subtle.ConstantTimeCompare` 遍历比较每个字节（没有短路）
2. 匹配成功后不 break，继续遍历其他条目
3. 总遍历次数 = len(entries)，不受密钥存在与否影响
4. 现实中 HTTP 栈噪声远大于这点差异，但 defense-in-depth 原则

### 利弊评估

**优势（Pros）**
- ✅ 7 层防御纵深，单点失效不等于全面失守
- ✅ JWT + API Key 双轨，交互用户 vs 服务账号都覆盖
- ✅ HMAC timestamp 必填 + 两端约束（旧 & 未来）
- ✅ APIKey 常量时间比对 + 哈希存储（零 plaintext）
- ✅ Egress 两层（URL + IP）防 SSRF + DNS rebinding

**代价（Cons）**
- ⚠️ JWT 撤销仅内存 map（重启即重置；Redis 撤销 store 已写但未接入）
- ⚠️ JWT Secret 无 rotation 机制
- ⚠️ API Key 无 expiration / rotation
- ⚠️ Egress 类库已写但 LLM/MCP client 未接（类库在等）
- ⚠️ HMAC 无 **nonce 缓存**——两次同样的 signed 请求都会被接受（依赖
  timestamp 窗口，但 5min 窗口内 replay 可行）
- ⚠️ Audit log 仅本地，重启可能丢

### 可改进点

**P0**
1. Egress validator 接入 LLM / MCP / rerank 的 HTTPClient
2. Redis rate limiter 接入 middleware（见 17 § 13）
3. JWT 撤销查 Redis（当前仅内存 map）

**P1**
4. HMAC nonce 缓存（5min TTL Redis SET），防 timestamp 窗口内重放
5. JWT Secret rotation：两个 key 并存，新签名用 new，验证接受 both，old 过期后淘汰
6. API Key 加 `expires_at` 字段，Validate 时检查

**P2**
7. Audit log 写 Kafka → S3/OSS 归档
8. Egress 规则热加载（改配置不重启）
9. eBPF 层真正 drop 包（iptables 规则外再一层）
10. API Key 哈希加 salt（防 rainbow table）—— 当前是纯 SHA-256

---

下一篇：`19_observability.md` —— Metrics (Prometheus) + Tracing (OTel) + Error 规范 + 对象池。
