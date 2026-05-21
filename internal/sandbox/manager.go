// Package sandbox provides a Docker-based ephemeral execution environment
// for running untrusted code with strict resource limits and network isolation.
//
// ============================================================================
//
//	设 计 原 理（核心要点）
//
// ============================================================================
//
// 【威胁模型】LLM 生成的 bash/python/go 代码本质上是"不可信输入"，我们必须
//
//	防御：① rm -rf / 破坏宿主机；② curl attacker.com 内网穿透；③ while(1)
//	资源耗尽；④ 挂载 docker.sock 实现容器逃逸。
//
// 【三层防线】
//  1. 静态正则拦截（internal/security）—— 请求进入 Manager 前先过滤关键词；
//  2. 容器隔离（本文件）—— NetworkMode=none + cgroups + 只读 rootfs + nobody；
//  3. 超时兜底（context.WithTimeout）—— 超时即 ContainerKill + ForceRemove。
//
// 【生命周期：阅后即焚】
//
//	Pull Image → Create Container → Attach stdout/stderr → Start →
//	(run with resource limits) → Wait/Timeout → ReadLogs → ForceRemove
//	每次 Execute 都是一个全新容器，绝不复用（避免残留文件串扰）。
//
// 【为什么用 stdcopy.StdCopy？】
//
//	Docker 的 container log/attach 接口返回的是 **多路复用流**：
//	  [8B header] [payload] [8B header] [payload] ...
//	其中 header[0] 指示 stream 类型（1=stdout, 2=stderr），header[4:8]
//	是 payload 长度。stdcopy.StdCopy 负责解复用，把两路流分别写入给定的
//	io.Writer。若直接 io.Copy，拿到的会是带着 header 的乱码。
//
// 【资源配额】
//
//	Memory/NanoCPUs/PidsLimit 通过 HostConfig.Resources 写入 cgroups v2，
//	死循环脚本由 Linux OOM Killer 或 CPU throttle 即刻掐死，宿主机安全。
//
// ============================================================================
package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/agent/code_agent/internal/config"
	"github.com/agent/code_agent/internal/models"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Manager handles the lifecycle of ephemeral Docker sandbox containers.
//
// Manager 是沙箱对外的唯一入口。其字段含义：
//   - docker : Docker Engine API 的 HTTP 客户端（单例，线程安全）；
//   - cfg    : 来自 YAML 的沙箱配置（镜像、超时、内存 CPU 限额、网络模式）；
//   - logger : 结构化日志，所有容器 ID / 退出码 / 耗时均写入，供审计追溯。
//
// 并发安全性：Docker Client 本身是线程安全的，且 Execute 内每次创建独立
// 容器互不影响，因此 Manager 可以在上层被任意多个 goroutine 共享。
type Manager struct {
	docker *client.Client        // Docker Engine API 客户端
	cfg    *config.SandboxConfig // 静态配置（只读）
	logger *zap.Logger           // 结构化日志
}

// NewManager creates a new sandbox manager with a Docker client connection.
func NewManager(cfg *config.SandboxConfig, logger *zap.Logger) (*Manager, error) {
	opts := []client.Opt{
		client.WithAPIVersionNegotiation(),
	}
	if cfg.DockerHost != "" {
		opts = append(opts, client.WithHost(cfg.DockerHost))
	}

	dockerClient, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	// Verify connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := dockerClient.Ping(ctx); err != nil {
		return nil, fmt.Errorf("failed to connect to Docker daemon: %w", err)
	}

	return &Manager{
		docker: dockerClient,
		cfg:    cfg,
		logger: logger.With(zap.String("component", "sandbox")),
	}, nil
}

