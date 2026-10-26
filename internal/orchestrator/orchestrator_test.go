package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/event"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/naming"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/scaler"
	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestNew(t *testing.T) {
	cfg := &config.Config{
		MaxRunners: 4,
		Labels:     []string{"self-hosted"},
		Repos: []config.Repo{
			{Name: "org/repo"},
		},
	}

	o := New(cfg, testLogger(), nil)
	if o == nil {
		t.Fatal("New() returned nil")
	}
	if o.cfg != cfg {
		t.Fatal("cfg not set correctly")
	}
	if o.scalers == nil {
		t.Fatal("scalers map not initialized")
	}
}

func TestRunnersByRepo_Empty(t *testing.T) {
	cfg := &config.Config{MaxRunners: 2}
	o := New(cfg, testLogger(), nil)

	result := o.RunnersByRepo()
	if len(result) != 0 {
		t.Fatalf("expected empty map, got %d entries", len(result))
	}
}

func TestMaxRunners(t *testing.T) {
	cfg := &config.Config{MaxRunners: 8}
	o := New(cfg, testLogger(), nil)

	if o.MaxRunners() != 8 {
		t.Fatalf("MaxRunners() = %d, want 8", o.MaxRunners())
	}
}

func TestAvailableSlots_NilSemaphore(t *testing.T) {
	cfg := &config.Config{MaxRunners: 4}
	o := New(cfg, testLogger(), nil)

	// Before Run() is called, sem is nil
	if o.AvailableSlots() != 4 {
		t.Fatalf("AvailableSlots() = %d, want 4", o.AvailableSlots())
	}
}

func TestAvailableSlots_WithSemaphore(t *testing.T) {
	cfg := &config.Config{MaxRunners: 4}
	o := New(cfg, testLogger(), nil)
	o.sem = scaler.NewSemaphore(4)

	if o.AvailableSlots() != 4 {
		t.Fatalf("AvailableSlots() = %d, want 4", o.AvailableSlots())
	}

	if err := o.sem.Acquire(context.Background()); err != nil {
		t.Fatalf("Acquire() error: %v", err)
	}
	if o.AvailableSlots() != 3 {
		t.Fatalf("AvailableSlots() = %d after acquire, want 3", o.AvailableSlots())
	}

	o.sem.Release()
	if o.AvailableSlots() != 4 {
		t.Fatalf("AvailableSlots() = %d after release, want 4", o.AvailableSlots())
	}
}

func TestSetMaxRunners(t *testing.T) {
	cfg := &config.Config{MaxRunners: 4}
	o := New(cfg, testLogger(), nil)
	o.sem = scaler.NewSemaphore(4)

	if err := o.SetMaxRunners(8); err != nil {
		t.Fatalf("SetMaxRunners(8) error: %v", err)
	}
	if o.cfg.MaxRunners != 8 {
		t.Fatalf("MaxRunners = %d after SetMaxRunners(8), want 8", o.cfg.MaxRunners)
	}
	// Verify the semaphore was actually resized.
	if o.sem.Available() != 8 {
		t.Fatalf("sem.Available() = %d after SetMaxRunners(8), want 8", o.sem.Available())
	}
}

type recordingCapacitySetter struct {
	counts []int
}

func (r *recordingCapacitySetter) SetMaxRunners(count int) {
	r.counts = append(r.counts, count)
}

func TestSetMaxRunnersUpdatesActiveAndLateListeners(t *testing.T) {
	o := New(&config.Config{MaxRunners: 4}, testLogger(), nil)
	o.sem = scaler.NewSemaphore(4)
	first := &recordingCapacitySetter{}
	o.registerListener("org/first", nil, first)

	if err := o.SetMaxRunners(8); err != nil {
		t.Fatal(err)
	}
	second := &recordingCapacitySetter{}
	o.registerListener("org/second", nil, second)
	if err := o.SetMaxRunners(2); err != nil {
		t.Fatal(err)
	}
	if got := o.sem.Max(); got != 2 {
		t.Fatalf("semaphore max = %d, want 2", got)
	}
	if !reflect.DeepEqual(first.counts, []int{4, 8, 2}) {
		t.Fatalf("first listener capacities = %v, want [4 8 2]", first.counts)
	}
	if !reflect.DeepEqual(second.counts, []int{8, 2}) {
		t.Fatalf("late listener capacities = %v, want [8 2]", second.counts)
	}

	o.unregisterListener("org/first")
	if err := o.SetMaxRunners(3); err != nil {
		t.Fatal(err)
	}
	if len(first.counts) != 3 || second.counts[len(second.counts)-1] != 3 {
		t.Fatalf("unregistered listener changed or active listener missed update: first=%v second=%v", first.counts, second.counts)
	}
	if err := o.SetMaxRunners(0); err == nil {
		t.Fatal("expected invalid capacity to be rejected")
	}
	if got := o.MaxRunners(); got != 3 {
		t.Fatalf("capacity changed after invalid update: got %d, want 3", got)
	}
}

type pollingScaleSetClient struct {
	capacities chan int
	nextPoll   chan struct{}
}

func (p *pollingScaleSetClient) Session() scaleset.RunnerScaleSetSession {
	return scaleset.RunnerScaleSetSession{
		SessionID:  uuid.New(),
		Statistics: &scaleset.RunnerScaleSetStatistic{},
	}
}

