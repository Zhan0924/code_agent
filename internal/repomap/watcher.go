// Package repomap builds and maintains a symbol-level "repo map" — a concise
// overview of every file's public identifiers, type signatures, and imports
// that fits into the LLM's prompt without blowing the budget.
//
// ============================================================================
//
//	设 计 原 理
//
// ============================================================================
//
// 【repomap 不是 RAG】
//
//	RAG 解决"在 10 万行代码里找到最相关的 3 段"；repomap 解决"你在进入一个
//	新仓库时要先看的东西"。它把整个仓库压缩成几千 token 的"目录+签名"清单，
//	让 LLM 在不检索的情况下知道：有哪些包、包里有哪些导出函数、函数签名
//	长什么样。这对跨文件重构任务至关重要。
//
// 【watcher 的职责】
//
//	watcher.go 是 repomap 的"实时更新守护"：用 fsnotify 订阅仓库文件变化，
//	有变动就触发对应文件的重解析 + map 更新。避免每次请求都全量重建
//	（大仓库可能要几秒）。
//
// 【watcher 的 debounce】
//
//	编辑器保存动作经常触发多次 fsnotify 事件。直接每个事件都重建会让 IDE
//	保存卡顿。watcher 聚合同一路径的事件，到窗口结束才重建。
//
// ============================================================================
package repomap

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"go.uber.org/zap"
)

// Watcher monitors a directory for file changes and triggers incremental
// re-indexing of the repo map.
//
// Strategy:
//   - Primary: fsnotify (event-driven, sub-second latency, zero idle CPU).
//   - Fallback: Periodic polling when fsnotify isn't available
//     (e.g. some NFS/Docker volumes, OS without inotify support).
//
// A debounce window coalesces bursts of events (IDE save, `git checkout`,
// build output, etc.) into a single onChange callback per affected file.
type Watcher struct {
	rootDir   string
	generator *Generator
	onChange  func(path string) // callback for each changed file
	logger    *zap.Logger

	// Debounce state
	mu             sync.Mutex
	pendingChanges map[string]time.Time
	debounceWindow time.Duration

	// Polling fallback configuration
	modTimes     map[string]time.Time
	pollInterval time.Duration
	forcePolling bool // test hook
}

// NewWatcher creates a file watcher for incremental repo map updates.
func NewWatcher(rootDir string, gen *Generator, logger *zap.Logger) *Watcher {
	return &Watcher{
		rootDir:        rootDir,
		generator:      gen,
		logger:         logger.With(zap.String("component", "repo_watcher")),
		modTimes:       make(map[string]time.Time),
		pendingChanges: make(map[string]time.Time),
		debounceWindow: 500 * time.Millisecond,
		pollInterval:   3 * time.Second,
	}
}

// SetOnChange sets an optional callback invoked for each changed file.
func (w *Watcher) SetOnChange(fn func(path string)) {
	w.onChange = fn
}

// SetDebounceWindow controls how long the watcher waits to coalesce
// successive events on the same file into one onChange notification.
func (w *Watcher) SetDebounceWindow(d time.Duration) {
	if d > 0 {
		w.debounceWindow = d
	}
}

// SetPollingFallback forces the use of the polling strategy even when fsnotify
// is available. This exists mainly so tests can exercise the fallback path
// deterministically across platforms.
func (w *Watcher) SetPollingFallback(force bool) {
	w.forcePolling = force
}

// Start begins watching for file changes. It runs until ctx is cancelled.
func (w *Watcher) Start(ctx context.Context) {
	w.logger.Info("repo watcher started", zap.String("root", w.rootDir))

	// Initial snapshot (needed by polling path; harmless for fsnotify path)
	w.snapshot()

	if !w.forcePolling {
		if err := w.runFsnotify(ctx); err == nil {
			w.logger.Info("repo watcher stopped (fsnotify)")
			return
		} else {
			w.logger.Warn("fsnotify unavailable, falling back to polling", zap.Error(err))
		}
	}

	w.runPolling(ctx)
	w.logger.Info("repo watcher stopped (polling)")
}

// ─── fsnotify path ──────────────────────────────────────────────────────────

func (w *Watcher) runFsnotify(ctx context.Context) error {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer fsw.Close()

	if err := w.addRecursive(fsw); err != nil {
		return err
	}

	// Debounce timer — fires whenever at least one event has been queued.
	var timer *time.Timer
	ensureTimer := func() {
		if timer == nil {
			timer = time.NewTimer(w.debounceWindow)
		}
	}

	for {
		if timer == nil {
			// No pending events — block on either event or cancel.
			select {
			case <-ctx.Done():
				return nil
			case ev, ok := <-fsw.Events:
				if !ok {
					return nil
				}
				if w.shouldEnqueue(ev, fsw) {
					ensureTimer()
				}
			case err, ok := <-fsw.Errors:
				if !ok {
					return nil
				}
				w.logger.Warn("fsnotify error", zap.Error(err))
			}
			continue
		}

		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case ev, ok := <-fsw.Events:
			if !ok {
				timer.Stop()
				return nil
			}
			if w.shouldEnqueue(ev, fsw) {
				// keep timer armed — ensure it is still running
				ensureTimer()
			}
		case err, ok := <-fsw.Errors:
			if !ok {
				timer.Stop()
				return nil
			}
			w.logger.Warn("fsnotify error", zap.Error(err))
		case <-timer.C:
			w.flushPending()
			timer = nil
		}
	}
}

