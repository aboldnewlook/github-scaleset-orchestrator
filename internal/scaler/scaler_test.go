package scaler

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/event"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/fleet"
	"github.com/actions/scaleset"
)

// track registers a fleet reservation as one of s's runners, as place does,
// without starting it. It returns the runner's name.
func track(t *testing.T, s *Scaler, fl *fleet.Fleet, cancel context.CancelFunc, spawnedAt time.Time) string {
	t.Helper()
	res, ok := fl.Reserve(testRequest())
	if !ok {
		t.Fatal("no slot to reserve")
	}
	t.Cleanup(res.Release)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runners[res.RunnerName()] = &tracked{res: res, cancel: cancel, spawnedAt: spawnedAt}
	return res.RunnerName()
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
	s, fl := newFleetScaler(t, 4, &fakeJITConfigGenerator{}, blockingExecutor())
	s.bus = bus
	track(t, s, fl, func() {}, time.Now()) // an already-running runner holds one slot

	s.stats = scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}
	for range 2 {
		if err := s.Scale(context.Background(), nil); err != nil || len(s.Runners()) != 1 {
			t.Fatalf("Scale(idle) = %v with %d runners, want nil with 1", err, len(s.Runners()))
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
	s, fl := newFleetScaler(t, 1, &fakeJITConfigGenerator{}, blockingExecutor())
	cancelled := false
	name := track(t, s, fl, func() { cancelled = true }, time.Now())
	s.mu.Lock()
	res := s.runners[name].res
	s.mu.Unlock()

	s.onCompleted(&scaleset.JobCompleted{RunnerName: name})
	if !cancelled {
		t.Fatal("completed runner was not cancelled")
	}
	if got := fl.Free(testRequest()); got != 1 {
		t.Fatalf("available capacity = %d immediately after completion, want 1", got)
	}
	if got := runnersToCreate(1, len(s.Runners()), fl.Free(testRequest())); got != 1 {
		t.Fatalf("next scale decision would create %d runners, want 1", got)
	}
	// The executor also releases on process exit; that must not free the slot twice.
	res.Release()
	if got := fl.Free(testRequest()); got != 1 {
		t.Fatalf("available capacity after worker exit = %d, want 1", got)
	}
}

func TestJobEventsIncludeMeasuredDurations(t *testing.T) {
	bus := event.NewBus(nil)
	id, events := bus.Subscribe(2)
	defer bus.Unsubscribe(id)
	s, fl := newFleetScaler(t, 1, &fakeJITConfigGenerator{}, blockingExecutor())
	s.bus = bus
	name := track(t, s, fl, func() {}, time.Now().Add(-3*time.Second))

	s.onStarted(&scaleset.JobStarted{RunnerName: name})
	started := <-events
	var startedPayload map[string]any
	if err := json.Unmarshal(started.Payload, &startedPayload); err != nil {
		t.Fatal(err)
	}
	if got := startedPayload["runner_startup_ms"].(float64); got < 2900 || got > 4000 {
		t.Fatalf("runner startup = %v ms, want about 3000", got)
	}

	s.mu.Lock()
	s.runners[name].startedAt = time.Now().Add(-2 * time.Second)
	s.mu.Unlock()
	s.onCompleted(&scaleset.JobCompleted{RunnerName: name})
	completed := <-events
	var completedPayload map[string]any
	if err := json.Unmarshal(completed.Payload, &completedPayload); err != nil {
		t.Fatal(err)
	}
	if got := completedPayload["job_duration_ms"].(float64); got < 1900 || got > 3000 {
		t.Fatalf("job duration = %v ms, want about 2000", got)
	}
	if len(s.Runners()) != 0 {
		t.Fatal("runner state retained after job completion")
	}
}

func TestUnknownJobEventsOmitUnmeasuredDurations(t *testing.T) {
	bus := event.NewBus(nil)
	id, events := bus.Subscribe(2)
	defer bus.Unsubscribe(id)
	s, _ := newFleetScaler(t, 1, &fakeJITConfigGenerator{}, blockingExecutor())
	s.bus = bus
	s.onStarted(&scaleset.JobStarted{RunnerName: "unknown"})
	s.onCompleted(&scaleset.JobCompleted{RunnerName: "unknown"})
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
	s, _ := newFleetScaler(t, 1, &fakeJITConfigGenerator{},
		workerFunc(func(context.Context, string, string) error {
			time.Sleep(20 * time.Millisecond)
			return nil
		}))
	s.bus = bus
	if err := s.Scale(context.Background(), assigned(1)); err != nil {
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
	mu          sync.Mutex
	calls       int
	names       []string
	scaleSetIDs []int
}

func (f *fakeJITConfigGenerator) GenerateJitRunnerConfig(_ context.Context, setting *scaleset.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	f.mu.Lock()
	f.calls++
	f.names = append(f.names, setting.Name)
	f.scaleSetIDs = append(f.scaleSetIDs, scaleSetID)
	f.mu.Unlock()
	return &scaleset.RunnerScaleSetJitRunnerConfig{EncodedJITConfig: "test"}, nil
}

func (f *fakeJITConfigGenerator) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// Names returns the runner names minted, in mint order.
func (f *fakeJITConfigGenerator) Names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.names...)
}

