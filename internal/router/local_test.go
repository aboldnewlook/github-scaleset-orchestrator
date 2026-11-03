package router

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghsim"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/naming"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/service"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
	"github.com/aboldnewlook/github-scaleset-orchestrator/worker"
	"github.com/actions/scaleset"
)

// dockerAtNowhere configures the docker executor against a socket that does
// not exist: building it fails at Reconcile, which proves whether it was built.
const dockerAtNowhere = "runner:\n  executor: docker\n  image: example/job:1\n  instance_id: rt\n  docker_host: unix:///nonexistent/gso-router-test/docker.sock\n"

type fakeLocal struct{}

type nopExec struct{}

func (nopExec) Run(ctx context.Context, _, _ string) error {
	<-ctx.Done()
	return nil
}

func (fakeLocal) ForRepo(string, string) runner.Executor { return nopExec{} }

func TestMaxRunnersZeroBuildsNoLocalExecutor(t *testing.T) {
	r := newTestRouter(t, loadTestConfig(t, workersYAML+dockerAtNowhere), "")
	if err := r.prepareLocal(context.Background()); err != nil {
		t.Fatalf("prepareLocal with max_runners 0: %v", err)
	}
	if got := r.fleet.LocalSlots(); got != 0 {
		t.Fatalf("local slots = %d, want 0", got)
	}
}

func TestLocalExecutorIsBuiltWhenMaxRunnersIsPositive(t *testing.T) {
	r := newTestRouter(t, loadTestConfig(t, "max_runners: 2\n"+dockerAtNowhere), "")
	err := r.prepareLocal(context.Background())
	if err == nil || !strings.Contains(err.Error(), "reconciling stale runner containers") {
		t.Fatalf("prepareLocal = %v, want the docker reconcile failure", err)
	}
}

