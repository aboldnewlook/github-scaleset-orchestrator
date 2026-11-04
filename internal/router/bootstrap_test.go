package router

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/fleet"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
	"github.com/aboldnewlook/github-scaleset-orchestrator/worker"
)

// syncBuffer is a log sink the test can read while the router writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

const sibling = "sibling"

// bootstrapYAML enables workers.bootstrap for the worker named "sibling".
func bootstrapYAML(joinFile string) string {
	return workersYAML + fmt.Sprintf("  advertise: 127.0.0.1:9200\n  bootstrap:\n    name: %s\n    classes: [linux-x64]\n    slots: 2\n    join_file: %q\n", sibling, joinFile)
}

// newBootstrapRouter builds a router whose bootstrap loop polls every 10 ms and
// logs at INFO into the returned buffer.
func newBootstrapRouter(t *testing.T, cfg *config.Config, stateDir string) (*Router, *syncBuffer) {
	t.Helper()
	if stateDir == "" {
		stateDir = t.TempDir()
	}
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	r, err := New(cfg, logger, nil, Options{StateDir: stateDir, FleetTiming: fastTiming()})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	r.bootstrapPoll = 10 * time.Millisecond
	return r, logs
}

func readJoin(t *testing.T, path string) worker.Join {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the join file: %v", err)
	}
	j, err := worker.ParseJoin(string(b))
	if err != nil {
		t.Fatalf("join file does not hold a join string: %v", err)
	}
	return j
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func enroll(ctx context.Context, r *Router, j worker.Join) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, _, err := worker.Enroll(ctx, worker.EnrollOptions{Join: j, DialAddr: r.WorkersAddr(), Name: sibling, MachineID: "machine-" + sibling})
	return err
}

func TestBootstrapEnrollsTheSiblingWithNoHumanStep(t *testing.T) {
	joinFile := filepath.Join(t.TempDir(), "bootstrap", "join")
	r, logs := newBootstrapRouter(t, loadTestConfig(t, bootstrapYAML(joinFile)), "")
	ctx := startWorkerSide(t, r)

	waitFor(t, "the join file", func() bool { return fileExists(joinFile) })
	j := readJoin(t, joinFile)
	if j.Pin != r.WorkersPin() || j.Addr != "127.0.0.1:9200" {
		t.Fatalf("join = %+v, want the router's pin and workers.advertise", j)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(joinFile); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("join file mode = %v (%v), want 0600", info.Mode().Perm(), err)
		}
		if info, err := os.Stat(filepath.Dir(joinFile)); err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("join file dir mode = %v (%v), want 0700 for a directory the router created", info.Mode().Perm(), err)
		}
	}

	if err := enroll(ctx, r, j); err != nil {
		t.Fatalf("enrolling with the join file's string: %v", err)
	}
	waitFor(t, "the join file to be removed", func() bool { return !fileExists(joinFile) })

	w := findWorker(r.ListWorkers(), sibling)
	if w.ID == "" || w.Revoked || w.Slots != 2 || len(w.Classes) != 1 || w.Classes[0] != "linux-x64" {
		t.Fatalf("enrolled worker = %+v, want linux-x64 with 2 slots from the bootstrap config", w)
	}
	<-r.bootstrapDone

	out := logs.String()
	if !strings.Contains(out, "level=INFO") || !strings.Contains(out, "bootstrap") {
		t.Errorf("no INFO bootstrap log in:\n%s", out)
	}
	if strings.Contains(out, j.Code) || strings.Contains(out, worker.JoinPrefix) || strings.Contains(out, "level=ERROR") {
		t.Errorf("log leaks the join string or code, or holds an error:\n%s", out)
	}
}

func TestBootstrapRemintsANearlyExpiredCodeAndInvalidatesTheOld(t *testing.T) {
	joinFile := filepath.Join(t.TempDir(), "join")
	r, _ := newBootstrapRouter(t, loadTestConfig(t, bootstrapYAML(joinFile)), "")
	// A code counts as nearly expired 200 ms after it is minted, so the loop
	// re-mints about every 200 ms.
	r.bootstrapMargin = fleet.EnrollmentTTL - 200*time.Millisecond
	ctx := startWorkerSide(t, r)

	waitFor(t, "the join file", func() bool { return fileExists(joinFile) })
	first := readJoin(t, joinFile)
	waitFor(t, "a re-minted code", func() bool {
		b, err := os.ReadFile(joinFile)
		if err != nil {
			return false
		}
		j, err := worker.ParseJoin(string(b))
		return err == nil && j.Code != first.Code
	})
	if err := enroll(ctx, r, first); err == nil {
		t.Fatal("the replaced code still enrolls; the previous pending code must be invalidated")
	}
	// A code can be replaced between the read and the redeem; retry until one
	// lands.
	waitFor(t, "a re-minted code to enroll", func() bool {
		b, err := os.ReadFile(joinFile)
		if err != nil {
			return false
		}
		j, err := worker.ParseJoin(string(b))
		return err == nil && enroll(ctx, r, j) == nil
	})
}