func (f *fakeJITConfigGenerator) ScaleSetIDs() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.scaleSetIDs...)
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
	s, fl := newFleetScaler(t, 1, client, worker)

	if err := s.Scale(ctx, assigned(1)); err != nil {
		t.Fatal(err)
	}
	first := <-worker.started
	if err := s.Scale(ctx, assigned(1)); err != nil {
		t.Fatal(err)
	}
	if got := client.Calls(); got != 1 {
		t.Fatalf("duplicate scale callback generated %d JIT configs, want 1", got)
	}

	done := &scaleset.RunnerScaleSetMessage{
		MessageID:  2,
		Statistics: &scaleset.RunnerScaleSetStatistic{},
		JobCompletedMessages: []*scaleset.JobCompleted{
			{RunnerName: first, Result: "succeeded", JobMessageBase: scaleset.JobMessageBase{JobID: "j1"}},
		},
	}
	if err := s.Scale(ctx, done); err != nil {
		t.Fatal(err)
	}
	// The first worker remains blocked in its exit path. The next assigned job
	// must still be able to take the slot immediately.
	if got := fl.Free(testRequest()); got != 1 {
		t.Fatalf("capacity after completion = %d, want 1", got)
	}
	nextCtx, nextCancel := context.WithTimeout(ctx, time.Second)
	defer nextCancel()
	if err := s.Scale(nextCtx, assigned(1)); err != nil {
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
	s, fl := newFleetScaler(t, 2, &fakeJITConfigGenerator{}, blockingExecutor())

	// Empty initially
	runners := s.Runners()
	if len(runners) != 0 {
		t.Fatalf("expected 0 runners, got %d", len(runners))
	}

	a := track(t, s, fl, func() {}, time.Now())
	b := track(t, s, fl, func() {}, time.Now())

	runners = s.Runners()
	if len(runners) != 2 {
		t.Fatalf("expected 2 runners, got %d", len(runners))
	}

	// Verify the names are present (order doesn't matter)
	found := make(map[string]bool)
	for _, name := range runners {
		found[name] = true
	}
	if !found[a] || !found[b] {
		t.Fatalf("expected %s and %s, got %v", a, b, runners)
	}
}

func TestScalerCancelRunner(t *testing.T) {
	cancelled := make(map[string]bool)
	s, fl := newFleetScaler(t, 2, &fakeJITConfigGenerator{}, blockingExecutor())
	var mu sync.Mutex
	a := track(t, s, fl, func() { mu.Lock(); cancelled["a"] = true; mu.Unlock() }, time.Now())
	b := track(t, s, fl, func() { mu.Lock(); cancelled["b"] = true; mu.Unlock() }, time.Now())

	tests := []struct {
		name     string
		runner   string
		wantOK   bool
		wantLeft int
	}{
		{"cancel existing runner", a, true, 1},
		{"cancel same runner again", a, false, 1},
		{"cancel nonexistent runner", "runner-z", false, 1},
		{"cancel last runner", b, true, 0},
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
	mu.Lock()
	defer mu.Unlock()
	if !cancelled["a"] || !cancelled["b"] {
		t.Fatalf("cancel functions called: %v", cancelled)
	}
}

func TestScalerPublish(t *testing.T) {
	bus := event.NewBus(nil)
	id, ch := bus.Subscribe(8)
	defer bus.Unsubscribe(id)

	s := &Scaler{
		repo:    "org/repo",
		bus:     bus,
		runners: make(map[string]*tracked),
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
		runners: make(map[string]*tracked),
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

type failingJITConfigGenerator struct{}

func (failingJITConfigGenerator) GenerateJitRunnerConfig(context.Context, *scaleset.RunnerScaleSetJitRunnerSetting, int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	return nil, errors.New("github: 500")
}
