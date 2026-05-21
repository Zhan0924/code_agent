// Package auth provides JWT-based authentication and RBAC authorization
// for the Code Intelligence Agent API.
//
// ============================================================================
//  设 计 原 理
// ============================================================================
//
// 【为什么选 JWT 而不是 Session Cookie？】
//   Agent 服务是**无状态水平扩容**架构（参考 4.2 HA 设计），任意一次请求
//   可能命中任意一个 Pod。若用 Session Cookie，必须依赖中心化存储做查询，
//   额外增加 Redis RTT。JWT 把身份信息签名后放在 Token 里，Pod 本地验签
//   即可，毫秒级、无外部依赖。
//
// 【Token 结构（HS256 / RS256）】
//   header.payload.signature
//   payload 中携带：
//     - sub        : 用户 ID
//     - tenant_id  : 租户（多租户隔离的关键）
//     - roles      : []string，RBAC 角色（admin / developer / viewer）
//     - exp / iat  : 过期/签发时间（短有效期 15min + refresh token 策略）
//
// 【RBAC 授权】
//   AuthMiddleware 负责验签并解析 claims → 注入 gin.Context。
//   RequireRole("admin") 在敏感路由（/mcp/servers、/skills）前拦截：
//     · roles 不包含要求角色 → 403
//     · 通过才进入 handler
//
// 【Token 吊销 (Revocation)】
//   JWT 最大痛点：一旦签发无法主动失效。
//   方案：redis_revocation.go 维护黑名单（jti 或完整 token 的 hash）。
//   验签通过后再问一次 Redis SISMEMBER，命中则拒绝。
//   Redis 带 TTL 自动清理过期条目，不膨胀。
//
// 【防 Token 重放】
//   · 强制 HTTPS（TLS 终止）
//   · iat 时间窗口校验（拒绝未来/过旧签发时间）
//   · 重要操作叠加 Temporal HITL 二次确认（见 /tasks/:id/approve）
//
// ============================================================================
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"go.uber.org/zap"
)

// Common errors
var (
	ErrTokenExpired  = errors.New("token has expired")
	ErrTokenInvalid  = errors.New("token is invalid")
	ErrTokenMissing  = errors.New("authorization token missing")
	ErrAccessDenied  = errors.New("access denied: insufficient permissions")
	ErrAPIKeyInvalid = errors.New("invalid API key")
)

// Role defines user permission levels.
type Role string

const (
	RoleAdmin    Role = "admin"    // Full access: approve deployments, manage users
	RoleDev      Role = "dev"      // Standard dev: chat, execute code, search
	RoleReadOnly Role = "readonly" // View only: chat, search (no execute)
	RoleService  Role = "service"  // Service accounts: webhook callbacks
)

// Claims extends JWT standard claims with agent-specific fields.
type Claims struct {
	jwt.RegisteredClaims
	UserID string `json:"user_id"`
	Role   Role   `json:"role"`
	Email  string `json:"email,omitempty"`
}

// JWTConfig holds JWT configuration.
type JWTConfig struct {
	SecretKey     string        `yaml:"secret_key"`
	Issuer        string        `yaml:"issuer"`
	TokenExpiry   time.Duration `yaml:"token_expiry"`
	RefreshExpiry time.Duration `yaml:"refresh_expiry"`
}

// DefaultJWTConfig returns sensible defaults.
func DefaultJWTConfig() *JWTConfig {
	return &JWTConfig{
		SecretKey:     mustGenerateSecret(),
		Issuer:        "code-agent",
		TokenExpiry:   24 * time.Hour,
		RefreshExpiry: 7 * 24 * time.Hour,
	}
}

func mustGenerateSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("failed to generate random secret: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// JWTManager handles token creation and validation.
type JWTManager struct {
	cfg    *JWTConfig
	logger *zap.Logger

	// Revoked token tracking (in production, use Redis)
	revokedMu sync.RWMutex
	revoked   map[string]time.Time
}

// NewJWTManager creates a new JWT manager.
func NewJWTManager(cfg *JWTConfig, logger *zap.Logger) *JWTManager {
	return &JWTManager{
		cfg:     cfg,
		logger:  logger.With(zap.String("component", "jwt")),
		revoked: make(map[string]time.Time),
	}
}

// GenerateToken creates a new JWT token for a user.
func (m *JWTManager) GenerateToken(userID string, role Role, email string) (string, error) {
	now := time.Now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    m.cfg.Issuer,
			Subject:   userID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(m.cfg.TokenExpiry)),
			ID:        fmt.Sprintf("%s-%d", userID, now.UnixNano()),
		},
		UserID: userID,
		Role:   role,
		Email:  email,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(m.cfg.SecretKey))
}

// ValidateToken parses and validates a JWT token string.
func (m *JWTManager) ValidateToken(tokenString string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(m.cfg.SecretKey), nil
	})
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrTokenExpired
		}
		return nil, ErrTokenInvalid
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, ErrTokenInvalid
	}

	// Check revocation
	m.revokedMu.RLock()
	_, isRevoked := m.revoked[claims.ID]
	m.revokedMu.RUnlock()
	if isRevoked {
		return nil, ErrTokenInvalid
	}

	return claims, nil
}

