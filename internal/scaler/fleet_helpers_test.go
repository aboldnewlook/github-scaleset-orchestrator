package scaler

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/fleet"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
	"github.com/actions/scaleset"
)

var testClass = runner.Class{OS: "linux", Arch: "x64"}

func testRequest() fleet.PlacementRequest {
	return fleet.PlacementRequest{Repo: "org/repo", Class: testClass, Cache: "none"}
}

type execFunc func(repo, cache string) runner.Executor

func (f execFunc) ForRepo(repo, cache string) runner.Executor { return f(repo, cache) }

func execFor(e runner.Executor) fleet.LocalExecutor {
	return execFunc(func(string, string) runner.Executor { return e })
}

func blockingExecutor() runner.Executor {
	return workerFunc(func(ctx context.Context, _, _ string) error {
		<-ctx.Done()
		return nil
	})
}

func quietTestLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newFleetScaler builds a scaler whose fleet has only a local member with
// `slots` slots, the P2b replacement for P2a's Semaphore(slots).
func newFleetScaler(t *testing.T, slots int, gen JITGenerator, exec runner.Executor) (*Scaler, *fleet.Fleet) {
	t.Helper()
	fl := fleet.New(fleet.Options{Logger: quietTestLogger(), Timing: fleet.DefaultTiming()})
	fl.SetLocal(slots, testClass, execFor(exec))
	s := New(Config{Repo: "org/repo", Class: testClass, ScaleSetID: 42, Client: gen, Placer: fl, Cache: "none", Logger: quietTestLogger()})
	return s, fl
}

func statsMessage(assigned int) *scaleset.RunnerScaleSetMessage {
	return &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: assigned}}
}

type capacityRecorder struct {
	mu     sync.Mutex
	counts []int
}

func (c *capacityRecorder) SetMaxRunners(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts = append(c.counts, n)
}

func (c *capacityRecorder) last() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.counts) == 0 {
		return -1
	}
	return c.counts[len(c.counts)-1]
}

func waitPlacement(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
