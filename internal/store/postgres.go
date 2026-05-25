// Package store provides PostgreSQL-backed persistent storage for tasks,
// audit logs, and API keys. This replaces in-memory storage for production use.
//
// ============================================================================
//
//	设 计 原 理
//
// ============================================================================
//
// 【Postgres 存什么 / 不存什么】
//
//	存：
//	  · tasks           — 每个 chat 任务的完整状态（intent、state、task_id）
//	  · audit_logs      — 审计事件（谁在什么时候批准了什么任务）
//	  · api_keys        — 服务账号的 API Key 哈希（见 P0 #4）
//	不存：
//	  · session messages — 在 Redis；24h TTL 后丢弃没问题
//	  · RAG embeddings   — 在 Qdrant；Postgres 不适合高维向量
//	  · 实时指标         — Prometheus
//
// 【迁移策略】
//
//	Migrate() 在 NewStore 时自动跑，用 CREATE TABLE IF NOT EXISTS + 版本号
//	列。不引入 golang-migrate / goose 等工具——schema 演进少，简单的
//	`ALTER TABLE IF NOT EXISTS` 足够；避免部署时多一个 migration step。
//
// 【连接池参数】
//
//	MaxOpenConns=25：典型单 Pod 并发请求数上限；
//	MaxIdleConns=10：保持少量常驻连接避免冷启动；
//	ConnMaxLifetime=5min：强制轮换避免 DB 端 idle kill 导致的首次请求失败。
//
// 【Postgres 可选性】
//
//	main.go 里 store 构造失败只 Warn；orchestrator 必须能在 store=nil 时工作
//	（session 和核心状态都在 Redis）。store 只对"持久化审计"场景是必须的。
//
// ============================================================================
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "github.com/lib/pq" // PostgreSQL driver
	"go.uber.org/zap"
)

// PostgresConfig holds database connection configuration.
type PostgresConfig struct {
	Host         string `yaml:"host"`
	Port         int    `yaml:"port"`
	User         string `yaml:"user"`
	Password     string `yaml:"password"`
	Database     string `yaml:"database"`
	SSLMode      string `yaml:"ssl_mode"`
	MaxOpenConns int    `yaml:"max_open_conns"`
	MaxIdleConns int    `yaml:"max_idle_conns"`
}

// DSN returns the PostgreSQL connection string.
func (c *PostgresConfig) DSN() string {
	return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		c.Host, c.Port, c.User, c.Password, c.Database, c.SSLMode)
}

// Store provides PostgreSQL-backed persistence.
type Store struct {
	db     *sql.DB
	logger *zap.Logger
}