// shouldEnqueue decides whether a raw fsnotify event represents a meaningful
// source-file change. It also maintains the per-directory watch list so that
// newly created sub-directories come under observation automatically.
func (w *Watcher) shouldEnqueue(ev fsnotify.Event, fsw *fsnotify.Watcher) bool {
	// If a new directory is created, add it to the watch list recursively.
	if ev.Op&fsnotify.Create != 0 {
		if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
			base := filepath.Base(ev.Name)
			if !strings.HasPrefix(base, ".") && !skipDirs[base] {
				_ = fsw.Add(ev.Name)
			}
			return false // directory creation itself isn't a file change
		}
	}

	// Filter out irrelevant events.
	if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
		return false
	}
	// Ignore anything outside supported source extensions.
	if _, ok := supportedExts[filepath.Ext(ev.Name)]; !ok {
		return false
	}
	// Ignore hidden files / skipped dirs.
	base := filepath.Base(ev.Name)
	if strings.HasPrefix(base, ".") {
		return false
	}

	rel, err := filepath.Rel(w.rootDir, ev.Name)
	if err != nil {
		rel = ev.Name
	}
	w.mu.Lock()
	w.pendingChanges[rel] = time.Now()
	w.mu.Unlock()
	return true
}

func (w *Watcher) addRecursive(fsw *fsnotify.Watcher) error {
	return filepath.WalkDir(w.rootDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		base := filepath.Base(p)
		if strings.HasPrefix(base, ".") || skipDirs[base] {
			return filepath.SkipDir
		}
		if err := fsw.Add(p); err != nil {
			w.logger.Warn("fsnotify watch add failed", zap.String("dir", p), zap.Error(err))
		}
		return nil
	})
}

func (w *Watcher) flushPending() {
	w.mu.Lock()
	changed := make([]string, 0, len(w.pendingChanges))
	for p := range w.pendingChanges {
		changed = append(changed, p)
	}
	w.pendingChanges = make(map[string]time.Time)
	w.mu.Unlock()

	if len(changed) == 0 {
		return
	}

	w.logger.Info("repo files changed",
		zap.Int("count", len(changed)),
		zap.Strings("files", truncateList(changed, 10)),
	)
	w.generator.InvalidateCache(w.rootDir)
	if w.onChange != nil {
		for _, p := range changed {
			w.onChange(p)
		}
	}
}

// ─── Polling fallback (retained for compatibility) ─────────────────────────

func (w *Watcher) runPolling(ctx context.Context) {
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.poll()
		}
	}
}

func (w *Watcher) snapshot() {
	_ = filepath.Walk(w.rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			name := info.Name()
			if strings.HasPrefix(name, ".") || skipDirs[name] {
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(path)
		if _, ok := supportedExts[ext]; !ok {
			return nil
		}
		rel, _ := filepath.Rel(w.rootDir, path)
		w.modTimes[rel] = info.ModTime()
		return nil
	})
	w.logger.Debug("initial snapshot taken", zap.Int("files", len(w.modTimes)))
}

func (w *Watcher) poll() {
	currentFiles := make(map[string]time.Time)
	var changed []string

	_ = filepath.Walk(w.rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			name := info.Name()
			if strings.HasPrefix(name, ".") || skipDirs[name] {
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(path)
		if _, ok := supportedExts[ext]; !ok {
			return nil
		}
		rel, _ := filepath.Rel(w.rootDir, path)
		currentFiles[rel] = info.ModTime()

		if oldTime, exists := w.modTimes[rel]; !exists || info.ModTime().After(oldTime) {
			changed = append(changed, rel)
		}
		return nil
	})

	for rel := range w.modTimes {
		if _, exists := currentFiles[rel]; !exists {
			changed = append(changed, rel)
		}
	}

	if len(changed) > 0 {
		w.logger.Info("files changed (polling)",
			zap.Int("changed", len(changed)),
			zap.Strings("files", truncateList(changed, 10)),
		)
		w.generator.InvalidateCache(w.rootDir)
		if w.onChange != nil {
			for _, p := range changed {
				w.onChange(p)
			}
		}
		w.modTimes = currentFiles
	}
}

func truncateList(items []string, max int) []string {
	if len(items) <= max {
		return items
	}
	return items[:max]
}
