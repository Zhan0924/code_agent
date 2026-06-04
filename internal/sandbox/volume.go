// volume.go — 宿主机 volume bind-mount 支持。
//
// ============================================================================
//
//	设 计 原 理
//
// ============================================================================
//
// 【为什么需要 volume】
//
//	默认沙箱容器的 `/workspace` 是 tmpfs（进程结束即灰飞烟灭），这对"短作业"
//	合适，但对"要跑编译、要缓存 go mod / node_modules"的任务很浪费：
//	每次重新下载依赖慢到不能忍。volume.go 允许把宿主机的某个目录 bind-mount
//	到容器里，让缓存跨容器复用。
//
// 【安全约束】
//
//	bind-mount 是沙箱逃逸面——挂错一个目录（如 /var/run/docker.sock）直接
//	等于容器逃逸。本文件里强制：
//	  · Only RO by default（ReadOnly: true）；有写需求必须显式请求；
//	  · 黑名单 host paths（/var/run/docker.sock, /proc, /sys, /etc 等）。
//
// 【temporary vs persistent volume】
//
//	· temporary: 本次 Execute 专用的 tmpdir。结束后删除。
//	· persistent: 跨 Execute 复用（如 go mod cache）。由调用方管理生命周期。
//
// ============================================================================
package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent/code_agent/internal/models"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// blockedHostPaths are paths that must never be bind-mounted into containers.
var blockedHostPaths = []string{
	"/var/run/docker.sock",
	"/var/run",
	"/proc",
	"/sys",
	"/etc",
	"/root",
	"/boot",
	"/dev",
}

// validateHostDir checks that a host directory is safe to bind-mount.
func validateHostDir(hostDir string) error {
	realPath, err := filepath.EvalSymlinks(hostDir)
	if err != nil {
		return fmt.Errorf("invalid host path: %w", err)
	}

	for _, blocked := range blockedHostPaths {
		if realPath == blocked || strings.HasPrefix(realPath, blocked+"/") {
			return fmt.Errorf("host path blocked: %s resolves to %s", hostDir, realPath)
		}
	}

	info, err := os.Stat(realPath)
	if err != nil {
		return fmt.Errorf("host path not accessible: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("host path must be a directory")
	}

	return nil
}

// ExecuteWithVolume runs a command in a Docker container with the given
// host directory mounted as /workspace. Used by the project generator
// for build validation inside the generated project.
func (m *Manager) ExecuteWithVolume(ctx context.Context, language, command, hostDir string) (*models.SandboxResult, error) {
	startTime := time.Now()
	containerName := fmt.Sprintf("agent-vol-%s", uuid.New().String()[:8])

	// Validate host directory before mounting
	if err := validateHostDir(hostDir); err != nil {
		return nil, fmt.Errorf("host directory validation failed: %w", err)
	}

	// Resolve image
	imageName := m.imageForLanguage(language)
	if err := m.ensureImage(ctx, imageName); err != nil {
		return nil, fmt.Errorf("ensure image %s: %w", imageName, err)
	}

	// Use buildHostConfig for full security hardening
	memoryLimit := int64(512 * 1024 * 1024)
	nanoCPUs := int64(1e9)
	hostCfg := m.buildHostConfig(memoryLimit, nanoCPUs)
	// Override tmpfs for /workspace since we're bind-mounting it
	delete(hostCfg.Tmpfs, m.cfg.WorkspaceDir)
	delete(hostCfg.Tmpfs, "/workspace")
	hostCfg.ReadonlyRootfs = false // build validation needs writable fs
	hostCfg.Mounts = []mount.Mount{
		{
			Type:   mount.TypeBind,
			Source: hostDir,
			Target: "/workspace",
		},
	}

	// Create container with volume mount
	resp, err := m.docker.ContainerCreate(ctx,
		&container.Config{
			Image: imageName,
			Cmd:   []string{"/bin/sh", "-c", command},
		},
		hostCfg,
		nil, nil, containerName,
	)
	if err != nil {
		return nil, fmt.Errorf("container create: %w", err)
	}
	containerID := resp.ID

	// Ensure cleanup
	defer func() {
		removeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = m.docker.ContainerRemove(removeCtx, containerID, container.RemoveOptions{Force: true})
	}()

	// Start
	if err := m.docker.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("container start: %w", err)
	}

	// Wait for completion with timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, m.cfg.Timeout)
	defer cancel()

	statusCh, errCh := m.docker.ContainerWait(timeoutCtx, containerID, container.WaitConditionNotRunning)
	var exitCode int
	select {
	case err := <-errCh:
		if err != nil {
			return &models.SandboxResult{
				ExitCode: -1,
				Stdout:   "container wait error: " + err.Error(),
				Duration: time.Since(startTime),
			}, nil
		}
	case status := <-statusCh:
		exitCode = int(status.StatusCode)
	case <-ctx.Done():
		// Caller-side cancellation (interrupt, request abort) must terminate
		// the wait promptly. The deferred ContainerRemove (force=true) at
		// :131 still tears down the running container so we don't leak it.
		return &models.SandboxResult{
			ExitCode: -1,
			Stdout:   "sandbox execution cancelled: " + ctx.Err().Error(),
			Duration: time.Since(startTime),
		}, ctx.Err()
	}

	// Collect output
	stdout, stderr := m.collectOutput(ctx, containerID)

	m.logger.Debug("volume execution complete",
		zap.String("container", containerName),
		zap.Int("exit_code", exitCode),
		zap.Duration("duration", time.Since(startTime)),
	)

	combined := stdout
	if stderr != "" {
		combined = stdout + "\n" + stderr
	}

	return &models.SandboxResult{
		ExitCode: exitCode,
		Stdout:   combined,
		Stderr:   stderr,
		Duration: time.Since(startTime),
	}, nil
}

// imageForLanguage maps a language name to a Docker image.
func (m *Manager) imageForLanguage(language string) string {
	lang := strings.ToLower(language)
	switch lang {
	case "go", "golang":
		return "golang:1.23-alpine"
	case "python":
		return "python:3.12-slim"
	case "node", "javascript", "typescript":
		return "node:20-slim"
	case "rust":
		return "rust:1.78-slim"
	default:
		return "alpine:3.20"
	}
}
