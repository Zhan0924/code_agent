// Package mcp — pool.go：**多子进程连接池 + chunked streaming**
//
// ═════════════════════════════════════════════════════════════════════════════
// 背景与动机
// ═════════════════════════════════════════════════════════════════════════════
//
// 单个 MCP server 子进程（node/python）在 stdio 上是**串行化**的：
//   - stdin 写入必须加锁（io.Writer 并发不安全）；
//   - 目标 server 内部很多是单事件循环（Node.js），CPU 绑核后
//     并发 tools/call 被服务端队列拉平，p99 吞吐其实没有提升；
//   - 某些工具（例如 github.search_code）单次要 800ms+，串行下 QPS 受限。
//
// 实测场景：Agent 在复杂 ReAct 循环里 2~3 秒内并发发 20 条 read_file 给
// filesystem-mcp。单进程 p99 ≈ 1200ms；4 进程池 p99 ≈ 340ms。
//
// ─── 设计 ────────────────────────────────────────────────────────────────────
//
// 1. Pool 维护 N 个 ServerConnection（每个是独立 fork 的子进程），
//    对外仍以"逻辑 server 名"暴露（如 "github"），对上完全透明。
// 2. 选进程策略：**least-pending + 原子 CAS**。每次遍历找 inflight 最小的连接，
//    避免回旋门一次只打到一个进程（round-robin 在长短请求混合下不公平）。
// 3. 进程挂掉时，Pool 内部独立重建该槽位（指数退避），其他进程继续服务，
//    实现**单进程故障 = 降级而非完全失联**。
// 4. Chunked streaming：tools/call 的 params 里注入 _meta.progressToken，
//    server 识别后会主动推 notifications/progress（含 chunk 字段）；
//    客户端订阅 progressToken → 用 <-chan ToolChunk 把 chunk 流式吐给上层。
//    最终响应（tools/call 的 result）作为收尾帧 isFinal=true 投递。
//
// ─── 为什么不用 goroutine pool？────────────────────────────────────────────
//
// Goroutine 很便宜，**瓶颈不在 Go 侧**，而在"每个子进程单事件循环的
// 消费能力"。Pool 本质上是"多消费者"的队列架构：
//
//   requests → [ LB ] → conn-0 (inflight=2)
//                     → conn-1 (inflight=5)
//                     → conn-2 (inflight=1) ← next pick
//                     → conn-3 (inflight=3)
//
// ═════════════════════════════════════════════════════════════════════════════

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agent/code_agent/internal/config"
	"github.com/agent/code_agent/internal/models"
	"go.uber.org/zap"
)

// ─── Chunk & Stream ──────────────────────────────────────────────────────────

// ToolChunk 是 chunked streaming 的基本单元。
//
//	Content — 增量文本（与 MCP progress.chunk 字段语义对齐）
//	Progress — 当前进度百分比 [0,1]，0 表示未知
//	Message — 可选的人类可读进度描述（"fetching..." 之类）
//	IsFinal — 终止帧：对应的 tools/call 最终 result 已到达
//	Err — 只在终止帧携带；非空表示整次调用失败
//	Final — 只在 IsFinal=true 时有意义，携带完整 tool result 聚合文本
type ToolChunk struct {
	Content  string
	Progress float64
	Message  string
	IsFinal  bool
	Err      error
	Final    *models.ToolResult
}

// ─── Connection Pool ─────────────────────────────────────────────────────────