func TestBootstrapKeepsACodeThatIsNotNearExpiry(t *testing.T) {
	joinFile := filepath.Join(t.TempDir(), "join")
	r, _ := newBootstrapRouter(t, loadTestConfig(t, bootstrapYAML(joinFile)), "")
	r.bootstrapMargin = time.Minute
	startWorkerSide(t, r)
	waitFor(t, "the join file", func() bool { return fileExists(joinFile) })
	first := readJoin(t, joinFile)
	time.Sleep(200 * time.Millisecond) // ~20 polls
	if got := readJoin(t, joinFile); got.Code != first.Code {
		t.Fatal("the code was re-minted long before it expired")
	}
}

// seedRegistry enrolls name directly in the registry under stateDir, as a
// previous router run would have, and returns the registry for further edits.
func seedRegistry(t *testing.T, stateDir, name string) *fleet.Registry {
	t.Helper()
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	reg, err := fleet.OpenRegistry(filepath.Join(stateDir, "workers.json"))
	if err != nil {
		t.Fatal(err)
	}
	b := fleet.Bounds{Classes: []runner.Class{{OS: "linux", Arch: "x64"}}, MaxSlots: 2}
	code, _, err := reg.AddEnrollment(name, b, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.Enroll(code, name, "m", time.Now()); err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestBootstrapDoesNothingForAnAlreadyEnrolledWorker(t *testing.T) {
	stateDir := t.TempDir()
	seedRegistry(t, stateDir, sibling)
	joinFile := filepath.Join(t.TempDir(), "join")
	r, logs := newBootstrapRouter(t, loadTestConfig(t, bootstrapYAML(joinFile)), stateDir)
	startWorkerSide(t, r)
	select {
	case <-r.bootstrapDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the bootstrap loop did not finish for an enrolled worker")
	}
	if fileExists(joinFile) {
		t.Fatal("a join file was written for a worker that is already enrolled")
	}
	if n := len(remoteWorkers(r)); n != 1 {
		t.Fatalf("registry has %d workers, want the 1 seeded", n)
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("unexpected error log:\n%s", logs.String())
	}
}

func TestBootstrapNeverReEnrollsARevokedName(t *testing.T) {
	stateDir := t.TempDir()
	reg := seedRegistry(t, stateDir, sibling)
	if _, err := reg.Revoke(sibling); err != nil {
		t.Fatal(err)
	}
	joinFile := filepath.Join(t.TempDir(), "join")
	r, logs := newBootstrapRouter(t, loadTestConfig(t, bootstrapYAML(joinFile)), stateDir)
	startWorkerSide(t, r)
	select {
	case <-r.bootstrapDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the bootstrap loop did not finish for a revoked worker")
	}
	if fileExists(joinFile) {
		t.Fatal("a join file was written for a revoked worker: revocation must stick")
	}
	w := findWorker(r.ListWorkers(), sibling)
	if !w.Revoked || len(remoteWorkers(r)) != 1 {
		t.Fatalf("workers = %+v, want only the revoked record", remoteWorkers(r))
	}
	if n := strings.Count(logs.String(), "revoked"); n != 1 {
		t.Errorf("logged \"revoked\" %d times, want exactly once:\n%s", n, logs.String())
	}
}

func TestBootstrapRefusesInvalidConfigAtStartup(t *testing.T) {
	file := filepath.Join(t.TempDir(), "join")
	block := func(name, classes, slots string) string {
		return fmt.Sprintf("  bootstrap:\n    name: %s\n    classes: %s\n    slots: %s\n    join_file: %q\n", name, classes, slots, file)
	}
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"bad worker name", workersYAML + "  advertise: 127.0.0.1:9200\n" + block("Bad_Name", "[linux-x64]", "1"), "name"},
		{"local is reserved", workersYAML + "  advertise: 127.0.0.1:9200\n" + block("local", "[linux-x64]", "1"), "name"},
		{"no classes", workersYAML + "  advertise: 127.0.0.1:9200\n" + block("w", "[]", "1"), "class"},
		{"unknown class", workersYAML + "  advertise: 127.0.0.1:9200\n" + block("w", "[plan9-x64]", "1"), "class"},
		{"zero slots", workersYAML + "  advertise: 127.0.0.1:9200\n" + block("w", "[linux-x64]", "0"), "slots"},
		{"slots over the cap", workersYAML + "  advertise: 127.0.0.1:9200\n  max_slots_per_worker: 4\n" + block("w", "[linux-x64]", "5"), "max_slots_per_worker"},
		{"no advertise address", workersYAML + block("w", "[linux-x64]", "1"), "advertise"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(loadTestConfig(t, tt.yaml), nil, nil, Options{StateDir: t.TempDir(), FleetTiming: fastTiming()})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), "workers.bootstrap") {
				t.Fatalf("New() error = %v, want a workers.bootstrap error mentioning %q", err, tt.wantErr)
			}
		})
	}
}

