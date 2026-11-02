package worker_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	workerv1 "github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
	"github.com/aboldnewlook/github-scaleset-orchestrator/worker"
	"github.com/aboldnewlook/github-scaleset-orchestrator/worker/workertest"
)

var linuxX64 = runner.Class{OS: "linux", Arch: "x64"}

// fakeLauncher stands in for Docker. A runner "runs" until finish(name) or Kill.
type fakeLauncher struct {
	classes []runner.Class

	mu      sync.Mutex
	exits   map[string]chan error
	started []string
	jits    []string
	waits   map[string]int
	killed  []string
	keeps   []map[string]bool
}

func newFakeLauncher(classes ...runner.Class) *fakeLauncher {
	return &fakeLauncher{classes: classes, exits: make(map[string]chan error), waits: make(map[string]int)}
}

func (f *fakeLauncher) Kind() runner.ExecutorKind { return runner.ExecutorDocker }

func (f *fakeLauncher) Classes(context.Context) ([]runner.Class, error) { return f.classes, nil }

func (f *fakeLauncher) Start(_ context.Context, rec worker.RunnerRecord, jit, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, rec.Name)
	f.jits = append(f.jits, jit)
	f.exitLocked(rec.Name)
	return nil
}

func (f *fakeLauncher) Wait(ctx context.Context, rec worker.RunnerRecord) error {
	f.mu.Lock()
	f.waits[rec.Name]++
	ch := f.exitLocked(rec.Name)
	f.mu.Unlock()
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeLauncher) Kill(_ context.Context, rec worker.RunnerRecord) error {
	f.mu.Lock()
	f.killed = append(f.killed, rec.Name)
	f.mu.Unlock()
	f.finish(rec.Name, errors.New("runner container exited with status 137"))
	return nil
}

func (f *fakeLauncher) RemoveOrphans(_ context.Context, keep map[string]bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keeps = append(f.keeps, maps.Clone(keep))
	return nil
}

func (f *fakeLauncher) exitLocked(name string) chan error {
	if ch, ok := f.exits[name]; ok {
		return ch
	}
	ch := make(chan error, 1)
	f.exits[name] = ch
	return ch
}

// finish makes the runner's Wait return err.
func (f *fakeLauncher) finish(name string, err error) {
	f.mu.Lock()
	ch := f.exitLocked(name)
	f.mu.Unlock()
	select {
	case ch <- err:
	default:
	}
}

func (f *fakeLauncher) Started() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.started)
}

func (f *fakeLauncher) JITs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.jits)
}

func (f *fakeLauncher) Killed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.killed)
}

func (f *fakeLauncher) WaitCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.waits[name]
}

func (f *fakeLauncher) Keeps() []map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.keeps)
}

// lockedBuffer collects log output from several goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// harness is one enrolled worker "box" against a workertest.Router.
type harness struct {
	t      *testing.T
	router *workertest.Router
	dir    string
	cred   worker.CredentialStore
	state  worker.State
	launch *fakeLauncher
	logs   *lockedBuffer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	r := workertest.NewRouter(t)
	j, err := worker.ParseJoin(r.Join("box"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	credential, st, err := worker.Enroll(ctx, worker.EnrollOptions{Join: j, Name: "box", MachineID: "machine-1"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store := worker.FileCredentialStore(filepath.Join(dir, "credential"))
	if err := store.Save(credential); err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, router: r, dir: dir, cred: store, state: st, launch: newFakeLauncher(linuxX64), logs: &lockedBuffer{}}
}

func (h *harness) config(mutate ...func(*worker.Config)) worker.Config {
	h.t.Helper()
	records, err := worker.OpenRecordStore(filepath.Join(h.dir, "runners.json"))
	if err != nil {
		h.t.Fatal(err)
	}
	cfg := worker.Config{
		Name:         "box",
		MachineID:    "machine-1",
		Version:      "test",
		DialAddr:     h.state.Addr,
		Pin:          h.state.Pin,
		Credential:   h.cred,
		Records:      records,
		Launchers:    []worker.Launcher{h.launch},
		Slots:        2,
		Allowlist:    []string{"acme/*"},
		AcceptPublic: worker.PublicFollow,
		Logger:       slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		BackoffMin:   5 * time.Millisecond,
		BackoffMax:   20 * time.Millisecond,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	return cfg
}

// start runs a Client (Available) until stop is called or the test ends, and
// waits for it to connect. stop cancels Run and returns its error.
func (h *harness) start(cfg worker.Config) (*worker.Client, func() error) {
	h.t.Helper()
	c, err := worker.New(cfg)
	if err != nil {
		h.t.Fatalf("New() error: %v", err)
	}
	c.SetAvailability(worker.Available)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	var once sync.Once
	var runErr error
	stop := func() error {
		once.Do(func() {
			cancel()
			select {
			case runErr = <-done:
			case <-time.After(5 * time.Second):
				h.t.Errorf("Run did not return within 5s of cancel")
			}
		})
		return runErr
	}
	h.t.Cleanup(func() { _ = stop() })
	waitFor(h.t, "client connected", func() bool { return c.Status().Connected })
	return c, stop
}

func (h *harness) records() []worker.RunnerRecord {
	h.t.Helper()
	s, err := worker.OpenRecordStore(filepath.Join(h.dir, "runners.json"))
	if err != nil {
		h.t.Fatal(err)
	}
	return s.List()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func reportFor(r *workertest.Router, name string, phase workerv1.RunnerPhase) *workerv1.ReportRunnerRequest {
	for _, rep := range r.Reports() {
		if rep.GetRunnerName() == name && rep.GetPhase() == phase {
			return rep
		}
	}
	return nil
}
