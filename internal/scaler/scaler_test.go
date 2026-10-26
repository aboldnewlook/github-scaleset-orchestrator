package scaler

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/event"
	"github.com/actions/scaleset"
)

func TestNewSemaphore(t *testing.T) {
	tests := []struct {
		name string
		size int
	}{
		{"size 1", 1},
		{"size 4", 4},
		{"size 16", 16},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sem := NewSemaphore(tt.size)
			if sem.Available() != tt.size {
				t.Fatalf("Available() = %d, want %d", sem.Available(), tt.size)
			}
			if sem.Max() != tt.size {
				t.Fatalf("Max() = %d, want %d", sem.Max(), tt.size)
			}
		})
	}
}

func TestSemaphoreAcquireRelease(t *testing.T) {
	sem := NewSemaphore(2)

	if err := sem.Acquire(context.Background()); err != nil {
		t.Fatalf("Acquire() error: %v", err)
	}
	if sem.Available() != 1 {
		t.Fatalf("Available() = %d after 1 acquire, want 1", sem.Available())
	}

	if err := sem.Acquire(context.Background()); err != nil {
		t.Fatalf("Acquire() error: %v", err)
	}
	if sem.Available() != 0 {
		t.Fatalf("Available() = %d after 2 acquires, want 0", sem.Available())
	}

	sem.Release()
	if sem.Available() != 1 {
		t.Fatalf("Available() = %d after 1 release, want 1", sem.Available())
	}

	sem.Release()
	if sem.Available() != 2 {
		t.Fatalf("Available() = %d after 2 releases, want 2", sem.Available())
	}
}

func TestSemaphoreAcquireContextCancellation(t *testing.T) {
	sem := NewSemaphore(1)

	// Fill the semaphore.
	if err := sem.Acquire(context.Background()); err != nil {
		t.Fatalf("Acquire() error: %v", err)
	}

	// Try to acquire with a cancelled context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := sem.Acquire(ctx)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}

	// Release and verify we can acquire again.
	sem.Release()
	if err := sem.Acquire(context.Background()); err != nil {
		t.Fatalf("Acquire() after release error: %v", err)
	}
}

func TestSemaphoreAcquireContextTimeout(t *testing.T) {
	sem := NewSemaphore(1)

	// Fill the semaphore.
	if err := sem.Acquire(context.Background()); err != nil {
		t.Fatalf("Acquire() error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := sem.Acquire(ctx)
	if err == nil {
		t.Fatal("expected error from timed-out context")
	}
}

func TestSemaphoreResize(t *testing.T) {
	sem := NewSemaphore(2)

	if err := sem.Acquire(context.Background()); err != nil {
		t.Fatalf("Acquire() error: %v", err)
	}
	if err := sem.Acquire(context.Background()); err != nil {
		t.Fatalf("Acquire() error: %v", err)
	}

	// Semaphore full — resize to 4.
	sem.Resize(4)

	if sem.Available() != 2 {
		t.Fatalf("Available() = %d after resize to 4, want 2", sem.Available())
	}
	if sem.Max() != 4 {
		t.Fatalf("Max() = %d after resize, want 4", sem.Max())
	}

	// Should be able to acquire 2 more.
	if err := sem.Acquire(context.Background()); err != nil {
		t.Fatalf("Acquire() after resize error: %v", err)
	}
	if err := sem.Acquire(context.Background()); err != nil {
		t.Fatalf("Acquire() after resize error: %v", err)
	}
	if sem.Available() != 0 {
		t.Fatalf("Available() = %d, want 0", sem.Available())
	}
}

func TestSemaphoreConcurrency(t *testing.T) {
	sem := NewSemaphore(3)
	var wg sync.WaitGroup
	active := make(chan int, 100)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := sem.Acquire(context.Background()); err != nil {
				return
			}
			active <- 1
			time.Sleep(time.Millisecond)
			active <- -1
			sem.Release()
		}()
	}

	wg.Wait()
	close(active)

	// Verify we never exceeded capacity
	current := 0
	maxSeen := 0
	for delta := range active {
		current += delta
		if current > maxSeen {
			maxSeen = current
		}
	}

	if maxSeen > 3 {
		t.Fatalf("max concurrent = %d, want <= 3", maxSeen)
	}
}

