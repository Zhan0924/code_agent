// Package pty - SessionManager implementation
package pty

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/docker/docker/client"
	"go.uber.org/zap"
)

// SessionManager manages the lifecycle of PTY sessions across workspaces.
type SessionManager interface {
	GetOrCreate(ctx context.Context, workspaceID string) (ShellSession, error)
	Create(ctx context.Context, workspaceID, name string) (ShellSession, error)
	Get(sessionID SessionID) (ShellSession, bool)
	Destroy(sessionID SessionID) error
	DestroyAll(workspaceID string) error
	ActiveSessions(workspaceID string) []SessionInfo
	Close() error
}

// ManagerConfig holds configuration for the session manager.
type ManagerConfig struct {
	Backend       string        // "docker" or "local"
	DockerClient  *client.Client
	WorkspaceBase string
	Image         string
	MaxSessions   int
	IdleTimeout   time.Duration
	MemoryLimit   int64
	CPUQuota      int64
	OutputLimit   int
	Shell         string
	Timeout       time.Duration
}

// manager implements SessionManager.
type manager struct {
	cfg      ManagerConfig
	logger   *zap.Logger
	mu       sync.RWMutex
	sessions map[SessionID]ShellSession
	// workspaceID -> list of session IDs
	workspaceSessions map[string][]SessionID
	stopReaper        chan struct{}
}

// NewManager creates a new PTY session manager.
func NewManager(cfg ManagerConfig, logger *zap.Logger) (SessionManager, error) {
	if cfg.Backend != "docker" && cfg.Backend != "local" {
		return nil, fmt.Errorf("unsupported backend: %s (must be 'docker' or 'local')", cfg.Backend)
	}

	if cfg.Backend == "docker" && cfg.DockerClient == nil {
		return nil, fmt.Errorf("docker backend requires DockerClient")
	}

	if cfg.MaxSessions == 0 {
		cfg.MaxSessions = 3
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 5 * time.Minute
	}
	if cfg.OutputLimit == 0 {
		cfg.OutputLimit = 1048576 // 1MB
	}
	if cfg.Shell == "" {
		cfg.Shell = "/bin/bash"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 120 * time.Second
	}

	m := &manager{
		cfg:               cfg,
		logger:            logger.With(zap.String("component", "pty-manager")),
		sessions:          make(map[SessionID]ShellSession),
		workspaceSessions: make(map[string][]SessionID),
		stopReaper:        make(chan struct{}),
	}

	// Start idle session reaper
	go m.reaper()

	return m, nil
}

// GetOrCreate returns the default session for a workspace, creating it if needed.
func (m *manager) GetOrCreate(ctx context.Context, workspaceID string) (ShellSession, error) {
	return m.Create(ctx, workspaceID, "default")
}

// Create creates a new named session for a workspace.
func (m *manager) Create(ctx context.Context, workspaceID, name string) (ShellSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if session already exists
	for _, sid := range m.workspaceSessions[workspaceID] {
		if sess, ok := m.sessions[sid]; ok {
			if info := m.getSessionInfo(sess); info.Name == name {
				return sess, nil
			}
		}
	}

	// Check max sessions limit
	if len(m.workspaceSessions[workspaceID]) >= m.cfg.MaxSessions {
		return nil, fmt.Errorf("workspace %s has reached max sessions limit (%d)", workspaceID, m.cfg.MaxSessions)
	}

	// Create new session based on backend
	var sess ShellSession
	var err error

	switch m.cfg.Backend {
	case "docker":
		sess, err = m.createDockerSession(ctx, workspaceID, name)
	case "local":
		sess, err = m.createLocalSession(ctx, workspaceID, name)
	default:
		return nil, fmt.Errorf("unsupported backend: %s", m.cfg.Backend)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to create %s session: %w", m.cfg.Backend, err)
	}

	// Register session
	sid := sess.ID()
	m.sessions[sid] = sess
	m.workspaceSessions[workspaceID] = append(m.workspaceSessions[workspaceID], sid)

	m.logger.Info("created PTY session",
		zap.String("session_id", string(sid)),
		zap.String("workspace_id", workspaceID),
		zap.String("name", name),
		zap.String("backend", m.cfg.Backend))

	return sess, nil
}