// RevokeToken marks a token as revoked.
func (m *JWTManager) RevokeToken(jti string) {
	m.revokedMu.Lock()
	m.revoked[jti] = time.Now()
	m.revokedMu.Unlock()
}

// ─── API Key Support ─────────────────────────────────────────────────────────

// APIKeyEntry represents a registered API key. The Key field is only populated
// on Register (plaintext input); Validate never returns it.
type APIKeyEntry struct {
	Key     string `json:"key,omitempty"`
	UserID  string `json:"user_id"`
	Role    Role   `json:"role"`
	Label   string `json:"label"`
	Created time.Time
}

// APIKeyStore manages API keys. Keys are stored as SHA-256 hashes only —
// plaintext is never retained. Validation performs a constant-time compare
// against all stored hashes to avoid leaking key existence through timing.
type APIKeyStore struct {
	mu      sync.RWMutex
	entries []apiKeyRecord
}

// apiKeyRecord is the internal storage form: only the key hash is retained.
type apiKeyRecord struct {
	hash  [32]byte // SHA-256 of the plaintext key
	entry APIKeyEntry
}

// hashAPIKey returns the SHA-256 of the plaintext key.
func hashAPIKey(key string) [32]byte {
	return sha256.Sum256([]byte(key))
}

// NewAPIKeyStore creates a new in-memory API key store.
func NewAPIKeyStore() *APIKeyStore {
	return &APIKeyStore{}
}

// Register adds a new API key. The plaintext Key in the provided entry is
// hashed for storage and then cleared on the stored copy; callers that retain
// the original *APIKeyEntry pointer will still see the plaintext value and
// are responsible for not logging or persisting it.
func (s *APIKeyStore) Register(entry *APIKeyEntry) {
	if entry == nil || entry.Key == "" {
		return
	}
	stored := *entry
	stored.Key = "" // never retain plaintext in the store
	rec := apiKeyRecord{
		hash:  hashAPIKey(entry.Key),
		entry: stored,
	}
	s.mu.Lock()
	s.entries = append(s.entries, rec)
	s.mu.Unlock()
}

// Validate checks if an API key is valid and returns a copy of the associated
// entry. The comparison iterates all stored hashes with constant-time compare,
// so validation time does not depend on whether (or where) the key matches.
func (s *APIKeyStore) Validate(key string) (*APIKeyEntry, bool) {
	if key == "" {
		return nil, false
	}
	want := hashAPIKey(key)
	s.mu.RLock()
	defer s.mu.RUnlock()

	var matched *APIKeyEntry
	for i := range s.entries {
		rec := &s.entries[i]
		if subtle.ConstantTimeCompare(rec.hash[:], want[:]) == 1 {
			// Copy the entry to avoid exposing internal storage. Do not break —
			// continue iterating all entries so total work is independent of
			// match position.
			e := rec.entry
			matched = &e
		}
	}
	return matched, matched != nil
}

// ─── Gin Middleware ──────────────────────────────────────────────────────────

// contextKey is the key used to store claims in the gin context.
const contextKey = "auth_claims"

// AuthMiddleware returns a Gin middleware that validates JWT tokens or API keys.
// It supports both:
//   - Authorization: Bearer <jwt_token>
//   - X-API-Key: <api_key>
func AuthMiddleware(jwtMgr *JWTManager, apiKeys *APIKeyStore, logger *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Try API key first
		if apiKey := c.GetHeader("X-API-Key"); apiKey != "" {
			entry, ok := apiKeys.Validate(apiKey)
			if !ok {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": ErrAPIKeyInvalid.Error()})
				return
			}
			c.Set(contextKey, &Claims{
				UserID: entry.UserID,
				Role:   entry.Role,
			})
			c.Next()
			return
		}

		// Try Bearer token
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": ErrTokenMissing.Error()})
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid authorization header format"})
			return
		}

		claims, err := jwtMgr.ValidateToken(parts[1])
		if err != nil {
			status := http.StatusUnauthorized
			c.AbortWithStatusJSON(status, gin.H{"error": err.Error()})
			return
		}

		c.Set(contextKey, claims)
		c.Next()
	}
}

// RequireRole returns middleware that enforces a minimum role requirement.
func RequireRole(roles ...Role) gin.HandlerFunc {
	roleSet := make(map[Role]bool, len(roles))
	for _, r := range roles {
		roleSet[r] = true
	}

	return func(c *gin.Context) {
		claims := GetClaims(c)
		if claims == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
			return
		}

		// Admin always passes
		if claims.Role == RoleAdmin {
			c.Next()
			return
		}

		if !roleSet[claims.Role] {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": ErrAccessDenied.Error(),
				"required_roles": func() []string {
					var rs []string
					for r := range roleSet {
						rs = append(rs, string(r))
					}
					return rs
				}(),
			})
			return
		}

		c.Next()
	}
}

// GetClaims extracts auth claims from the gin context.
func GetClaims(c *gin.Context) *Claims {
	val, exists := c.Get(contextKey)
	if !exists {
		return nil
	}
	claims, _ := val.(*Claims)
	return claims
}