func TestRunnersToCreate(t *testing.T) {
	tests := []struct {
		name      string
		desired   int
		existing  int
		available int
		want      int
	}{
		{"one job already has a runner", 1, 1, 3, 0},
		{"fill missing runners", 5, 3, 2, 2},
		{"respect global capacity", 5, 1, 1, 1},
		{"do not create after demand drops", 0, 1, 3, 0},
		{"do not create when over capacity", 5, 1, -1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runnersToCreate(tt.desired, tt.existing, tt.available); got != tt.want {
				t.Fatalf("runnersToCreate(%d, %d, %d) = %d, want %d", tt.desired, tt.existing, tt.available, got, tt.want)
			}
		})
	}
}

func TestScaleDecisionRecordsChangesWithoutRepeatingIdlePolls(t *testing.T) {
	bus := event.NewBus(nil)
	id, events := bus.Subscribe(8)
	defer bus.Unsubscribe(id)
	sem := NewSemaphore(4)
	if err := sem.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := &Scaler{
		repo:    "org/repo",
		sem:     sem,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		bus:     bus,
		runners: map[string]context.CancelFunc{"already-running": func() {}},
	}

	for range 2 {
		if got, err := s.HandleDesiredRunnerCount(context.Background(), 1); err != nil || got != 1 {
			t.Fatalf("HandleDesiredRunnerCount() = (%d, %v), want (1, nil)", got, err)
		}
	}
	select {
	case got := <-events:
		if got.Type != event.EventScaleDecision || got.Repo != "org/repo" {
			t.Fatalf("unexpected event: %+v", got)
		}
		var payload map[string]int
		if err := json.Unmarshal(got.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		for key, want := range map[string]int{"desired": 1, "existing": 1, "available": 3, "created": 0} {
			if payload[key] != want {
				t.Errorf("%s = %d, want %d", key, payload[key], want)
			}
		}
	default:
		t.Fatal("expected scale decision event")
	}
	select {
	case got := <-events:
		t.Fatalf("duplicate decision on unchanged poll: %+v", got)
	default:
	}
}

func TestJobCompletedReleasesCapacityBeforeNextScaleDecision(t *testing.T) {
	sem := NewSemaphore(1)
	if err := sem.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(sem.Release) }
	cancelled := false
	s := &Scaler{
		repo:     "org/repo",
		sem:      sem,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		runners:  map[string]context.CancelFunc{"done": func() { cancelled = true }},
		releases: map[string]func(){"done": release},
	}
	if err := s.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: "done"}); err != nil {
		t.Fatal(err)
	}
	if !cancelled {
		t.Fatal("completed runner was not cancelled")
	}
	if got := sem.Available(); got != 1 {
		t.Fatalf("available capacity = %d immediately after completion, want 1", got)
	}
	if got := runnersToCreate(1, len(s.Runners()), sem.Available()); got != 1 {
		t.Fatalf("next scale decision would create %d runners, want 1", got)
	}
	// The worker also releases on process exit; that must not free the slot twice.
	release()
	if got := sem.Available(); got != 1 {
		t.Fatalf("available capacity after worker exit = %d, want 1", got)
	}
}

func TestJobEventsIncludeMeasuredDurations(t *testing.T) {
	bus := event.NewBus(nil)
	id, events := bus.Subscribe(2)
	defer bus.Unsubscribe(id)
	s := &Scaler{
		repo: "org/repo", logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		bus: bus, spawnedAt: map[string]time.Time{"runner-1": time.Now().Add(-3 * time.Second)},
		runners: map[string]context.CancelFunc{"runner-1": func() {}},
	}
	if err := s.HandleJobStarted(context.Background(), &scaleset.JobStarted{RunnerName: "runner-1"}); err != nil {
		t.Fatal(err)
	}
	started := <-events
	var startedPayload map[string]any
	if err := json.Unmarshal(started.Payload, &startedPayload); err != nil {
		t.Fatal(err)
	}
	if got := startedPayload["runner_startup_ms"].(float64); got < 2900 || got > 4000 {
		t.Fatalf("runner startup = %v ms, want about 3000", got)
	}

	s.mu.Lock()
	s.jobStartedAt["runner-1"] = time.Now().Add(-2 * time.Second)
	s.mu.Unlock()
	if err := s.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: "runner-1"}); err != nil {
		t.Fatal(err)
	}
	completed := <-events
	var completedPayload map[string]any
	if err := json.Unmarshal(completed.Payload, &completedPayload); err != nil {
		t.Fatal(err)
	}
	if got := completedPayload["job_duration_ms"].(float64); got < 1900 || got > 3000 {
		t.Fatalf("job duration = %v ms, want about 2000", got)
	}
	if len(s.spawnedAt) != 0 || len(s.jobStartedAt) != 0 {
		t.Fatal("timing state retained after job completion")
	}
}

