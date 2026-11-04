package router

import (
	"bytes"
	"context"
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
