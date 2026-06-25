package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// RedisCoreMemory implements CoreMemoryManager using Redis.
type RedisCoreMemory struct {
	client *redis.Client
	logger *zap.Logger
}

// NewRedisCoreMemory creates a new CoreMemoryManager backed by Redis.
func NewRedisCoreMemory(client *redis.Client, logger *zap.Logger) *RedisCoreMemory {
	return &RedisCoreMemory{
		client: client,
		logger: logger.With(zap.String("component", "memory.core_memory")),
	}
}

func (r *RedisCoreMemory) key(userID, projectID string) string {
	return fmt.Sprintf("core_memory:%s:%s", userID, projectID)
}

// GetCoreMemory retrieves the core memory from Redis, initializing it if not exists.
func (r *RedisCoreMemory) GetCoreMemory(ctx context.Context, userID, projectID string) (*CoreMemory, error) {
	data, err := r.client.Get(ctx, r.key(userID, projectID)).Bytes()
	if err == redis.Nil {
		return &CoreMemory{
			UserID:    userID,
			ProjectID: projectID,
			Sections: map[string]*CoreMemorySection{
				"persona":         {Name: "persona", Content: "I am a helpful AI coding assistant."},
				"human_context":   {Name: "human_context", Content: ""},
				"project_context": {Name: "project_context", Content: ""},
			},
		}, nil
	} else if err != nil {
		return nil, err
	}

	var mem CoreMemory
	if err := json.Unmarshal(data, &mem); err != nil {
		return nil, err
	}
	return &mem, nil
}

func (r *RedisCoreMemory) saveCoreMemory(ctx context.Context, mem *CoreMemory) error {
	data, err := json.Marshal(mem)
	if err != nil {
		return err
	}
	// Core memory persists for a long time, e.g., 30 days of inactivity
	return r.client.Set(ctx, r.key(mem.UserID, mem.ProjectID), data, 30*24*time.Hour).Err()
}

// AppendToSection appends text to a specific section.
func (r *RedisCoreMemory) AppendToSection(ctx context.Context, userID, projectID, section, content string) error {
	mem, err := r.GetCoreMemory(ctx, userID, projectID)
	if err != nil {
		return err
	}

	sec, ok := mem.Sections[section]
	if !ok {
		sec = &CoreMemorySection{Name: section, Content: ""}
		mem.Sections[section] = sec
	}

	if sec.Content != "" {
		sec.Content += "\n"
	}
	sec.Content += content

	return r.saveCoreMemory(ctx, mem)
}

// ReplaceInSection replaces an exact old string with a new string in a specific section.
func (r *RedisCoreMemory) ReplaceInSection(ctx context.Context, userID, projectID, section, oldContent, newContent string) error {
	mem, err := r.GetCoreMemory(ctx, userID, projectID)
	if err != nil {
		return err
	}

	sec, ok := mem.Sections[section]
	if !ok {
		return fmt.Errorf("section %s not found in core memory", section)
	}

	if !strings.Contains(sec.Content, oldContent) {
		return fmt.Errorf("old content not found in section %s", section)
	}

	sec.Content = strings.Replace(sec.Content, oldContent, newContent, 1)
	return r.saveCoreMemory(ctx, mem)
}