// NewStore creates a new PostgreSQL store and verifies connectivity.
func NewStore(cfg *PostgresConfig, logger *zap.Logger) (*Store, error) {
	db, err := sql.Open("postgres", cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	return &Store{db: db, logger: logger.With(zap.String("component", "store"))}, nil
}

// NewStoreFromDSN creates a new PostgreSQL store using a DSN connection string directly.
func NewStoreFromDSN(dsn string, maxOpen, maxIdle int, logger *zap.Logger) (*Store, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	if maxOpen > 0 {
		db.SetMaxOpenConns(maxOpen)
	}
	if maxIdle > 0 {
		db.SetMaxIdleConns(maxIdle)
	}
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	return &Store{db: db, logger: logger.With(zap.String("component", "store"))}, nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// Migrate runs database migrations to create required tables.
func (s *Store) Migrate(ctx context.Context) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS tasks (
			id          TEXT PRIMARY KEY,
			session_id  TEXT NOT NULL,
			user_id     TEXT NOT NULL DEFAULT '',
			intent      TEXT NOT NULL DEFAULT '',
			state       TEXT NOT NULL DEFAULT 'pending',
			user_input  TEXT NOT NULL,
			result      TEXT,
			plan_json   JSONB,
			created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			completed_at TIMESTAMPTZ
		)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_session ON tasks(session_id)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_state ON tasks(state)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_created ON tasks(created_at)`,

		`CREATE TABLE IF NOT EXISTS audit_logs (
			id          BIGSERIAL PRIMARY KEY,
			task_id     TEXT NOT NULL,
			user_id     TEXT NOT NULL DEFAULT '',
			action      TEXT NOT NULL,
			details     JSONB,
			risk_level  TEXT NOT NULL DEFAULT 'low',
			created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_task ON audit_logs(task_id)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_user ON audit_logs(user_id)`,

		`CREATE TABLE IF NOT EXISTS api_keys (
			key_hash    TEXT PRIMARY KEY,
			user_id     TEXT NOT NULL,
			role        TEXT NOT NULL DEFAULT 'dev',
			label       TEXT NOT NULL DEFAULT '',
			created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			last_used   TIMESTAMPTZ
		)`,
		`CREATE INDEX IF NOT EXISTS idx_apikeys_user ON api_keys(user_id)`,

		`CREATE TABLE IF NOT EXISTS approvals (
			task_id     TEXT PRIMARY KEY,
			session_id  TEXT NOT NULL,
			action      TEXT NOT NULL,
			risk_level  TEXT NOT NULL DEFAULT 'high',
			details     TEXT,
			status      TEXT NOT NULL DEFAULT 'pending',
			approved_by TEXT,
			created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			resolved_at TIMESTAMPTZ
		)`,
		`CREATE INDEX IF NOT EXISTS idx_approvals_status ON approvals(status)`,

		// Plan state persistence (added for existing databases)
		`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS plan_json JSONB`,

		// Dynamic tools table
		`CREATE TABLE IF NOT EXISTS dynamic_tools (
			name TEXT PRIMARY KEY,
			description TEXT NOT NULL,
			parameters JSONB NOT NULL,
			executor_type TEXT NOT NULL,
			executor_config JSONB NOT NULL,
			risk_level INTEGER NOT NULL DEFAULT 0,
			ttl BIGINT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_dynamic_tools_ttl ON dynamic_tools(created_at, ttl) WHERE ttl IS NOT NULL`,
	}

	for _, m := range migrations {
		if _, err := s.db.ExecContext(ctx, m); err != nil {
			return fmt.Errorf("migration failed: %w\nSQL: %s", err, m[:80])
		}
	}

	s.logger.Info("database migrations completed", zap.Int("count", len(migrations)))
	return nil
}

// ─── Task CRUD ───────────────────────────────────────────────────────────────

// TaskRecord represents a persisted task.
type TaskRecord struct {
	ID          string     `json:"id"`
	SessionID   string     `json:"session_id"`
	UserID      string     `json:"user_id"`
	Intent      string     `json:"intent"`
	State       string     `json:"state"`
	UserInput   string     `json:"user_input"`
	Result      *string    `json:"result,omitempty"`
	PlanJSON    *string    `json:"plan_json,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// CreateTask persists a new task record.
func (s *Store) CreateTask(ctx context.Context, t *TaskRecord) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tasks (id, session_id, user_id, intent, state, user_input, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		t.ID, t.SessionID, t.UserID, t.Intent, t.State, t.UserInput, t.CreatedAt, t.UpdatedAt)
	return err
}

// UpdateTaskState updates a task's state and optionally its result.
func (s *Store) UpdateTaskState(ctx context.Context, taskID, state string, result *string) error {
	now := time.Now()
	var completedAt *time.Time
	if state == "completed" || state == "failed" || state == "cancelled" {
		completedAt = &now
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET state=$2, result=$3, updated_at=$4, completed_at=$5 WHERE id=$1`,
		taskID, state, result, now, completedAt)
	return err
}

// GetTask retrieves a task by ID.
func (s *Store) GetTask(ctx context.Context, taskID string) (*TaskRecord, error) {
	var t TaskRecord
	err := s.db.QueryRowContext(ctx,
		`SELECT id, session_id, user_id, intent, state, user_input, result, plan_json, created_at, updated_at, completed_at
		 FROM tasks WHERE id=$1`, taskID).
		Scan(&t.ID, &t.SessionID, &t.UserID, &t.Intent, &t.State, &t.UserInput, &t.Result, &t.PlanJSON, &t.CreatedAt, &t.UpdatedAt, &t.CompletedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("task not found: %s", taskID)
	}
	return &t, err
}

// SavePlan persists a plan's JSON state for a given task.
func (s *Store) SavePlan(ctx context.Context, taskID string, planJSON []byte) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET plan_json=$2, updated_at=$3 WHERE id=$1`,
		taskID, planJSON, time.Now())
	return err
}

// LoadPlan retrieves the persisted plan JSON for a task.
func (s *Store) LoadPlan(ctx context.Context, taskID string) ([]byte, error) {
	var planJSON *string
	err := s.db.QueryRowContext(ctx,
		`SELECT plan_json FROM tasks WHERE id=$1`, taskID).Scan(&planJSON)
	if err != nil {
		return nil, err
	}
	if planJSON == nil {
		return nil, nil
	}
	return []byte(*planJSON), nil
}

// ─── Audit Log ───────────────────────────────────────────────────────────────

// AuditRecord represents an audit log entry.
type AuditRecord struct {
	TaskID    string          `json:"task_id"`
	UserID    string          `json:"user_id"`
	Action    string          `json:"action"`
	Details   json.RawMessage `json:"details,omitempty"`
	RiskLevel string          `json:"risk_level"`
}

// InsertAuditLog persists an audit log entry.
func (s *Store) InsertAuditLog(ctx context.Context, r *AuditRecord) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_logs (task_id, user_id, action, details, risk_level)
		 VALUES ($1, $2, $3, $4, $5)`,
		r.TaskID, r.UserID, r.Action, r.Details, r.RiskLevel)
	return err
}

// ─── Approval Persistence ────────────────────────────────────────────────────

// ApprovalRecord represents a persisted HITL approval request.
type ApprovalRecord struct {
	TaskID     string     `json:"task_id"`
	SessionID  string     `json:"session_id"`
	Action     string     `json:"action"`
	RiskLevel  string     `json:"risk_level"`
	Details    string     `json:"details"`
	Status     string     `json:"status"` // pending, approved, rejected, timeout
	ApprovedBy *string    `json:"approved_by,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}

// CreateApproval persists an approval request.
func (s *Store) CreateApproval(ctx context.Context, a *ApprovalRecord) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO approvals (task_id, session_id, action, risk_level, details, status, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (task_id) DO NOTHING`,
		a.TaskID, a.SessionID, a.Action, a.RiskLevel, a.Details, a.Status, a.CreatedAt)
	return err
}

// ResolveApproval updates an approval to approved/rejected.
func (s *Store) ResolveApproval(ctx context.Context, taskID, status, approvedBy string) error {
	now := time.Now()
	_, err := s.db.ExecContext(ctx,
		`UPDATE approvals SET status=$2, approved_by=$3, resolved_at=$4 WHERE task_id=$1`,
		taskID, status, approvedBy, now)
	return err
}

// GetPendingApprovals lists all pending approval requests.
func (s *Store) GetPendingApprovals(ctx context.Context) ([]*ApprovalRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT task_id, session_id, action, risk_level, details, status, created_at
		 FROM approvals WHERE status='pending' ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*ApprovalRecord
	for rows.Next() {
		var a ApprovalRecord
		if err := rows.Scan(&a.TaskID, &a.SessionID, &a.Action, &a.RiskLevel, &a.Details, &a.Status, &a.CreatedAt); err != nil {
			return nil, err
		}
		results = append(results, &a)
	}
	return results, rows.Err()
}

// Ping checks database connectivity.
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// ─── Dynamic Tools CRUD ─────────────────────────────────────────────────────

// DynamicToolRecord represents a persisted dynamic tool configuration.
type DynamicToolRecord struct {
	Name           string          `json:"name"`
	Description    string          `json:"description"`
	Parameters     json.RawMessage `json:"parameters"`
	ExecutorType   string          `json:"executor_type"`
	ExecutorConfig json.RawMessage `json:"executor_config"`
	RiskLevel      int             `json:"risk_level"`
	TTLSeconds     *int64          `json:"ttl_seconds,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
}

// SaveDynamicTool persists a dynamic tool configuration (upsert).
func (s *Store) SaveDynamicTool(ctx context.Context, rec *DynamicToolRecord) error {
	query := `
		INSERT INTO dynamic_tools (name, description, parameters, executor_type, executor_config, risk_level, ttl, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (name) DO UPDATE SET
			description = EXCLUDED.description,
			parameters = EXCLUDED.parameters,
			executor_type = EXCLUDED.executor_type,
			executor_config = EXCLUDED.executor_config,
			risk_level = EXCLUDED.risk_level,
			ttl = EXCLUDED.ttl,
			created_at = EXCLUDED.created_at
	`
	_, err := s.db.ExecContext(ctx, query,
		rec.Name, rec.Description, rec.Parameters,
		rec.ExecutorType, rec.ExecutorConfig, rec.RiskLevel,
		rec.TTLSeconds, rec.CreatedAt,
	)
	return err
}

// LoadDynamicTools returns all non-expired dynamic tool configurations.
func (s *Store) LoadDynamicTools(ctx context.Context) ([]*DynamicToolRecord, error) {
	query := `
		SELECT name, description, parameters, executor_type, executor_config, risk_level, ttl, created_at
		FROM dynamic_tools
		WHERE ttl IS NULL OR (created_at + (ttl || ' seconds')::interval) > NOW()
	`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*DynamicToolRecord
	for rows.Next() {
		var rec DynamicToolRecord
		if err := rows.Scan(
			&rec.Name, &rec.Description, &rec.Parameters,
			&rec.ExecutorType, &rec.ExecutorConfig, &rec.RiskLevel,
			&rec.TTLSeconds, &rec.CreatedAt,
		); err != nil {
			return nil, err
		}
		results = append(results, &rec)
	}
	return results, rows.Err()
}

// DeleteDynamicTool removes a dynamic tool by name.
func (s *Store) DeleteDynamicTool(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM dynamic_tools WHERE name = $1`, name)
	return err
}
