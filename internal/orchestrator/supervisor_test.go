package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/event"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/fleet"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghapi"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghsim"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/naming"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
	"github.com/actions/scaleset"
)

// blockingExecutor runs a "runner" until its context is cancelled.
type blockingExecutor struct{}

func (blockingExecutor) Run(ctx context.Context, _, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

// trackingExecutor blocks like blockingExecutor and records which runners
// started and which had their context cancelled.
type trackingExecutor struct {
	mu        sync.Mutex
	started   int
	cancelled int
}

func (e *trackingExecutor) Run(ctx context.Context, _, _ string) error {
	e.mu.Lock()
	e.started++
	e.mu.Unlock()
	<-ctx.Done()
	e.mu.Lock()
	e.cancelled++
	e.mu.Unlock()
	return ctx.Err()
}

func (e *trackingExecutor) counts() (started, cancelled int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.started, e.cancelled
}

func fastTiming() SupervisorTiming {
	return SupervisorTiming{
		BackoffMin: time.Millisecond, BackoffMax: 4 * time.Millisecond,
		ConflictWait: 2 * time.Millisecond, ConflictRetries: 10, DegradeAfter: 3,
	}
}

func newModel() *ghsim.Model {
	return ghsim.NewModel(ghsim.Options{PollWait: 20 * time.Millisecond, SessionTTL: 200 * time.Millisecond})
}

// tap wraps the simulator's clients to record session owners and to fail
// message polls on demand. Everything else passes through.
type tap struct {
	mu        sync.Mutex
	owners    []string
	opened    int
	failPolls int
}

func (tp *tap) factory(inner ghapi.Factory) ghapi.Factory {
	return func(ctx context.Context, repo config.Repo) (ghapi.Client, error) {
		c, err := inner(ctx, repo)
		if err != nil {
			return nil, err
		}
		return &tapClient{Client: c, tap: tp}, nil
	}
}

func (tp *tap) snapshot() (owners []string, opened int) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	return slices.Clone(tp.owners), tp.opened
}

type tapClient struct {
	ghapi.Client
	tap *tap
}

func (c *tapClient) OpenSession(ctx context.Context, scaleSetID int, owner string) (ghapi.Session, error) {
	s, err := c.Client.OpenSession(ctx, scaleSetID, owner)
	if err != nil {
		return nil, err
	}
	c.tap.mu.Lock()
	c.tap.owners = append(c.tap.owners, owner)
	c.tap.opened++
	c.tap.mu.Unlock()
	return &tapSession{inner: s, tap: c.tap}, nil
}

type tapSession struct {
	inner ghapi.Session
	tap   *tap
}

func (s *tapSession) GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*scaleset.RunnerScaleSetMessage, error) {
	s.tap.mu.Lock()
	fail := s.tap.failPolls > 0
	if fail {
		s.tap.failPolls--
	}
	s.tap.mu.Unlock()
	if fail {
		return nil, errors.New("github: 502 Bad Gateway")
	}
	return s.inner.GetMessage(ctx, lastMessageID, maxCapacity)
}

func (s *tapSession) DeleteMessage(ctx context.Context, id int) error {
	return s.inner.DeleteMessage(ctx, id)
}
func (s *tapSession) AcquireJobs(ctx context.Context, ids []int64) ([]int64, error) {
	return s.inner.AcquireJobs(ctx, ids)
}
func (s *tapSession) Session() scaleset.RunnerScaleSetSession { return s.inner.Session() }
func (s *tapSession) Close(ctx context.Context) error         { return s.inner.Close(ctx) }

func newTestOrchestrator(t *testing.T, gh ghapi.Factory, bus *event.Bus, tm SupervisorTiming, repos ...string) *Orchestrator {
	t.Helper()
	cfg := &config.Config{
		MaxRunners: 4,
		Labels:     []string{"self-hosted"},
		Runner:     config.RunnerConfig{InstanceID: "unraid"},
	}
	for _, r := range repos {
		cfg.Repos = append(cfg.Repos, config.Repo{Name: r})
	}
	fl := fleet.New(fleet.Options{Logger: testLogger(), Timing: fleet.DefaultTiming()})
	return New(cfg, testLogger(), bus, WithFleet(fl), WithGitHub(gh), WithSupervisorTiming(tm))
}

// execLocal serves one executor for every repo, as fleet.LocalExecutor.
type execLocal struct{ exec runner.Executor }

func (e execLocal) ForRepo(string, string) runner.Executor { return e.exec }

// supervise runs one repo's supervisor until the test ends, and checks that
// it returns once its context is cancelled.
func supervise(t *testing.T, o *Orchestrator, repo string) {
	t.Helper()
	superviseWith(t, o, repo, blockingExecutor{})
}