func (p *pollingScaleSetClient) GetMessage(ctx context.Context, _, capacity int) (*scaleset.RunnerScaleSetMessage, error) {
	select {
	case p.capacities <- capacity:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-p.nextPoll:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (*pollingScaleSetClient) DeleteMessage(context.Context, int) error { return nil }

type noOpScaleSetScaler struct{}

func (noOpScaleSetScaler) HandleDesiredRunnerCount(context.Context, int) (int, error) {
	return 0, nil
}
func (noOpScaleSetScaler) HandleJobStarted(context.Context, *scaleset.JobStarted) error {
	return nil
}
func (noOpScaleSetScaler) HandleJobCompleted(context.Context, *scaleset.JobCompleted) error {
	return nil
}

func TestSetMaxRunnersChangesAdvertisedCapacityOnNextPoll(t *testing.T) {
	client := &pollingScaleSetClient{
		capacities: make(chan int, 3),
		nextPoll:   make(chan struct{}),
	}
	l, err := listener.New(client, listener.Config{ScaleSetID: 42, MaxRunners: 4, Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	o := New(&config.Config{MaxRunners: 4}, testLogger(), nil)
	o.sem = scaler.NewSemaphore(4)
	o.registerListener("org/repo", nil, l)
	defer o.unregisterListener("org/repo")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx, noOpScaleSetScaler{}) }()

	wantCapacity := func(want int) {
		t.Helper()
		select {
		case got := <-client.capacities:
			if got != want {
				t.Fatalf("advertised capacity = %d, want %d", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("listener did not poll with capacity %d", want)
		}
	}
	wantCapacity(4)
	if err := o.SetMaxRunners(7); err != nil {
		t.Fatal(err)
	}
	client.nextPoll <- struct{}{}
	wantCapacity(7)
	if err := o.SetMaxRunners(2); err != nil {
		t.Fatal(err)
	}
	client.nextPoll <- struct{}{}
	wantCapacity(2)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("listener exited with %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("listener did not exit after cancellation")
	}
}

func TestCancelRunner_NoScalers(t *testing.T) {
	cfg := &config.Config{MaxRunners: 4}
	o := New(cfg, testLogger(), nil)

	if o.CancelRunner("nonexistent") {
		t.Fatal("CancelRunner should return false when no scalers exist")
	}
}

func TestScalers_ReturnsCopy(t *testing.T) {
	cfg := &config.Config{MaxRunners: 4}
	o := New(cfg, testLogger(), nil)

	scalers := o.Scalers()
	if scalers == nil {
		t.Fatal("Scalers() returned nil")
	}
	if len(scalers) != 0 {
		t.Fatalf("expected empty scalers map, got %d", len(scalers))
	}

	// Adding to the returned map should not affect the orchestrator
	scalers["fake"] = nil
	if len(o.Scalers()) != 0 {
		t.Fatal("modification of returned map affected orchestrator")
	}
}

func TestSemaphore(t *testing.T) {
	cfg := &config.Config{MaxRunners: 4}
	o := New(cfg, testLogger(), nil)

	if o.Semaphore() != nil {
		t.Fatal("Semaphore() should be nil before Run()")
	}

	o.sem = scaler.NewSemaphore(4)
	if o.Semaphore() == nil {
		t.Fatal("Semaphore() should not be nil after setting sem")
	}
}

func TestPublish_WithBus(t *testing.T) {
	bus := event.NewBus(nil)
	id, ch := bus.Subscribe(8)
	defer bus.Unsubscribe(id)

	cfg := &config.Config{MaxRunners: 4}
	o := New(cfg, testLogger(), bus)

	o.publish(event.Event{Type: event.EventDaemonStarted})

	select {
	case got := <-ch:
		if got.Type != event.EventDaemonStarted {
			t.Fatalf("event type = %q, want %q", got.Type, event.EventDaemonStarted)
		}
	default:
		t.Fatal("expected event on bus")
	}
}

func TestPublish_NilBus(t *testing.T) {
	cfg := &config.Config{MaxRunners: 4}
	o := New(cfg, testLogger(), nil)

	// Should not panic
	o.publish(event.Event{Type: event.EventDaemonStarted})
}

func TestRepoShortName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"org/repo", "repo"},
		{"my-org/my-repo", "my-repo"},
		{"single", "single"},
		{"a/b/c", "a/b/c"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := naming.RepoShortName(tt.input)
			if got != tt.want {
				t.Fatalf("RepoShortName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestOsLabel(t *testing.T) {
	label := osLabel()
	switch runtime.GOOS {
	case "darwin":
		if label != "macOS" {
			t.Fatalf("osLabel() = %q, want macOS", label)
		}
	case "linux":
		if label != "Linux" {
			t.Fatalf("osLabel() = %q, want Linux", label)
		}
	case "windows":
		if label != "Windows" {
			t.Fatalf("osLabel() = %q, want Windows", label)
		}
	default:
		if label != runtime.GOOS {
			t.Fatalf("osLabel() = %q, want %q", label, runtime.GOOS)
		}
	}
}

func TestArchLabel(t *testing.T) {
	label := archLabel()
	switch runtime.GOARCH {
	case "amd64":
		if label != "X64" {
			t.Fatalf("archLabel() = %q, want X64", label)
		}
	case "arm64":
		if label != "ARM64" {
			t.Fatalf("archLabel() = %q, want ARM64", label)
		}
	default:
		if label != runtime.GOARCH {
			t.Fatalf("archLabel() = %q, want %q", label, runtime.GOARCH)
		}
	}
}

func TestMustMarshal(t *testing.T) {
	tests := []struct {
		name  string
		input any
		want  string
	}{
		{"simple", map[string]any{"key": "val"}, `{"key":"val"}`},
		{"number", map[string]any{"n": 42}, `{"n":42}`},
		{"null", nil, `null`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(mustMarshal(tt.input))
			if got != tt.want {
				t.Fatalf("mustMarshal() = %q, want %q", got, tt.want)
			}
		})
	}
}