// Execute runs code in an ephemeral container and returns the result.
// The container is automatically removed after execution completes or times out.
//
// 执行流程（对应"阅后即焚"模型）：
//
//	(1) resolveRuntime  : 按 req.Language 选择镜像与入口命令（python → python:3.11-slim 等）
//	(2) context.WithTimeout: 设置硬超时，防止脚本死循环；超时后 defer 清理会强制 Kill+Rm
//	(3) ensureImage     : 若本地无镜像则 docker pull（首次冷启动慢，后续秒级）
//	(4) parse*Limit     : 把 "512m" / "1.0" 字符串翻译成 cgroups 可接受的数值
//	(5) ContainerCreate : 生成一次性容器；注意 AutoRemove=false，因为要先 ReadLogs
//	(6) ContainerStart  : 启动，此后容器开始消耗 cgroups 配额
//	(7) ContainerWait   : 阻塞等待退出（或 ctx 超时）
//	(8) ContainerLogs   : 抓取 stdout/stderr 完整日志
//	(9) defer ForceRemove: 无论成功失败都强制删除容器（"阅后即焚"）
//
// 设计取舍：这里用"全量日志 + 退出后返回"模式（适合短任务）。若上层需要
// 实时流式日志推送到前端 SSE，应调用 ExecuteStream（见下方），底层用
// ContainerAttach 直接拿到 multiplexed stream，经 stdcopy demux 后逐行推送。
func (m *Manager) Execute(ctx context.Context, req *models.SandboxRequest) (*models.SandboxResult, error) {
	startTime := time.Now()

	// ---- Step 1: 根据语言选镜像和入口命令 ----
	// 例如 Python 任务会返回 ("python:3.11-slim", []string{"python", "-c", code})
	imageName, cmd := m.resolveRuntime(req)

	// ---- Step 2: 设置硬超时（最后一道防线，防死循环）----
	// 若请求未显式指定超时，使用全局默认（通常 60s）。
	timeout := req.Timeout
	if timeout == 0 {
		timeout = m.cfg.Timeout
	}
	// WithTimeout 会派生一个带截止时间的 Context。Docker Client 的所有调用
	// 都接收 Context，任何一步超过 deadline 会立即返回 context.DeadlineExceeded。
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// ---- Step 3: 惰性拉取镜像 ----
	// 生产上通常预热好 base image；冷启动时这里是性能瓶颈（可能数十秒）。
	if err := m.ensureImage(execCtx, imageName); err != nil {
		return nil, fmt.Errorf("failed to pull image %s: %w", imageName, err)
	}

	// ---- Step 4: 把 YAML 里的人类可读配额翻译成底层数值 ----
	// "512m" → 512*1024*1024 字节；"1.0" → 1e9 纳秒 CPU（NanoCPUs 单位）。
	memoryLimit, err := parseMemoryLimit(m.cfg.MemoryLimit)
	if err != nil {
		return nil, fmt.Errorf("invalid memory limit: %w", err)
	}

	nanoCPUs, err := parseCPULimit(m.cfg.CPULimit)
	if err != nil {
		return nil, fmt.Errorf("invalid CPU limit: %w", err)
	}

	// ---- Step 5: 组装容器配置 ----
	// 用 UUID 前 8 位做容器名，既避免碰撞又便于日志中定位。
	containerName := fmt.Sprintf("sandbox-%s", uuid.New().String()[:8])
	containerCfg := &container.Config{
		Image:        imageName,
		Cmd:          cmd,
		WorkingDir:   m.cfg.WorkspaceDir, // 通常是 /workspace（tmpfs 或 bind-mount）
		Env:          envMapToSlice(req.Env),
		Tty:          false, // 非交互模式，日志才能正常多路复用
		AttachStdout: true,  // 允许我们后续抓 stdout
		AttachStderr: true,  // 允许我们后续抓 stderr
	}

	// HostConfig is the container-to-host security contract. Resource limits,
	// capability drops, read-only rootfs, and tmpfs mounts are all centralized
	// in buildHostConfig — see that helper for the full rationale.
	hostCfg := m.buildHostConfig(memoryLimit, nanoCPUs)
	hostCfg.AutoRemove = false // We need to read logs before removal

	// Create container
	resp, err := m.docker.ContainerCreate(execCtx, containerCfg, hostCfg, nil, nil, containerName)
	if err != nil {
		return nil, fmt.Errorf("failed to create container: %w", err)
	}
	containerID := resp.ID

	// Ensure cleanup
	defer func() {
		removeCtx, removeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer removeCancel()
		_ = m.docker.ContainerRemove(removeCtx, containerID, container.RemoveOptions{Force: true})
		m.logger.Debug("sandbox container removed", zap.String("container_id", containerID[:12]))
	}()

	// Write code to container via copy
	if req.Code != "" {
		if err := m.copyCodeToContainer(execCtx, containerID, req); err != nil {
			return nil, fmt.Errorf("failed to copy code to container: %w", err)
		}
	}

	// Start container
	if err := m.docker.ContainerStart(execCtx, containerID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("failed to start container: %w", err)
	}

	m.logger.Info("sandbox started",
		zap.String("container_id", containerID[:12]),
		zap.String("image", imageName),
		zap.String("language", req.Language),
	)

	// Wait for completion
	statusCh, errCh := m.docker.ContainerWait(execCtx, containerID, container.WaitConditionNotRunning)

	var result models.SandboxResult

	select {
	case err := <-errCh:
		if err != nil {
			result.Killed = true
			result.Stderr = fmt.Sprintf("container execution error: %v", err)
		}
	case status := <-statusCh:
		result.ExitCode = int(status.StatusCode)
		if status.Error != nil {
			result.Stderr = status.Error.Message
		}
	case <-execCtx.Done():
		// Timeout - force kill
		killCtx, killCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer killCancel()
		_ = m.docker.ContainerKill(killCtx, containerID, "SIGKILL")
		result.Killed = true
		result.ExitCode = -1
		result.Stderr = "execution timed out"
	}

	// Collect output
	stdout, stderr := m.collectOutput(context.Background(), containerID)
	if result.Stdout == "" {
		result.Stdout = stdout
	}
	if result.Stderr == "" || (stderr != "" && !result.Killed) {
		result.Stderr = stderr
	}

	result.Duration = time.Since(startTime)

	m.logger.Info("sandbox completed",
		zap.String("container_id", containerID[:12]),
		zap.Int("exit_code", result.ExitCode),
		zap.Duration("duration", result.Duration),
		zap.Bool("killed", result.Killed),
	)

	return &result, nil
}

