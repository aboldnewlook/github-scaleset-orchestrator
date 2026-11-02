package router

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/fleet"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
	"github.com/aboldnewlook/github-scaleset-orchestrator/worker"
)

// config validation rejects port 0 (validPort), so the file names a real
// port and loadTestConfig swaps in an ephemeral one after validation.
const workersYAML = "max_runners: 0\nworkers:\n  listen: 127.0.0.1:9200\n  default_class: linux-x64\n"

// loadTestConfig loads a real config file so validation and defaults apply.
// body supplies max_runners, workers and runner keys; repos and auth are fixed.
func loadTestConfig(t *testing.T, body string) *config.Config {
	t.Helper()
	t.Setenv("GSO_ROUTER_TEST_TOKEN", "unused")
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "auth:\n  env: GSO_ROUTER_TEST_TOKEN\nrepos:\n  - name: o/a\n  - name: p/b\n" + body
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Workers.Listen != "" {
		cfg.Workers.Listen = "127.0.0.1:0"
	}
	return cfg
}

func fastTiming() *fleet.Timing {
	return &fleet.Timing{
		Heartbeat: 50 * time.Millisecond, Lost: 300 * time.Millisecond, Keepalive: 100 * time.Millisecond,
		OfferTTL: 500 * time.Millisecond, StartTTL: 2 * time.Second, Grace: 200 * time.Millisecond,
		LostUnstartedRemove: 300 * time.Millisecond, LostStartedCap: time.Minute, Tick: 20 * time.Millisecond,
	}
}

func newTestRouter(t *testing.T, cfg *config.Config, stateDir string) *Router {
	t.Helper()
	if stateDir == "" {
		stateDir = t.TempDir()
	}
	r, err := New(cfg, nil, nil, Options{StateDir: stateDir, FleetTiming: fastTiming()})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	return r
}