// Get retrieves an existing session by ID.
func (m *manager) Get(sessionID SessionID) (ShellSession, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sess, ok := m.sessions[sessionID]
	return sess, ok
}

// Destroy terminates and removes a session.
func (m *manager) Destroy(sessionID SessionID) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	sess, ok := m.sessions[sessionID]
	if !ok {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	if err := sess.Close(); err != nil {
		m.logger.Warn("error closing session", zap.String("session_id", string(sessionID)), zap.Error(err))
	}

	delete(m.sessions, sessionID)

	// Remove from workspace index
	for wsID, sids := range m.workspaceSessions {
		for i, sid := range sids {
			if sid == sessionID {
				m.workspaceSessions[wsID] = append(sids[:i], sids[i+1:]...)
				break
			}
		}
	}

	m.logger.Info("destroyed PTY session", zap.String("session_id", string(sessionID)))
	return nil
}

// DestroyAll terminates all sessions for a workspace.
func (m *manager) DestroyAll(workspaceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	sids := m.workspaceSessions[workspaceID]
	var errs []error

	for _, sid := range sids {
		if sess, ok := m.sessions[sid]; ok {
			if err := sess.Close(); err != nil {
				errs = append(errs, err)
			}
			delete(m.sessions, sid)
		}
	}

	delete(m.workspaceSessions, workspaceID)

	m.logger.Info("destroyed all PTY sessions for workspace",
		zap.String("workspace_id", workspaceID),
		zap.Int("count", len(sids)))

	if len(errs) > 0 {
		return fmt.Errorf("errors destroying sessions: %v", errs)
	}
	return nil
}

// ActiveSessions returns info about all active sessions for a workspace.
func (m *manager) ActiveSessions(workspaceID string) []SessionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var infos []SessionInfo
	for _, sid := range m.workspaceSessions[workspaceID] {
		if sess, ok := m.sessions[sid]; ok {
			infos = append(infos, m.getSessionInfo(sess))
		}
	}
	return infos
}

// Close shuts down the manager and all sessions.
func (m *manager) Close() error {
	close(m.stopReaper)

	m.mu.Lock()
	defer m.mu.Unlock()

	var errs []error
	for sid, sess := range m.sessions {
		if err := sess.Close(); err != nil {
			errs = append(errs, fmt.Errorf("session %s: %w", sid, err))
		}
	}

	m.sessions = nil
	m.workspaceSessions = nil

	if len(errs) > 0 {
		return fmt.Errorf("errors closing sessions: %v", errs)
	}
	return nil
}

// reaper periodically checks for idle sessions and destroys them.
func (m *manager) reaper() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.reapIdleSessions()
		case <-m.stopReaper:
			return
		}
	}
}

func (m *manager) reapIdleSessions() {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	var toDestroy []SessionID

	for sid, sess := range m.sessions {
		info := m.getSessionInfo(sess)
		if now.Sub(info.LastActive) > m.cfg.IdleTimeout {
			toDestroy = append(toDestroy, sid)
		}
	}

	for _, sid := range toDestroy {
		if sess, ok := m.sessions[sid]; ok {
			sess.Close()
			delete(m.sessions, sid)

			// Remove from workspace index
			for wsID, sids := range m.workspaceSessions {
				for i, s := range sids {
					if s == sid {
						m.workspaceSessions[wsID] = append(sids[:i], sids[i+1:]...)
						break
					}
				}
			}

			m.logger.Info("reaped idle PTY session",
				zap.String("session_id", string(sid)),
				zap.Duration("idle", now.Sub(m.getSessionInfo(sess).LastActive)))
		}
	}
}

func (m *manager) getSessionInfo(sess ShellSession) SessionInfo {
	info := SessionInfo{
		ID:      sess.ID(),
		IsAlive: sess.IsAlive(),
	}

	// Extract additional info based on session type
	switch s := sess.(type) {
	case *dockerSession:
		info.Name = s.name
		info.WorkspaceID = s.workspaceID
		info.CreatedAt = s.createdAt
		info.LastActive = s.lastActive
	case *localSession:
		info.Name = s.name
		info.WorkspaceID = s.workspaceID
		info.CreatedAt = s.createdAt
		info.LastActive = s.lastActive
	}

	return info
}
