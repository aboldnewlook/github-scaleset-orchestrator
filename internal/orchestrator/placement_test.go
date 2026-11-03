package orchestrator

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/fleet"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghsim"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
	"github.com/actions/scaleset"
)

var placementClass = runner.Class{OS: "linux", Arch: "x64"}

type placementExec struct{}

func (placementExec) Run(ctx context.Context, _, _ string) error {
	<-ctx.Done()
	return nil
}

type placementLocal struct{}

func (placementLocal) ForRepo(string, string) runner.Executor { return placementExec{} }

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

func newPlacementOrchestrator(t *testing.T, repos ...string) (*Orchestrator, *fleet.Fleet, *ghsim.Model) {
	t.Helper()
	cfg := &config.Config{MaxRunners: 2, Runner: config.RunnerConfig{Cache: "none"}}
	for _, r := range repos {
		cfg.Repos = append(cfg.Repos, config.Repo{Name: r})
	}
	fl := fleet.New(fleet.Options{Logger: testLogger(), Timing: fleet.DefaultTiming()})
	gh := ghsim.NewModel(ghsim.Options{PollWait: 20 * time.Millisecond})
	return New(cfg, testLogger(), nil, WithFleet(fl), WithGitHub(gh.Factory())), fl, gh
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

func TestNewScalerIsCreatedOncePerRepoAndClass(t *testing.T) {
	o, _, gh := newPlacementOrchestrator(t, "o/a")
	first := o.newScaler(config.Repo{Name: "o/a"}, placementClass, gh.Client("o/a"))
	if again := o.newScaler(config.Repo{Name: "o/a"}, placementClass, gh.Client("o/a")); again != first {
		t.Fatal("a second scaler was created for the same repo and class")
	}
	if upper := o.newScaler(config.Repo{Name: "O/A"}, placementClass, gh.Client("o/a")); upper != first {
		t.Fatal("a repo differing only by case got its own scaler")
	}
	if o.scalerFor("o/a", runner.Class{OS: "linux", Arch: "arm64"}) != nil {
		t.Fatal("found a scaler for a class that has none")
	}
}

func TestAdoptRoutesToTheMatchingScaler(t *testing.T) {
	o, fl, gh := newPlacementOrchestrator(t, "o/a", "o/b")
	a := o.newScaler(config.Repo{Name: "o/a"}, placementClass, gh.Client("o/a"))
	b := o.newScaler(config.Repo{Name: "o/b"}, placementClass, gh.Client("o/b"))
	fl.SetLocal(2, placementClass, placementLocal{})
	res, ok := fl.Reserve(fleet.PlacementRequest{Repo: "o/b", Class: placementClass, Cache: "none"})
	if !ok {
		t.Fatal("no slot")
	}
	if !o.Adopt("o/b", placementClass, res) {
		t.Fatal("Adopt refused a runner for a known scaler")
	}
	if !slices.Contains(b.Runners(), res.RunnerName()) || len(a.Runners()) != 0 {
		t.Fatalf("o/a %v, o/b %v", a.Runners(), b.Runners())
	}
	if o.Adopt("o/c", placementClass, res) {
		t.Fatal("Adopt accepted a runner for an unknown repo")
	}
	if o.Adopt("o/b", runner.Class{OS: "linux", Arch: "arm64"}, res) {
		t.Fatal("Adopt accepted a runner for a class with no scaler")
	}
	b.CancelRunner(res.RunnerName())
}

func TestScaleSetIDComesFromTheScaler(t *testing.T) {
	o, _, gh := newPlacementOrchestrator(t, "o/a")
	if _, ok := o.ScaleSetID("o/a", placementClass); ok {
		t.Fatal("a scale set ID before any scaler exists")
	}
	s := o.newScaler(config.Repo{Name: "o/a"}, placementClass, gh.Client("o/a"))
	if _, ok := o.ScaleSetID("o/a", placementClass); ok {
		t.Fatal("a scale set ID before the supervisor set it")
	}
	s.SetScaleSetID(7)
	if id, ok := o.ScaleSetID("o/a", placementClass); !ok || id != 7 {
		t.Fatalf("ScaleSetID = %d, %v", id, ok)
	}
}

func TestGetRunnerByNameAndRemoveRunnerUseTheRepoClient(t *testing.T) {
	o, _, gh := newPlacementOrchestrator(t, "o/a")
	ctx := context.Background()
	c := gh.Client("o/a")
	ss, err := c.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{Name: "gso-test-a-linux-x64", RunnerGroupID: 1})
	if err != nil {
		t.Fatal(err)
	}
	jit, err := c.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: "gso-alpha-a-abc123"}, ss.ID)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := o.GetRunnerByName(ctx, "o/a", "gso-alpha-a-abc123")
	if err != nil || ref == nil || ref.ID != int64(jit.Runner.ID) || ref.ScaleSetID != ss.ID {
		t.Fatalf("ref = %+v, err %v", ref, err)
	}
	if missing, err := o.GetRunnerByName(ctx, "o/a", "gso-nobody-a-zzzzzz"); missing != nil || err != nil {
		t.Fatalf("missing runner = %+v, %v", missing, err)
	}
	if err := o.RemoveRunner(ctx, "o/a", ref.ID); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(gh.Removed("o/a"), ref.ID) {
		t.Fatalf("removed = %v", gh.Removed("o/a"))
	}
	if _, err := o.GetRunnerByName(ctx, "o/zzz", "x"); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("unconfigured repo err = %v", err)
	}
}

