// Package mcp - reconnect.go adds automatic reconnection logic for MCP server connections.
// (F8) When an MCP server process crashes or becomes unresponsive, the gateway
// automatically attempts to restart it with exponential backoff.
package mcp

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/agent/code_agent/internal/config"
	"go.uber.org/zap"
)

// reconnectConfig controls reconnection behavior for crashed MCP servers.
type reconnectConfig struct {
	MaxRetries     int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

var defaultReconnectConfig = reconnectConfig{
	MaxRetries:     5,
	InitialBackoff: 1 * time.Second,
	MaxBackoff:     30 * time.Second,
}

// healthChecker periodically monitors MCP server connections and triggers
// reconnection for unresponsive servers.
type healthChecker struct {
	gateway  *Gateway
	cfg      reconnectConfig
	logger   *zap.Logger
	stopCh   chan struct{}
	stopOnce sync.Once
}

// newHealthChecker creates a health checker that monitors MCP server liveness.
func newHealthChecker(gw *Gateway, logger *zap.Logger) *healthChecker {
	return &healthChecker{
		gateway: gw,
		cfg:     defaultReconnectConfig,
		logger:  logger.With(zap.String("component", "mcp-health")),
		stopCh:  make(chan struct{}),
	}
}

// Start begins periodic health checks. It runs in a goroutine.
func (hc *healthChecker) Start(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				hc.checkAll()
			case <-hc.stopCh:
				return
			}
		}
	}()
	hc.logger.Info("MCP health checker started", zap.Duration("interval", interval))
}

// Stop terminates the health checker.
func (hc *healthChecker) Stop() {
	hc.stopOnce.Do(func() {
		close(hc.stopCh)
		hc.logger.Info("MCP health checker stopped")
	})
}

// checkAll iterates over all connected MCP servers and verifies they are alive.
func (hc *healthChecker) checkAll() {
	hc.gateway.mu.RLock()
	type serverInfo struct {
		name string
		conn *ServerConnection
	}
	var servers []serverInfo
	for name, conn := range hc.gateway.servers {
		servers = append(servers, serverInfo{name: name, conn: conn})
	}
	hc.gateway.mu.RUnlock()

	for _, s := range servers {
		if !hc.isAlive(s.conn) {
			hc.logger.Warn("MCP server unresponsive, attempting reconnect",
				zap.String("server", s.name),
			)
			go hc.reconnect(s.name)
		}
	}
}

// isAlive checks if an MCP server process is still running.
func (hc *healthChecker) isAlive(conn *ServerConnection) bool {
	if conn.cmd == nil || conn.cmd.Process == nil {
		return false
	}
	// ProcessState is non-nil only after the process has exited
	if conn.cmd.ProcessState != nil {
		return false
	}
	return true
}

// reconnect attempts to restart a crashed MCP server with exponential backoff.
func (hc *healthChecker) reconnect(serverName string) {
	backoff := hc.cfg.InitialBackoff

	for attempt := 1; attempt <= hc.cfg.MaxRetries; attempt++ {
		select {
		case <-hc.stopCh:
			return
		default:
		}

		hc.logger.Info("reconnecting MCP server",
			zap.String("server", serverName),
			zap.Int("attempt", attempt),
			zap.Duration("backoff", backoff),
		)

		time.Sleep(backoff)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := hc.gateway.reconnectServer(ctx, serverName)
		cancel()

		if err == nil {
			hc.logger.Info("MCP server reconnected successfully",
				zap.String("server", serverName),
				zap.Int("attempt", attempt),
			)
			return
		}

		hc.logger.Warn("reconnection failed",
			zap.String("server", serverName),
			zap.Int("attempt", attempt),
			zap.Error(err),
		)

		// Exponential backoff with cap
		backoff *= 2
		if backoff > hc.cfg.MaxBackoff {
			backoff = hc.cfg.MaxBackoff
		}
	}

	hc.logger.Error("failed to reconnect MCP server after all retries",
		zap.String("server", serverName),
		zap.Int("max_retries", hc.cfg.MaxRetries),
	)
}

// reconnectServer kills the old connection and creates a new one.
// It must be called with the server name that matches a serverCfg in the original config.
func (gw *Gateway) reconnectServer(ctx context.Context, serverName string) error {
	gw.mu.Lock()
	oldConn, ok := gw.servers[serverName]
	if ok {
		_ = oldConn.close()
		delete(gw.servers, serverName)
	}
	gw.mu.Unlock()

	if !ok {
		return fmt.Errorf("unknown MCP server: %s", serverName)
	}

	// Look up original config from the stored configs
	gw.mu.RLock()
	serverCfg, cfgOk := gw.serverConfigs[serverName]
	gw.mu.RUnlock()
	if !cfgOk {
		return fmt.Errorf("no stored config for MCP server: %s", serverName)
	}

	conn, err := newServerConnection(serverCfg, gw.logger)
	if err != nil {
		return fmt.Errorf("failed to start MCP server %s: %w", serverName, err)
	}

	if err := gw.initializeServer(ctx, conn); err != nil {
		conn.close()
		return fmt.Errorf("failed to initialize MCP server %s: %w", serverName, err)
	}

	gw.mu.Lock()
	gw.servers[serverName] = conn
	gw.mu.Unlock()

	return nil
}

// addServerConfig stores a server config for future reconnection.
func (gw *Gateway) addServerConfig(name string, cfg *config.MCPServerConfig) {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if gw.serverConfigs == nil {
		gw.serverConfigs = make(map[string]*config.MCPServerConfig)
	}
	gw.serverConfigs[name] = cfg
}
