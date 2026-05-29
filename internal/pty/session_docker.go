package pty

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

type dockerSession struct {
	id          SessionID
	name        string
	workspaceID string
	containerID string
	docker      *client.Client
	conn        net.Conn
	reader      *bufio.Reader
	mu          sync.Mutex
	lastActive  time.Time
	createdAt   time.Time
	closed      bool
	cfg         ManagerConfig
	logger      *zap.Logger
}

func (m *manager) createDockerSession(ctx context.Context, workspaceID, name string) (ShellSession, error) {
	wsIDShort := workspaceID
	if len(wsIDShort) > 8 {
		wsIDShort = wsIDShort[:8]
	}
	containerName := fmt.Sprintf("pty-%s-%s", wsIDShort, uuid.New().String()[:8])

	workDir := "/workspace"
	wsPath := fmt.Sprintf("%s/%s", m.cfg.WorkspaceBase, workspaceID)

	containerCfg := &container.Config{
		Image:        m.cfg.Image,
		Cmd:          []string{m.cfg.Shell},
		WorkingDir:   workDir,
		Tty:          true,
		OpenStdin:    true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	}

	hostCfg := &container.HostConfig{
		NetworkMode: "none",
		Binds:       []string{fmt.Sprintf("%s:%s", wsPath, workDir)},
		Resources: container.Resources{
			Memory:   m.cfg.MemoryLimit,
			NanoCPUs: m.cfg.CPUQuota * 1e9 / 100000,
		},
	}

	resp, err := m.cfg.DockerClient.ContainerCreate(ctx, containerCfg, hostCfg, nil, nil, containerName)
	if err != nil {
		return nil, fmt.Errorf("container create: %w", err)
	}

	attachResp, err := m.cfg.DockerClient.ContainerAttach(ctx, resp.ID, container.AttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		m.cfg.DockerClient.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return nil, fmt.Errorf("container attach: %w", err)
	}

	if err := m.cfg.DockerClient.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		attachResp.Close()
		m.cfg.DockerClient.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return nil, fmt.Errorf("container start: %w", err)
	}

	now := time.Now()
	sess := &dockerSession{
		id:          SessionID(uuid.New().String()),
		name:        name,
		workspaceID: workspaceID,
		containerID: resp.ID,
		docker:      m.cfg.DockerClient,
		conn:        attachResp.Conn,
		reader:      bufio.NewReader(attachResp.Reader),
		lastActive:  now,
		createdAt:   now,
		cfg:         m.cfg,
		logger:      m.logger.With(zap.String("container", resp.ID[:12])),
	}

	sess.drainInitialOutput(ctx)
	return sess, nil
}

func (s *dockerSession) ID() SessionID { return s.id }

func (s *dockerSession) Execute(ctx context.Context, command string) (*ExecResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil, fmt.Errorf("session is closed")
	}

	start := time.Now()
	s.lastActive = start

	marker := fmt.Sprintf("__PTY_DONE_%s__", uuid.New().String()[:8])
	fullCmd := fmt.Sprintf("%s; __ec=$?; echo \"%s $__ec\"\n", command, marker)

	if _, err := io.WriteString(s.conn, fullCmd); err != nil {
		return nil, fmt.Errorf("write command: %w", err)
	}

	var output strings.Builder
	timeout := s.cfg.Timeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < timeout {
			timeout = remaining
		}
	}

	readDeadline := time.Now().Add(timeout)
	truncated := false

	for {
		if time.Now().After(readDeadline) {
			return &ExecResult{
				Output:    stripANSI(output.String()),
				ExitCode:  -1,
				Duration:  time.Since(start),
				Truncated: true,
			}, nil
		}

		if tc, ok := s.conn.(interface{ SetReadDeadline(time.Time) error }); ok {
			tc.SetReadDeadline(readDeadline)
		}

		line, err := s.reader.ReadString('\n')
		if err != nil {
			if output.Len() > 0 {
				return &ExecResult{
					Output:   stripANSI(output.String()),
					ExitCode: -1,
					Duration: time.Since(start),
				}, nil
			}
			return nil, fmt.Errorf("read output: %w", err)
		}

		line = strings.TrimRight(line, "\r\n")

		if strings.HasPrefix(line, marker) {
			exitCode := 0
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				fmt.Sscanf(parts[1], "%d", &exitCode)
			}
			return &ExecResult{
				Output:    stripANSI(output.String()),
				ExitCode:  exitCode,
				Duration:  time.Since(start),
				Truncated: truncated,
			}, nil
		}

		// Skip lines that contain the marker (echoed command)
		if strings.Contains(line, marker) {
			continue
		}

		if output.Len()+len(line) > s.cfg.OutputLimit {
			truncated = true
			continue
		}
		output.WriteString(line + "\n")
	}
}

func (s *dockerSession) Resize(rows, cols uint16) error {
	return s.docker.ContainerResize(context.Background(), s.containerID, container.ResizeOptions{
		Height: uint(rows),
		Width:  uint(cols),
	})
}

func (s *dockerSession) IsAlive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	info, err := s.docker.ContainerInspect(ctx, s.containerID)
	if err != nil {
		return false
	}
	return info.State.Running
}

func (s *dockerSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s.conn.Close()

	timeout := 5
	s.docker.ContainerStop(ctx, s.containerID, container.StopOptions{Timeout: &timeout})
	s.docker.ContainerRemove(ctx, s.containerID, container.RemoveOptions{Force: true})

	s.logger.Info("docker PTY session closed")
	return nil
}

func (s *dockerSession) drainInitialOutput(_ context.Context) {
	// Synchronously drain initial shell output using short read deadlines.
	for {
		if tc, ok := s.conn.(interface{ SetReadDeadline(time.Time) error }); ok {
			tc.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		}
		_, err := s.reader.ReadString('\n')
		if err != nil {
			break
		}
	}
}
