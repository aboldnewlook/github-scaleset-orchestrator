package orchestrator

import (
	"log/slog"
	"os"
	"testing"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/event"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/naming"
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