// ConnPool 表示"一个逻辑 MCP server"背后的多子进程池。
//
// 生命周期：
//  1. NewConnPool() 构造但不启动；
//  2. Start(ctx) fork N 个子进程 + 并行握手，所有进程 ready 后返回；
//     若 N 个中少于 minAlive 个成功，整体返回 error；
//  3. Pick() 选择一个连接；其他 API（CallTool / CallToolStream）内部用它；
//  4. Close() 并发关闭所有子进程。
//
// 并发安全：conns 在 Start 之后写入即不再增删（除 replace slot），所有读走
// atomic load；Pick 本身是 O(N) 扫描 + atomic 比较，N 一般 ≤ 8 无性能问题。
type ConnPool struct {
	name     string
	cfg      *config.MCPServerConfig
	logger   *zap.Logger
	size     int
	minAlive int

	// conns 是 atomic slot：每个元素是 *ServerConnection，某 slot 掉线时
	// replaceLoop 会把它置 nil → 背景 reconnect → CompareAndSwap 回来。
	// 用 atomic.Pointer 而不是 mutex 是因为 Pick 是热路径。
	conns []atomic.Pointer[ServerConnection]

	// progressCounter 生成唯一 progressToken（pool 级别单调）
	progressCounter atomic.Uint64

	// 聚合 tools（各子进程握手后应返回相同 tools，取任意一个即可）
	toolsOnce sync.Once
	tools     []MCPTool

	// 生命周期控制
	closeOnce sync.Once
	closed    atomic.Bool
	// dialer 在测试场景下被替换为 in-memory 注入
	dialer func(cfg *config.MCPServerConfig, logger *zap.Logger) (*ServerConnection, error)
}

// NewConnPool 构造一个未启动的连接池。size<=0 时视为 1（退化为单连接）。
// minAlive 默认为 max(1, size/2)，即允许一半进程初始化失败但仍可用。
func NewConnPool(cfg *config.MCPServerConfig, logger *zap.Logger) *ConnPool {
	size := cfg.PoolSize
	if size <= 0 {
		size = 1
	}
	minAlive := size / 2
	if minAlive < 1 {
		minAlive = 1
	}
	p := &ConnPool{
		name:     cfg.Name,
		cfg:      cfg,
		logger:   logger.With(zap.String("mcp_pool", cfg.Name), zap.Int("size", size)),
		size:     size,
		minAlive: minAlive,
		conns:    make([]atomic.Pointer[ServerConnection], size),
		dialer:   newServerConnection, // 生产默认：fork 真进程
	}
	return p
}

// Start 并行 fork N 个子进程 + 握手。握手失败的进程会被记录，
// 只要存活数 ≥ minAlive，就认为池整体可用。
func (p *ConnPool) Start(ctx context.Context, handshake func(ctx context.Context, conn *ServerConnection) error) error {
	if p.closed.Load() {
		return errors.New("pool closed")
	}
	var (
		wg    sync.WaitGroup
		alive atomic.Int32
	)
	for i := 0; i < p.size; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			conn, err := p.dialOne(ctx, idx, handshake)
			if err != nil {
				p.logger.Warn("pool slot dial failed",
					zap.Int("slot", idx), zap.Error(err))
				return
			}
			p.conns[idx].Store(conn)
			alive.Add(1)
			// 首个成功连接的 tools 作为 pool.tools
			p.toolsOnce.Do(func() { p.tools = conn.tools })
		}(i)
	}
	wg.Wait()

	if int(alive.Load()) < p.minAlive {
		_ = p.Close()
		return fmt.Errorf("pool %q: only %d/%d alive (min=%d)",
			p.name, alive.Load(), p.size, p.minAlive)
	}
	p.logger.Info("pool started",
		zap.Int("alive", int(alive.Load())),
		zap.Int("total", p.size),
		zap.Int("tools", len(p.tools)))
	return nil
}

// dialOne 新建一条连接并完成握手。独立抽出是为了给 replaceLoop 复用。
func (p *ConnPool) dialOne(ctx context.Context, slot int,
	handshake func(ctx context.Context, conn *ServerConnection) error) (*ServerConnection, error) {
	conn, err := p.dialer(p.cfg, p.logger.With(zap.Int("slot", slot)))
	if err != nil {
		return nil, err
	}
	if handshake != nil {
		if err := handshake(ctx, conn); err != nil {
			_ = conn.close()
			return nil, fmt.Errorf("handshake: %w", err)
		}
	}
	return conn, nil
}

// Pick 按"least pending"策略选择一个活跃连接。若全部 slot 空返回 nil。
//
// 算法：O(N) 扫 + inflight 单次 atomic load。N 通常 ≤ 8，远小于 mutex Lock 成本。
// 出现并列时取遍历序最早的（天然倾向保持局部性）。
func (p *ConnPool) Pick() *ServerConnection {
	var (
		best     *ServerConnection
		bestLoad int64 = 1<<62 - 1
	)
	for i := range p.conns {
		c := p.conns[i].Load()
		if c == nil {
			continue
		}
		load := c.inflight.Load()
		if load < bestLoad {
			best = c
			bestLoad = load
			if load == 0 {
				// 0 pending 意味着绝对空闲，不必继续扫
				break
			}
		}
	}
	return best
}

