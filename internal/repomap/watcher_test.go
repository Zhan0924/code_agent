package repomap

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

// helper: create a file watcher with short interval for fast tests.
func newFastWatcher(t *testing.T, root string) *Watcher {
	t.Helper()
	gen := NewGenerator(zap.NewNop())
	w := NewWatcher(root, gen, zap.NewNop())
	w.pollInterval = 50 * time.Millisecond
	w.debounceWindow = 20 * time.Millisecond
	w.SetPollingFallback(true) // tests exercise the polling path
	return w
}

func TestWatcher_NewWatcher_Defaults(t *testing.T) {
	dir := t.TempDir()
	gen := NewGenerator(zap.NewNop())
	w := NewWatcher(dir, gen, zap.NewNop())

	if w.rootDir != dir {
		t.Errorf("rootDir = %q, want %q", w.rootDir, dir)
	}
	if w.generator != gen {
		t.Error("generator not set correctly")
	}
	if w.pollInterval != 3*time.Second {
		t.Errorf("default pollInterval = %v, want 3s", w.pollInterval)
	}
	if w.debounceWindow != 500*time.Millisecond {
		t.Errorf("default debounceWindow = %v, want 500ms", w.debounceWindow)
	}
	if w.modTimes == nil {
		t.Error("modTimes should be initialized")
	}
}

func TestWatcher_SetOnChange(t *testing.T) {
	w := newFastWatcher(t, t.TempDir())
	called := false
	w.SetOnChange(func(_ string) { called = true })

	// Trigger via direct invocation
	if w.onChange == nil {
		t.Fatal("onChange should be set")
	}
	w.onChange("test.go")
	if !called {
		t.Error("onChange callback was not invoked")
	}
}

func TestWatcher_Snapshot(t *testing.T) {
	dir := t.TempDir()
	// Create a supported file
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Create an unsupported file (should be ignored)
	if err := os.WriteFile(filepath.Join(dir, "README.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Create a hidden dir (should be skipped)
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: x"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := newFastWatcher(t, dir)
	w.snapshot()

	if len(w.modTimes) != 1 {
		t.Errorf("snapshot size = %d, want 1 (only main.go)", len(w.modTimes))
	}
	if _, ok := w.modTimes["main.go"]; !ok {
		t.Error("main.go should be in snapshot")
	}
}

func TestWatcher_Poll_DetectsNewAndModified(t *testing.T) {
	dir := t.TempDir()
	// Initial file
	initial := filepath.Join(dir, "main.go")
	if err := os.WriteFile(initial, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := newFastWatcher(t, dir)
	w.snapshot()

	// Track change events
	var changes int32
	w.SetOnChange(func(_ string) { atomic.AddInt32(&changes, 1) })

	// Add a NEW file
	newFile := filepath.Join(dir, "util.go")
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(newFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Modify an existing file (need to advance mod time)
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(initial, future, future)

	w.poll()

	got := atomic.LoadInt32(&changes)
	if got < 1 {
		t.Errorf("poll should detect changes, got %d events", got)
	}

	// util.go should now be in snapshot
	if _, ok := w.modTimes["util.go"]; !ok {
		t.Error("util.go should be in modTimes after poll")
	}
}

func TestWatcher_Poll_DetectsDeletion(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "gone.go")
	if err := os.WriteFile(f, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := newFastWatcher(t, dir)
	w.snapshot()

	if _, ok := w.modTimes["gone.go"]; !ok {
		t.Fatal("file must be in initial snapshot")
	}

	var detected int32
	w.SetOnChange(func(_ string) { atomic.AddInt32(&detected, 1) })

	// Delete the file
	if err := os.Remove(f); err != nil {
		t.Fatal(err)
	}

	w.poll()

	if atomic.LoadInt32(&detected) != 1 {
		t.Errorf("deletion should trigger 1 change event, got %d", atomic.LoadInt32(&detected))
	}
	if _, ok := w.modTimes["gone.go"]; ok {
		t.Error("gone.go should be removed from modTimes after poll")
	}
}

func TestWatcher_Start_StopsOnContextCancel(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := newFastWatcher(t, dir)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		w.Start(ctx)
		close(done)
	}()

	// Let the watcher run a couple of poll cycles
	time.Sleep(120 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// ok: watcher exited
	case <-time.After(1 * time.Second):
		t.Fatal("watcher did not stop within 1s of context cancellation")
	}
}

func TestTruncateList(t *testing.T) {
	got := truncateList([]string{"a", "b", "c"}, 5)
	if len(got) != 3 {
		t.Errorf("short list should be unchanged, got len=%d", len(got))
	}

	got = truncateList([]string{"a", "b", "c", "d", "e"}, 3)
	if len(got) != 3 {
		t.Errorf("truncate len=%d, want 3", len(got))
	}
	if got[0] != "a" || got[2] != "c" {
		t.Errorf("truncate wrong elements: %v", got)
	}

	// Empty
	got = truncateList(nil, 5)
	if len(got) != 0 {
		t.Errorf("nil list should yield empty, got %v", got)
	}
}
