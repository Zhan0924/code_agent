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

// checkAll iterates over all connected MCP servers and verifies they are
// alive. We can't rely on ConnPool.Alive() alone because it only counts
// non-nil slot pointers — a slot still holding a *ServerConnection whose
// transport has already died (child exited, SSE stream stale) would look
// "alive" until something explicitly clears it. So we walk slots, probe
// each via transport.Alive() (transport-specific liveness — Signal(0) +
// reaper flag for stdio; last-event timestamp for SSE), and trigger a
// pool rebuild only if zero slots are still healthy.
func (hc *healthChecker) checkAll() {
	hc.gateway.mu.RLock()
	type serverInfo struct {
		name string
		pool *ConnPool
	}
	var servers []serverInfo
	for name, pool := range hc.gateway.servers {
		servers = append(servers, serverInfo{name: name, pool: pool})
	}
	hc.gateway.mu.RUnlock()

	for _, s := range servers {
		if hc.processAlive(s.pool) == 0 {
			hc.logger.Warn("MCP server pool has no alive child processes, attempting reconnect",
				zap.String("server", s.name),
				zap.Int("size", s.pool.Size()),
			)
			go hc.reconnect(s.name)
		}
	}
}

// connAlive delegates liveness to the transport. Each Transport encodes
// its own notion of "still healthy":
//
//   - stdio: Signal(0) on the child PID + the reaper-set `exited` flag
//     (Signal(0) alone is not enough — a zombie answers signals as "exists"
//     until reaped, so without the flag we'd keep dispatching to a dead
//     slot for the brief window between exit and Wait returning).
//
//   - SSE: stream open AND traffic seen within keepaliveTimeout. A long
//     quiet stream is indistinguishable from a silently-dropped connection,
//     so we treat it as dead and let the gateway reconnect.
//
// Either way: when this returns false the slot is dropped (CAS to nil) so
// future Pick() skips it, and the reconnect goroutine builds a fresh one.
func connAlive(conn *ServerConnection) bool {
	if conn == nil || conn.transport == nil {
		return false
	}
	return conn.transport.Alive()
}

func (hc *healthChecker) processAlive(p *ConnPool) int {
	cnt := 0
	for i := range p.conns {
		conn := p.conns[i].Load()
		if conn == nil {
			continue
		}
		if !connAlive(conn) {
			// Transport dead — drop the slot so Pick() skips it.
			if p.conns[i].CompareAndSwap(conn, nil) {
				_ = conn.close()
			}
			continue
		}
		cnt++
	}
	return cnt
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

// reconnectServer tears down the old pool and starts a fresh one.
// It must be called with a server name that matches a serverCfg in the original config.
func (gw *Gateway) reconnectServer(ctx context.Context, serverName string) error {
	gw.mu.Lock()
	oldPool, ok := gw.servers[serverName]
	if ok {
		_ = oldPool.Close()
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

	pool := NewConnPool(serverCfg, gw.httpClient, gw.logger)
	if err := pool.Start(ctx, gw.initializeServer); err != nil {
		return fmt.Errorf("failed to start MCP server pool %s: %w", serverName, err)
	}

	gw.mu.Lock()
	gw.servers[serverName] = pool
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