func superviseWith(t *testing.T, o *Orchestrator, repo string, exec runner.Executor) {
	t.Helper()
	o.fleet.SetLocal(4, o.class, execLocal{exec})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		o.superviseRepo(ctx, config.Repo{Name: repo})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("superviseRepo did not return after cancellation")
		}
	})
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func listenerState(o *Orchestrator, repo string) control.ListenerLiveStatus {
	for _, l := range o.Listeners() {
		if l.Repo == repo {
			return l
		}
	}
	return control.ListenerLiveStatus{}
}

func drainEvents(ch <-chan event.Event) []event.Event {
	var out []event.Event
	for {
		select {
		case e := <-ch:
			out = append(out, e)
		default:
			return out
		}
	}
}

func listenerEventTypes(evs []event.Event) []event.EventType {
	var out []event.EventType
	for _, e := range evs {
		if e.Type == event.EventListenerDegraded || e.Type == event.EventListenerRecovered {
			out = append(out, e.Type)
		}
	}
	return out
}

// precreate makes the scale set the supervisor will use, so failures can be
// injected for it before the supervisor starts.
func precreate(t *testing.T, m *ghsim.Model, repo string) string {
	t.Helper()
	name := naming.ScaleSetNameFor("unraid", repo, runner.HostClass())
	_, err := m.Client(repo).CreateRunnerScaleSet(context.Background(), &scaleset.RunnerScaleSet{
		Name: name, RunnerGroupID: 1, RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return name
}

func TestSupervisorNamesScaleSetByInstanceAndClass(t *testing.T) {
	m := newModel()
	tp := &tap{}
	o := newTestOrchestrator(t, tp.factory(m.Factory()), nil, fastTiming(), "org/repo")
	supervise(t, o, "org/repo")
	eventually(t, "listener running", func() bool { return listenerState(o, "org/repo").State == "running" })

	class := runner.HostClass()
	want := naming.ScaleSetNameFor("unraid", "org/repo", class)
	sets := m.ScaleSets("org/repo")
	if len(sets) != 1 || sets[0].Name != want {
		t.Fatalf("scale sets = %+v, want one named %s", sets, want)
	}
	var labels []string
	for _, l := range sets[0].Labels {
		labels = append(labels, l.Name)
	}
	if !slices.Equal(labels, []string{"self-hosted", class.OSLabel(), class.ArchLabel()}) {
		t.Fatalf("labels = %v, want today's labels unchanged until P2c", labels)
	}
	if owners, _ := tp.snapshot(); len(owners) != 1 || owners[0] != "unraid" {
		t.Fatalf("session owners = %v, want [unraid] (instance_id, not the hostname)", owners)
	}
	eventually(t, "capacity advertised on GetMessage", func() bool { return m.LastCapacity("org/repo", want) == 4 })
	if st := listenerState(o, "org/repo"); st.ScaleSetName != want || st.Class != class.String() || st.Capacity != 4 {
		t.Fatalf("listener status = %+v", st)
	}
}

func TestSupervisorRetriesConflictWithoutDeleting(t *testing.T) {
	m := newModel()
	name := precreate(t, m, "org/repo")
	m.FailSessions("org/repo", name, fmt.Errorf("creating message session: %w", scaleset.ConflictError), 3)
	bus := event.NewBus(nil)
	id, ch := bus.Subscribe(64)
	defer bus.Unsubscribe(id)
	o := newTestOrchestrator(t, m.Factory(), bus, fastTiming(), "org/repo")
	supervise(t, o, "org/repo")

	eventually(t, "listener running after three conflicts", func() bool { return listenerState(o, "org/repo").State == "running" })
	if d := m.Deleted("org/repo"); len(d) != 0 {
		t.Fatalf("deleted scale sets %v on 409; 409 must never delete (spec §7, F1)", d)
	}
	if got := listenerEventTypes(drainEvents(ch)); len(got) != 0 {
		t.Fatalf("listener events %v; three conflicts are within the 10 retries and must not degrade", got)
	}
}

// After a router restart GitHub may keep the old message session for a long
// time. A worker reconnecting meanwhile reports its running runner, and the
// fleet verifies it against the scale set: that ID must be known while the
// listener is still retrying 409s, or the worker's job is killed.
func TestScaleSetIsKnownWhileTheSessionConflicts(t *testing.T) {
	m := newModel()
	name := precreate(t, m, "org/repo")
	m.FailSessions("org/repo", name, fmt.Errorf("creating message session: %w", scaleset.ConflictError), 1_000_000)
	o := newTestOrchestrator(t, m.Factory(), nil, fastTiming(), "org/repo")
	supervise(t, o, "org/repo")

	// "retrying" is set at startup, so wait for a counted conflict instead.
	eventually(t, "the listener to hit a conflict", func() bool {
		l := listenerState(o, "org/repo")
		return l.Failures >= 1 && strings.Contains(l.LastError, "creating message session")
	})
	id, ok := o.ScaleSetID("org/repo", o.class)
	if !ok || id == 0 {
		t.Fatalf("ScaleSetID = %d, %v while the session conflicts; the scale set exists and must be known", id, ok)
	}
	if n := len(m.Runners("org/repo")); n != 0 {
		t.Fatalf("%d runners minted before any session opened, want 0", n)
	}
}

func TestSupervisorDegradesAfterThreeFailuresThenRecovers(t *testing.T) {
	m := newModel()
	name := precreate(t, m, "org/repo")
	m.FailSessions("org/repo", name, errors.New("github: 502 Bad Gateway"), 5)
	bus := event.NewBus(nil)
	id, ch := bus.Subscribe(64)
	defer bus.Unsubscribe(id)
	o := newTestOrchestrator(t, m.Factory(), bus, fastTiming(), "org/repo")
	supervise(t, o, "org/repo")

	eventually(t, "listener running after five failures", func() bool { return listenerState(o, "org/repo").State == "running" })
	got := listenerEventTypes(drainEvents(ch))
	want := []event.EventType{event.EventListenerDegraded, event.EventListenerRecovered}
	if !slices.Equal(got, want) {
		t.Fatalf("listener events = %v, want %v", got, want)
	}
}

func TestSupervisorDegradesWhenConflictRetriesRunOut(t *testing.T) {
	m := newModel()
	name := precreate(t, m, "org/repo")
	m.FailSessions("org/repo", name, fmt.Errorf("creating message session: %w", scaleset.ConflictError), 4)
	bus := event.NewBus(nil)
	id, ch := bus.Subscribe(64)
	defer bus.Unsubscribe(id)
	tm := fastTiming()
	tm.ConflictRetries = 2
	o := newTestOrchestrator(t, m.Factory(), bus, tm, "org/repo")
	supervise(t, o, "org/repo")

	eventually(t, "listener running", func() bool { return listenerState(o, "org/repo").State == "running" })
	if got := listenerEventTypes(drainEvents(ch)); !slices.Equal(got, []event.EventType{event.EventListenerDegraded, event.EventListenerRecovered}) {
		t.Fatalf("listener events = %v", got)
	}
	if d := m.Deleted("org/repo"); len(d) != 0 {
		t.Fatalf("deleted %v after exhausting conflict retries", d)
	}
}

func TestFailingRepoDoesNotStopAnother(t *testing.T) {
	m := newModel()
	broken := precreate(t, m, "org/broken")
	m.FailSessions("org/broken", broken, errors.New("github: 503 Service Unavailable"), 1_000_000)
	o := newTestOrchestrator(t, m.Factory(), nil, fastTiming(), "org/broken", "org/healthy")
	supervise(t, o, "org/broken")
	supervise(t, o, "org/healthy")

	healthy := naming.ScaleSetNameFor("unraid", "org/healthy", runner.HostClass())
	eventually(t, "healthy listener running", func() bool { return listenerState(o, "org/healthy").State == "running" })
	m.Push("org/healthy", healthy, &scaleset.RunnerScaleSetMessage{
		MessageID: 1, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1},
	})
	eventually(t, "a runner minted for the healthy repo", func() bool { return m.MintCount("org/healthy") == 1 })
	eventually(t, "broken repo degraded", func() bool { return listenerState(o, "org/broken").State == "degraded" })
}

func TestStaleScaleSetsRaiseAnAlert(t *testing.T) {
	m := newModel()
	if _, err := m.Client("org/repo").CreateRunnerScaleSet(context.Background(), &scaleset.RunnerScaleSet{Name: "gso-oldhost-repo", RunnerGroupID: 1}); err != nil {
		t.Fatal(err)
	}
	bus := event.NewBus(nil)
	id, ch := bus.Subscribe(64)
	defer bus.Unsubscribe(id)
	o := newTestOrchestrator(t, m.Factory(), bus, fastTiming(), "org/repo")
	supervise(t, o, "org/repo")
	eventually(t, "listener running", func() bool { return listenerState(o, "org/repo").State == "running" })

	for _, e := range drainEvents(ch) {
		if e.Type != event.EventAlert {
			continue
		}
		var p struct {
			Reason    string   `json:"reason"`
			ScaleSets []string `json:"scale_sets"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Reason == "stale_scale_sets" && len(p.ScaleSets) == 1 && strings.HasPrefix(p.ScaleSets[0], "gso-oldhost-repo ") {
			return
		}
	}
	t.Fatal("no stale_scale_sets alert for gso-oldhost-repo")
}

// A listener error and then two 409s while reopening the session must leave
// the in-flight runner untouched: its context derives from the supervisor's
// long-lived context, not from one listener attempt (design spec §5.3, G7).
func TestListenerRestartLeavesInFlightRunnerUncancelled(t *testing.T) {
	m := newModel()
	tp := &tap{}
	exec := &trackingExecutor{}
	o := newTestOrchestrator(t, tp.factory(m.Factory()), nil, fastTiming(), "org/repo")
	superviseWith(t, o, "org/repo", exec)
	name := naming.ScaleSetNameFor("unraid", "org/repo", runner.HostClass())
	eventually(t, "listener running", func() bool { return listenerState(o, "org/repo").State == "running" })

	busy := scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}
	m.SetStats("org/repo", name, busy)
	m.Push("org/repo", name, &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: &busy})
	eventually(t, "the runner started", func() bool { started, _ := exec.counts(); return started == 1 })

	m.FailSessions("org/repo", name, fmt.Errorf("creating message session: %w", scaleset.ConflictError), 2)
	tp.mu.Lock()
	tp.failPolls = 1 // the next poll fails, so the supervisor restarts the listener
	tp.mu.Unlock()
	eventually(t, "a second session", func() bool { _, opened := tp.snapshot(); return opened >= 2 })
	eventually(t, "listener running again", func() bool { return listenerState(o, "org/repo").State == "running" })
	time.Sleep(100 * time.Millisecond) // let the new session's initial Scale run

	if started, cancelled := exec.counts(); started != 1 || cancelled != 0 {
		t.Fatalf("after the restart: started=%d cancelled=%d, want 1 and 0", started, cancelled)
	}
	if got := m.MintCount("org/repo"); got != 1 {
		t.Fatalf("minted %d runners across a listener restart, want 1 (the runner kept its slot)", got)
	}
	if got := len(o.RunnersByRepo()["org/repo"]); got != 1 {
		t.Fatalf("tracked runners = %d after the restart, want 1", got)
	}
	if d := m.Deleted("org/repo"); len(d) != 0 {
		t.Fatalf("deleted %v during the restart", d)
	}
}

func TestDefaultSupervisorTiming(t *testing.T) {
	if _, set := os.LookupEnv("GSO_TIMING_SCALE"); set {
		t.Skip("GSO_TIMING_SCALE is set for this run")
	}
	want := SupervisorTiming{BackoffMin: time.Second, BackoffMax: 5 * time.Minute, ConflictWait: 30 * time.Second, ConflictRetries: 10, DegradeAfter: 3,
		CleanupBackoff: time.Minute, CleanupRetries: 5} // 1+2+4+8+16 min: stale-set retries stop after ~31 min
	if got := DefaultSupervisorTiming(); got != want {
		t.Fatalf("DefaultSupervisorTiming() = %+v, want %+v", got, want)
	}
}

// B8a relies on this: with an injected factory the orchestrator never resolves
// a token and never builds the real scaleset client. The config has no token
// source at all, so cfg.TokenForRepo would fail if it were reached.
func TestWithGitHubNeverResolvesATokenOrBuildsARealClient(t *testing.T) {
	m := newModel()
	var mu sync.Mutex
	var asked []string
	factory := func(ctx context.Context, repo config.Repo) (ghapi.Client, error) {
		mu.Lock()
		asked = append(asked, repo.Name)
		mu.Unlock()
		return m.Factory()(ctx, repo)
	}
	o := newTestOrchestrator(t, factory, nil, fastTiming(), "org/repo")
	if _, err := o.cfg.TokenForRepo(config.Repo{Name: "org/repo"}); err == nil {
		t.Fatal("precondition: the config must have no token source")
	}
	supervise(t, o, "org/repo")
	eventually(t, "repo started through the injected factory", func() bool { return listenerState(o, "org/repo").State == "running" })

	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(asked, []string{"org/repo"}) {
		t.Fatalf("factory asked for %v, want [org/repo] exactly once", asked)
	}
	if len(m.ScaleSets("org/repo")) != 1 {
		t.Fatal("the scale set was not created on the simulator")
	}
}

// listFailClient fails the first scale-set listing, like a transient GitHub
// error during the stale check.
type listFailClient struct {
	ghapi.Client
	mu    sync.Mutex
	calls int
}

func (c *listFailClient) ListRunnerScaleSets(ctx context.Context, group int) ([]scaleset.RunnerScaleSet, error) {
	c.mu.Lock()
	c.calls++
	first := c.calls == 1
	c.mu.Unlock()
	if first {
		return nil, errors.New("github: 502 Bad Gateway")
	}
	return c.Client.ListRunnerScaleSets(ctx, group)
}

func TestStaleCheckIsRetriedAfterAListError(t *testing.T) {
	m := newModel()
	if _, err := m.Client("org/repo").CreateRunnerScaleSet(context.Background(), &scaleset.RunnerScaleSet{Name: "gso-oldhost-repo", RunnerGroupID: 1}); err != nil {
		t.Fatal(err)
	}
	bus := event.NewBus(nil)
	id, ch := bus.Subscribe(64)
	defer bus.Unsubscribe(id)
	tp := &tap{failPolls: 1} // the first poll fails, so the listener restarts
	inner := tp.factory(m.Factory())
	factory := func(ctx context.Context, repo config.Repo) (ghapi.Client, error) {
		c, err := inner(ctx, repo)
		if err != nil {
			return nil, err
		}
		return &listFailClient{Client: c}, nil
	}
	o := newTestOrchestrator(t, factory, bus, fastTiming(), "org/repo")
	supervise(t, o, "org/repo")

	var got []event.Event
	eventually(t, "stale_scale_sets alert after the retry", func() bool {
		for _, e := range drainEvents(ch) {
			if e.Type == event.EventAlert && strings.Contains(string(e.Payload), "stale_scale_sets") {
				got = append(got, e)
			}
		}
		return len(got) > 0
	})
}

// scriptedSessionClient fails OpenSession with the scripted errors, then
// passes through.
type scriptedSessionClient struct {
	ghapi.Client
	mu   sync.Mutex
	errs []error
}

func (c *scriptedSessionClient) OpenSession(ctx context.Context, id int, owner string) (ghapi.Session, error) {
	c.mu.Lock()
	if len(c.errs) > 0 {
		err := c.errs[0]
		c.errs = c.errs[1:]
		c.mu.Unlock()
		return nil, err
	}
	c.mu.Unlock()
	return c.Client.OpenSession(ctx, id, owner)
}

// Two 409s then a 502: the failure count restarts at 1 for the new kind of
// failure rather than adding the conflicts (gso status would show FAILURES 3
// next to STATE retrying).
func TestFailureCountRestartsWhenTheFailureKindChanges(t *testing.T) {
	m := newModel()
	conflict := fmt.Errorf("creating message session: %w", scaleset.ConflictError)
	inner := m.Factory()
	factory := func(ctx context.Context, repo config.Repo) (ghapi.Client, error) {
		c, err := inner(ctx, repo)
		if err != nil {
			return nil, err
		}
		return &scriptedSessionClient{Client: c, errs: []error{conflict, conflict, errors.New("github: 502 Bad Gateway")}}, nil
	}
	tm := fastTiming()
	tm.BackoffMin, tm.BackoffMax = 10*time.Second, 10*time.Second // hold in "retrying" after the 502
	o := newTestOrchestrator(t, factory, nil, tm, "org/repo")
	supervise(t, o, "org/repo")

	eventually(t, "the 502 recorded", func() bool { return strings.Contains(listenerState(o, "org/repo").LastError, "502") })
	if st := listenerState(o, "org/repo"); st.Failures != 1 || st.State != "retrying" {
		t.Fatalf("status after 409, 409, 502 = %+v, want Failures 1 and state retrying", st)
	}
}

func TestWithSupervisorTimingFloorsZeroDurations(t *testing.T) {
	o := New(&config.Config{MaxRunners: 1}, testLogger(), nil, WithSupervisorTiming(SupervisorTiming{}))
	def := DefaultSupervisorTiming()
	if o.timing.BackoffMin != def.BackoffMin || o.timing.BackoffMax != def.BackoffMax || o.timing.ConflictWait != def.ConflictWait {
		t.Fatalf("timing = %+v, want zero durations floored to %+v", o.timing, def)
	}
	custom := SupervisorTiming{BackoffMin: time.Millisecond, BackoffMax: 2 * time.Millisecond, ConflictWait: time.Millisecond}
	o = New(&config.Config{MaxRunners: 1}, testLogger(), nil, WithSupervisorTiming(custom))
	if o.timing.BackoffMin != time.Millisecond || o.timing.BackoffMax != 2*time.Millisecond || o.timing.ConflictWait != time.Millisecond {
		t.Fatalf("explicit durations changed: %+v", o.timing)
	}
}
