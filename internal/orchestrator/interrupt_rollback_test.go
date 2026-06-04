package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent/code_agent/internal/models"
	"go.uber.org/zap"
)

// TestReactLoopCore_InterruptRollsBackTransaction is the unit test for PR 4:
// when an interrupt signal arrives at a step boundary and the ToolTransaction
// has dirty files, reactLoopCore must (a) call tx.Rollback() so the dirty
// files revert to their pre-step content, and (b) surface a "Rolled back N
// file(s)" message in the result content.
//
// Without the fix, the interrupt branch only emitted a message and ignored
// the transaction — dirty files stayed on disk after a Cancel/Pause.
func TestReactLoopCore_InterruptRollsBackTransaction(t *testing.T) {
	tests := []struct {
		name       string
		signal     InterruptType
		wantRolled bool
	}{
		{"cancel_rolls_back", InterruptCancel, true},
		{"pause_rolls_back", InterruptPause, true},
		{"redirect_does_not_rollback", InterruptRedirect, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "edited.txt")
			originalContent := []byte("original\n")
			if err := os.WriteFile(file, originalContent, 0o644); err != nil {
				t.Fatalf("seed file: %v", err)
			}

			o := &Orchestrator{
				logger: zap.NewNop(),
				txMap:  make(map[string]*ToolTransaction),
			}

			tx, release := o.registerTx("sess-1")
			defer release()

			// Simulate "edited_engine has captured the before-state, then a
			// tool overwrote the file": capture the original, then write a
			// dirty version that Rollback should undo.
			tx.CaptureBeforeWrite(file)
			if err := os.WriteFile(file, []byte("dirty edit\n"), 0o644); err != nil {
				t.Fatalf("dirty write: %v", err)
			}
			if !tx.HasDirtyFiles() {
				t.Fatalf("expected tx to have dirty files after CaptureBeforeWrite")
			}

			interruptCh := make(chan InterruptSignal, 1)
			interruptCh <- InterruptSignal{Type: tc.signal}

			result := o.reactLoopCore(context.Background(), reactCoreOpts{
				task: &models.Task{
					ID:        "task-1",
					SessionID: "sess-1",
					UserInput: "test",
					Intent:    models.IntentCodeQuery,
				},
				messages:    []models.Message{},
				tools:       nil,
				maxSteps:    5,
				interruptCh: interruptCh,
				tx:          tx,
			}, noopSink{})

			if !result.done {
				t.Fatalf("expected result.done=true after interrupt, got false")
			}

			got, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("read after rollback: %v", err)
			}
			rolledBack := string(got) == string(originalContent)

			if tc.wantRolled {
				if !rolledBack {
					t.Errorf("expected file restored to %q, got %q", originalContent, got)
				}
				if !strings.Contains(result.content, "Rolled back") {
					t.Errorf("expected content to mention rollback, got %q", result.content)
				}
				if tx.HasDirtyFiles() {
					t.Error("expected tx to be drained after Rollback")
				}
			} else {
				if rolledBack {
					t.Errorf("redirect must NOT roll back, but file was restored")
				}
			}
		})
	}
}

// TestReactLoopCore_InterruptNoTxDoesNotPanic exercises the nil-tx guard: a
// caller that does not register a transaction (e.g., a planner path that does
// not write files) must still get the interrupt message back without
// dereferencing the nil pointer.
func TestReactLoopCore_InterruptNoTxDoesNotPanic(t *testing.T) {
	o := &Orchestrator{logger: zap.NewNop(), txMap: make(map[string]*ToolTransaction)}
	interruptCh := make(chan InterruptSignal, 1)
	interruptCh <- InterruptSignal{Type: InterruptCancel}

	result := o.reactLoopCore(context.Background(), reactCoreOpts{
		task: &models.Task{
			ID:        "task-2",
			SessionID: "sess-2",
			UserInput: "test",
			Intent:    models.IntentCodeQuery,
		},
		messages:    []models.Message{},
		maxSteps:    1,
		interruptCh: interruptCh,
		tx:          nil,
	}, noopSink{})

	if !result.done {
		t.Fatal("expected result.done=true")
	}
	if !strings.Contains(result.content, "interrupted") {
		t.Errorf("expected interrupted message, got %q", result.content)
	}
}

// TestRegisterTx_PopulatesTxMap verifies the helper registers and releases.
// The CaptureBeforeWrite path (orchestrator.go preWriteCapture) iterates
// o.txMap to attribute writes to active sessions; a release must remove the
// entry so old transactions do not accumulate or capture cross-session.
func TestRegisterTx_PopulatesTxMap(t *testing.T) {
	o := &Orchestrator{logger: zap.NewNop(), txMap: make(map[string]*ToolTransaction)}

	tx, release := o.registerTx("sess-A")
	if tx == nil {
		t.Fatal("expected non-nil tx")
	}

	o.txMu.RLock()
	got, ok := o.txMap["sess-A"]
	o.txMu.RUnlock()
	if !ok || got != tx {
		t.Fatalf("expected txMap to hold the returned tx; ok=%v same=%v", ok, got == tx)
	}

	release()
	o.txMu.RLock()
	_, stillThere := o.txMap["sess-A"]
	o.txMu.RUnlock()
	if stillThere {
		t.Error("expected release() to remove the tx from the map")
	}
}
