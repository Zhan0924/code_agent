// Package rag —— redis_embedding_cache
//
// Redis 持久化 Embedding 缓存层
// ============================================================================
//
// 背景：memLRUCache 只在单进程内有效，重启即失效。生产环境需要跨实例、
// 跨重启共享缓存，避免每次部署都要重新 embed 整个仓库。
//
// 实现：
//   - Key: `emb:{namespace}:{content_hash}` (namespace 通常是 embedding model 名)
//   - Value: []float32 序列化为 binary (encoding/binary LittleEndian)
//   - TTL: 7 天（足够覆盖一个 sprint 周期，过期后自动淘汰）
//
// 性能：
//   - Redis GET 延迟 ~1ms (局域网) vs OpenAI API ~150ms
//   - 单个 1536 维向量占用 6KB (1536 × 4 字节)
//   - 1 万条缓存 ≈ 60MB Redis 内存
//
// 并发安全：go-redis/v9 客户端本身线程安全，无需额外加锁。
package rag

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

const (
	// redisEmbeddingKeyPrefix 是所有 embedding 缓存 key 的前缀
	redisEmbeddingKeyPrefix = "emb"
	// redisEmbeddingTTL 是缓存过期时间（7 天）
	redisEmbeddingTTL = 7 * 24 * time.Hour
)

// RedisEmbeddingCache 实现基于 Redis 的 EmbeddingCache 接口。
type RedisEmbeddingCache struct {
	client *redis.Client
	ns     string // namespace (embedding model name)
	hits   atomic.Uint64
	misses atomic.Uint64
	logger *zap.Logger
}

// NewRedisEmbeddingCache 构造一个 Redis 缓存实例。
// namespace 建议填 embedding 模型名，防止模型切换后向量维度不匹配。
func NewRedisEmbeddingCache(client *redis.Client, namespace string, logger *zap.Logger) EmbeddingCache {
	return &RedisEmbeddingCache{
		client: client,
		ns:     namespace,
		logger: logger.With(zap.String("component", "redis_embedding_cache")),
	}
}

// Get 从 Redis 读取缓存的 embedding 向量。
func (c *RedisEmbeddingCache) Get(ctx context.Context, key string) ([]float32, bool) {
	fullKey := c.buildKey(key)
	data, err := c.client.Get(ctx, fullKey).Bytes()
	if err != nil {
		if err != redis.Nil {
			c.logger.Warn("redis get failed", zap.String("key", fullKey), zap.Error(err))
		}
		c.misses.Add(1)
		return nil, false
	}

	vec, err := deserializeFloat32(data)
	if err != nil {
		c.logger.Warn("deserialize failed", zap.String("key", fullKey), zap.Error(err))
		c.misses.Add(1)
		return nil, false
	}

	c.hits.Add(1)
	return vec, true
}

// Put 将 embedding 向量写入 Redis，带 TTL。
func (c *RedisEmbeddingCache) Put(ctx context.Context, key string, vec []float32) {
	fullKey := c.buildKey(key)
	data := serializeFloat32(vec)
	if err := c.client.Set(ctx, fullKey, data, redisEmbeddingTTL).Err(); err != nil {
		c.logger.Warn("redis set failed", zap.String("key", fullKey), zap.Error(err))
	}
}

// Stats 返回缓存统计信息。
// 注意：Redis 模式下 Size 返回 -1，因为获取准确 key 数量需要 SCAN 全库（代价高）。
func (c *RedisEmbeddingCache) Stats() CacheStats {
	return CacheStats{
		Hits:   c.hits.Load(),
		Misses: c.misses.Load(),
		Size:   -1, // Redis 模式下无法高效获取准确 size
	}
}

// buildKey 构造完整的 Redis key: emb:{namespace}:{content_hash}
func (c *RedisEmbeddingCache) buildKey(contentHash string) string {
	return fmt.Sprintf("%s:%s:%s", redisEmbeddingKeyPrefix, c.ns, contentHash)
}

// ─── Binary Serialization ────────────────────────────────────────────────────

// serializeFloat32 将 []float32 序列化为 binary (LittleEndian)。
// 格式：[length:4字节][f32_0:4字节][f32_1:4字节]...
func serializeFloat32(vec []float32) []byte {
	buf := make([]byte, 4+len(vec)*4)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(len(vec)))
	for i, v := range vec {
		binary.LittleEndian.PutUint32(buf[4+i*4:4+i*4+4], math.Float32bits(v))
	}
	return buf
}

// deserializeFloat32 从 binary 反序列化为 []float32。
func deserializeFloat32(data []byte) ([]float32, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("data too short: %d bytes", len(data))
	}
	length := binary.LittleEndian.Uint32(data[0:4])
	if len(data) != int(4+length*4) {
		return nil, fmt.Errorf("data length mismatch: expected %d, got %d", 4+length*4, len(data))
	}
	vec := make([]float32, length)
	for i := range vec {
		bits := binary.LittleEndian.Uint32(data[4+i*4 : 4+i*4+4])
		vec[i] = math.Float32frombits(bits)
	}
	return vec, nil
}
