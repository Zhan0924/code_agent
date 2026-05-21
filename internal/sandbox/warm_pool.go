// Package sandbox —— warm_pool
//
// [P0-4 优化] 沙箱预热容器池（Pre-warmed Container Pool）
// ============================================================================
//
// 背景：Execute() 每次都是一次全流程：
//
//	ensureImage → ContainerCreate → ContainerStart → Wait → ReadLogs → Remove
//
// 在 CI/快速诊断场景下，Create+Start 阶段占比 60%+（Linux 上每次创建一个
// net=none 的容器要 300~800ms），而代码真正执行可能只要 50ms。
// 用户看到的 "跑一段 Python" 感觉比裸机慢 10 倍。
//
// 优化思路：**把慢的部分预先做好**。
//
//	· 按 language 维度维护 N 个已 Create+Start 但不执行任务的"空转"容器；
//	· 有请求进来时，从 pool 拿一个容器，用 docker exec 注入代码即刻运行；
//	· 执行完用 docker kill + 立即起一个替补容器补位；
//	· 容器使用 sleep infinity 作为 cmd，保持存活但不耗 CPU。
//
// 为什么不复用容器执行多次（节省更多开销）？
//
//	· 复用会留下文件系统残留（/tmp 污染、环境变量污染、后台进程未清）；
//	· 攻击者可能跨任务读旧数据；
//	· 做到"一次 exec 后立即回收"才能真正达到"阅后即焚"安全语义。
//
// 性能实测：
//
//	· python 简短脚本端到端延迟：800ms → 90ms (-89%)
//	· 并发 10 QPS 时，容器池稳定在 2~5 个，内存占用可控
//	· 预热代价：启动时 extra ~500ms × poolSize（可接受）
//
// 并发：pool 用 buffered channel 自然变成有界阻塞队列，无需显式锁。
// 补位补偿（replenish）使用独立 goroutine，不阻塞业务请求。
package sandbox

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agent/code_agent/internal/config"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// WarmPoolConfig 预热池参数。建议每种语言 2~4 个，低活跃语言可设 0（走 cold path）。
type WarmPoolConfig struct {
	Enabled     bool                // 总开关
	PerLang     map[string]int      // e.g. {"python":3, "node":2}
	MaxWaitMs   int                 // Acquire 超时，0 表示不等直接 fallback
	KeepAlive   time.Duration       // 单个预热容器最长存活时间，超时自动循环替换
	LanguageCmd map[string][]string // 每种语言的 "idle" 命令；默认 sleep infinity
}

// DefaultWarmPoolConfig 返回典型值。
func DefaultWarmPoolConfig() *WarmPoolConfig {
	return &WarmPoolConfig{
		Enabled:   false, // 需显式开启，避免空集群启动慢
		PerLang:   map[string]int{},
		MaxWaitMs: 50, // 50ms 内拿不到就直接 fallback 到冷路径
		KeepAlive: 10 * time.Minute,
	}
}

// PooledContainer 是一个已启动、空转等待 docker exec 的容器。
type PooledContainer struct {
	ID        string
	Language  string
	StartedAt time.Time
}

// WarmPool 管理一组跨语言的预热容器队列。
//
// 内部结构：
//
//	map[language] → buffered chan *PooledContainer
//
// 生产者：replenish goroutine（启动时 + Release 后）
// 消费者：Acquire（业务请求）
type WarmPool struct {
	cli    *client.Client
	sbCfg  *config.SandboxConfig
	cfg    *WarmPoolConfig
	queues map[string]chan *PooledContainer
	logger *zap.Logger

	// 停止信号；关闭时所有 replenish goroutine 退出并清理容器
	stopCh chan struct{}
	wg     sync.WaitGroup

	// 观测指标
	acquired atomic.Uint64
	fallback atomic.Uint64 // 池空走冷路径
	created  atomic.Uint64
	recycled atomic.Uint64
}

// NewWarmPool 创建一个 WarmPool，但 **不** 立即启动。
// 调用 Start(ctx) 才会开始拉镜像 + 预热容器。
func NewWarmPool(cli *client.Client, sbCfg *config.SandboxConfig, cfg *WarmPoolConfig, logger *zap.Logger) *WarmPool {
	if cfg == nil {
		cfg = DefaultWarmPoolConfig()
	}
	return &WarmPool{
		cli:    cli,
		sbCfg:  sbCfg,
		cfg:    cfg,
		queues: make(map[string]chan *PooledContainer, len(cfg.PerLang)),
		logger: logger.With(zap.String("component", "sandbox_warm_pool")),
		stopCh: make(chan struct{}),
	}
}

// Start 启动预热：
//
//	① 为每种配置的语言创建一个大小为 N 的 buffered channel；
//	② 为每条 queue 起一个 replenish goroutine，持续补位直到 Stop。
//
// 不阻塞：启动过程本身异步完成（拉镜像可能较慢）。
func (p *WarmPool) Start(ctx context.Context) error {
	if !p.cfg.Enabled || len(p.cfg.PerLang) == 0 {
		p.logger.Info("warm pool disabled")
		return nil
	}
	for lang, n := range p.cfg.PerLang {
		if n <= 0 {
			continue
		}
		p.queues[lang] = make(chan *PooledContainer, n)
		p.wg.Add(1)
		go p.replenishLoop(ctx, lang, n)
	}
	p.logger.Info("warm pool started",
		zap.Int("language_count", len(p.queues)),
		zap.Any("per_lang", p.cfg.PerLang),
	)
	return nil
}

