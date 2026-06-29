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
	masker *PIIMasker
}

// NewRedisCoreMemory creates a new CoreMemoryManager backed by Redis.
func NewRedisCoreMemory(client *redis.Client, logger *zap.Logger) *RedisCoreMemory {
	return &RedisCoreMemory{
		client: client,
		logger: logger.With(zap.String("component", "memory.core_memory")),
		masker: NewPIIMasker(),
	}
}

// key returns the Redis key for a (scope, userID, projectID) tuple.
//
// Scope encoding:
//   - project → "core_memory:project:<userID>:<projectID>"
//   - user    → "core_memory:user:<userID>"  (projectID ignored)
//
// The legacy keyspace "core_memory:<userID>:<projectID>" (no scope prefix)
// is aliased to project scope on read so existing deployments are not
// orphaned. On write we always use the new keyspace; the alias only
// applies during reads on the project scope.
func (r *RedisCoreMemory) key(scope CoreMemoryScope, userID, projectID string) string {
	switch scope {
	case CoreScopeUser:
		return fmt.Sprintf("core_memory:user:%s", userID)
	default:
		return fmt.Sprintf("core_memory:project:%s:%s", userID, projectID)
	}
}

func (r *RedisCoreMemory) legacyKey(userID, projectID string) string {
	return fmt.Sprintf("core_memory:%s:%s", userID, projectID)
}

// defaultSections returns the initial sections for a freshly-created core
// memory. Default content lives only at project scope; user scope starts
// empty so user-global state must be explicitly opted into.
func defaultSections(scope CoreMemoryScope) map[string]*CoreMemorySection {
	if scope == CoreScopeUser {
		return map[string]*CoreMemorySection{}
	}
	return map[string]*CoreMemorySection{
		"persona":         {Name: "persona", Content: "I am a helpful AI coding assistant."},
		"human_context":   {Name: "human_context", Content: ""},
		"project_context": {Name: "project_context", Content: ""},
	}
}

// GetCoreMemory retrieves the project-scoped core memory (backward-compatible).
func (r *RedisCoreMemory) GetCoreMemory(ctx context.Context, userID, projectID string) (*CoreMemory, error) {
	return r.GetCoreMemoryScoped(ctx, userID, projectID, CoreScopeProject)
}

// GetCoreMemoryScoped retrieves core memory at a specific scope, initializing
// defaults if the key does not yet exist.
func (r *RedisCoreMemory) GetCoreMemoryScoped(ctx context.Context, userID, projectID string, scope CoreMemoryScope) (*CoreMemory, error) {
	if !scope.IsValid() {
		return nil, fmt.Errorf("invalid core memory scope: %q", scope)
	}

	primary := r.key(scope, userID, projectID)
	data, err := r.client.Get(ctx, primary).Bytes()
	if err == redis.Nil && scope == CoreScopeProject {
		// Legacy alias read: prior versions wrote without a scope prefix.
		// Try the legacy key once before declaring "new memory".
		if legacy, lerr := r.client.Get(ctx, r.legacyKey(userID, projectID)).Bytes(); lerr == nil {
			data = legacy
			err = nil
		}
	}
	if err == redis.Nil {
		return &CoreMemory{
			UserID:    userID,
			ProjectID: projectID,
			Sections:  defaultSections(scope),
		}, nil
	} else if err != nil {
		return nil, err
	}

	var mem CoreMemory
	if err := json.Unmarshal(data, &mem); err != nil {
		return nil, err
	}
	if mem.Sections == nil {
		mem.Sections = map[string]*CoreMemorySection{}
	}
	return &mem, nil
}