func TestBootstrapIsOffWithoutNameAndJoinFile(t *testing.T) {
	r := newTestRouter(t, loadTestConfig(t, workersYAML+"  bootstrap:\n    name: sibling\n"), "")
	if r.bootstrapDone != nil {
		t.Fatal("bootstrap is running with no join_file")
	}
	startWorkerSide(t, r)
	if w := findWorker(r.ListWorkers(), sibling); w.Name != "" {
		t.Fatalf("worker = %+v, want none", w)
	}
}

// remoteWorkers lists registry-backed workers, leaving out the local member.
func remoteWorkers(r *Router) []control.WorkerLiveStatus {
	var out []control.WorkerLiveStatus
	for _, w := range r.ListWorkers() {
		if !w.Local {
			out = append(out, w)
		}
	}
	return out
}

func waitDone(t *testing.T, r *Router, what string) {
	t.Helper()
	select {
	case <-r.bootstrapDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("the bootstrap loop did not end: %s", what)
	}
}

func loopEnded(r *Router) bool {
	select {
	case <-r.bootstrapDone:
		return true
	default:
		return false
	}
}

func runInBackground(r *Router, ctx context.Context) chan error {
	r.runOrchestrator = func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	return done
}

func TestBootstrapLoopIsJoinedWhenTheContextIsCancelled(t *testing.T) {
	joinFile := filepath.Join(t.TempDir(), "join")
	r, _ := newBootstrapRouter(t, loadTestConfig(t, bootstrapYAML(joinFile)), "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runInBackground(r, ctx)
	waitFor(t, "the join file", func() bool { return fileExists(joinFile) })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	if !loopEnded(r) {
		t.Fatal("Run returned while the bootstrap loop was still running")
	}
}

func TestBootstrapLoopIsJoinedWhenTheOrchestratorEndsFirst(t *testing.T) {
	joinFile := filepath.Join(t.TempDir(), "join")
	r, _ := newBootstrapRouter(t, loadTestConfig(t, bootstrapYAML(joinFile)), "")
	want := errors.New("orchestrator failed")
	r.runOrchestrator = func(ctx context.Context) error {
		for !fileExists(joinFile) {
			time.Sleep(5 * time.Millisecond)
		}
		return want
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // never cancelled before Run returns
	if err := r.Run(ctx); !errors.Is(err, want) {
		t.Fatalf("Run = %v, want the orchestrator's error", err)
	}
	if !loopEnded(r) {
		t.Fatal("Run returned while the bootstrap loop was still running")
	}
}

// enrollDirect enrolls name in the live registry, as a worker redeeming a code would.
func enrollDirect(t *testing.T, reg *fleet.Registry, name string) {
	t.Helper()
	b := fleet.Bounds{Classes: []runner.Class{{OS: "linux", Arch: "x64"}}, MaxSlots: 2}
	code, _, err := reg.AddEnrollment(name, b, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.Enroll(code, name, "m", time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestBootstrapTreatsAnEnrollmentRaceAsEnrolled(t *testing.T) {
	joinFile := filepath.Join(t.TempDir(), "join")
	r, logs := newBootstrapRouter(t, loadTestConfig(t, bootstrapYAML(joinFile)), "")
	// The worker enrolls (some other way) after the loop saw "pending" and
	// before it mints: the mint then fails with ErrNameTaken.
	r.bootstrapBeforeMint = func() { enrollDirect(t, r.registry, sibling) }
	startWorkerSide(t, r)
	waitDone(t, r, "the name was enrolled between the check and the mint")
	if fileExists(joinFile) {
		t.Fatal("a join file is left for an enrolled worker")
	}
	if out := logs.String(); strings.Contains(out, "level=WARN") || strings.Contains(out, "level=ERROR") {
		t.Errorf("benign race logged a warning:\n%s", out)
	}
}

func TestBootstrapStopsOnANameSegmentCollision(t *testing.T) {
	stateDir := t.TempDir()
	seedRegistry(t, stateDir, "un-raid") // same runner segment as "unraid"
	joinFile := filepath.Join(t.TempDir(), "join")
	r, logs := newBootstrapRouter(t, loadTestConfig(t, bootstrapYAML(joinFile)), stateDir)
	r.cfg.Workers.Bootstrap.Name = "unraid"
	plan, err := r.newBootstrapPlan()
	if err != nil {
		t.Fatal(err)
	}
	r.bootstrap = plan
	startWorkerSide(t, r)
	waitDone(t, r, "a colliding name")
	if fileExists(joinFile) {
		t.Fatal("a join file was written for a colliding name")
	}
	out := logs.String()
	if n := strings.Count(out, "un-raid"); n != 1 || !strings.Contains(out, "level=WARN") {
		t.Errorf("want one WARN naming the colliding worker \"un-raid\", got %d mentions:\n%s", n, out)
	}
}

func TestBootstrapGivesUpAndRemovesTheFile(t *testing.T) {
	joinFile := filepath.Join(t.TempDir(), "join")
	r, logs := newBootstrapRouter(t, loadTestConfig(t, bootstrapYAML(joinFile)), "")
	r.bootstrapGiveUp = 200 * time.Millisecond
	startWorkerSide(t, r)
	waitDone(t, r, "the sibling never enrolled")
	if fileExists(joinFile) {
		t.Fatal("the join file survives the give-up")
	}
	out := logs.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "gave up") || !strings.Contains(out, "restart") {
		t.Errorf("no WARN saying bootstrap gave up until the next restart:\n%s", out)
	}
}

func TestBootstrapRewritesTheFileWhenAnAdminReplacesThePendingCode(t *testing.T) {
	joinFile := filepath.Join(t.TempDir(), "join")
	r, logs := newBootstrapRouter(t, loadTestConfig(t, bootstrapYAML(joinFile)), "")
	ctx := startWorkerSide(t, r)
	waitFor(t, "the join file", func() bool { return fileExists(joinFile) })
	first := readJoin(t, joinFile)

	// `gso workers add sibling` replaces the pending code the file holds.
	res, err := r.AddWorker(ctx, control.WorkersAddParams{Name: sibling, Classes: []string{"linux-x64"}, Slots: 1})
	if err != nil {
		t.Fatal(err)
	}
	admin, _ := worker.ParseJoin(res.Join)
	waitFor(t, "the join file to be rewritten", func() bool {
		b, err := os.ReadFile(joinFile)
		if err != nil {
			return false
		}
		j, err := worker.ParseJoin(string(b))
		return err == nil && j.Code != first.Code && j.Code != admin.Code
	})
	if err := enroll(ctx, r, readJoin(t, joinFile)); err != nil {
		t.Fatalf("the rewritten code does not enroll: %v", err)
	}
	if out := logs.String(); !strings.Contains(out, "replaced") {
		t.Errorf("the replacement was not logged:\n%s", out)
	}
}

func TestBootstrapReportsAnUnwritableDirectoryOnceAndBacksOff(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a non-root POSIX user to make a directory unwritable")
	}
	dir := filepath.Join(t.TempDir(), "bootstrap")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	r, logs := newBootstrapRouter(t, loadTestConfig(t, bootstrapYAML(filepath.Join(dir, "join"))), "")
	r.bootstrapBackoff = time.Hour
	startWorkerSide(t, r)
	waitFor(t, "an ERROR log", func() bool { return strings.Contains(logs.String(), "level=ERROR") })
	time.Sleep(300 * time.Millisecond) // ~30 polls
	out := logs.String()
	if n := strings.Count(out, "level=ERROR"); n != 1 {
		t.Errorf("%d ERROR lines, want exactly 1:\n%s", n, out)
	}
	if !strings.Contains(out, dir) || !strings.Contains(out, "gso-bootstrap volume") {
		t.Errorf("the ERROR must name the directory and the fix (remove the gso-bootstrap volume and redeploy):\n%s", out)
	}
	if strings.Contains(out, "level=WARN") {
		t.Errorf("warned instead of backing off:\n%s", out)
	}
}

func TestBootstrapRefusesUnsafeJoinFilePaths(t *testing.T) {
	stateDir := t.TempDir()
	tests := []struct {
		name, path, wantErr string
	}{
		{"relative", "bootstrap/join", "absolute"},
		{"not clean", "/bootstrap/../bootstrap/join", "clean"},
		{"under the state dir", filepath.Join(stateDir, "workers.json"), "state"},
		{"the state dir tls key", filepath.Join(stateDir, "workers", "tls", "key.pem"), "state"},
		{"under /etc/gso", "/etc/gso/config.yaml", "/etc/gso"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(loadTestConfig(t, bootstrapYAML(tt.path)), nil, nil, Options{StateDir: stateDir, FleetTiming: fastTiming()})
			if err == nil || !strings.Contains(err.Error(), "workers.bootstrap.join_file") || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("New() error = %v, want a workers.bootstrap.join_file error mentioning %q", err, tt.wantErr)
			}
		})
	}
}
