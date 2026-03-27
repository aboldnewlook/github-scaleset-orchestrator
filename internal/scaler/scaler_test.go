package scaler

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/event"
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
			if cap(sem) != tt.size {
				t.Fatalf("cap = %d, want %d", cap(sem), tt.size)
			}
			if sem.Available() != tt.size {
				t.Fatalf("Available() = %d, want %d", sem.Available(), tt.size)
			}
		})
	}
}

func TestSemaphoreAcquireRelease(t *testing.T) {
	sem := NewSemaphore(2)

	sem.Acquire()
	if sem.Available() != 1 {
		t.Fatalf("Available() = %d after 1 acquire, want 1", sem.Available())
	}

	sem.Acquire()
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

func TestSemaphoreConcurrency(t *testing.T) {
	sem := NewSemaphore(3)
	var wg sync.WaitGroup
	active := make(chan int, 100)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem.Acquire()
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
