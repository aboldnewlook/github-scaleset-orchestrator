package orchestrator

import (
	"log/slog"
	"os"
	"runtime"
	"testing"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/event"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/scaler"
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

	o.sem.Acquire()
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

	if err := o.SetMaxRunners(8); err != nil {
		t.Fatalf("SetMaxRunners(8) error: %v", err)
	}
	if o.cfg.MaxRunners != 8 {
		t.Fatalf("MaxRunners = %d after SetMaxRunners(8), want 8", o.cfg.MaxRunners)
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
			got := repoShortName(tt.input)
			if got != tt.want {
				t.Fatalf("repoShortName(%q) = %q, want %q", tt.input, got, tt.want)
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
