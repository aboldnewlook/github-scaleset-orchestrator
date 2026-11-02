package ghsim

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghapi"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestModel(t *testing.T) (*Model, *fakeClock) {
	t.Helper()
	clk := newFakeClock()
	m := NewModel(Options{
		Now:          clk.Now,
		PollWait:     20 * time.Millisecond,
		ClaimTimeout: time.Minute,
		MaxReassign:  3,
		SessionTTL:   2 * time.Minute,
		ServerURL:    "http://sim.test:9300",
	})
	return m, clk
}

var defaultLabels = []scaleset.Label{{Name: "self-hosted"}, {Name: "Linux"}, {Name: "X64"}}

// openSet creates a scale set in repo and opens its message session.
func openSet(t *testing.T, m *Model, repo, name string, labels []scaleset.Label) (ghapi.Client, *scaleset.RunnerScaleSet, ghapi.Session) {
	t.Helper()
	c := m.Client(repo)
	ss, err := c.CreateRunnerScaleSet(context.Background(), &scaleset.RunnerScaleSet{Name: name, RunnerGroupID: 1, Labels: labels})
	if err != nil {
		t.Fatalf("CreateRunnerScaleSet: %v", err)
	}
	sess, err := c.OpenSession(context.Background(), ss.ID, "test-owner")
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	return c, ss, sess
}

// poll returns the next message and acks it, or nil after an idle poll.
func poll(t *testing.T, s ghapi.Session, capacity int) *scaleset.RunnerScaleSetMessage {
	t.Helper()
	msg, err := s.GetMessage(context.Background(), 0, capacity)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if msg != nil {
		if err := s.DeleteMessage(context.Background(), msg.MessageID); err != nil {
			t.Fatalf("DeleteMessage: %v", err)
		}
	}
	return msg
}

// mintRunner registers a runner on the scale set and decodes its JIT config.
func mintRunner(t *testing.T, c ghapi.Client, scaleSetID int, name string) SimJIT {
	t.Helper()
	cfg, err := c.GenerateJitRunnerConfig(context.Background(), &scaleset.RunnerScaleSetJitRunnerSetting{Name: name}, scaleSetID)
	if err != nil {
		t.Fatalf("GenerateJitRunnerConfig: %v", err)
	}
	sj, ok, err := DecodeSimJIT(cfg.EncodedJITConfig)
	if err != nil || !ok {
		t.Fatalf("DecodeSimJIT: ok=%v err=%v", ok, err)
	}
	return sj
}