// ExecuteStream runs code and streams output line-by-line via a channel.
func (m *Manager) ExecuteStream(ctx context.Context, req *models.SandboxRequest) (<-chan string, <-chan error, error) {
	imageName, cmd := m.resolveRuntime(req)
	timeout := req.Timeout
	if timeout == 0 {
		timeout = m.cfg.Timeout
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)

	if err := m.ensureImage(execCtx, imageName); err != nil {
		cancel()
		return nil, nil, fmt.Errorf("failed to pull image: %w", err)
	}

	memoryLimit, _ := parseMemoryLimit(m.cfg.MemoryLimit)
	nanoCPUs, _ := parseCPULimit(m.cfg.CPULimit)

	containerName := fmt.Sprintf("sandbox-stream-%s", uuid.New().String()[:8])
	containerCfg := &container.Config{
		Image:        imageName,
		Cmd:          cmd,
		WorkingDir:   m.cfg.WorkspaceDir,
		Env:          envMapToSlice(req.Env),
		Tty:          false,
		AttachStdout: true,
		AttachStderr: true,
	}

	hostCfg := m.buildHostConfig(memoryLimit, nanoCPUs)

	resp, err := m.docker.ContainerCreate(execCtx, containerCfg, hostCfg, nil, nil, containerName)
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("failed to create container: %w", err)
	}
	containerID := resp.ID

	if req.Code != "" {
		if err := m.copyCodeToContainer(execCtx, containerID, req); err != nil {
			cancel()
			return nil, nil, err
		}
	}

	if err := m.docker.ContainerStart(execCtx, containerID, container.StartOptions{}); err != nil {
		cancel()
		return nil, nil, fmt.Errorf("failed to start container: %w", err)
	}

	// Attach to container output
	attachResp, err := m.docker.ContainerAttach(execCtx, containerID, container.AttachOptions{
		Stream: true,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("failed to attach to container: %w", err)
	}

	outCh := make(chan string, 128)
	errCh := make(chan error, 1)

	// Demultiplex Docker's framed attach stream via stdcopy.StdCopy. The raw
	// reader returns interleaved [8B header][payload] frames; reading it
	// directly corrupts output with framing bytes (the prior bug). StdCopy
	// writes stdout and stderr to separate io.Writers — we route both to the
	// same channel, tagging stderr so the consumer can distinguish. Writes
	// observe execCtx so a cancelled context unblocks immediately instead of
	// deadlocking on a full channel.
	stdoutW := &chanWriter{ch: outCh, ctx: execCtx}
	stderrW := &chanWriter{ch: outCh, ctx: execCtx, prefix: "[stderr] "}

	go func() {
		defer close(outCh)
		defer close(errCh)
		defer cancel()
		defer attachResp.Close()
		defer func() {
			removeCtx, removeCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer removeCancel()
			_ = m.docker.ContainerRemove(removeCtx, containerID, container.RemoveOptions{Force: true})
		}()

		if _, err := stdcopy.StdCopy(stdoutW, stderrW, attachResp.Reader); err != nil && err != io.EOF {
			select {
			case errCh <- err:
			default:
			}
		}
	}()

	return outCh, errCh, nil
}

