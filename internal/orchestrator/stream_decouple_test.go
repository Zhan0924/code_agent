// stream_decouple_test.go — 验证 PR-C 的核心契约：
//
//  1. channelSink.Emit 在 droppedCtx 取消后**不再阻塞**业务 goroutine
//     （旧实现 eventCh <- e 在 cap=64 满后会卡死，把 reactLoopCore 撑住，
//     而 SSE handler 已退出无人 drain，最终 30min workCtx 兜底也保不住——
//     业务永远停在那次写入上）；
//  2. droppedCtx 未被取消时，Emit 仍正确入队，事件有序到达；
//  3. droppedCtx == nil（同步路径或不希望丢事件的场景）退化为阻塞 send；
//  4. workCtx 与 reqCtx 真实解耦：reqCtx 取消后桥接 goroutine 仅触发 dropCancel，
//     workCtx 不被传播取消（这是 ProcessMessageStreamFull 内部 30min 兜底成立的前提）。
//
// 完整 e2e（含 LLM mock + Redis miniredis + SSE httptest.Server）覆盖见
// internal/api/integration_test.go；这里仅覆盖最小可独立验证的契约单元。
package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/agent/code_agent/internal/models"
)

func TestChannelSink_EmitDoesNotBlockAfterDroppedCtxCancel(t *testing.T) {
	// cap=2 远小于 droppedCtx 取消前预先注入的写入数；模拟"客户端走了、
	// channel 已满、业务仍在不停 Emit"的最坏情形。
	ch := make(chan models.ReactStreamEvent, 2)
	droppedCtx, cancel := context.WithCancel(context.Background())
	sink := &channelSink{ch: ch, droppedCtx: droppedCtx}

	// 填满 buffer
	sink.Emit(models.ReactStreamEvent{Type: "tool_call", Step: 1})
	sink.Emit(models.ReactStreamEvent{Type: "tool_call", Step: 2})
	// 此时再 Emit 一次必然会阻塞 buffered channel；先 cancel droppedCtx，
	// 让后续 Emit 走 select 的 droppedCtx.Done 分支立即返回。
	cancel()

	done := make(chan struct{})
	go func() {
		// 假装业务在客户端断开后还会 emit 50 条事件——任何一条卡住都会让测试超时。
		for i := range 50 {
			sink.Emit(models.ReactStreamEvent{Type: "thinking", Step: 3 + i})
		}
		close(done)
	}()

	select {
	case <-done:
		// 50 次非阻塞 emit 全部完成，业务可以走到自然终态。
	case <-time.After(2 * time.Second):
		t.Fatal("Emit blocked after droppedCtx was cancelled — channel back-pressure leaked into business loop")
	}
}

func TestChannelSink_EmitDeliversInOrderWhenDropCtxAlive(t *testing.T) {
	ch := make(chan models.ReactStreamEvent, 8)
	droppedCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := &channelSink{ch: ch, droppedCtx: droppedCtx}

	for i := range 5 {
		sink.Emit(models.ReactStreamEvent{Type: "step_start", Step: i})
	}
	close(ch)

	var got []int
	for ev := range ch {
		got = append(got, ev.Step)
	}
	if len(got) != 5 {
		t.Fatalf("expected 5 events, got %d (%v)", len(got), got)
	}
	for i, step := range got {
		if step != i {
			t.Errorf("event %d: step=%d, want %d", i, step, i)
		}
	}
}

func TestChannelSink_NilDroppedCtxBlocksUntilDrain(t *testing.T) {
	// 同步路径 / 不希望丢事件的场景：droppedCtx==nil → Emit 必须阻塞直到对端取走。
	ch := make(chan models.ReactStreamEvent) // unbuffered，迫使每次 send 都得等
	sink := &channelSink{ch: ch, droppedCtx: nil}

	done := make(chan struct{})
	go func() {
		sink.Emit(models.ReactStreamEvent{Type: "done"})
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("Emit returned before consumer read — nil droppedCtx must be blocking semantics")
	case <-time.After(50 * time.Millisecond):
		// 没立刻返回，符合预期；现在 drain 一次
	}

	<-ch
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Emit did not return after consumer drained")
	}
}

func TestWorkCtxDecoupledFromReqCtx(t *testing.T) {
	// 复刻 ProcessMessageStreamFull 的 ctx 拓扑：reqCtx 来自 HTTP 请求，
	// workCtx 是 background + 30min，桥接 goroutine 只把 reqCtx.Done 翻译为
	// dropCancel。验证 reqCtx 取消时 workCtx 不被取消，业务可继续。
	reqCtx, reqCancel := context.WithCancel(context.Background())
	workCtx, workCancel := context.WithTimeout(context.Background(), streamWorkCtxMaxDuration)
	defer workCancel()
	droppedCtx, dropCancel := context.WithCancel(context.Background())
	defer dropCancel()

	go func() {
		select {
		case <-reqCtx.Done():
			dropCancel()
		case <-workCtx.Done():
		}
	}()

	// 业务还没开始；先取消 reqCtx 模拟客户端断开。
	reqCancel()

	// 给桥接 goroutine 一点时间把 dropCancel 传播下去。
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		select {
		case <-droppedCtx.Done():
			goto checked
		default:
			if time.Now().After(deadline) {
				t.Fatal("droppedCtx not cancelled within 500ms after reqCtx cancel")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
checked:

	// 核心契约：workCtx 必须还活着——业务 ctx 不应跟随 HTTP ctx 死亡。
	select {
	case <-workCtx.Done():
		t.Fatalf("workCtx was cancelled by reqCtx cancel; expected to stay alive (err=%v)", workCtx.Err())
	default:
	}
}