func TestInjectedLocalExecutorSkipsDocker(t *testing.T) {
	cfg := loadTestConfig(t, "max_runners: 2\n"+dockerAtNowhere)
	r, err := New(cfg, nil, nil, Options{StateDir: t.TempDir(), FleetTiming: fastTiming(), LocalExecutor: fakeLocal{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.prepareLocal(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := r.fleet.LocalSlots(); got != 2 {
		t.Fatalf("local slots = %d, want 2", got)
	}
}

func TestSetMaxRunnersResizesTheLocalMember(t *testing.T) {
	cfg := loadTestConfig(t, "max_runners: 2\n")
	r, err := New(cfg, nil, nil, Options{StateDir: t.TempDir(), FleetTiming: fastTiming(), LocalExecutor: fakeLocal{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.prepareLocal(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.SetMaxRunners(5); err != nil {
		t.Fatal(err)
	}
	if r.fleet.LocalSlots() != 5 || r.MaxRunners() != 5 {
		t.Fatalf("local slots %d, max runners %d", r.fleet.LocalSlots(), r.MaxRunners())
	}
	if err := r.SetMaxRunners(0); err == nil || !strings.Contains(err.Error(), "at least 1") {
		t.Fatalf("SetMaxRunners(0) without workers = %v", err)
	}
}

func TestSetMaxRunnersWithWorkersEnabled(t *testing.T) {
	r := newTestRouter(t, loadTestConfig(t, workersYAML), "")
	if err := r.prepareLocal(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.SetMaxRunners(0); err != nil {
		t.Fatalf("SetMaxRunners(0) with workers = %v", err)
	}
	if err := r.SetMaxRunners(3); err == nil || !strings.Contains(err.Error(), "max_runners: 0") {
		t.Fatalf("growing a router started without a local executor = %v", err)
	}
	if got := r.fleet.LocalSlots(); got != 0 {
		t.Fatalf("local slots = %d after a refused grow, want 0", got)
	}
}

func TestRunningLocalMemberCannotShrinkToZero(t *testing.T) {
	cfg := loadTestConfig(t, "max_runners: 2\nworkers:\n  listen: 127.0.0.1:9200\n")
	r, err := New(cfg, nil, nil, Options{StateDir: t.TempDir(), FleetTiming: fastTiming(), LocalExecutor: fakeLocal{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.prepareLocal(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.SetMaxRunners(0); err == nil || !strings.Contains(err.Error(), "restart") {
		t.Fatalf("SetMaxRunners(0) on a running local member = %v, want a restart hint", err)
	}
	if got := r.fleet.LocalSlots(); got != 2 {
		t.Fatalf("local slots = %d after a refused shrink, want 2", got)
	}
}

func TestGitHubFactoryReachesTheOrchestrator(t *testing.T) {
	gh := ghsim.NewModel(ghsim.Options{PollWait: 20 * time.Millisecond})
	cfg := loadTestConfig(t, workersYAML)
	r, err := New(cfg, nil, nil, Options{StateDir: t.TempDir(), FleetTiming: fastTiming(), GitHub: gh.Factory()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c := gh.Client("o/a")
	ss, err := c.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{Name: "gso-rt-a-linux-x64", RunnerGroupID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: "gso-alpha-a-abc123"}, ss.ID); err != nil {
		t.Fatal(err)
	}
	ref, err := r.Orchestrator().GetRunnerByName(ctx, "o/a", "gso-alpha-a-abc123")
	if err != nil || ref == nil || ref.ScaleSetID != ss.ID {
		t.Fatalf("lookup through the injected factory = %+v, %v", ref, err)
	}
}

func TestLiveStatusThroughTheRouterIncludesListeners(t *testing.T) {
	gh := ghsim.NewModel(ghsim.Options{PollWait: 20 * time.Millisecond})
	r, err := New(loadTestConfig(t, workersYAML), nil, nil, Options{StateDir: t.TempDir(), FleetTiming: fastTiming(), GitHub: gh.Factory()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }() // the real orchestrator, on the fake GitHub
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("router stopped with %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("router did not stop")
		}
	})
	rt := service.NewRuntime(r, func() {}, slog.New(slog.DiscardHandler))
	waitFor(t, "both repos' listeners in live_status", func() bool {
		st, err := rt.LiveStatus(ctx)
		if err != nil {
			return false
		}
		seen := map[string]bool{}
		for _, l := range st.Listeners {
			seen[l.Repo] = true
		}
		return seen["o/a"] && seen["p/b"]
	})
}

// recordingLauncher serves linux-x64 and records every runner the router
// places on the worker. Nothing runs: the fake runner is ghsim's job.
type recordingLauncher struct {
	mu      sync.Mutex
	started []worker.RunnerRecord
	jits    int
}

func (*recordingLauncher) Kind() runner.ExecutorKind { return runner.ExecutorDocker }
func (*recordingLauncher) Classes(context.Context) ([]runner.Class, error) {
	return []runner.Class{{OS: "linux", Arch: "x64"}}, nil
}

func (l *recordingLauncher) Start(_ context.Context, rec worker.RunnerRecord, jit, _ string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.started = append(l.started, rec)
	if jit != "" {
		l.jits++
	}
	return nil
}

func (*recordingLauncher) Wait(ctx context.Context, _ worker.RunnerRecord) error {
	<-ctx.Done()
	return ctx.Err()
}
func (*recordingLauncher) Kill(context.Context, worker.RunnerRecord) error      { return nil }
func (*recordingLauncher) RemoveOrphans(context.Context, map[string]bool) error { return nil }

func (l *recordingLauncher) snapshot() ([]worker.RunnerRecord, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]worker.RunnerRecord(nil), l.started...), l.jits
}

// The first end-to-end proof for placement: a job injected into the simulator
// reaches a real worker client through the real router, orchestrator, scaler
// and fleet. The router has no local executor (max_runners: 0) and its config
// has no token source, so GitHub is only ever reached through the injected
// factory.
//
// Not covered here: the job is not claimed or completed, because the launcher
// runs no fake runner (`gso sim runner`). That half is the e2e suite's.
func TestInjectedJobReachesARemoteWorker(t *testing.T) {
	gh := ghsim.NewModel(ghsim.Options{PollWait: 20 * time.Millisecond})
	cfg := loadTestConfig(t, workersYAML)
	cfg.Auth = config.TokenSource{}
	for _, repo := range cfg.Repos {
		if _, err := cfg.TokenForRepo(repo); err == nil {
			t.Fatalf("precondition: %s must have no token source", repo.Name)
		}
	}
	r, err := New(cfg, nil, nil, Options{StateDir: t.TempDir(), FleetTiming: fastTiming(), GitHub: gh.Factory()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("router stopped with %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("router did not stop")
		}
	})
	select {
	case <-r.Ready():
	case err := <-done:
		t.Fatalf("router exited before it was ready: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("router not ready")
	}

	res, err := r.AddWorker(ctx, control.WorkersAddParams{Name: "alpha", Classes: []string{"linux-x64"}, Slots: 1, Advertise: r.WorkersAddr()})
	if err != nil {
		t.Fatal(err)
	}
	join, err := worker.ParseJoin(res.Join)
	if err != nil {
		t.Fatal(err)
	}
	enrollCtx, cancelEnroll := context.WithTimeout(ctx, 5*time.Second)
	defer cancelEnroll()
	cred, st, err := worker.Enroll(enrollCtx, worker.EnrollOptions{Join: join, Name: "alpha", MachineID: "machine-alpha"})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	dir := t.TempDir()
	store := worker.FileCredentialStore(filepath.Join(dir, "credential"))
	if err := store.Save(cred); err != nil {
		t.Fatal(err)
	}
	records, err := worker.OpenRecordStore(filepath.Join(dir, "runners.json"))
	if err != nil {
		t.Fatal(err)
	}
	launcher := &recordingLauncher{}
	c, err := worker.New(worker.Config{
		Name: "alpha", MachineID: "machine-alpha", Version: "test",
		DialAddr: r.WorkersAddr(), Pin: st.Pin,
		Credential: store, Records: records,
		Launchers: []worker.Launcher{launcher},
		Slots:     1,
		Allowlist: st.OwnerScopes, AcceptPublic: worker.PublicFollow,
		BackoffMin: 10 * time.Millisecond, BackoffMax: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.SetAvailability(worker.Available)
	clientCtx, stopClient := context.WithCancel(ctx)
	clientDone := make(chan error, 1)
	go func() { clientDone <- c.Run(clientCtx) }()
	t.Cleanup(func() {
		stopClient()
		select {
		case err := <-clientDone:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("worker client stopped with %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("worker client did not stop")
		}
	})

	waitFor(t, "worker available on the router", func() bool {
		w := findWorker(r.ListWorkers(), "alpha")
		return w.Connected && w.Availability == "available"
	})
	waitFor(t, "o/a's scale set to be known", func() bool {
		_, ok := r.Orchestrator().ScaleSetID("o/a", cfg.DefaultClass())
		return ok
	})

	job, err := gh.InjectJob(ghsim.JobSpec{Repo: "o/a", Duration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the worker to be told to start a runner", func() bool {
		started, _ := launcher.snapshot()
		return len(started) == 1
	})

	started, jits := launcher.snapshot()
	rec := started[0]
	if rec.Repo != "o/a" || rec.Class != "linux-x64" {
		t.Fatalf("worker was given %+v for job %d, want a linux-x64 runner for o/a", rec, job.ID)
	}
	if seg, _ := naming.RunnerWorkerSegment(rec.Name); seg != naming.WorkerSegment("alpha") {
		t.Fatalf("runner %q is not named for worker alpha", rec.Name)
	}
	if jits != 1 {
		t.Fatalf("worker received %d JIT configs, want 1 (minted on AcceptOffer)", jits)
	}
	if got := gh.MintCount("o/a"); got != 1 {
		t.Fatalf("simulator minted %d JIT configs, want 1", got)
	}
}
