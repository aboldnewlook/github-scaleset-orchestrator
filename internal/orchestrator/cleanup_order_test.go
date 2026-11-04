package orchestrator

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/event"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghapi"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghsim"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/naming"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
	"github.com/actions/scaleset"
)

// opLog records the cleanup's session and delete calls in order. Calls of the
// listener's own session are not recorded.
type opLog struct {
	mu  sync.Mutex
	ops []string
}

func (l *opLog) add(op string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ops = append(l.ops, op)
}

func (l *opLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.ops)
}

type recClient struct {
	ghapi.Client
	log      *opLog
	closeErr error
}

func (c recClient) OpenSession(ctx context.Context, id int, owner string) (ghapi.Session, error) {
	s, err := c.Client.OpenSession(ctx, id, owner)
	if err != nil || !strings.HasPrefix(owner, "gso-cleanup-") {
		return s, err
	}
	c.log.add("open")
	return recSession{sessionBase: sessionBase{in: s}, log: c.log, closeErr: c.closeErr}, nil
}

// sessionBase forwards every ghapi.Session method to in; wrappers embed it and
// override one. (Embedding ghapi.Session itself would clash with its Session method.)
type sessionBase struct{ in ghapi.Session }

func (b sessionBase) GetMessage(ctx context.Context, last, capacity int) (*scaleset.RunnerScaleSetMessage, error) {
	return b.in.GetMessage(ctx, last, capacity)
}
func (b sessionBase) DeleteMessage(ctx context.Context, id int) error {
	return b.in.DeleteMessage(ctx, id)
}
func (b sessionBase) AcquireJobs(ctx context.Context, ids []int64) ([]int64, error) {
	return b.in.AcquireJobs(ctx, ids)
}
func (b sessionBase) Session() scaleset.RunnerScaleSetSession { return b.in.Session() }
func (b sessionBase) Close(ctx context.Context) error         { return b.in.Close(ctx) }

type recSession struct {
	sessionBase
	log      *opLog
	closeErr error
}

func (s recSession) Close(ctx context.Context) error {
	s.log.add("close")
	return errors.Join(s.in.Close(ctx), s.closeErr)
}

type recDeleter struct {
	inner ghapi.ScaleSetDeleter
	log   *opLog
	err   error
}

func (d recDeleter) DeleteRunnerScaleSet(ctx context.Context, id int) error {
	d.log.add("delete")
	if d.err != nil {
		return d.err
	}
	return d.inner.DeleteRunnerScaleSet(ctx, id)
}

func recording(m *ghsim.Model, log *opLog, closeErr, deleteErr error) (ghapi.Factory, ghapi.DeleterFactory) {
	gh := func(ctx context.Context, repo config.Repo) (ghapi.Client, error) {
		c, err := m.Factory()(ctx, repo)
		if err != nil {
			return nil, err
		}
		return recClient{Client: c, log: log, closeErr: closeErr}, nil
	}
	del := func(ctx context.Context, repo config.Repo) (ghapi.ScaleSetDeleter, error) {
		d, err := m.Deleter()(ctx, repo)
		if err != nil {
			return nil, err
		}
		return recDeleter{inner: d, log: log, err: deleteErr}, nil
	}
	return gh, del
}

// The check session is held across the DELETE, so nothing can open the set in
// between, and closed after it. A Close error after a successful delete only
// gets logged.
func TestCleanupHoldsTheSessionAcrossTheDelete(t *testing.T) {
	m := cleanupModel()
	seedSet(t, m, orphanName)
	log := &opLog{}
	gh, del := recording(m, log, errors.New("session already gone"), nil)
	o, logs := newCleanupOrchestrator(t, gh, m, true, cleanupTiming(0), nil)
	o.deleter = del
	supervise(t, o, cleanupRepo)

	eventually(t, "the orphan deleted", func() bool { return !slices.Contains(setNames(m), orphanName) })
	eventually(t, "the session closed", func() bool { return len(log.get()) == 3 })
	if got, want := log.get(), []string{"open", "delete", "close"}; !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
	eventually(t, "the close error logged", func() bool { return strings.Contains(logs.String(), "session already gone") })
	if !strings.Contains(logs.String(), "deleted stale scale set") {
		t.Errorf("deletion not logged:\n%s", logs.String())
	}
}

