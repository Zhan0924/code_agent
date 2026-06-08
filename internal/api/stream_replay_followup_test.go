// stream_replay_followup_test.go — verifies streamReplayFollow synthesizes a
// terminal `done` when history (or Follow tail) lacks one. 真实场景:
// 长 LLM finalize 完成后 MarkDone 已写,但 done 事件因竞态/截断没进 Stream;
// 没有兜底前端 SSE reader 会永远收不到结束信号、UI 卡 spinner。
package api

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/agent/code_agent/internal/models"
	"github.com/agent/code_agent/internal/orchestrator"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

func newReplayHarness(t *testing.T) (*orchestrator.StreamCache, *miniredis.Miniredis, func()) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	sc := orchestrator.NewStreamCache(rdb, zap.NewNop())
	if sc == nil {
		t.Fatal("NewStreamCache returned nil")
	}
	cleanup := func() {
		_ = rdb.Close()
		mr.Close()
	}
	return sc, mr, cleanup
}

// captureSink 收集所有 sendEvent 写出的事件,供断言。
type captureSink struct {
	mu     sync.Mutex
	events []models.StreamEvent
}

func (c *captureSink) send(ev models.StreamEvent) {
	c.mu.Lock()
	c.events = append(c.events, ev)
	c.mu.Unlock()
}

func (c *captureSink) types() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.events))
	for i, ev := range c.events {
		out[i] = ev.Type
	}
	return out
}

func (c *captureSink) doneReason() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ev := range c.events {
		if ev.Type != "done" {
			continue
		}
		var payload map[string]string
		if json.Unmarshal(ev.Data, &payload) != nil {
			continue
		}
		if r := payload["reason"]; r != "" {
			return r
		}
	}
	return ""
}

// TestStreamReplayFollow_NoTerminalInHistory_SynthesizesDone:
// 当 history 含若干业务事件、但末尾不是 done/error,且 Status 报 not running 时,
// streamReplayFollow 必须主动 emit 一条合成 done,否则 SSE 客户端会永远卡死。
func TestStreamReplayFollow_NoTerminalInHistory_SynthesizesDone(t *testing.T) {
	sc, _, cleanup := newReplayHarness(t)
	defer cleanup()
	ctx := context.Background()

	// 模拟:任务跑过几步 → 未走到 done,但状态已 MarkDone(典型的竞态/截断)
	sc.Append(ctx, "s1", models.ReactStreamEvent{Type: "step_start", Step: 1})
	sc.Append(ctx, "s1", models.ReactStreamEvent{Type: "thinking", Step: 1, Content: "..."})
	sc.Append(ctx, "s1", models.ReactStreamEvent{Type: "tool_call", Step: 1, ToolName: "read_file"})
	// 注意:故意不 Append done。MarkRunning 然后 MarkDone 模拟任务已经完成。
	sc.MarkRunning(ctx, "s1", "task-A")
	sc.MarkDone(ctx, "s1")

	sink := &captureSink{}
	streamReplayFollow(ctx, sc, "s1", sink.send, nil, nil)

	gotTypes := sink.types()
	// 必须以一条 done 收尾
	if len(gotTypes) == 0 || gotTypes[len(gotTypes)-1] != "done" {
		t.Fatalf("expected trailing synthesized done, got %v", gotTypes)
	}
	if reason := sink.doneReason(); reason != "synthesized_after_replay" {
		t.Fatalf("expected reason=synthesized_after_replay, got %q (events=%v)", reason, gotTypes)
	}
}

// TestStreamReplayFollow_TerminalInHistory_NoSyntheticDone:
// 当 history 已含真正的 done 事件,不应重复 emit 合成 done(客户端会双重 setLoading
// 但更重要的是 reason 字段不应被覆盖,审计/排查可识别真伪)。
func TestStreamReplayFollow_TerminalInHistory_NoSyntheticDone(t *testing.T) {
	sc, _, cleanup := newReplayHarness(t)
	defer cleanup()
	ctx := context.Background()

	sc.Append(ctx, "s2", models.ReactStreamEvent{Type: "step_start", Step: 1})
	sc.Append(ctx, "s2", models.ReactStreamEvent{Type: "message", Step: 1, Content: "answer"})
	// 真实 done(无 reason 字段)
	sc.Append(ctx, "s2", models.ReactStreamEvent{Type: "done", Step: 1})
	sc.MarkRunning(ctx, "s2", "task-B")
	sc.MarkDone(ctx, "s2")

	sink := &captureSink{}
	streamReplayFollow(ctx, sc, "s2", sink.send, nil, nil)

	doneCount := 0
	for _, tp := range sink.types() {
		if tp == "done" {
			doneCount++
		}
	}
	if doneCount != 1 {
		t.Fatalf("expected exactly 1 done event, got %d (types=%v)", doneCount, sink.types())
	}
	// 真 done 不该带 synthesized_* reason
	if reason := sink.doneReason(); strings.HasPrefix(reason, "synthesized_") {
		t.Fatalf("real done unexpectedly carries synthesized reason %q", reason)
	}
}
