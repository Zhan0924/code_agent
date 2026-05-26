package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// RedisHot stores recent memories (last 24h) in Redis with TTL auto-expiry.
type RedisHot struct {
	client *redis.Client
	ttl    time.Duration
	logger *zap.Logger
}

// NewRedisHot creates a hot memory store backed by Redis.
func NewRedisHot(client *redis.Client, logger *zap.Logger) *RedisHot {
	return &RedisHot{
		client: client,
		ttl:    24 * time.Hour,
		logger: logger.With(zap.String("component", "memory.redis_hot")),
	}
}

func (r *RedisHot) keyPrefix(userID, projectID string) string {
	return fmt.Sprintf("memory:%s:%s", userID, projectID)
}

// Store saves a memory to Redis with TTL.
func (r *RedisHot) Store(ctx context.Context, m *Memory) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	key := fmt.Sprintf("%s:%s", r.keyPrefix(m.UserID, m.ProjectID), m.ID)
	return r.client.Set(ctx, key, data, r.ttl).Err()
}

// Retrieve returns recent memories for a user/project from Redis.
func (r *RedisHot) Retrieve(ctx context.Context, userID, projectID string, limit int) ([]Memory, error) {
	pattern := r.keyPrefix(userID, projectID) + ":*"

	// Use SCAN instead of KEYS to avoid O(N) blocking on large Redis instances
	var keys []string
	iter := r.client.Scan(ctx, 0, pattern, int64(limit*2)).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
		if len(keys) >= limit*2 {
			break
		}
	}
	if err := iter.Err(); err != nil {
		return nil, err
	}

	if len(keys) == 0 {
		return nil, nil
	}

	// Sort by key (timestamp embedded) and take most recent
	sort.Strings(keys)
	if len(keys) > limit {
		keys = keys[len(keys)-limit:]
	}

	pipe := r.client.Pipeline()
	cmds := make([]*redis.StringCmd, len(keys))
	for i, key := range keys {
		cmds[i] = pipe.Get(ctx, key)
	}
	_, _ = pipe.Exec(ctx)

	var memories []Memory
	for _, cmd := range cmds {
		data, err := cmd.Result()
		if err != nil {
			continue
		}
		var m Memory
		if json.Unmarshal([]byte(data), &m) == nil {
			memories = append(memories, m)
		}
	}
	return memories, nil
}

// Delete removes a memory from Redis.
func (r *RedisHot) Delete(ctx context.Context, userID, projectID, memoryID string) error {
	key := fmt.Sprintf("%s:%s", r.keyPrefix(userID, projectID), memoryID)
	return r.client.Del(ctx, key).Err()
}

// RetrieveByQuery returns memories ranked by cosine similarity to the query embedding.
// Since hot layer is small (< 50 items with 24h TTL), in-memory ranking is acceptable.
func (r *RedisHot) RetrieveByQuery(ctx context.Context, userID, projectID string, queryEmbedding []float32, limit int) ([]Memory, error) {
	pattern := r.keyPrefix(userID, projectID) + ":*"

	var keys []string
	iter := r.client.Scan(ctx, 0, pattern, 100).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
		if len(keys) >= 50 {
			break
		}
	}
	if err := iter.Err(); err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, nil
	}

	pipe := r.client.Pipeline()
	cmds := make([]*redis.StringCmd, len(keys))
	for i, key := range keys {
		cmds[i] = pipe.Get(ctx, key)
	}
	_, _ = pipe.Exec(ctx)

	type scored struct {
		memory Memory
		sim    float64
	}
	var results []scored
	for _, cmd := range cmds {
		data, err := cmd.Result()
		if err != nil {
			continue
		}
		var m Memory
		if json.Unmarshal([]byte(data), &m) != nil {
			continue
		}
		if len(m.Embedding) == 0 {
			continue
		}
		sim := CosineSimilarity(queryEmbedding, m.Embedding)
		results = append(results, scored{memory: m, sim: sim})
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].sim > results[j].sim
	})

	memories := make([]Memory, 0, limit)
	for i := range results {
		if i >= limit {
			break
		}
		memories = append(memories, results[i].memory)
	}
	return memories, nil
}