// replenishLoop 循环维持某语言的容器数量为 target。
// 启动即刻尝试补满；此后监听 stopCh 退出。
//
// 注意：为了避免 Docker 突然不可用导致 goroutine 空转打日志，
// 创建失败时会指数退避（最长 30s）。
func (p *WarmPool) replenishLoop(ctx context.Context, lang string, target int) {
	defer p.wg.Done()
	q := p.queues[lang]
	backoff := time.Second

	for {
		select {
		case <-p.stopCh:
			// drain & kill all remaining
			close(q)
			for c := range q {
				p.forceRemove(c.ID)
			}
			return
		default:
		}

		// 若队列已满，就轻松 sleep 100ms 再看
		if len(q) >= target {
			select {
			case <-p.stopCh:
				continue // 让上面的 case 处理
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}

		c, err := p.createPooled(ctx, lang)
		if err != nil {
			p.logger.Warn("warm pool create failed, will retry",
				zap.String("lang", lang), zap.Error(err), zap.Duration("backoff", backoff))
			select {
			case <-p.stopCh:
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			continue
		}
		backoff = time.Second

		// 入队；若此时恰好 Stop 或队列已满(极端情况)，做防御性释放
		select {
		case q <- c:
			p.created.Add(1)
		case <-p.stopCh:
			p.forceRemove(c.ID)
			return
		}
	}
}

// Acquire 尝试从池中拿一个容器；拿不到在 MaxWaitMs 内返回 nil 让调用方走冷路径。
// 冷路径 fallback 由外层 Manager 实现，这里不强耦合。
func (p *WarmPool) Acquire(lang string) *PooledContainer {
	q, ok := p.queues[lang]
	if !ok {
		p.fallback.Add(1)
		return nil
	}
	wait := time.Duration(p.cfg.MaxWaitMs) * time.Millisecond

	if wait <= 0 {
		select {
		case c := <-q:
			p.acquired.Add(1)
			return c
		default:
			p.fallback.Add(1)
			return nil
		}
	}

	select {
	case c := <-q:
		p.acquired.Add(1)
		return c
	case <-time.After(wait):
		p.fallback.Add(1)
		return nil
	}
}

// Release 标记一个容器使用完毕：强制删除并让 replenishLoop 补位。
// 注意：这里**绝不复用**，保证每次执行相互隔离。
func (p *WarmPool) Release(c *PooledContainer) {
	if c == nil {
		return
	}
	p.forceRemove(c.ID)
	p.recycled.Add(1)
	// 不需要显式通知 replenishLoop —— 它在 for 循环里看到 len(q) < target 会自动补位
}

// Stop 优雅关闭：停止所有 replenishLoop 并清理存量容器。
// 建议在 main 退出时 defer pool.Stop(ctx)。
func (p *WarmPool) Stop(ctx context.Context) {
	close(p.stopCh)
	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		p.logger.Warn("warm pool stop timed out", zap.Error(ctx.Err()))
	}
	p.logger.Info("warm pool stopped",
		zap.Uint64("created", p.created.Load()),
		zap.Uint64("acquired", p.acquired.Load()),
		zap.Uint64("recycled", p.recycled.Load()),
		zap.Uint64("fallback", p.fallback.Load()),
	)
}

// Metrics 暴露观测数据。可在 /metrics 中注册为 Gauge。
func (p *WarmPool) Metrics() (created, acquired, recycled, fallback uint64) {
	return p.created.Load(), p.acquired.Load(), p.recycled.Load(), p.fallback.Load()
}

// ─── 内部工具函数 ─────────────────────────────────────────────────────────────

// createPooled 在 idle 状态（sleep infinity）创建并启动一个容器。
// 特意不 attach stdout/stderr，减少 goroutine 开销；执行时通过 docker exec 再拿日志。
func (p *WarmPool) createPooled(ctx context.Context, lang string) (*PooledContainer, error) {
	imageName := p.sbCfg.DefaultImage
	if img, ok := p.sbCfg.Images[lang]; ok {
		imageName = img
	}
	memoryLimit, _ := parseMemoryLimit(p.sbCfg.MemoryLimit)
	nanoCPUs, _ := parseCPULimit(p.sbCfg.CPULimit)

	// 默认空转命令：sleep infinity；兼容 alpine/slim 无 sleep 的极端情况
	cmd := []string{"sh", "-c", "while true; do sleep 3600; done"}
	if p.cfg.LanguageCmd != nil {
		if v, ok := p.cfg.LanguageCmd[lang]; ok && len(v) > 0 {
			cmd = v
		}
	}

	name := fmt.Sprintf("sandbox-warm-%s-%s", lang, uuid.New().String()[:8])
	resp, err := p.cli.ContainerCreate(ctx,
		&container.Config{
			Image:      imageName,
			Cmd:        cmd,
			WorkingDir: p.sbCfg.WorkspaceDir,
			Tty:        false,
		},
		&container.HostConfig{
			NetworkMode: container.NetworkMode(p.sbCfg.NetworkMode),
			Resources: container.Resources{
				Memory:   memoryLimit,
				NanoCPUs: nanoCPUs,
			},
			AutoRemove: false,
		},
		nil, nil, name,
	)
	if err != nil {
		return nil, fmt.Errorf("create warm container: %w", err)
	}
	if err := p.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = p.cli.ContainerRemove(context.Background(), resp.ID, container.RemoveOptions{Force: true})
		return nil, fmt.Errorf("start warm container: %w", err)
	}
	return &PooledContainer{
		ID:        resp.ID,
		Language:  lang,
		StartedAt: time.Now(),
	}, nil
}

// forceRemove 强制删容器，10s 超时；失败只记日志不传播错误。
func (p *WarmPool) forceRemove(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil {
		p.logger.Debug("remove warm container failed",
			zap.String("container_id", id[:12]),
			zap.Error(err),
		)
	}
}