// startWorkerSide runs the router with the orchestrator stubbed out: the fleet
// and the worker listener are real, GitHub is never contacted. Configs passed
// here use max_runners: 0, so B8a's local-member preparation in Run builds
// nothing either.
func startWorkerSide(t *testing.T, r *Router) context.Context {
	t.Helper()
	r.runOrchestrator = func(ctx context.Context) error {
		<-ctx.Done()
		return nil
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
	return ctx
}

func TestRunStopsWhenTheOrchestratorReturns(t *testing.T) {
	r := newTestRouter(t, loadTestConfig(t, workersYAML), "")
	want := errors.New("orchestrator failed")
	r.runOrchestrator = func(context.Context) error { return want }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // stops the worker listener Run started
	if err := r.Run(ctx); !errors.Is(err, want) {
		t.Fatalf("Run = %v, want the orchestrator's error", err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestJoinStringCarriesTheListenerPin(t *testing.T) {
	r := newTestRouter(t, loadTestConfig(t, workersYAML), "")
	ctx := startWorkerSide(t, r)
	if r.WorkersAddr() == "" {
		t.Fatal("WorkersAddr empty after start")
	}
	res, err := r.AddWorker(ctx, control.WorkersAddParams{Name: "mac", Classes: []string{"linux-arm64"}, Slots: 4, Advertise: "router.lan:9200"})
	if err != nil {
		t.Fatal(err)
	}
	join, err := worker.ParseJoin(res.Join)
	if err != nil {
		t.Fatal(err)
	}
	if join.Pin != r.WorkersPin() || join.Addr != "router.lan:9200" || len(join.Code) != 32 {
		t.Fatalf("join = %+v, pin %s", join, r.WorkersPin())
	}
	if !res.ExpiresAt.After(time.Now()) {
		t.Fatalf("expires %v", res.ExpiresAt)
	}
}

func TestAddWorkerRefusesWithoutAnAdvertiseAddress(t *testing.T) {
	r := newTestRouter(t, loadTestConfig(t, workersYAML), "")
	ctx := startWorkerSide(t, r)
	_, err := r.AddWorker(ctx, control.WorkersAddParams{Name: "mac", Classes: []string{"linux-x64"}, Slots: 1})
	if err == nil || !strings.Contains(err.Error(), "no advertise address: set workers.advertise (GSO_WORKERS_ADVERTISE) or pass --advertise host:port") {
		t.Fatalf("err = %v", err)
	}
}

func TestAddWorkerUsesTheConfiguredAdvertiseAddress(t *testing.T) {
	cfg := loadTestConfig(t, workersYAML+"  advertise: unraid.lan:9200\n")
	r := newTestRouter(t, cfg, "")
	ctx := startWorkerSide(t, r)
	res, err := r.AddWorker(ctx, control.WorkersAddParams{Name: "unraid", Classes: []string{"linux-x64"}, Slots: 2})
	if err != nil {
		t.Fatal(err)
	}
	if join, _ := worker.ParseJoin(res.Join); join.Addr != "unraid.lan:9200" {
		t.Fatalf("join addr = %q", join.Addr)
	}
}

func TestAddWorkerValidatesBounds(t *testing.T) {
	r := newTestRouter(t, loadTestConfig(t, workersYAML), "")
	ctx := startWorkerSide(t, r)
	for _, tc := range []struct {
		name string
		p    control.WorkersAddParams
		want string
	}{
		{"no classes", control.WorkersAddParams{Name: "w", Slots: 1}, "at least one class"},
		{"unknown class", control.WorkersAddParams{Name: "w", Classes: []string{"linux-sparc"}, Slots: 1}, "linux-sparc"},
		{"zero slots", control.WorkersAddParams{Name: "w", Classes: []string{"linux-x64"}}, "slots must be at least 1"},
		{"too many slots", control.WorkersAddParams{Name: "w", Classes: []string{"linux-x64"}, Slots: 17}, "max_slots_per_worker"},
		{"bad repo", control.WorkersAddParams{Name: "w", Classes: []string{"linux-x64"}, Slots: 1, Repos: []string{"noslash"}}, "noslash"},
		{"bad advertise", control.WorkersAddParams{Name: "w", Classes: []string{"linux-x64"}, Slots: 1, Advertise: "no-port"}, "advertise address"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.p.Advertise == "" {
				tc.p.Advertise = "router.lan:9200"
			}
			_, err := r.AddWorker(ctx, tc.p)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestWorkersDisabled(t *testing.T) {
	r := newTestRouter(t, loadTestConfig(t, "max_runners: 1\n"), "")
	// Not Run: with max_runners 1, B8a's Run would prepare a local executor.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go r.fleet.Start(ctx)
	if err := r.startWorkers(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("router not ready")
	}
	if r.WorkersAddr() != "" || r.WorkersPin() != "" {
		t.Fatalf("addr %q pin %q with workers disabled", r.WorkersAddr(), r.WorkersPin())
	}
	if _, err := r.AddWorker(ctx, control.WorkersAddParams{Name: "w", Classes: []string{"linux-x64"}, Slots: 1, Advertise: "h:1"}); !errors.Is(err, errWorkersDisabled) {
		t.Fatalf("err = %v", err)
	}
	if err := r.DrainWorker("w", ""); !errors.Is(err, errWorkersDisabled) {
		t.Fatalf("drain err = %v", err)
	}
}

func TestPinIsStableAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	cfg := loadTestConfig(t, workersYAML)
	first := newTestRouter(t, cfg, dir).WorkersPin()
	second := newTestRouter(t, cfg, dir).WorkersPin()
	if first == "" || first != second {
		t.Fatalf("pins %q then %q", first, second)
	}
}

func TestOwnerScopes(t *testing.T) {
	r := newTestRouter(t, loadTestConfig(t, workersYAML), "")
	if got := r.ownerScopes(fleet.Bounds{Repos: []string{"o/a"}}); !reflect.DeepEqual(got, []string{"o/a"}) {
		t.Fatalf("bounded scopes = %v", got)
	}
	if got := r.ownerScopes(fleet.Bounds{}); !reflect.DeepEqual(got, []string{"o/*", "p/*"}) {
		t.Fatalf("unbounded scopes = %v", got)
	}
}

type stubPlacement struct {
	scaleSetID int
	adopted    []string
}

func (s *stubPlacement) GetRunnerByName(context.Context, string, string) (*fleet.RunnerRef, error) {
	return &fleet.RunnerRef{ID: 7, ScaleSetID: s.scaleSetID}, nil
}
func (s *stubPlacement) RemoveRunner(context.Context, string, int64) error { return nil }
func (s *stubPlacement) ScaleSetID(string, runner.Class) (int, bool)       { return s.scaleSetID, true }
func (s *stubPlacement) Adopt(repo string, _ runner.Class, _ *fleet.Reservation) bool {
	s.adopted = append(s.adopted, repo)
	return true
}

func TestPlacementWrappersDelegateOnlyWhenWired(t *testing.T) {
	ctx := context.Background()
	linux := runner.Class{OS: "linux", Arch: "x64"}
	unwired := placementGitHub{target: func() any { return struct{}{} }}
	if _, err := unwired.GetRunnerByName(ctx, "o/a", "x"); !errors.Is(err, errPlacementNotWired) {
		t.Fatalf("unwired lookup err = %v", err)
	}
	if _, ok := unwired.ScaleSetID("o/a", linux); ok {
		t.Fatal("unwired ScaleSetID reported a scale set")
	}
	if (placementAdopter{target: func() any { return nil }}).Adopt("o/a", linux, nil) {
		t.Fatal("unwired Adopt accepted a runner")
	}

	stub := &stubPlacement{scaleSetID: 42}
	wired := placementGitHub{target: func() any { return stub }}
	ref, err := wired.GetRunnerByName(ctx, "o/a", "x")
	if err != nil || ref.ScaleSetID != 42 {
		t.Fatalf("wired lookup = %+v, %v", ref, err)
	}
	if id, ok := wired.ScaleSetID("o/a", linux); !ok || id != 42 {
		t.Fatalf("wired ScaleSetID = %d, %v", id, ok)
	}
	if !(placementAdopter{target: func() any { return stub }}).Adopt("o/a", linux, nil) || !slices.Equal(stub.adopted, []string{"o/a"}) {
		t.Fatal("wired Adopt did not delegate")
	}
}

func TestRouterDelegatesOrchestratorState(t *testing.T) {
	cfg := loadTestConfig(t, "max_runners: 3\n")
	r := newTestRouter(t, cfg, "")
	if r.MaxRunners() != 3 {
		t.Fatalf("MaxRunners = %d", r.MaxRunners())
	}
	if r.CancelRunner("nobody") {
		t.Fatal("CancelRunner found a runner before Run")
	}
	if got := r.RunnersByRepo(); len(got) != 0 {
		t.Fatalf("RunnersByRepo = %v", got)
	}
	if r.Orchestrator() == nil || r.Fleet() == nil {
		t.Fatal("accessors returned nil")
	}
}