// GetMerged returns the project-scoped CoreMemory overlaid on top of the
// user-scoped one. Project sections override user sections of the same
// name; the returned sections carry their origin Scope so the caller can
// annotate the prompt if desired.
func (r *RedisCoreMemory) GetMerged(ctx context.Context, userID, projectID string) (*CoreMemory, error) {
	userMem, err := r.GetCoreMemoryScoped(ctx, userID, projectID, CoreScopeUser)
	if err != nil {
		return nil, err
	}
	projMem, err := r.GetCoreMemoryScoped(ctx, userID, projectID, CoreScopeProject)
	if err != nil {
		return nil, err
	}

	merged := &CoreMemory{
		UserID:    userID,
		ProjectID: projectID,
		Sections:  make(map[string]*CoreMemorySection, len(userMem.Sections)+len(projMem.Sections)),
	}
	for name, sec := range userMem.Sections {
		copyOf := *sec
		copyOf.Scope = CoreScopeUser
		merged.Sections[name] = &copyOf
	}
	for name, sec := range projMem.Sections {
		copyOf := *sec
		copyOf.Scope = CoreScopeProject
		merged.Sections[name] = &copyOf
	}
	return merged, nil
}

func (r *RedisCoreMemory) saveCoreMemory(ctx context.Context, scope CoreMemoryScope, mem *CoreMemory) error {
	data, err := json.Marshal(mem)
	if err != nil {
		return err
	}
	return r.client.Set(ctx, r.key(scope, mem.UserID, mem.ProjectID), data, 30*24*time.Hour).Err()
}

// AppendToSection appends to a project-scoped section (backward-compatible).
func (r *RedisCoreMemory) AppendToSection(ctx context.Context, userID, projectID, section, content string) error {
	return r.AppendToSectionScoped(ctx, userID, projectID, CoreScopeProject, section, content)
}

// AppendToSectionScoped appends to a section at the specified scope.
func (r *RedisCoreMemory) AppendToSectionScoped(ctx context.Context, userID, projectID string, scope CoreMemoryScope, section, content string) error {
	if !scope.IsValid() {
		return fmt.Errorf("invalid core memory scope: %q", scope)
	}
	mem, err := r.GetCoreMemoryScoped(ctx, userID, projectID, scope)
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
	sec.Content += r.maskForPersist("core_memory_append", section, userID, projectID, scope, content)

	return r.saveCoreMemory(ctx, scope, mem)
}

// maskForPersist applies PIIMasker before any core-memory write path.
// All writers (tools, extractor auto-promote, future backfill) funnel here.
func (r *RedisCoreMemory) maskForPersist(op, section, userID, projectID string, scope CoreMemoryScope, raw string) string {
	if r.masker == nil {
		return raw
	}
	masked := r.masker.Mask(raw)
	if masked != raw && r.logger != nil {
		r.logger.Info("core_memory content masked",
			zap.String("audit_id", "REAUDIT-P0-2"),
			zap.String("op", op),
			zap.String("tenant.user_id", userID),
			zap.String("tenant.project_id", projectID),
			zap.String("scope", string(scope)),
			zap.String("section", section),
			zap.Bool("pii_masked", true),
			zap.String("result", "ok"))
	}
	return masked
}

// ReplaceInSection replaces text in a project-scoped section (backward-compatible).
func (r *RedisCoreMemory) ReplaceInSection(ctx context.Context, userID, projectID, section, oldContent, newContent string) error {
	return r.ReplaceInSectionScoped(ctx, userID, projectID, CoreScopeProject, section, oldContent, newContent)
}

// ReplaceInSectionScoped replaces text in a section at the specified scope.
func (r *RedisCoreMemory) ReplaceInSectionScoped(ctx context.Context, userID, projectID string, scope CoreMemoryScope, section, oldContent, newContent string) error {
	if !scope.IsValid() {
		return fmt.Errorf("invalid core memory scope: %q", scope)
	}
	mem, err := r.GetCoreMemoryScoped(ctx, userID, projectID, scope)
	if err != nil {
		return err
	}

	sec, ok := mem.Sections[section]
	if !ok {
		return fmt.Errorf("section %s not found in core memory (scope=%s)", section, scope)
	}

	if !strings.Contains(sec.Content, oldContent) {
		return fmt.Errorf("old content not found in section %s (scope=%s)", section, scope)
	}

	maskedNew := r.maskForPersist("core_memory_replace", section, userID, projectID, scope, newContent)
	sec.Content = strings.Replace(sec.Content, oldContent, maskedNew, 1)
	return r.saveCoreMemory(ctx, scope, mem)
}