// A failed DELETE keeps the set, closes the held session, and is reported as
// a retryable skip.
func TestCleanupDeleteErrorClosesTheSessionAndSkips(t *testing.T) {
	m := cleanupModel()
	seedSet(t, m, orphanName)
	log := &opLog{}
	gh, del := recording(m, log, nil, errors.New("403 forbidden"))
	bus := event.NewBus(nil)
	sub, ch := bus.Subscribe(64)
	defer bus.Unsubscribe(sub)
	o, _ := newCleanupOrchestrator(t, gh, m, true, cleanupTiming(0), bus)
	o.deleter = del
	supervise(t, o, cleanupRepo)

	p := waitForStaleAlert(t, ch)
	var reason string
	for label, r := range p.Skipped {
		if strings.HasPrefix(label, orphanName+" ") {
			reason = r
		}
	}
	if !strings.Contains(reason, "delete failed") || !strings.Contains(reason, "403") {
		t.Errorf("skip reason = %q, want the delete failure", reason)
	}
	if !slices.Contains(setNames(m), orphanName) {
		t.Error("set gone although the delete failed")
	}
	if got, want := log.get(), []string{"open", "delete", "close"}; !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v (session closed after the failed delete)", got, want)
	}
}

// The fresh statistics in the held session gate the delete.
func TestCleanupSessionStatisticsGateTheDelete(t *testing.T) {
	m := cleanupModel()
	seedSet(t, m, orphanName)
	log := &opLog{}
	gh, del := recording(m, log, nil, nil)
	busy := func(ctx context.Context, repo config.Repo) (ghapi.Client, error) {
		c, err := gh(ctx, repo)
		if err != nil {
			return nil, err
		}
		return busySessionClient{Client: c}, nil
	}
	bus := event.NewBus(nil)
	sub, ch := bus.Subscribe(64)
	defer bus.Unsubscribe(sub)
	o, _ := newCleanupOrchestrator(t, busy, m, true, cleanupTiming(0), bus)
	o.deleter = del
	supervise(t, o, cleanupRepo)

	waitForStaleAlert(t, ch)
	if !slices.Contains(setNames(m), orphanName) {
		t.Error("set deleted although its session reported running jobs")
	}
	if got, want := log.get(), []string{"open", "close"}; !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v (no delete)", got, want)
	}
}

// busySessionClient reports running jobs on the cleanup's sessions only.
type busySessionClient struct{ ghapi.Client }

func (c busySessionClient) OpenSession(ctx context.Context, id int, owner string) (ghapi.Session, error) {
	s, err := c.Client.OpenSession(ctx, id, owner)
	if err != nil || !strings.HasPrefix(owner, "gso-cleanup-") {
		return s, err
	}
	return busySession{sessionBase{in: s}}, nil
}

type busySession struct{ sessionBase }

func (s busySession) Session() scaleset.RunnerScaleSetSession {
	info := s.in.Session()
	info.Statistics = &scaleset.RunnerScaleSetStatistic{TotalRunningJobs: 1, TotalAssignedJobs: 1}
	return info
}

// If the current scale set never starts (its session keeps returning 409),
// cleanup never runs, so the plain stale warning must still appear.
func TestStaleWarningStillAppearsWhenTheCurrentListenerNeverStarts(t *testing.T) {
	m := cleanupModel()
	seedSet(t, m, orphanName)
	current := naming.ScaleSetNameFor("unraid", cleanupRepo, runner.HostClass())
	seedSet(t, m, current)
	m.FailSessions(cleanupRepo, current, scaleset.ConflictError, 100000)
	bus := event.NewBus(nil)
	sub, ch := bus.Subscribe(64)
	defer bus.Unsubscribe(sub)
	o, logs := newCleanupOrchestrator(t, m.Factory(), m, true, cleanupTiming(0), bus)
	supervise(t, o, cleanupRepo)

	p := waitForStaleAlert(t, ch)
	if p.AutoCleanup {
		t.Error("alert auto_cleanup = true although cleanup never ran")
	}
	if len(p.ScaleSets) != 1 || !strings.HasPrefix(p.ScaleSets[0], orphanName+" ") {
		t.Errorf("scale_sets = %v, want just the orphan", p.ScaleSets)
	}
	if !slices.Contains(setNames(m), orphanName) {
		t.Error("orphan deleted before the current listener started")
	}
	if !strings.Contains(logs.String(), "has not started") {
		t.Errorf("warning does not say why cleanup has not run:\n%s", logs.String())
	}
}
