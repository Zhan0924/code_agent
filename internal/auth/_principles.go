// Package auth —— JWT 鉴权 + RBAC + Redis 吊销 + 限流
//
// =============================================================================
//                                   设 计 原 理
// =============================================================================
//
// 1. 为什么选 JWT 而不是 Session Cookie？
//    Agent 服务是 **无状态水平扩容** 架构（参考 4.2 HA 设计），
//    任意一次请求可能命中任意一个 Pod。若用 Session Cookie，必须
//    依赖中心化存储做查询，额外增加 Redis RTT。JWT 把身份签名后
//    放在 Token 里，Pod 本地验签即可，毫秒级、无外部依赖。
//
// 2. Token 结构（HS256 / RS256）
//    header.payload.signature
//    payload 携带：
//      · sub       : 用户 ID
//      · tenant_id : 租户（多租户隔离）
//      · roles     : []string (admin / dev / readonly / service)
//      · exp / iat : 过期/签发时间（短有效期 + refresh token 策略）
//
// 3. RBAC 授权中间件
//    AuthMiddleware  验签 → 解析 Claims → 注入 gin.Context
//    RequireRole(...) 在敏感路由前拦截：
//      · admin 角色一律放行
//      · 其他角色按白名单匹配，不满足 → 403
//    双因素防御：Handler 内部仍可基于 Claims.UserID 做资源级所有权校验。
//
// 4. Token 吊销 (redis_revocation.go)
//    JWT 天然痛点：一旦签发无法主动失效。
//    方案：Redis 维护 **黑名单 Set** (jti hash)，key 自带 TTL 到 exp。
//    验签通过后再 SISMEMBER 一次，命中则拒绝。
//    TTL 等于原 token 剩余有效期，过期后自动清理，集合不会膨胀。
//
// 5. 限流（ratelimit.go）—— 令牌桶 + Redis 原子脚本
//
//      · 每租户 / 每用户 / 每路由多维度限流
//      · Redis Lua：GET → 按时间差补充 token → DECR → 回写
//        全部在 Redis 单线程原子完成，避免并发超限
//      · 响应头带 X-RateLimit-Remaining / X-RateLimit-Reset
//      · 被限流返回 429 + Retry-After
//
// 6. API Key 备用通道
//    机器账户（CI / 服务间调用）不适合 JWT（无法续签）。
//    APIKeyStore 维护 key hash → UserID + Role 映射。
//    中间件优先检查 X-API-Key 头，命中即认证，否则才检查 Authorization。
//
// 7. 防 Token 重放
//    · 强制 HTTPS（TLS 终止在 Ingress）
//    · iat 时间窗口校验（拒绝未来签发 / 过旧签发）
//    · 重要操作叠加 Temporal HITL 二次确认（/tasks/:id/approve）
//    · 敏感动作记入 audit log（internal/audit）
//
// 8. Secret 管理
//    JWT SecretKey：
//      · 启动时从 ENV / Vault 读取（不准硬编码）
//      · 轮换策略：支持多 Key 并存（旧 Key 仅验签，不签发）
//      · NewJWTManager 默认 mustGenerateSecret() 仅用于本地开发
//
// =============================================================================
//
// 9. 模块结构图
//
//   ┌──────────────────────────────────────────────────────────────────────┐
//   │                           auth package                                │
//   │                                                                       │
//   │  ┌──────────────────────────────────────────────────────────────┐   │
//   │  │ JWTManager (jwt.go)                                           │   │
//   │  │ ─────────────────────────────────────────────────────────     │   │
//   │  │  cfg       *JWTConfig    (secret / issuer / ttl)              │   │
//   │  │  revoked   map[jti]time  (in-mem 黑名单，备份在 Redis)          │   │
//   │  │                                                               │   │
//   │  │  + GenerateToken(userID, role, email) (token, error)          │   │
//   │  │  + ValidateToken(token) (*Claims, error)                      │   │
//   │  │  + RevokeToken(jti)                                           │   │
//   │  └──────────────────────────────────────────────────────────────┘   │
//   │                                                                       │
//   │  ┌──────────────────────────────────────────────────────────────┐   │
//   │  │ RedisRevocation (redis_revocation.go)                         │   │
//   │  │ ─────────────────────────────────────────────────────────     │   │
//   │  │  rdb      redis.UniversalClient                               │   │
//   │  │  keyPrefix "auth:rev:"                                        │   │
//   │  │                                                               │   │
//   │  │  + Revoke(jti, ttl)                                           │   │
//   │  │  + IsRevoked(jti) bool                                        │   │
//   │  └──────────────────────────────────────────────────────────────┘   │
//   │                                                                       │
//   │  ┌──────────────────────────────────────────────────────────────┐   │
//   │  │ APIKeyStore (jwt.go)                                          │   │
//   │  │ ─────────────────────────────────────────────────────────     │   │
//   │  │  keys      map[hash]APIKeyEntry                               │   │
//   │  │                                                               │   │
//   │  │  + Register(entry)                                            │   │
//   │  │  + Validate(key) (*APIKeyEntry, ok)                           │   │
//   │  └──────────────────────────────────────────────────────────────┘   │
//   │                                                                       │
//   │  ┌──────────────────────────────────────────────────────────────┐   │
//   │  │ RateLimiter (ratelimit.go)                                    │   │
//   │  │ ─────────────────────────────────────────────────────────     │   │
//   │  │  rdb      redis.UniversalClient                               │   │
//   │  │  script   *redis.Script  (token-bucket Lua)                   │   │
//   │  │  buckets  map[scope]Config (tenant/user/route limits)         │   │
//   │  │                                                               │   │
//   │  │  + Allow(ctx, scope, key) (allowed, retryAfter)               │   │
//   │  └──────────────────────────────────────────────────────────────┘   │
//   │                                                                       │
//   │  Middleware (Gin):                                                    │
//   │  ─────────────────                                                    │
//   │  · AuthMiddleware(jwtMgr, apiKeys, logger)                            │
//   │  · RequireRole(roles ...Role)                                         │
//   │  · RateLimitMiddleware(scope)                                         │
//   │                                                                       │
//   │  Used by:                                                             │
//   │  ────────                                                             │
//   │  · internal/api (所有路由前置中间件)                                   │
//   │  · internal/audit (记录身份到审计日志)                                 │
//   └──────────────────────────────────────────────────────────────────────┘
//
// 10. 请求鉴权 + 限流 完整流程图
//
//     HTTP Request (e.g. POST /agent/chat)
//           │
//           ▼
//     ┌──────────────────────────────┐
//     │ AuthMiddleware                │
//     │  1. X-API-Key 头存在？          │ ──yes──▶ APIKeyStore.Validate ──▶ c.Set(claims)
//     │  2. Authorization: Bearer xxx │
//     │     → JWTManager.ValidateToken│
//     │     → Redis SISMEMBER revoked?│ ──yes──▶ 401
//     │  3. 无任何凭证 → 401           │
//     └──────────────┬───────────────┘
//                    │ 成功（claims 注入）
//                    ▼
//     ┌──────────────────────────────┐
//     │ RequireRole(required...)      │
//     │   claims.Role == admin? yes   │
//     │   claims.Role ∈ required? yes │
//     │   else → 403                  │
//     └──────────────┬───────────────┘
//                    │
//                    ▼
//     ┌──────────────────────────────┐
//     │ RateLimitMiddleware           │
//     │   key = tenant + user + route │
//     │   redis EVAL token-bucket Lua │
//     │   allowed? no → 429 + Retry-After
//     │   X-RateLimit-Remaining       │
//     └──────────────┬───────────────┘
//                    │
//                    ▼
//              Business Handler
//                    │
//                    ▼
//     ┌──────────────────────────────┐
//     │ audit.Logger.Record(...)     │  (who, what, when, result)
//     └──────────────────────────────┘
//
// 11. JWT 颁发 / 验证 / 吊销 时序
//
//     client              api/login           JWTManager        Redis (rev)
//        │ POST /login        │                   │                  │
//        ├───────────────────▶│ 校验密码           │                  │
//        │                    │  ok → GenerateToken(u,role,email)    │
//        │                    │──────────────────▶│                  │
//        │                    │◀── signed token ──│                  │
//        │◀── {token} ────────│                   │                  │
//        │                    │                   │                  │
//        │ 后续请求 Bearer     │ AuthMiddleware    │                  │
//        ├───────────────────▶│──ValidateToken──▶│                  │
//        │                    │                   │ SISMEMBER jti?   │
//        │                    │                   │─────────────────▶│
//        │                    │                   │◀───── 0 ──────── │ (not revoked)
//        │                    │◀─── *Claims ─────│                  │
//        │◀── 200 业务响应 ───│                   │                  │
//        │                    │                   │                  │
//        │ 登出 POST /logout   │ RevokeToken(jti) │                  │
//        ├───────────────────▶│──────────────────▶│ SET jti TTL=exp  │
//        │                    │                   │─────────────────▶│
//
// 12. 令牌桶限流（Redis Lua，原子 + 水平共享）
//
//       key = "rl:{tenant}:{user}:{route}"
//                         │
//                         ▼
//       Lua script (在 Redis 单线程原子执行):
//         now       = redis.call('TIME')
//         state     = redis.call('HMGET', key, 'tokens', 'ts')
//         elapsed   = now - state.ts
//         refill    = min(burst, state.tokens + elapsed * rate)
//         if refill < 1:
//             return {0, retryAfter=(1-refill)/rate}
//         else:
//             redis.call('HMSET', key, 'tokens', refill-1, 'ts', now)
//             redis.call('EXPIRE', key, ttl)
//             return {1, 0}
//
//         ┌──────────────────────────┐
//         │ burst=30  rate=10 rps   │
//         │ tokens: ▓▓▓▓▓▓▓░░░   7  │
//         │  ──1ms──▶   ▓▓▓▓▓▓▓░░  7 +0
//         │  ──100ms─▶  ▓▓▓▓▓▓▓▓░  8.0
//         │  ──call ─▶  ▓▓▓▓▓▓▓░░  7
//         └──────────────────────────┘
//
// =============================================================================
//
// 13. 深度原理剖析 + 实战案例
// -----------------------------------------------------------------------------
//
// [案例一] JWT 密钥轮换 —— 用户全部重新登录 vs 无感续约
//
//   生产要求：JWT 签名密钥每 90 天轮换一次（合规 / 防泄漏）。
//
//   幼稚做法：
//
//     // 直接替换 secret
//     jwtMgr.secret = newSecret
//     // 后果：所有用现有 token 的请求立即 401
//     //      100 万用户同时需要重新登录
//     //      移动 app 的后台请求瞬间失败
//
//   正确做法：**多 Key 并存 + 滚动淘汰**
//
//     type KeySet struct {
//         mu      sync.RWMutex
//         signing *SigningKey          // 当前签发用（只有一把）
//         valid   map[string]*SigningKey  // 所有可验签的（包括旧的）
//     }
//
//     type SigningKey struct {
//         KID       string   // key id，写入 JWT header
//         Secret    []byte
//         NotBefore time.Time
//         NotAfter  time.Time  // 到期后从 valid 移除
//     }
//
//     // 签发：用 signing key
//     func (ks *KeySet) Sign(claims jwt.Claims) (string, error) {
//         ks.mu.RLock()
//         sk := ks.signing
//         ks.mu.RUnlock()
//
//         token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
//         token.Header["kid"] = sk.KID   // ← 关键：在 header 标识 key
//         return token.SignedString(sk.Secret)
//     }
//
//     // 验签：按 header.kid 查 valid map
//     func (ks *KeySet) Verify(tokenStr string) (*Claims, error) {
//         token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (any, error) {
//             kid, ok := t.Header["kid"].(string)
//             if !ok { return nil, errors.New("missing kid") }
//
//             ks.mu.RLock()
//             sk, ok := ks.valid[kid]
//             ks.mu.RUnlock()
//             if !ok { return nil, errors.New("unknown kid") }
//             if time.Now().After(sk.NotAfter) { return nil, errors.New("kid expired") }
//             return sk.Secret, nil
//         })
//         ...
//     }
//
//     // 每日定时任务：轮换
//     func (ks *KeySet) Rotate() {
//         newKey := generateKey()
//
//         ks.mu.Lock()
//         defer ks.mu.Unlock()
//
//         // 1. 新 key 加入 valid（开始接受它签的 token）
//         ks.valid[newKey.KID] = newKey
//
//         // 2. signing 换成新 key（新签发都用它）
//         ks.signing = newKey
//
//         // 3. 清理超过 90 天的旧 key（那些 token 都应该过期了）
//         for kid, sk := range ks.valid {
//             if time.Now().After(sk.NotAfter.Add(30 * 24 * time.Hour)) {
//                 delete(ks.valid, kid)
//             }
//         }
//     }
//
//   时间线：
//     Day 1:   KeyA 签发、验签
//     Day 90:  Rotate → KeyB 开始签发；老 token 用 KeyA 还能验
//     Day 91:  所有新 token 用 KeyB；KeyA 持续验老 token
//     Day 120: 老 token 已全部过期（exp=30d），删除 KeyA
//
//   **用户零感知，安全合规**。这也是为什么 JWT 标准要设计 `kid` header 字段。
//
// -----------------------------------------------------------------------------
//
// [案例二] Token 吊销的"Redis 膨胀"陷阱
//
//   JWT 吊销的朴素思路：Redis 维护黑名单 Set
//
//     // 登出
//     func (m *JWTManager) RevokeToken(jti string) {
//         m.redis.SAdd(ctx, "revoked_tokens", jti)
//     }
//
//     // 验签时检查
//     func (m *JWTManager) ValidateToken(token string) (*Claims, error) {
//         claims := parseToken(token)
//         isRevoked, _ := m.redis.SIsMember(ctx, "revoked_tokens", claims.ID).Result()
//         if isRevoked { return nil, errors.New("revoked") }
//         return claims, nil
//     }
//
//   一年后的问题：
//     · 100 万用户，平均每人 100 次登出/登录 = 1 亿条 jti
//     · Redis Set 内存：1 亿 * 50B = 5GB（单 key 爆炸）
//     · SIsMember 对超大 Set 仍然 O(1)，但迁移 / backup 很痛苦
//     · 没有自动清理，10 年后是 50GB
//
//   正确做法：**每个 jti 独立 key + TTL 自动过期**
//
//     func (m *RedisRevocation) Revoke(ctx context.Context, jti string, ttl time.Duration) error {
//         key := m.keyPrefix + jti                          // "auth:rev:{jti}"
//         return m.rdb.Set(ctx, key, "1", ttl).Err()        // TTL = 原 token 剩余有效期
//     }
//
//     func (m *RedisRevocation) IsRevoked(ctx context.Context, jti string) (bool, error) {
//         key := m.keyPrefix + jti
//         err := m.rdb.Get(ctx, key).Err()
//         if err == redis.Nil { return false, nil }
//         if err != nil { return false, err }
//         return true, nil
//     }
//
//     // 使用
//     claims := parseToken(token)
//     remainingTTL := claims.ExpiresAt.Sub(time.Now())
//     revocation.Revoke(ctx, claims.ID, remainingTTL)
//
//   关键洞察：
//     · 一个 jti 吊销后，只需活到原 exp 时间即可（过期了本来就会被 ValidateToken 拒绝）
//     · 例如 token 还有 2h 就过期，吊销 key 只存 2h
//     · Redis 自动清理过期 key，存储自然稳定在"当前活跃 session 数"量级
//
//   实测（相同场景）：
//     方案              存储大小       查询延迟     清理方式
//     ────────────    ──────────    ──────────   ────────
//     单个大 Set        5GB           0.5ms        手动
//     独立 key+TTL      ~50MB         0.2ms        Redis 自动过期
//
// -----------------------------------------------------------------------------
//
// [案例三] 令牌桶限流的"时钟漂移"陷阱 + Lua 原子保障
//
//   分布式限流的天然难题：
//     · 10 个 Agent Pod 同时服务同一个用户
//     · 限流规则：每用户 10 rps
//     · 如果每个 Pod 维护本地计数器，合计可能达到 100 rps
//
//   错误做法：应用层维护桶状态
//
//     // Pod A
//     bucket := buckets[userID]
//     if bucket.tokens >= 1 {
//         bucket.tokens--
//         allow()
//     }
//
//   → 每 Pod 独立桶，限流失效。
//
//   分布式共享：全部操作集中到 Redis
//
//     GET rl:user:123:tokens   // 查
//     DECR rl:user:123:tokens  // 减
//     ...
//
//   → 存在竞态：两个 Pod 同时 GET 拿到 tokens=1，都 DECR，实际放过 2 个请求。
//
//   Lua 脚本原子化（本包采用）：
//
//     -- 令牌桶 Lua 脚本（Redis 单线程原子执行）
//     -- KEYS[1] = rate limit key
//     -- ARGV[1] = burst (桶容量)
//     -- ARGV[2] = rate (每秒补充速率)
//     -- ARGV[3] = now (当前秒)
//     local key = KEYS[1]
//     local burst = tonumber(ARGV[1])
//     local rate  = tonumber(ARGV[2])
//     local now   = tonumber(ARGV[3])
//
//     -- 读取桶状态（tokens 和上次更新时间）
//     local state = redis.call('HMGET', key, 'tokens', 'ts')
//     local tokens = tonumber(state[1]) or burst
//     local ts     = tonumber(state[2]) or now
//
//     -- 按时间差补充 tokens
//     local elapsed = math.max(0, now - ts)
//     tokens = math.min(burst, tokens + elapsed * rate)
//
//     -- 判断是否放行
//     if tokens < 1 then
//         local retryAfter = math.ceil((1 - tokens) / rate)
//         return {0, retryAfter}   -- 拒绝
//     else
//         tokens = tokens - 1
//         redis.call('HMSET', key, 'tokens', tokens, 'ts', now)
//         redis.call('EXPIRE', key, 3600)
//         return {1, 0}            -- 放行
//     end
//
//   Go 端调用：
//
//     type RateLimiter struct {
//         rdb    redis.UniversalClient
//         script *redis.Script  // preload once
//     }
//
//     func NewRateLimiter(rdb redis.UniversalClient) *RateLimiter {
//         return &RateLimiter{
//             rdb: rdb,
//             script: redis.NewScript(tokenBucketLua),
//         }
//     }
//
//     func (r *RateLimiter) Allow(ctx context.Context, key string, burst, rate int) (bool, time.Duration, error) {
//         now := time.Now().Unix()
//         result, err := r.script.Run(ctx, r.rdb,
//             []string{key}, burst, rate, now,
//         ).Slice()
//         if err != nil { return false, 0, err }
//
//         allowed := result[0].(int64) == 1
//         retryAfter := time.Duration(result[1].(int64)) * time.Second
//         return allowed, retryAfter, nil
//     }
//
//   中间件：
//
//     func RateLimitMiddleware(r *RateLimiter, scope string) gin.HandlerFunc {
//         return func(c *gin.Context) {
//             claims := c.MustGet("claims").(*Claims)
//             key := fmt.Sprintf("rl:%s:%s:%s", scope, claims.TenantID, claims.UserID)
//
//             allowed, retryAfter, err := r.Allow(c, key, 30, 10)  // burst=30 rate=10rps
//             if err != nil {
//                 c.Next()  // Redis 挂了时 fail open，避免误伤
//                 return
//             }
//             if !allowed {
//                 c.Header("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
//                 c.JSON(429, gin.H{"error": "rate_limited", "retry_after": retryAfter})
//                 c.Abort()
//                 return
//             }
//             c.Next()
//         }
//     }
//
//   测试验证（100 Pod × 200 并发压同一用户）：
//     · 无 Lua：实际通过率 ~1000 rps（限流失效）
//     · Lua 原子：实际通过率精确 10 rps（符合预期）
//
// -----------------------------------------------------------------------------
//
// [案例四] API Key vs JWT —— 为什么 CI/CD 不用 JWT
//
//   场景：Agent 需要支持 GitHub Actions 调用的 API。
//
//   方案 A：CI 按账号密码 /login 换 JWT，带着 JWT 调用
//     问题：
//       · CI 里明文存账号密码风险高
//       · JWT 短期有效（1h），CI 作业跑 30min 可能超时
//       · 无法"一键吊销某个 CI 而不影响用户"
//       · 审计日志看不出这是"用户 A"还是"用户 A 的 CI"
//
//   方案 B：API Key（本包采用）
//
//     type APIKeyEntry struct {
//         KeyHash     string         // 不存明文，只存 sha256
//         Name        string         // "github-actions-prod"
//         OwnerID     string
//         Permissions []string       // ["agent:chat", "rag:search"]
//         CreatedAt   time.Time
//         LastUsedAt  time.Time
//         ExpiresAt   *time.Time     // 可选过期
//         Revoked     bool
//     }
//
//     // 创建 API key（返回明文一次，之后只存 hash）
//     func (s *APIKeyStore) Create(ownerID, name string, perms []string) (string, error) {
//         // 生成 32 字节随机
//         rawBytes := make([]byte, 32)
//         rand.Read(rawBytes)
//         key := "cak_" + base64.URLEncoding.EncodeToString(rawBytes)
//
//         hash := sha256.Sum256([]byte(key))
//         s.storage.Save(&APIKeyEntry{
//             KeyHash:     hex.EncodeToString(hash[:]),
//             Name:        name,
//             OwnerID:     ownerID,
//             Permissions: perms,
//             CreatedAt:   time.Now(),
//         })
//         return key, nil    // 明文只返回这一次，用户自己保存
//     }
//
//     // 验证
//     func (s *APIKeyStore) Validate(key string) (*APIKeyEntry, bool) {
//         hash := sha256.Sum256([]byte(key))
//         entry, ok := s.storage.Get(hex.EncodeToString(hash[:]))
//         if !ok || entry.Revoked { return nil, false }
//         if entry.ExpiresAt != nil && time.Now().After(*entry.ExpiresAt) {
//             return nil, false
//         }
//         // 异步更新 lastUsed（不阻塞请求）
//         go s.storage.UpdateLastUsed(entry.KeyHash, time.Now())
//         return entry, true
//     }
//
//   中间件优先级（AuthMiddleware）：
//
//     1. 先看 X-API-Key header → API Key 验证
//     2. 再看 Authorization: Bearer → JWT 验证
//     3. 都没有 → 401
//
//   好处：
//     · CI 用 API Key，永不过期（除非手动吊销）
//     · 每个 CI 一把独立 Key，粒度吊销 / 权限隔离
//     · 审计日志写 API Key name，一看就知道是谁
//     · 平台侧保存的是 hash，即使数据库泄漏也无法反推原 Key
//
//   额外增强：
//     · Key 前缀 `cak_xxx` 让 GitHub secret scanner 可以自动识别泄漏
//     · 监测 LastUsedAt，90 天不用的 Key 自动禁用
//     · 按 IP 白名单绑定 Key，进一步降低泄漏影响
//
// =============================================================================
//
// 14. 端到端数据流示例 —— 从 HTTP 请求到 RBAC 通过的一次完整鉴权
// -----------------------------------------------------------------------------
//
// 场景：用户 alice（role: developer, tenant: acme）在 Web UI 点
//      "执行部署到 staging" 按钮，请求 POST /api/tasks。
//
// ── Step 0：登录拿 JWT（前一阶段）──────────────────────────────────────
//
//   alice 输入账号密码 → POST /auth/login
//   后端校验通过，签发 JWT：
//
//     header  = {"alg":"HS256","typ":"JWT","kid":"k-2024-08"}
//     payload = {
//       sub:   "u-42",
//       email: "alice@acme.com",
//       ten:   "acme",
//       roles: ["developer","pr_reviewer"],
//       iat:   1714025000,
//       exp:   1714111400,   // 24h
//       jti:   "tok-f3a1b"
//     }
//     signature = HMAC-SHA256(base64(header) + "." + base64(payload), secretK-2024-08)
//
//   token = "eyJhbGciOi...<header>.eyJzdWIiOi...<payload>.4xYz...<sig>"
//
//   前端保存在 Cookie（HttpOnly + Secure + SameSite=Strict）。
//
// ── Step 1：浏览器发请求 ──────────────────────────────────────────────
//
//   POST /api/tasks HTTP/2
//   Host: agent.acme.com
//   Cookie: access_token=eyJhbGciOi...
//   Content-Type: application/json
//
//   {"type":"deploy","env":"staging","image":"acme/api:v2.3.7"}
//
// ── Step 2：Gin 中间件链 ──────────────────────────────────────────────
//
//   router.Use(
//       middleware.RequestID(),     // 生成 X-Request-ID 用于 tracing
//       middleware.RateLimit(),     // tenant 级别 QPS 限流
//       auth.JWTMiddleware(),       // ← 本模块
//       auth.RBACMiddleware(),      // ← 本模块
//       middleware.AuditLog(),      // 调用上下文落审计
//   )
//
// ── Step 3：JWTMiddleware 解析 ────────────────────────────────────────
//
//   func JWTMiddleware() gin.HandlerFunc {
//       return func(c *gin.Context) {
//           raw, _ := c.Cookie("access_token")
//           if raw == "" { unauth(c, "missing token"); return }
//
//           // 3.1 Parse header to get kid
//           parts := strings.Split(raw, ".")
//           headerB, _ := base64.RawURLEncoding.DecodeString(parts[0])
//           var hdr struct{ Kid string `json:"kid"` }
//           json.Unmarshal(headerB, &hdr)
//
//           // 3.2 Pick key from keystore
//           key, ok := keyStore.Get(hdr.Kid)  // in-memory map, hot rotatable
//           if !ok { unauth(c, "unknown kid"); return }
//
//           // 3.3 Validate signature
//           token, err := jwt.Parse(raw, func(t *jwt.Token) (any, error) {
//               if t.Method != jwt.SigningMethodHS256 {
//                   return nil, errors.New("alg mismatch")
//               }
//               return key, nil
//           })
//           if err != nil || !token.Valid {
//               unauth(c, fmt.Sprintf("invalid token: %v", err))
//               return
//           }
//
//           claims := token.Claims.(jwt.MapClaims)
//
//           // 3.4 Expiry check (lib does, but double check)
//           if float64(time.Now().Unix()) > claims["exp"].(float64) {
//               unauth(c, "expired"); return
//           }
//
//           // 3.5 Revocation check (Redis SET jti:revoked:xxx)
//           jti := claims["jti"].(string)
//           if revoked, _ := redis.SIsMember(ctx, "jti:revoked", jti).Result(); revoked {
//               unauth(c, "token revoked"); return
//           }
//
//           // 3.6 注入 context
//           c.Set("user_id",  claims["sub"])
//           c.Set("tenant",   claims["ten"])
//           c.Set("roles",    claims["roles"])
//           c.Set("jti",      jti)
//           c.Next()
//       }
//   }
//
//   解析成功，claims 注入 Gin ctx。
//   耗时：0.3ms（HS256 纯 CPU + Redis 小 SIsMember RTT 0.2ms）
//
// ── Step 4：RBACMiddleware 权限判定 ───────────────────────────────────
//
//   路由注册时声明所需权限：
//
//     router.POST("/api/tasks",
//         auth.RequirePermission("task:create"),  // ← 标签
//         taskHandler.Create,
//     )
//
//   中间件实现：
//
//   func RequirePermission(perm string) gin.HandlerFunc {
//       return func(c *gin.Context) {
//           roles := c.MustGet("roles").([]any)
//           tenant := c.MustGet("tenant").(string)
//
//           // 4.1 Casbin enforce
//           for _, r := range roles {
//               ok, _ := enforcer.Enforce(tenant+":"+r.(string), "task", "create")
//               if ok { c.Next(); return }
//           }
//
//           // 4.2 特殊策略：ABAC 补充（基于请求属性）
//           body, _ := c.GetRawData()
//           c.Request.Body = io.NopCloser(bytes.NewBuffer(body)) // rewind
//           var req CreateTaskReq
//           json.Unmarshal(body, &req)
//           if req.Env == "prod" {
//               // 生产部署需要 senior 角色
//               if !containsRole(roles, "senior_developer") {
//                   forbid(c, "prod deploy requires senior_developer"); return
//               }
//           }
//
//           forbid(c, "permission denied: need "+perm)
//       }
//   }
//
//   Casbin 规则查询（内存索引，~10μs）：
//     p, acme:developer, task, create, allow
//     p, acme:pr_reviewer, pr, review, allow
//     p, acme:sre, deploy, execute, allow
//
//   alice 的角色 developer 匹配 (task, create) → 允许通过。
//
// ── Step 5：Handler 业务逻辑 ──────────────────────────────────────────
//
//   func (h *TaskHandler) Create(c *gin.Context) {
//       userID := c.MustGet("user_id").(string)
//       tenant := c.MustGet("tenant").(string)
//
//       // 5.1 租户隔离 —— 所有后端调用都带 tenant
//       ctx := WithTenant(c.Request.Context(), tenant)
//       ctx = WithUserID(ctx, userID)
//
//       // 5.2 调 orchestrator
//       taskID, err := h.orchestrator.Submit(ctx, &Task{
//           Type:   req.Type,
//           Env:    req.Env,
//           Image:  req.Image,
//           UserID: userID,
//           Tenant: tenant,
//       })
//
//       // 5.3 审计日志（独立于 middleware，业务级）
//       audit.Log(ctx, AuditEvent{
//           Actor:    userID,
//           Tenant:   tenant,
//           Action:   "task.create",
//           Resource: "task:" + taskID,
//           Outcome:  errOutcome(err),
//       })
//
//       if err != nil { c.JSON(500, ...); return }
//       c.JSON(202, gin.H{"task_id": taskID})
//   }
//
// ── Step 6：下游调用链都带着身份 ─────────────────────────────────────
//
//   ctx 向下传递：
//     orchestrator → skill.Registry.Invoke(ctx, tc)
//       → 检查 skill.RiskLevel 与 ctx 中 roles 匹配
//     orchestrator → rag.Search(ctx, {tenant: "acme"})
//       → Qdrant 查询加 tenant 过滤
//     orchestrator → session.AddMessage(ctx, ...)
//       → Redis key 加 tenant 前缀
//     orchestrator → temporal.StartWorkflow(ctx, ...)
//       → Workflow Input 含 userID/tenant，后续审计可查
//
//   每一层都"带着护照"，避免跨租户数据泄露。
//
// ── Step 7：响应与审计 ────────────────────────────────────────────────
//
//   HTTP 202 Accepted
//   {"task_id":"task-deploy-9f3a1b","status":"submitted"}
//
//   同时异步写入审计：
//     PG audit_logs:
//       (ts, actor="u-42", tenant="acme", action="task.create",
//        resource="task:deploy-9f3a1b", outcome="success",
//        trace_id="req-xxxx", ip="10.1.2.3", user_agent="...")
//
// ── 异常分支：Token 失效 ──────────────────────────────────────────────
//
//   Scenario A: JWT 过期 (exp < now)
//     → JWTMiddleware unauth(c, "expired")
//     → 返回 401 {error:"token expired, please refresh"}
//     → 前端自动调 /auth/refresh（用 refresh_token）拿新 JWT 后重试
//
//   Scenario B: JWT 被撤销（用户改密码/管理员踢人）
//     → Redis SADD jti:revoked "tok-f3a1b"
//     → 下次请求 SIsMember 命中 → 401 revoked
//     → 用户被迫重登
//
//   Scenario C: 签名被篡改
//     → jwt.Parse 返回 signature invalid
//     → 401，同时触发告警（WAF 可能介入）
//
// ── 异常分支：RBAC 拒绝 ───────────────────────────────────────────────
//
//   alice 尝试部署到 prod（req.env="prod"）：
//     RBACMiddleware 发现缺 senior_developer → 403 Forbidden
//     审计记录：
//       action="task.create.prod", outcome="denied", reason="insufficient_role"
//     告警：同一用户 1 分钟内 >3 次 403 → 触发安全侧关注
//
// ── 整体数据形变 ──────────────────────────────────────────────────────
//
//   [登录]
//   username/password → HMAC-SHA256 签名 → JWT token
//     ↓ Cookie(HttpOnly)
//
//   [请求]
//   HTTP POST + Cookie → Gin → JWTMiddleware
//     ↓ base64 decode + HMAC verify + exp check + revocation check
//   claims {sub, ten, roles, jti} → gin.Context
//     ↓ RBACMiddleware
//   casbin enforce(tenant+role, resource, action)
//     ↓ 允许
//   Handler → ctx 注入 tenant/userID
//     ↓
//   下游所有 (orchestrator, skill, rag, temporal) 调用都带 ctx
//     ↓
//   审计日志双写（middleware + 业务级）
//
//   [失败分支]
//   Token expired → 401 refresh
//   Token revoked → 401 force relogin
//   Permission denied → 403 audit + alert
//
//   关键指标：
//     · JWT 解析：~0.3ms (内存 key + Redis jti 检查)
//     · RBAC enforce：~10μs (Casbin 内存索引)
//     · Zero-trust：每个下游调用都重新检查（纵深防御）
//     · 审计覆盖：success + denied 全记录，合规可审计
//
// =============================================================================

package auth
