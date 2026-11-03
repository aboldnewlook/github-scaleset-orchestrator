package router

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghapi"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghsim"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
	"github.com/aboldnewlook/github-scaleset-orchestrator/worker"
)

// runSimRouter runs a router on a fresh simulator whose repo o/a is already
// public (visibility is read when a listener starts) and waits until it is
// ready.
func runSimRouter(t *testing.T, cfg *config.Config, opts Options) (*Router, *ghsim.Model, context.Context) {
	t.Helper()
	gh := ghsim.NewModel(ghsim.Options{PollWait: 20 * time.Millisecond})
	gh.SetRepo("o/a", ghapi.RepoInfo{FullName: "o/a", Visibility: "public"})
	opts.StateDir, opts.FleetTiming, opts.GitHub = t.TempDir(), fastTiming(), gh.Factory()
	r, err := New(cfg, nil, nil, opts)
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
	return r, gh, ctx
}

// connectWorker enrolls a worker named alpha and runs its client with the
// given public-repo mode until the test ends.
func connectWorker(t *testing.T, ctx context.Context, r *Router, launcher worker.Launcher, mode worker.PublicMode) {
	t.Helper()
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
	c, err := worker.New(worker.Config{
		Name: "alpha", MachineID: "machine-alpha", Version: "test",
		DialAddr: r.WorkersAddr(), Pin: st.Pin,
		Credential: store, Records: records,
		Launchers: []worker.Launcher{launcher}, Slots: 1,
		Allowlist: st.OwnerScopes, AcceptPublic: mode,
		BackoffMin: 10 * time.Millisecond, BackoffMax: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.SetAvailability(worker.Available)
	clientCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- c.Run(clientCtx) }()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-done:
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
}

func waitScaleSet(t *testing.T, r *Router, cfg *config.Config) {
	t.Helper()
	waitFor(t, "o/a's scale set to be known", func() bool {
		_, ok := r.Orchestrator().ScaleSetID("o/a", cfg.DefaultClass())
		return ok
	})
}

func TestPublicJobIsNotOfferedToAWorkerThatRefusesPublic(t *testing.T) {
	cfg := loadTestConfig(t, workersYAML)
	r, gh, ctx := runSimRouter(t, cfg, Options{})
	launcher := &recordingLauncher{}
	connectWorker(t, ctx, r, launcher, worker.PublicRefuse)
	waitScaleSet(t, r, cfg)

	if _, err := gh.InjectJob(ghsim.JobSpec{Repo: "o/a", Public: true, Duration: time.Minute}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * fastTiming().Heartbeat)
	if started, _ := launcher.snapshot(); len(started) != 0 {
		t.Fatalf("a worker that refuses public repos was given %+v", started)
	}
	if got := gh.MintCount("o/a"); got != 0 {
		t.Fatalf("minted %d JIT configs for a public job nobody may run, want 0", got)
	}
}

func TestPublicJobIsOfferedToAWorkerThatAcceptsPublicWithoutACache(t *testing.T) {
	cfg := loadTestConfig(t, workersYAML)
	r, gh, ctx := runSimRouter(t, cfg, Options{})
	launcher := &cacheLauncher{}
	connectWorker(t, ctx, r, launcher, worker.PublicAccept)
	waitScaleSet(t, r, cfg)

	if _, err := gh.InjectJob(ghsim.JobSpec{Repo: "o/a", Public: true, Duration: time.Minute}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the public job to reach the opted-in worker", func() bool { return len(launcher.caches()) == 1 })
	if got := launcher.caches()[0]; got != "none" {
		t.Fatalf("cache mode for a public repo = %q, want none", got)
	}
}

// cacheLauncher records the cache mode of every runner it is asked to start.
type cacheLauncher struct {
	recordingLauncher
	mu2    sync.Mutex
	cached []string
}

func (l *cacheLauncher) Start(ctx context.Context, rec worker.RunnerRecord, jit, cache string) error {
	l.mu2.Lock()
	l.cached = append(l.cached, cache)
	l.mu2.Unlock()
	return l.recordingLauncher.Start(ctx, rec, jit, cache)
}

func (l *cacheLauncher) caches() []string {
	l.mu2.Lock()
	defer l.mu2.Unlock()
	return append([]string(nil), l.cached...)
}

type recordingExec struct {
	mu    sync.Mutex
	names []string
}

func (e *recordingExec) Run(ctx context.Context, name, _ string) error {
	e.mu.Lock()
	e.names = append(e.names, name)
	e.mu.Unlock()
	<-ctx.Done()
	return nil
}

func (e *recordingExec) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.names)
}

type singleLocal struct{ exec runner.Executor }

func (s singleLocal) ForRepo(string, string) runner.Executor { return s.exec }

// Single-machine gso users run public repos on the local member; going
// through the fleet must not change that.
func TestLocalMemberStillRunsAPublicRepo(t *testing.T) {
	cfg := loadTestConfig(t, "max_runners: 1\n")
	exec := &recordingExec{}
	_, gh, _ := runSimRouter(t, cfg, Options{LocalExecutor: singleLocal{exec}})
	waitFor(t, "o/a's scale set", func() bool { return len(gh.ScaleSets("o/a")) == 1 })

	if _, err := gh.InjectJob(ghsim.JobSpec{Repo: "o/a", Public: true, Duration: time.Minute}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the local member to run the public job", func() bool { return exec.count() == 1 })
}