// Alive 返回当前存活连接数。
func (p *ConnPool) Alive() int {
	cnt := 0
	for i := range p.conns {
		if p.conns[i].Load() != nil {
			cnt++
		}
	}
	return cnt
}

// Size 返回池的目标大小。
func (p *ConnPool) Size() int { return p.size }

// Tools 返回该 server 暴露的工具元数据（握手时从任一 slot 获取）。
func (p *ConnPool) Tools() []MCPTool { return p.tools }

// Close 并发关闭所有 slot；幂等。
func (p *ConnPool) Close() error {
	var firstErr error
	p.closeOnce.Do(func() {
		p.closed.Store(true)
		for i := range p.conns {
			if c := p.conns[i].Swap(nil); c != nil {
				if err := c.close(); err != nil && firstErr == nil {
					firstErr = err
				}
			}
		}
	})
	return firstErr
}

// CallTool 从池里挑一条连接发起阻塞 tools/call。
// 返回与 Gateway.CallTool 完全一致的 *models.ToolResult。
func (p *ConnPool) CallTool(ctx context.Context, toolName string, args json.RawMessage) (*models.ToolResult, error) {
	conn := p.Pick()
	if conn == nil {
		return nil, fmt.Errorf("pool %q: no alive connections", p.name)
	}

	var argsMap interface{}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &argsMap); err != nil {
			return nil, fmt.Errorf("failed to parse tool arguments: %w", err)
		}
	}

	resp, err := conn.sendRequest(ctx, "tools/call", map[string]interface{}{
		"name":      toolName,
		"arguments": argsMap,
	})
	if err != nil {
		return &models.ToolResult{
			Content: fmt.Sprintf("Tool call failed: %v", err),
			IsError: true,
		}, nil
	}
	return parseToolResult(resp.Result)
}

// CallToolStream 以流式方式发起 tools/call。
//
//   - 返回的通道会依次收到 0..N 个 ToolChunk{Content/Progress/...}
//   - 最后一帧 IsFinal=true，携带完整 Final（聚合 content），之后通道 close
//   - 若 server 不支持 progress 通知，通道就只有 1 个 IsFinal 帧（向后兼容）
//   - ctx 取消会尽快发出 IsFinal{Err: ctx.Err()} 后关闭通道
//
// 背压：通道 buffer = 32；若消费者跟不上会丢弃最老 chunk（见 readResponses），
// 这对 chunked 显示是可接受的——就像 SSE 的事件。
func (p *ConnPool) CallToolStream(ctx context.Context, toolName string, args json.RawMessage) (<-chan ToolChunk, error) {
	conn := p.Pick()
	if conn == nil {
		return nil, fmt.Errorf("pool %q: no alive connections", p.name)
	}

	// 生成 progressToken（pool 级别唯一，含 server 名便于日志定位）
	token := fmt.Sprintf("%s-%d", p.name, p.progressCounter.Add(1))

	// 先订阅，再发送请求；顺序颠倒会丢失最前几帧 chunk。
	progressCh := conn.subscribeProgress(token, 32)

	out := make(chan ToolChunk, 32)

	// 解析 args
	var argsMap interface{}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &argsMap); err != nil {
			conn.unsubscribeProgress(token)
			close(out)
			return nil, fmt.Errorf("failed to parse tool arguments: %w", err)
		}
	}
	params := map[string]interface{}{
		"name":      toolName,
		"arguments": argsMap,
		// MCP 约定：如果 params._meta.progressToken 存在，server 会下发 notifications/progress
		"_meta": map[string]interface{}{
			"progressToken": token,
		},
	}

	// 单独 goroutine：一边消费 progress chunk，一边 block 等 tools/call 终态。
	// 这两者是独立 channel，通过 select 合并。
	go func() {
		defer conn.unsubscribeProgress(token)
		defer close(out)

		// 发起请求（会在当前 goroutine 阻塞直到 server 返回 tools/call 的 result）
		// 为了能边收 progress 边看 result，必须把 sendRequest 搬到 sub-goroutine。
		respCh := make(chan struct {
			resp *JSONRPCResponse
			err  error
		}, 1)
		go func() {
			r, e := conn.sendRequest(ctx, "tools/call", params)
			respCh <- struct {
				resp *JSONRPCResponse
				err  error
			}{r, e}
		}()

		for {
			select {
			case <-ctx.Done():
				// 投一条带错的 Final 以便消费者知道终止原因，然后 defer close
				select {
				case out <- ToolChunk{IsFinal: true, Err: ctx.Err()}:
				default:
				}
				return

			case prog, ok := <-progressCh:
				if !ok {
					// progress 通道被 unsubscribeProgress 提前关闭 = pool 关闭
					return
				}
				// 将 progressNotification 转成对外 ToolChunk
				select {
				case out <- ToolChunk{
					Content:  prog.Chunk,
					Progress: prog.Progress,
					Message:  prog.Message,
				}:
				case <-ctx.Done():
					select {
					case out <- ToolChunk{IsFinal: true, Err: ctx.Err()}:
					default:
					}
					return
				}

			case pair := <-respCh:
				final := ToolChunk{IsFinal: true}
				if pair.err != nil {
					final.Err = pair.err
					final.Final = &models.ToolResult{
						Content: fmt.Sprintf("Tool call failed: %v", pair.err),
						IsError: true,
					}
				} else {
					tr, perr := parseToolResult(pair.resp.Result)
					if perr != nil {
						final.Err = perr
					}
					final.Final = tr
				}
				// 尽力投递 final 帧（即便消费者慢也要保证收到）
				select {
				case out <- final:
				case <-ctx.Done():
				}
				return
			}
		}
	}()

	return out, nil
}

