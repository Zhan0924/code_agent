// redis_revocation.go — JWT 撤销黑名单的 Redis 持久化。
//
// ============================================================================
//
//	设 计 原 理
//
// ============================================================================
//
// 【为什么 JWT 需要撤销列表】
//
//	JWT 的本质缺点：签发后 Pod 无法主动作废——没有服务端存储就没办法知道
//	这个 token 是否被管理员吊销。常见解决方案：
//	  · 用很短 TTL（15 min）+ 刷新 token：把"撤销窗口"压缩到最小
//	  · 维护撤销黑名单：ValidateToken 时查一次
//	本实现二者都支持。黑名单 key 是 jti（JWT ID，签发时随机生成），
//	TTL 设为 token 的 exp - now，这样黑名单条目自动随 token 过期一起清理，
//	不用手动 GC。
//
// 【为什么用 Redis 而不是 Postgres】
//
//	撤销检查要在每个请求走一遍，Redis 亚毫秒 RTT，Postgres 小几毫秒。
//	Redis 的 TTL 自动过期也比 PG 省事（PG 需要定时任务删过期行）。
//
// 【和内存黑名单的关系】
//
//	JWTManager 本身还保留了一个 map[jti]time.Time 的内存黑名单。原因：
//	·  Redis 临时不可用时不能让所有吊销操作失效；
//	·  开发环境 / 单元测试无 Redis；
//	生产环境读写都优先 Redis，内存版作为 L1 缓存。
//
// ============================================================================
package auth

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisRevocationStore persists JWT revocations in Redis so they survive restarts
// and work across multiple pods.
type RedisRevocationStore struct {
	rdb    *redis.Client
	prefix string
}

// NewRedisRevocationStore creates a Redis-backed token revocation store.
func NewRedisRevocationStore(rdb *redis.Client) *RedisRevocationStore {
	return &RedisRevocationStore{rdb: rdb, prefix: "jwt:revoked:"}
}

// Revoke marks a token JTI as revoked with an expiry matching the token's lifetime.
func (s *RedisRevocationStore) Revoke(ctx context.Context, jti string, ttl time.Duration) error {
	return s.rdb.Set(ctx, s.prefix+jti, "1", ttl).Err()
}

// IsRevoked checks if a token JTI has been revoked.
func (s *RedisRevocationStore) IsRevoked(ctx context.Context, jti string) bool {
	val, err := s.rdb.Exists(ctx, s.prefix+jti).Result()
	return err == nil && val > 0
}

// JWTManagerWithRedis extends JWTManager to use Redis for revocation checks.
type JWTManagerWithRedis struct {
	*JWTManager
	redisRevoke *RedisRevocationStore
}

// NewJWTManagerWithRedis creates a JWT manager with Redis-backed revocation.
func NewJWTManagerWithRedis(mgr *JWTManager, rdb *redis.Client) *JWTManagerWithRedis {
	return &JWTManagerWithRedis{
		JWTManager:  mgr,
		redisRevoke: NewRedisRevocationStore(rdb),
	}
}

// ValidateToken overrides the base ValidateToken to check Redis revocation.
func (m *JWTManagerWithRedis) ValidateToken(tokenString string) (*Claims, error) {
	claims, err := m.JWTManager.ValidateToken(tokenString)
	if err != nil {
		return nil, err
	}

	// Check Redis revocation (survives restarts, works cross-pod)
	if m.redisRevoke.IsRevoked(context.Background(), claims.ID) {
		return nil, ErrTokenInvalid
	}

	return claims, nil
}

// RevokeToken persists revocation to both memory and Redis.
func (m *JWTManagerWithRedis) RevokeToken(ctx context.Context, jti string, ttl time.Duration) error {
	m.JWTManager.RevokeToken(jti)
	return m.redisRevoke.Revoke(ctx, jti, ttl)
}