func TestFleetChangeReevaluatesAndPushesCapacity(t *testing.T) {
	o, fl, gh := newPlacementOrchestrator(t, "o/a")
	s := o.newScaler(config.Repo{Name: "o/a"}, placementClass, gh.Client("o/a"))
	rec := &capacityRecorder{}
	s.SetCapacitySink(rec)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fl.OnChange(o.signalReevaluate)
	go o.reevaluateLoop(ctx)
	fl.SetLocal(3, placementClass, placementLocal{})
	o.signalReevaluate() // whether or not SetLocal fires OnChange
	waitPlacement(t, "capacity 3 pushed to the listener", func() bool { return rec.last() == 3 })
}

func TestAvailableSlotsComesFromTheFleet(t *testing.T) {
	o, fl, _ := newPlacementOrchestrator(t, "o/a")
	fl.SetLocal(4, placementClass, placementLocal{})
	if got := o.AvailableSlots(); got != 4 {
		t.Fatalf("AvailableSlots = %d, want 4", got)
	}
	res, ok := fl.Reserve(fleet.PlacementRequest{Repo: "o/a", Class: placementClass, Cache: "none"})
	if !ok {
		t.Fatal("no slot")
	}
	defer res.Release()
	if got := o.AvailableSlots(); got != 3 {
		t.Fatalf("AvailableSlots = %d after a reservation, want 3", got)
	}
	if got := New(&config.Config{}, testLogger(), nil).AvailableSlots(); got != 0 {
		t.Fatalf("AvailableSlots without a fleet = %d", got)
	}
}

func TestSetMaxRunnersRecordsTheCount(t *testing.T) {
	o, _, _ := newPlacementOrchestrator(t, "o/a")
	if err := o.SetMaxRunners(-1); err == nil {
		t.Fatal("negative count accepted")
	}
	if err := o.SetMaxRunners(0); err != nil {
		t.Fatal(err)
	}
	if got := o.MaxRunners(); got != 0 {
		t.Fatalf("MaxRunners = %d", got)
	}
}

func TestRunRequiresAFleet(t *testing.T) {
	err := New(&config.Config{MaxRunners: 1}, testLogger(), nil).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "WithFleet") {
		t.Fatalf("Run without a fleet: %v", err)
	}
}
