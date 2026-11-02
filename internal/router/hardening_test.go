package router

import (
	"context"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/fleet"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

func TestConfigStillRejectsPortZero(t *testing.T) {
	t.Setenv("GSO_ROUTER_TEST_TOKEN", "unused")
	path := t.TempDir() + "/config.yaml"
	body := "auth:\n  env: GSO_ROUTER_TEST_TOKEN\nrepos:\n  - name: o/a\nmax_runners: 0\nworkers:\n  listen: ':0'\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(path); err == nil || !strings.Contains(err.Error(), "workers.listen") {
		t.Fatalf("err = %v, want a workers.listen validation error", err)
	}
}

func TestWorkerListenerHasConnectionTimeouts(t *testing.T) {
	srv := newWorkerHTTPServer(context.Background(), http.NotFoundHandler(), nil, nil)
	if srv.ReadHeaderTimeout <= 0 {
		t.Error("ReadHeaderTimeout is unset")
	}
	if srv.ReadTimeout <= 0 {
		t.Error("ReadTimeout is unset")
	}
	if srv.IdleTimeout <= 0 {
		t.Error("IdleTimeout is unset")
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0: Subscribe is a long-lived stream", srv.WriteTimeout)
	}
}

func TestUnknownWorkerIsNotFound(t *testing.T) {
	r := newTestRouter(t, loadTestConfig(t, workersYAML), "")
	ctx := context.Background()
	check := func(what string, err error) {
		t.Helper()
		if err == nil || !errors.Is(err, errWorkerNotFound) || !strings.Contains(err.Error(), `worker "ghost" not found`) {
			t.Errorf("%s: err = %v, want a not-found error", what, err)
		}
	}
	check("drain", r.DrainWorker("ghost", ""))
	check("revoke", r.RevokeWorker(ctx, "ghost"))
	check("update", r.UpdateWorker(ctx, control.WorkersUpdateParams{Name: "ghost", Classes: []string{"linux-x64"}, Slots: 1}))
}

func TestMaxRunnersZeroRoutesOnlyAndRefusesRuntimeZero(t *testing.T) {
	r := newTestRouter(t, loadTestConfig(t, workersYAML), "")
	ctx := startWorkerSide(t, r)
	if r.MaxRunners() != 0 {
		t.Fatalf("MaxRunners = %d", r.MaxRunners())
	}
	done := make(chan error, 1)
	go func() { done <- r.SetMaxRunners(0) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SetMaxRunners(0) on a route-only router = %v, want a no-op", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SetMaxRunners(0) blocked")
	}
	if r.MaxRunners() != 0 {
		t.Fatalf("MaxRunners = %d after SetMaxRunners(0)", r.MaxRunners())
	}
	if err := r.SetMaxRunners(-1); err == nil {
		t.Fatal("SetMaxRunners(-1) accepted")
	}
	// A worker can still be enrolled on a route-only router.
	if _, err := r.AddWorker(ctx, control.WorkersAddParams{Name: "alpha", Classes: []string{"linux-x64"}, Slots: 1, Advertise: r.WorkersAddr()}); err != nil {
		t.Fatalf("AddWorker on a max_runners 0 router: %v", err)
	}
}

func TestSetMaxRunnersCannotShrinkToZeroAtRuntime(t *testing.T) {
	r := newTestRouter(t, loadTestConfig(t, "max_runners: 2\nworkers:\n  listen: 127.0.0.1:9200\n"), "")
	if err := r.SetMaxRunners(0); err == nil || !strings.Contains(err.Error(), "restart") {
		t.Fatalf("err = %v, want a refusal that points at the config", err)
	}
	if r.MaxRunners() != 2 {
		t.Fatalf("MaxRunners = %d, want 2 unchanged", r.MaxRunners())
	}
}

func TestSetMaxRunnersZeroRefusedWithoutWorkers(t *testing.T) {
	r := newTestRouter(t, loadTestConfig(t, "max_runners: 2\n"), "")
	if err := r.SetMaxRunners(0); err == nil || !strings.Contains(err.Error(), "workers.listen") {
		t.Fatalf("err = %v, want it to mention workers.listen", err)
	}
}

func TestScaleSetKeyFoldsCase(t *testing.T) {
	a := scaleSetKey("Acme/Repo", runner.Class{OS: "linux", Arch: "x64"})
	b := scaleSetKey("acme/repo", runner.Class{OS: "linux", Arch: "x64"})
	c := scaleSetKey("acme/repo", runner.Class{OS: "linux", Arch: "arm64"})
	if a != b {
		t.Errorf("keys differ by case: %q vs %q", a, b)
	}
	if a == c {
		t.Errorf("keys ignore the class: %q", a)
	}
}

type countingGitHub struct {
	calls map[string]int
	known map[string]bool
}

func (g *countingGitHub) ScaleSetID(repo string, _ runner.Class) (int, bool) {
	g.calls[repo]++
	return 1, g.known[repo]
}

func TestWaitKnownDedupesRepoCaseInsensitively(t *testing.T) {
	gh := &countingGitHub{calls: map[string]int{}, known: map[string]bool{"Acme/Repo": true}}
	repos := []string{"Acme/Repo", "acme/repo", "ACME/REPO"}
	waitKnown(context.Background(), gh, repos, runner.Class{OS: "linux", Arch: "x64"}, time.Second)
	total := 0
	for _, n := range gh.calls {
		total += n
	}
	if total != 1 {
		t.Fatalf("ScaleSetID called %d times (%v), want once for the one scale set", total, gh.calls)
	}
}

func TestWaitKnownGivesUpAfterGrace(t *testing.T) {
	gh := &countingGitHub{calls: map[string]int{}, known: map[string]bool{}}
	start := time.Now()
	waitKnown(context.Background(), gh, []string{"o/a"}, runner.Class{OS: "linux", Arch: "x64"}, 60*time.Millisecond)
	if d := time.Since(start); d < 50*time.Millisecond || d > 2*time.Second {
		t.Fatalf("waited %v, want about the grace period", d)
	}
}

func TestOwnerScopesFoldCase(t *testing.T) {
	r := newTestRouter(t, loadTestConfig(t, workersYAML), "")
	r.cfg.Repos = append(r.cfg.Repos, config.Repo{Name: "O/c"}, config.Repo{Name: "Q/d"})
	got := r.ownerScopes(fleet.Bounds{})
	want := []string{"o/*", "p/*", "q/*"}
	if !slices.Equal(got, want) {
		t.Fatalf("scopes = %v, want %v", got, want)
	}
}
