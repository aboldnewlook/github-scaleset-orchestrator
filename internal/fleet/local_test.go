package fleet

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

type execFunc func(ctx context.Context, name, jit string) error

func (f execFunc) Run(ctx context.Context, name, jit string) error { return f(ctx, name, jit) }

type fakeExec struct {
	mu    sync.Mutex
	calls []string // "repo|cache|name|jit"
	block chan struct{}
	err   error
}

func (e *fakeExec) ForRepo(repo, cache string) runner.Executor {
	return execFunc(func(ctx context.Context, name, jit string) error {
		e.mu.Lock()
		e.calls = append(e.calls, repo+"|"+cache+"|"+name+"|"+jit)
		block, err := e.block, e.err
		e.mu.Unlock()
		if block != nil {
			select {
			case <-block:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return err
	})
}

func (e *fakeExec) callList() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

func waitCalls(t *testing.T, e *fakeExec, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(e.callList()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("executor saw %d calls, want %d", len(e.callList()), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestLocalMemberMintsThenRuns(t *testing.T) {
	tf := newTestFleet(t)
	exec := &fakeExec{}
	tf.SetLocal(2, linuxX64, exec)
	req := PlacementRequest{Repo: "o/prune", Class: linuxX64, Cache: "repo"}
	if got := tf.Free(req); got != 2 {
		t.Fatalf("Free = %d, want 2", got)
	}
	r := mustReserve(t, tf.Fleet, req)
	if !strings.HasPrefix(r.RunnerName(), "gso-local-prune-") || r.Member() != "local" || r.Repo() != "o/prune" || r.Class() != linuxX64 {
		t.Fatalf("reservation = %s on %s", r.RunnerName(), r.Member())
	}
	if got := tf.Free(req); got != 1 {
		t.Fatalf("Free after Reserve = %d, want 1", got)
	}
	mint := &mintRecorder{}
	if err := r.Run(context.Background(), mint.mint); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if mint.calls() != 1 {
		t.Fatalf("mint calls = %d, want 1", mint.calls())
	}
	want := "o/prune|repo|" + r.RunnerName() + "|jit-" + r.RunnerName()
	if calls := exec.callList(); len(calls) != 1 || calls[0] != want {
		t.Fatalf("executor calls = %v, want [%s]", calls, want)
	}
	if r.RunnerID() != 1001 {
		t.Fatalf("RunnerID = %d", r.RunnerID())
	}
	if got := tf.Free(req); got != 2 {
		t.Fatalf("Free after Run = %d, want 2", got)
	}
}

// Parity with P2a: JobCompleted frees the slot before the runner process exits.
func TestLocalReleaseFreesSlotBeforeTheRunnerExits(t *testing.T) {
	tf := newTestFleet(t)
	exec := &fakeExec{block: make(chan struct{})}
	tf.SetLocal(1, linuxX64, exec)
	req := PlacementRequest{Repo: "o/r", Class: linuxX64}
	r := mustReserve(t, tf.Fleet, req)
	errc := runAsync(context.Background(), r, (&mintRecorder{}).mint)
	waitCalls(t, exec, 1)
	if got := tf.Free(req); got != 0 {
		t.Fatalf("Free while running = %d", got)
	}
	r.Release()
	r.Release() // idempotent
	if got := tf.Free(req); got != 1 {
		t.Fatalf("Free after Release = %d, want 1", got)
	}
	assertPending(t, errc)
	close(exec.block)
	if err := waitErr(t, errc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := tf.Free(req); got != 1 {
		t.Fatalf("Free after exit = %d, want 1 (no double release)", got)
	}
}

func TestLocalMintFailureReturnsTheSlot(t *testing.T) {
	tf := newTestFleet(t)
	exec := &fakeExec{}
	tf.SetLocal(1, linuxX64, exec)
	req := PlacementRequest{Repo: "o/r", Class: linuxX64}
	r := mustReserve(t, tf.Fleet, req)
	boom := errors.New("github: 500")
	if err := r.Run(context.Background(), (&mintRecorder{err: boom}).mint); !errors.Is(err, boom) {
		t.Fatalf("Run = %v, want %v", err, boom)
	}
	if len(exec.callList()) != 0 {
		t.Fatal("executor ran without a JIT config")
	}
	if got := tf.Free(req); got != 1 {
		t.Fatalf("Free = %d, want 1", got)
	}
}

func TestLocalRunCancelledByContext(t *testing.T) {
	tf := newTestFleet(t)
	exec := &fakeExec{block: make(chan struct{})}
	tf.SetLocal(1, linuxX64, exec)
	r := mustReserve(t, tf.Fleet, PlacementRequest{Repo: "o/r", Class: linuxX64})
	ctx, cancel := context.WithCancel(context.Background())
	errc := runAsync(ctx, r, (&mintRecorder{}).mint)
	waitCalls(t, exec, 1)
	cancel()
	if err := waitErr(t, errc); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
}

func TestLocalMemberEligibility(t *testing.T) {
	tests := []struct {
		name string
		req  PlacementRequest
		want int
	}{
		{"own class", PlacementRequest{Repo: "o/r", Class: linuxX64}, 1},
		{"other class", PlacementRequest{Repo: "o/r", Class: linuxArm64}, 0},
		{"public work never runs locally", PlacementRequest{Repo: "o/r", Class: linuxX64, Public: true}, 0},
		{"pinned to local", PlacementRequest{Repo: "o/r", Class: linuxX64, Member: "local"}, 1},
		{"pinned elsewhere", PlacementRequest{Repo: "o/r", Class: linuxX64, Member: "box"}, 0},
	}
	for _, tt := range tests {
		// A fresh fleet per case: Reserve below takes the only slot.
		tf := newTestFleet(t)
		tf.SetLocal(1, linuxX64, &fakeExec{})
		if got := tf.Free(tt.req); got != tt.want {
			t.Errorf("%s: Free = %d, want %d", tt.name, got, tt.want)
		}
		if got := tf.Slots(tt.req); got != tt.want {
			t.Errorf("%s: Slots = %d, want %d", tt.name, got, tt.want)
		}
		if _, ok := tf.Reserve(tt.req); ok != (tt.want > 0) {
			t.Errorf("%s: Reserve ok = %v", tt.name, ok)
		}
	}
}

func TestSetLocalResizesAndZeroes(t *testing.T) {
	tf := newTestFleet(t)
	exec := &fakeExec{}
	tf.SetLocal(1, linuxX64, exec)
	req := PlacementRequest{Repo: "o/r", Class: linuxX64}
	held := mustReserve(t, tf.Fleet, req)
	tf.SetLocal(3, linuxX64, exec)
	if got := tf.Free(req); got != 2 {
		t.Fatalf("Free after resize = %d, want 2 (busy slot kept)", got)
	}
	tf.SetLocal(0, linuxX64, exec)
	if tf.LocalSlots() != 0 || tf.Free(req) != 0 || tf.Slots(req) != 0 {
		t.Fatalf("after SetLocal(0): slots=%d free=%d", tf.LocalSlots(), tf.Free(req))
	}
	held.Release() // still safe after the resize
}

func TestLocalRunnersCannotBeAdopted(t *testing.T) {
	tf := newTestFleet(t)
	tf.SetLocal(1, linuxX64, &fakeExec{})
	r := mustReserve(t, tf.Fleet, PlacementRequest{Repo: "o/r", Class: linuxX64})
	if err := r.Run(context.Background(), nil); err == nil {
		t.Fatal("Run(nil mint) on the local member succeeded")
	}
}

func TestReserveWithoutMembers(t *testing.T) {
	tf := newTestFleet(t)
	if _, ok := tf.Reserve(PlacementRequest{Repo: "o/r", Class: linuxX64}); ok {
		t.Fatal("Reserve succeeded with no members")
	}
	if ms := tf.Members(); len(ms) != 0 {
		t.Fatalf("Members = %+v", ms)
	}
}

func TestMembersReportsTheLocalMember(t *testing.T) {
	tf := newTestFleet(t)
	tf.SetLocal(2, linuxX64, &fakeExec{})
	mustReserve(t, tf.Fleet, PlacementRequest{Repo: "o/r", Class: linuxX64})
	ms := tf.Members()
	if len(ms) != 1 || ms[0].Name != "local" || !ms[0].Local || !ms[0].Connected || ms[0].Slots != 2 || ms[0].Busy != 1 || ms[0].Availability != "available" || ms[0].Classes[0] != "linux-x64" {
		t.Fatalf("Members = %+v", ms)
	}
}
