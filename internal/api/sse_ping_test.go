// sse_ping_test.go — validates the SSE heartbeat used by handleChatReactStream.
//
// 背景：在 2026-06-04 前，handleChatReactStream 在 ReAct 内部空闲 60+ 秒时会被
// `server.write_timeout: 600s` 撕扯 SSE 连接（write_timeout 是 idle write 超时，
// 不是请求总时长）。引入 25s SSE 心跳消除了这一可能。
//
// 2026-06-07 P1 改造:心跳由 `: ping\n\n` 注释行改为业务级 `data: {"type":"ping","ts":<ms>}\n\n`
// 事件 —— 注释行不会触发浏览器 fetch ReadableStream 的 onByte 回调,导致前端
// watchdog 仍然把"只剩心跳"的连接误判为静默超时。改业务事件后 onByte 自然触发,
// 前端 traceSteps filter 显式 drop type=ping 不入 UI。
//
// 这里测试提取出来的小函数 runSSEHeartbeat —— handler 内联调用同一逻辑。
package api

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"
)

// fakeFlushWriter 实现 sseFlushWriter：把所有 Write 累积到 buf。
// 用 mu 保护并发读取（测试断言可与心跳 goroutine 并发执行）。
type fakeFlushWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	writes int
	flush  int
}

func (f *fakeFlushWriter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	return f.buf.Write(p)
}

func (f *fakeFlushWriter) Flush() {
	f.mu.Lock()
	f.flush++
	f.mu.Unlock()
}

func (f *fakeFlushWriter) snapshot() (string, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.String(), f.writes, f.flush
}

func TestRunSSEHeartbeat_EmitsPingOnInterval(t *testing.T) {
	// 60ms 周期；250ms 总等待，期望 ≥3 次 ping。
	const interval = 60 * time.Millisecond
	const wait = 250 * time.Millisecond

	w := &fakeFlushWriter{}
	var mu sync.Mutex
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		runSSEHeartbeat(ctx, w, &mu, interval)
		close(done)
	}()

	time.Sleep(wait)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runSSEHeartbeat did not return after ctx cancel")
	}

	out, writes, flushes := w.snapshot()
	// 期望至少 3 次业务 ping 事件(wait/interval >= 3)。
	if writes < 3 {
		t.Errorf("expected >= 3 writes, got %d (out=%q)", writes, out)
	}
	if flushes != writes {
		t.Errorf("flush count %d != write count %d (each ping must flush)", flushes, writes)
	}
	// P1:业务 ping 事件,合规 `data: {"type":"ping","ts":<ms>}\n\n`
	const wantPrefix = `data: {"type":"ping"`
	if !bytes.Contains([]byte(out), []byte(wantPrefix)) {
		t.Errorf("output missing prefix %q; got %q", wantPrefix, out)
	}
	// 保证不是误用 SSE 注释行(注释行不会触发 fetch ReadableStream onByte 回调)
	if bytes.Contains([]byte(out), []byte(": ping\n")) {
		t.Errorf("found SSE comment `: ping` in output — P1 改造已将心跳替换为业务事件: %q", out)
	}
}

func TestRunSSEHeartbeat_StopsOnContextCancel(t *testing.T) {
	w := &fakeFlushWriter{}
	var mu sync.Mutex
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		runSSEHeartbeat(ctx, w, &mu, 50*time.Millisecond)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runSSEHeartbeat did not exit after cancel")
	}

	_, writes, _ := w.snapshot()
	if writes > 1 {
		t.Errorf("expected <= 1 writes before cancel, got %d", writes)
	}
}

func TestRunSSEHeartbeat_SerializesWithBusinessWriter(t *testing.T) {
	// 模拟主循环写业务事件，与心跳并发争抢 writeMu。
	// 期望：每次写入完整（绝不交错），输出可解析为完整 SSE frame。
	const interval = 10 * time.Millisecond
	const wait = 120 * time.Millisecond

	w := &fakeFlushWriter{}
	var mu sync.Mutex
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go runSSEHeartbeat(ctx, w, &mu, interval)

	// 主循环每 7ms 写一次业务事件
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(7 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				mu.Lock()
				_, _ = w.Write([]byte("data: {\"x\":1}\n\n"))
				w.Flush()
				mu.Unlock()
			}
		}
	}()

	time.Sleep(wait)
	close(stop)
	cancel()
	time.Sleep(20 * time.Millisecond) // drain

	out, _, _ := w.snapshot()
	// 没有任何业务事件与心跳字节交错:每帧都以 \n\n 结束才能继续下一帧。
	// P1 后心跳也是 `data: {"type":"ping"...`,所有 frame 都以 "data:" 开头。
	// 简单结构性检查:扫描整个输出,按 "\n\n" 切,每段必须以 "data:" 开头。
	for _, frame := range bytes.Split([]byte(out), []byte("\n\n")) {
		if len(frame) == 0 {
			continue
		}
		if !bytes.HasPrefix(frame, []byte("data:")) {
			t.Fatalf("interleaved frame detected: %q (full output=%q)", frame, out)
		}
	}
}