func TestUnknownJobEventsOmitUnmeasuredDurations(t *testing.T) {
	bus := event.NewBus(nil)
	id, events := bus.Subscribe(2)
	defer bus.Unsubscribe(id)
	s := &Scaler{repo: "org/repo", logger: slog.New(slog.NewTextHandler(io.Discard, nil)), bus: bus}
	if err := s.HandleJobStarted(context.Background(), &scaleset.JobStarted{RunnerName: "unknown"}); err != nil {
		t.Fatal(err)
	}
	if err := s.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: "unknown"}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"runner_startup_ms", "job_duration_ms"} {
		e := <-events
		var payload map[string]any
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if _, ok := payload[key]; ok {
			t.Fatalf("%s event has unmeasured %s", e.Type, key)
		}
	}
}

type workerFunc func(context.Context, string, string) error

func (f workerFunc) Run(ctx context.Context, name, jit string) error { return f(ctx, name, jit) }

func TestRunnerExitEventIncludesElapsedTime(t *testing.T) {
	bus := event.NewBus(nil)
	id, events := bus.Subscribe(4)
	defer bus.Unsubscribe(id)
	s := New("org/repo", 42, &fakeJITConfigGenerator{},
		workerFunc(func(context.Context, string, string) error {
			time.Sleep(20 * time.Millisecond)
			return nil
		}), NewSemaphore(1), slog.New(slog.NewTextHandler(io.Discard, nil)), bus)
	if _, err := s.HandleDesiredRunnerCount(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	timeout := time.After(time.Second)
	for {
		select {
		case e := <-events:
			if e.Type != event.EventRunnerCompleted {
				continue
			}
			var payload map[string]any
			if err := json.Unmarshal(e.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if got := payload["runner_elapsed_ms"].(float64); got < 20 {
				t.Fatalf("runner elapsed = %v ms, want at least 20", got)
			}
			return
		case <-timeout:
			t.Fatal("runner completion event not received")
		}
	}
}

type fakeJITConfigGenerator struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeJITConfigGenerator) GenerateJitRunnerConfig(_ context.Context, _ *scaleset.RunnerScaleSetJitRunnerSetting, _ int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return &scaleset.RunnerScaleSetJitRunnerConfig{EncodedJITConfig: "test"}, nil
}

func (f *fakeJITConfigGenerator) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type controlledWorker struct {
	mu            sync.Mutex
	calls         int
	started       chan string
	firstExitGate chan struct{}
}

func (w *controlledWorker) Run(ctx context.Context, name, _ string) error {
	w.mu.Lock()
	w.calls++
	first := w.calls == 1
	w.mu.Unlock()
	w.started <- name
	<-ctx.Done()
	if first {
		<-w.firstExitGate
	}
	return ctx.Err()
}

func TestCompletedRunnerHandsCapacityToNextAssignedJob(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &fakeJITConfigGenerator{}
	worker := &controlledWorker{
		started:       make(chan string, 2),
		firstExitGate: make(chan struct{}),
	}
	t.Cleanup(func() {
		cancel()
		close(worker.firstExitGate)
	})
	sem := NewSemaphore(1)
	s := New("org/repo", 42, client, worker, sem, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	if _, err := s.HandleDesiredRunnerCount(ctx, 1); err != nil {
		t.Fatal(err)
	}
	first := <-worker.started
	if _, err := s.HandleDesiredRunnerCount(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if got := client.Calls(); got != 1 {
		t.Fatalf("duplicate scale callback generated %d JIT configs, want 1", got)
	}

	if err := s.HandleJobCompleted(ctx, &scaleset.JobCompleted{RunnerName: first}); err != nil {
		t.Fatal(err)
	}
	// The first worker remains blocked in its exit path. The next assigned job
	// must still be able to take the slot immediately.
	if got := sem.Available(); got != 1 {
		t.Fatalf("capacity after completion = %d, want 1", got)
	}
	nextCtx, nextCancel := context.WithTimeout(ctx, time.Second)
	defer nextCancel()
	if _, err := s.HandleDesiredRunnerCount(nextCtx, 1); err != nil {
		t.Fatal(err)
	}
	select {
	case second := <-worker.started:
		if second == first {
			t.Fatal("replacement runner reused the completed runner name")
		}
	case <-nextCtx.Done():
		t.Fatal("replacement runner did not start while previous worker exited")
	}
	if got := client.Calls(); got != 2 {
		t.Fatalf("JIT configs generated = %d, want 2", got)
	}
}

func TestScalerRunners(t *testing.T) {
	s := &Scaler{
		runners: make(map[string]context.CancelFunc),
	}

	// Empty initially
	runners := s.Runners()
	if len(runners) != 0 {
		t.Fatalf("expected 0 runners, got %d", len(runners))
	}

	// Add some runners
	s.mu.Lock()
	s.runners["runner-1"] = func() {}
	s.runners["runner-2"] = func() {}
	s.mu.Unlock()

	runners = s.Runners()
	if len(runners) != 2 {
		t.Fatalf("expected 2 runners, got %d", len(runners))
	}

	// Verify the names are present (order doesn't matter)
	found := make(map[string]bool)
	for _, name := range runners {
		found[name] = true
	}
	if !found["runner-1"] || !found["runner-2"] {
		t.Fatalf("expected runner-1 and runner-2, got %v", runners)
	}
}

func TestScalerCancelRunner(t *testing.T) {
	cancelled := make(map[string]bool)
	s := &Scaler{
		runners: make(map[string]context.CancelFunc),
	}

	s.mu.Lock()
	s.runners["runner-a"] = func() { cancelled["runner-a"] = true }
	s.runners["runner-b"] = func() { cancelled["runner-b"] = true }
	s.mu.Unlock()

	tests := []struct {
		name     string
		runner   string
		wantOK   bool
		wantLeft int
	}{
		{"cancel existing runner", "runner-a", true, 1},
		{"cancel same runner again", "runner-a", false, 1},
		{"cancel nonexistent runner", "runner-z", false, 1},
		{"cancel last runner", "runner-b", true, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok := s.CancelRunner(tt.runner)
			if ok != tt.wantOK {
				t.Fatalf("CancelRunner(%q) = %v, want %v", tt.runner, ok, tt.wantOK)
			}
			if len(s.Runners()) != tt.wantLeft {
				t.Fatalf("remaining runners = %d, want %d", len(s.Runners()), tt.wantLeft)
			}
		})
	}

	// Verify cancel functions were called
	if !cancelled["runner-a"] {
		t.Fatal("runner-a cancel function was not called")
	}
	if !cancelled["runner-b"] {
		t.Fatal("runner-b cancel function was not called")
	}
}

func TestScalerPublish(t *testing.T) {
	bus := event.NewBus(nil)
	id, ch := bus.Subscribe(8)
	defer bus.Unsubscribe(id)

	s := &Scaler{
		repo:    "org/repo",
		bus:     bus,
		runners: make(map[string]context.CancelFunc),
	}

	e := event.Event{
		Time: time.Now(),
		Type: event.EventRunnerSpawned,
		Repo: "org/repo",
	}
	s.publish(e)

	select {
	case got := <-ch:
		if got.Type != event.EventRunnerSpawned {
			t.Fatalf("event type = %q, want %q", got.Type, event.EventRunnerSpawned)
		}
		if got.Repo != "org/repo" {
			t.Fatalf("event repo = %q, want %q", got.Repo, "org/repo")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestScalerPublish_NilBus(t *testing.T) {
	s := &Scaler{
		bus:     nil,
		runners: make(map[string]context.CancelFunc),
	}

	// Should not panic
	s.publish(event.Event{
		Time: time.Now(),
		Type: event.EventRunnerSpawned,
	})
}

func TestMustMarshal(t *testing.T) {
	tests := []struct {
		name  string
		input any
	}{
		{"simple map", map[string]any{"key": "value"}},
		{"nested", map[string]any{"a": map[string]any{"b": 1}}},
		{"nil", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mustMarshal(tt.input)
			if result == nil {
				t.Fatal("expected non-nil result")
			}

			// Verify it's valid JSON
			if !json.Valid(result) {
				t.Fatalf("result is not valid JSON: %s", result)
			}
		})
	}
}