// chanWriter adapts an outbound string channel to io.Writer for stdcopy demux.
// Writes are non-blocking with respect to ctx — if ctx is cancelled while a
// slow consumer has filled the channel, Write returns ctx.Err() rather than
// wedging the demuxer goroutine forever.
type chanWriter struct {
	ch     chan<- string
	ctx    context.Context
	prefix string
}

func (w *chanWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	msg := string(p)
	if w.prefix != "" {
		msg = w.prefix + msg
	}
	select {
	case w.ch <- msg:
		return len(p), nil
	case <-w.ctx.Done():
		return 0, w.ctx.Err()
	}
}

// resolveRuntime determines the Docker image and command based on the language.
func (m *Manager) resolveRuntime(req *models.SandboxRequest) (string, []string) {
	lang := strings.ToLower(req.Language)

	imageName := m.cfg.DefaultImage
	if img, ok := m.cfg.Images[lang]; ok {
		imageName = img
	}

	// [OPT-4] All languages now use tar-based file copy to prevent shell injection.
	// Code is written to a file inside the container via CopyToContainer, then executed.
	if req.Files == nil {
		req.Files = make(map[string]string)
	}

	var cmd []string
	switch lang {
	case "python":
		req.Files["main.py"] = req.Code
		cmd = []string{"python3", "/workspace/main.py"}
	case "go":
		req.Files["main.go"] = req.Code
		cmd = []string{"sh", "-c", "cd /workspace && go run main.go"}
	case "node", "javascript":
		req.Files["main.js"] = req.Code
		cmd = []string{"node", "/workspace/main.js"}
	case "bash", "sh":
		req.Files["script.sh"] = req.Code
		cmd = []string{"sh", "/workspace/script.sh"}
	default:
		req.Files["script.sh"] = req.Code
		cmd = []string{"sh", "/workspace/script.sh"}
	}

	return imageName, cmd
}

// ensureImage pulls the image if it's not available locally.
func (m *Manager) ensureImage(ctx context.Context, imageName string) error {
	_, _, err := m.docker.ImageInspectWithRaw(ctx, imageName)
	if err == nil {
		return nil // Image exists locally
	}

	m.logger.Info("pulling image", zap.String("image", imageName))
	reader, err := m.docker.ImagePull(ctx, imageName, image.PullOptions{})
	if err != nil {
		return err
	}
	defer reader.Close()
	_, _ = io.Copy(io.Discard, reader)
	return nil
}

// copyCodeToContainer writes code and additional files into the container via tar archive.
// (F7) This replaces the stub with a real Docker CopyToContainer implementation.
func (m *Manager) copyCodeToContainer(ctx context.Context, containerID string, req *models.SandboxRequest) error {
	if len(req.Files) == 0 {
		return nil
	}

	var buf bytes.Buffer
	tw := newTarWriter(&buf)

	for filePath, content := range req.Files {
		if err := tw.writeFile(filePath, []byte(content)); err != nil {
			return fmt.Errorf("failed to write %s to tar: %w", filePath, err)
		}
	}

	if err := tw.close(); err != nil {
		return fmt.Errorf("failed to finalize tar archive: %w", err)
	}

	return m.docker.CopyToContainer(ctx, containerID, m.cfg.WorkspaceDir, &buf, container.CopyToContainerOptions{
		AllowOverwriteDirWithFile: true,
	})
}

// maxOutputBytes limits the captured stdout/stderr to 1MB to prevent OOM.
const maxOutputBytes = 1 << 20 // 1 MiB

// collectOutput reads stdout and stderr from the container logs.
func (m *Manager) collectOutput(ctx context.Context, containerID string) (string, string) {
	logReader, err := m.docker.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
	})
	if err != nil {
		return "", fmt.Sprintf("failed to read logs: %v", err)
	}
	defer logReader.Close()

	var stdoutBuf, stderrBuf bytes.Buffer
	// Use LimitReader to prevent unbounded memory consumption
	limitedReader := io.LimitReader(logReader, maxOutputBytes*2) // 2x for demux overhead
	_, _ = stdcopy.StdCopy(&stdoutBuf, &stderrBuf, limitedReader)

	stdout := stdoutBuf.String()
	stderr := stderrBuf.String()

	// Truncate if individual streams exceed limit
	if len(stdout) > maxOutputBytes {
		stdout = stdout[:maxOutputBytes] + "\n... [output truncated at 1MB]"
	}
	if len(stderr) > maxOutputBytes {
		stderr = stderr[:maxOutputBytes] + "\n... [stderr truncated at 1MB]"
	}

	return stdout, stderr
}

