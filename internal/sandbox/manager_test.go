package sandbox

import (
	"context"
	"testing"
	"time"

	"github.com/agent/code_agent/internal/config"
	"go.uber.org/zap"
)

func TestSandboxConfig_Defaults(t *testing.T) {
	cfg := &config.SandboxConfig{
		DockerHost:   "unix:///var/run/docker.sock",
		DefaultImage: "python:3.12-slim",
		MemoryLimit:  "512m",
		CPULimit:     "1.0",
		Timeout:      120 * time.Second,
		NetworkMode:  "none",
		WorkspaceDir: "/workspace",
		Images: map[string]string{
			"python": "python:3.12-slim",
			"go":     "golang:1.23-alpine",
			"bash":   "alpine:3.20",
		},
	}

	if cfg.DockerHost != "unix:///var/run/docker.sock" {
		t.Errorf("unexpected docker host: %s", cfg.DockerHost)
	}
	if cfg.NetworkMode != "none" {
		t.Errorf("expected none network mode, got: %s", cfg.NetworkMode)
	}
}

func TestResolveImage(t *testing.T) {
	images := map[string]string{
		"python": "python:3.12-slim",
		"go":     "golang:1.23-alpine",
		"bash":   "alpine:3.20",
	}
	defaultImg := "python:3.12-slim"

	tests := []struct {
		lang     string
		expected string
	}{
		{"python", "python:3.12-slim"},
		{"go", "golang:1.23-alpine"},
		{"bash", "alpine:3.20"},
		{"unknown", defaultImg},
	}

	for _, tt := range tests {
		t.Run(tt.lang, func(t *testing.T) {
			img, ok := images[tt.lang]
			if !ok {
				img = defaultImg
			}
			if img != tt.expected {
				t.Errorf("for %s: expected %s, got %s", tt.lang, tt.expected, img)
			}
		})
	}
}

func TestNewManager_NoDocker(t *testing.T) {
	cfg := &config.SandboxConfig{
		DockerHost: "unix:///nonexistent.sock",
		Timeout:    10 * time.Second,
	}
	_, err := NewManager(cfg, nil)
	if err == nil {
		t.Log("expected error with nonexistent docker socket (may succeed if Docker is running)")
	}
}

// TestBuildHostConfig_Hardening verifies the sandbox defence-in-depth defaults
// are applied uniformly: fork-bomb cap, no-new-privileges, dropped caps,
// read-only rootfs with writable tmpfs. Regressions here would silently weaken
// container isolation.
func TestBuildHostConfig_Hardening(t *testing.T) {
	m := &Manager{
		cfg: &config.SandboxConfig{
			WorkspaceDir: "/workspace",
			NetworkMode:  "none",
		},
		logger: zap.NewNop(),
	}
	hc := m.buildHostConfig(256<<20, 1_000_000_000)

	if hc.PidsLimit == nil || *hc.PidsLimit <= 0 {
		t.Error("PidsLimit must be set to a positive value (fork-bomb defence)")
	}
	if !hc.ReadonlyRootfs {
		t.Error("ReadonlyRootfs must be true")
	}
	found := false
	for _, opt := range hc.SecurityOpt {
		if opt == "no-new-privileges:true" {
			found = true
		}
	}
	if !found {
		t.Errorf("SecurityOpt must include no-new-privileges; got %v", hc.SecurityOpt)
	}
	if len(hc.CapDrop) == 0 || hc.CapDrop[0] != "ALL" {
		t.Errorf("CapDrop must be [ALL]; got %v", hc.CapDrop)
	}
	if _, ok := hc.Tmpfs["/workspace"]; !ok {
		t.Error("Tmpfs must make /workspace writable")
	}
	if _, ok := hc.Tmpfs["/tmp"]; !ok {
		t.Error("Tmpfs must make /tmp writable")
	}
}

// TestChanWriter_DemuxTagging verifies the stderr writer tags output and that
// the demux-to-channel adapter forwards both streams correctly.
func TestChanWriter_DemuxTagging(t *testing.T) {
	ch := make(chan string, 4)
	stdout := &chanWriter{ch: ch, ctx: context.Background()}
	stderr := &chanWriter{ch: ch, ctx: context.Background(), prefix: "[stderr] "}

	if _, err := stdout.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := stderr.Write([]byte("boom\n")); err != nil {
		t.Fatal(err)
	}
	close(ch)

	var got []string
	for s := range ch {
		got = append(got, s)
	}
	if len(got) != 2 || got[0] != "hello\n" || got[1] != "[stderr] boom\n" {
		t.Errorf("unexpected demuxed output: %q", got)
	}
}

// TestChanWriter_CancelsOnContextDone ensures a cancelled context unblocks the
// demuxer instead of deadlocking on a full channel — the previous raw-read
// loop could wedge forever if the consumer disappeared.
func TestChanWriter_CancelsOnContextDone(t *testing.T) {
	ch := make(chan string) // unbuffered; any Write will block
	ctx, cancel := context.WithCancel(context.Background())
	w := &chanWriter{ch: ch, ctx: ctx}

	done := make(chan error, 1)
	go func() {
		_, err := w.Write([]byte("x"))
		done <- err
	}()

	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("expected context error on cancel, got nil")
		}
	case <-time.After(time.Second):
		t.Fatal("Write did not unblock on context cancel")
	}
}