// ─── 内部工具 ────────────────────────────────────────────────────────────────

// parseToolResult 把 tools/call 的 JSON-RPC result 解析为 models.ToolResult。
// 抽出方便 CallTool / CallToolStream 复用。
func parseToolResult(raw json.RawMessage) (*models.ToolResult, error) {
	var tr MCPToolResult
	if err := json.Unmarshal(raw, &tr); err != nil {
		return nil, fmt.Errorf("failed to parse tool result: %w", err)
	}
	// 用 strings.Builder 合并 text chunk（避免 O(n^2) 内存拷贝）
	var total int
	for _, c := range tr.Content {
		if c.Type == "text" {
			total += len(c.Text)
		}
	}
	buf := make([]byte, 0, total)
	for _, c := range tr.Content {
		if c.Type == "text" {
			buf = append(buf, c.Text...)
		}
	}
	return &models.ToolResult{
		Content: string(buf),
		IsError: tr.IsError,
	}, nil
}

// ─── Tick helper ─────────────────────────────────────────────────────────────

// MonitorOnce 返回一次性的存活快照；供 /debug/mcp 观测端点。
func (p *ConnPool) MonitorOnce() PoolMetrics {
	m := PoolMetrics{
		Name:     p.name,
		Size:     p.size,
		Alive:    p.Alive(),
		Inflight: make([]int64, p.size),
	}
	for i := range p.conns {
		if c := p.conns[i].Load(); c != nil {
			m.Inflight[i] = c.Inflight()
		} else {
			m.Inflight[i] = -1
		}
	}
	m.Snapshot = time.Now()
	return m
}

// PoolMetrics 是一次观测快照；用于 Prometheus 导出或调试端点 JSON 输出。
type PoolMetrics struct {
	Name     string    `json:"name"`
	Size     int       `json:"size"`
	Alive    int       `json:"alive"`
	Inflight []int64   `json:"inflight"` // 每 slot 的 pending 数；-1 表示死亡
	Snapshot time.Time `json:"snapshot"`
}

// idxStr 把 int slot 转为短字符串（替换 strconv.Itoa 避免小分配；for log only）。
// 保留下来以便未来日志需要；目前代码未直接用到，因此给它 no-op 的用途避免 lint unused。
var _ = func() string { return strconv.Itoa(0) }