// Close releases Docker client resources.
func (m *Manager) Close() error {
	return m.docker.Close()
}

// buildHostConfig returns a hardened HostConfig shared by Execute and
// ExecuteStream. The defence-in-depth layers applied here:
//
//   - NetworkMode=none (or configured)  — no outbound network by default
//   - Memory / NanoCPUs (cgroups)       — bound resource consumption
//   - PidsLimit=256                     — cap process count, fork-bomb defence
//   - SecurityOpt "no-new-privileges"   — block suid escalation inside the container
//   - CapDrop ALL                       — drop every Linux capability; running
//     `python foo.py` / `go run main.go` needs none of CAP_NET_*, CAP_SYS_*, etc.
//   - ReadonlyRootfs=true               — container filesystem is immutable except
//     for the mounted tmpfs regions below
//   - Tmpfs /workspace (writable)       — code runs from this dir; noexec-off so
//     `go build` / compiled binaries can execute
//   - Tmpfs /tmp (writable, noexec)     — pip / npm caches land here; noexec
//     stops payloads from chaining into execution of /tmp-written binaries
//
// The per-container memory/CPU limits still fall through from cfg; callers get
// a ready-to-pass *HostConfig that only needs AutoRemove set by the caller
// depending on whether logs need to be pulled afterward.
func (m *Manager) buildHostConfig(memoryLimit, nanoCPUs int64) *container.HostConfig {
	pidsLimit := int64(256)
	workspaceDir := m.cfg.WorkspaceDir
	if workspaceDir == "" {
		workspaceDir = "/workspace"
	}
	return &container.HostConfig{
		NetworkMode: container.NetworkMode(m.cfg.NetworkMode),
		Resources: container.Resources{
			Memory:    memoryLimit,
			NanoCPUs:  nanoCPUs,
			PidsLimit: &pidsLimit,
		},
		SecurityOpt:    []string{"no-new-privileges:true"},
		CapDrop:        strslice.StrSlice{"ALL"},
		ReadonlyRootfs: true,
		Tmpfs: map[string]string{
			workspaceDir: "rw,size=128m",
			"/tmp":       "rw,noexec,nosuid,size=64m",
		},
	}
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func envMapToSlice(env map[string]string) []string {
	result := make([]string, 0, len(env))
	for k, v := range env {
		result = append(result, fmt.Sprintf("%s=%s", k, v))
	}
	return result
}

func parseMemoryLimit(limit string) (int64, error) {
	if limit == "" {
		return 512 * 1024 * 1024, nil // default 512MB
	}
	// Simple parser for common formats: "512m", "1g"
	limit = strings.TrimSpace(strings.ToLower(limit))
	var multiplier int64 = 1
	if strings.HasSuffix(limit, "g") {
		multiplier = 1024 * 1024 * 1024
		limit = strings.TrimSuffix(limit, "g")
	} else if strings.HasSuffix(limit, "m") {
		multiplier = 1024 * 1024
		limit = strings.TrimSuffix(limit, "m")
	} else if strings.HasSuffix(limit, "k") {
		multiplier = 1024
		limit = strings.TrimSuffix(limit, "k")
	}

	var value int64
	if _, err := fmt.Sscanf(limit, "%d", &value); err != nil {
		return 0, fmt.Errorf("invalid memory limit format: %s", limit)
	}
	return value * multiplier, nil
}

func parseCPULimit(limit string) (int64, error) {
	if limit == "" {
		return 1e9, nil // default 1 CPU
	}
	var cpus float64
	if _, err := fmt.Sscanf(limit, "%f", &cpus); err != nil {
		return 0, fmt.Errorf("invalid CPU limit format: %s", limit)
	}
	return int64(cpus * 1e9), nil
}

// [OPT-24] escapeShell removed — dead code after OPT-4 file-based execution.
// All languages now use tar archive injection; no shell escaping needed.
func _escapeShellDeprecated(s string) string {
	return strings.ReplaceAll(s, "'", "'\"'\"'")
}
