package fleet

import (
	"context"
	"sync"
	"testing"
	"time"

	workerv1 "github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/event"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/naming"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

var (
	linuxX64   = runner.Class{OS: "linux", Arch: "x64"}
	linuxArm64 = runner.Class{OS: "linux", Arch: "arm64"}
	macosArm64 = runner.Class{OS: "macos", Arch: "arm64"}
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type fakeGitHub struct {
	mu        sync.Mutex
	runners   map[string]*RunnerRef // repo|name
	scaleSets map[string]int        // repo|class
	removed   []int64
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{runners: map[string]*RunnerRef{}, scaleSets: map[string]int{}}
}

func (g *fakeGitHub) setScaleSet(repo string, c runner.Class, id int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.scaleSets[repo+"|"+c.String()] = id
}

func (g *fakeGitHub) addRunner(repo, name string, id int64, scaleSetID int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.runners[repo+"|"+name] = &RunnerRef{ID: id, ScaleSetID: scaleSetID}
}

func (g *fakeGitHub) GetRunnerByName(_ context.Context, repo, name string) (*RunnerRef, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.runners[repo+"|"+name], nil
}

func (g *fakeGitHub) RemoveRunner(_ context.Context, _ string, runnerID int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.removed = append(g.removed, runnerID)
	return nil
}

func (g *fakeGitHub) ScaleSetID(repo string, c runner.Class) (int, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	id, ok := g.scaleSets[repo+"|"+c.String()]
	return id, ok
}

func (g *fakeGitHub) Removed() []int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]int64(nil), g.removed...)
}

type fakeAdopter struct {
	mu      sync.Mutex
	refuse  bool
	adopted []*Reservation
}

func (a *fakeAdopter) Adopt(_ string, _ runner.Class, res *Reservation) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.refuse {
		return false
	}
	a.adopted = append(a.adopted, res)
	return true
}

func (a *fakeAdopter) list() []*Reservation {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*Reservation(nil), a.adopted...)
}

type mintRecorder struct {
	mu    sync.Mutex
	names []string
	next  int64
	err   error
}

func (m *mintRecorder) mint(_ context.Context, runnerName string) (int64, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.names = append(m.names, runnerName)
	if m.err != nil {
		return 0, "", m.err
	}
	m.next++
	return 1000 + m.next, "jit-" + runnerName, nil
}

func (m *mintRecorder) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.names)
}

type testFleet struct {
	*Fleet
	clock   *testClock
	gh      *fakeGitHub
	adopter *fakeAdopter
	events  <-chan event.Event
}

func newTestFleet(t *testing.T, mutate ...func(*Options)) *testFleet {
	t.Helper()
	clock := newTestClock()
	gh := newFakeGitHub()
	adopter := &fakeAdopter{}
	bus := event.NewBus(nil)
	_, events := bus.Subscribe(256)
	o := Options{Timing: DefaultTiming(), GitHub: gh, Adopter: adopter, Bus: bus, Now: clock.Now}
	for _, m := range mutate {
		m(&o)
	}
	return &testFleet{Fleet: New(o), clock: clock, gh: gh, adopter: adopter, events: events}
}

// sawEvent drains buffered events and reports whether one of type typ was published.
func (tf *testFleet) sawEvent(typ event.EventType) bool {
	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case e := <-tf.events:
			if e.Type == typ {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

func testWorker(name string, b Bounds) WorkerRecord {
	return WorkerRecord{ID: "id-" + name, Name: name, Segment: naming.WorkerSegment(name), Bounds: b}
}

func dockerCaps(slots uint32, classes ...runner.Class) *workerv1.Capabilities {
	ex := &workerv1.Executor{Kind: workerv1.ExecutorKind_EXECUTOR_KIND_DOCKER}
	for _, c := range classes {
		ex.Classes = append(ex.Classes, ClassToProto(c))
	}
	return &workerv1.Capabilities{WorkerVersion: "test", Slots: slots, Executors: []*workerv1.Executor{ex}}
}

func (tf *testFleet) open(t *testing.T, w WorkerRecord, caps *workerv1.Capabilities, running ...*workerv1.RunnerState) OpenSessionResult {
	t.Helper()
	res, err := tf.OpenSession(context.Background(), w, OpenSessionParams{
		MachineID: "machine-" + w.Name, Capabilities: caps,
		Availability: workerv1.Availability_AVAILABILITY_AVAILABLE, Running: running,
	})
	if err != nil {
		t.Fatalf("OpenSession(%s): %v", w.Name, err)
	}
	return res
}

func (tf *testFleet) directives(t *testing.T, sessionID, workerID string) <-chan *workerv1.Directive {
	t.Helper()
	ch, err := tf.Directives(sessionID, workerID)
	if err != nil {
		t.Fatalf("Directives: %v", err)
	}
	return ch
}

func recvDirective(t *testing.T, ch <-chan *workerv1.Directive) *workerv1.Directive {
	t.Helper()
	select {
	case d, ok := <-ch:
		if !ok {
			t.Fatal("directive channel closed")
		}
		return d
	case <-time.After(2 * time.Second):
		t.Fatal("no directive within 2s")
	}
	return nil
}

func runAsync(ctx context.Context, r *Reservation, mint MintFunc) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- r.Run(ctx, mint) }()
	return ch
}

func waitErr(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s")
	}
	return nil
}

func assertPending(t *testing.T, ch <-chan error) {
	t.Helper()
	select {
	case err := <-ch:
		t.Fatalf("Run returned early: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
}

func runningState(name, repo string, c runner.Class, phase workerv1.RunnerPhase) *workerv1.RunnerState {
	return &workerv1.RunnerState{RunnerName: name, Repo: repo, Class: ClassToProto(c), Phase: phase}
}

func mustReserve(t *testing.T, f *Fleet, req PlacementRequest) *Reservation {
	t.Helper()
	r, ok := f.Reserve(req)
	if !ok {
		t.Fatalf("Reserve(%+v) = false", req)
	}
	return r
}
