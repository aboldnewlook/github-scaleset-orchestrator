package service_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/service"
)

// mockOrchState implements service.OrchestratorState for testing.
type mockOrchState struct {
	runners      map[string][]string
	maxRunners   int
	available    int
	cancelled    []string
	setMaxCalled int
	lastMaxCount int
}

func (m *mockOrchState) RunnersByRepo() map[string][]string {
	return m.runners
}

func (m *mockOrchState) MaxRunners() int {
	return m.maxRunners
}

func (m *mockOrchState) AvailableSlots() int {
	return m.available
}

func (m *mockOrchState) SetMaxRunners(count int) error {
	m.setMaxCalled++
	m.lastMaxCount = count
	m.maxRunners = count
	return nil
}

func (m *mockOrchState) CancelRunner(name string) bool {
	for _, runners := range m.runners {
		for _, r := range runners {
			if r == name {
				m.cancelled = append(m.cancelled, name)
				return true
			}
		}
	}
	return false
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestRuntimeLiveStatus(t *testing.T) {
	orch := &mockOrchState{
		runners: map[string][]string{
			"org/repo-a": {"gso-repo-a-abc12345", "gso-repo-a-def67890"},
			"org/repo-b": {"gso-repo-b-11111111"},
		},
		maxRunners: 4,
		available:  1,
	}

	rt := service.NewRuntime(orch, func() {}, testLogger())
	result, err := rt.LiveStatus(context.Background())
	if err != nil {
		t.Fatalf("LiveStatus failed: %v", err)
	}

	if result.MaxRunners != 4 {
		t.Fatalf("expected max_runners 4, got %d", result.MaxRunners)
	}
	if result.Available != 1 {
		t.Fatalf("expected available 1, got %d", result.Available)
	}
	if len(result.Repos) != 2 {
		t.Fatalf("expected 2 repos, got %d", len(result.Repos))
	}
}

func TestRuntimeRecycleRunner(t *testing.T) {
	tests := []struct {
		name      string
		runner    string
		wantErr   bool
		errSubstr string
	}{
		{
			name:   "existing runner",
			runner: "gso-repo-a-abc12345",
		},
		{
			name:      "nonexistent runner",
			runner:    "gso-nonexistent",
			wantErr:   true,
			errSubstr: "not found",
		},
		{
			name:      "empty name",
			runner:    "",
			wantErr:   true,
			errSubstr: "required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orch := &mockOrchState{
				runners: map[string][]string{
					"org/repo-a": {"gso-repo-a-abc12345"},
				},
			}
			rt := service.NewRuntime(orch, func() {}, testLogger())
			err := rt.RecycleRunner(context.Background(), tt.runner)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				if tt.errSubstr != "" && !contains(err.Error(), tt.errSubstr) {
					t.Fatalf("error %q should contain %q", err.Error(), tt.errSubstr)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestRuntimeSetMaxRunners(t *testing.T) {
	tests := []struct {
		name    string
		count   int
		wantErr bool
	}{
		{name: "valid count", count: 8},
		{name: "minimum count", count: 1},
		{name: "zero count", count: 0, wantErr: true},
		{name: "negative count", count: -1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orch := &mockOrchState{maxRunners: 4}
			rt := service.NewRuntime(orch, func() {}, testLogger())
			err := rt.SetMaxRunners(tt.count)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if orch.lastMaxCount != tt.count {
					t.Fatalf("expected max count %d, got %d", tt.count, orch.lastMaxCount)
				}
			}
		})
	}
}

func TestRuntimeShutdown(t *testing.T) {
	var called atomic.Bool
	cancelFunc := func() { called.Store(true) }

	orch := &mockOrchState{}
	rt := service.NewRuntime(orch, cancelFunc, testLogger())

	err := rt.Shutdown(context.Background())
	if err != nil {
		t.Fatalf("shutdown failed: %v", err)
	}
	if !called.Load() {
		t.Fatal("cancel function was not called")
	}
}

func TestRuntimeHandleRequest(t *testing.T) {
	orch := &mockOrchState{
		runners: map[string][]string{
			"org/repo-a": {"gso-repo-a-abc12345"},
		},
		maxRunners: 4,
		available:  3,
	}

	var cancelled atomic.Bool
	cancelFunc := func() { cancelled.Store(true) }
	rt := service.NewRuntime(orch, cancelFunc, testLogger())

	tests := []struct {
		name      string
		method    string
		params    any
		wantErr   bool
		checkFunc func(t *testing.T, resp control.Response)
	}{
		{
			name:   "live_status",
			method: control.MethodLiveStatus,
			checkFunc: func(t *testing.T, resp control.Response) {
				var result control.LiveStatusResult
				if err := json.Unmarshal(resp.Result, &result); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if result.MaxRunners != 4 {
					t.Fatalf("expected max_runners 4, got %d", result.MaxRunners)
				}
			},
		},
		{
			name:   "recycle_runner success",
			method: control.MethodRecycleRunner,
			params: control.RecycleRunnerParams{Name: "gso-repo-a-abc12345"},
		},
		{
			name:    "recycle_runner not found",
			method:  control.MethodRecycleRunner,
			params:  control.RecycleRunnerParams{Name: "nonexistent"},
			wantErr: true,
		},
		{
			name:   "set_max_runners",
			method: control.MethodSetMaxRunners,
			params: control.SetMaxRunnersParams{Count: 8},
		},
		{
			name:   "shutdown",
			method: control.MethodShutdown,
			checkFunc: func(t *testing.T, resp control.Response) {
				if !cancelled.Load() {
					t.Fatal("cancel was not called")
				}
			},
		},
		{
			name:    "unknown method",
			method:  "unknown",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := control.Request{Method: tt.method}
			if tt.params != nil {
				p, _ := json.Marshal(tt.params)
				req.Params = p
			}

			resp := rt.HandleRequest(context.Background(), req)

			if tt.wantErr {
				if resp.Error == "" {
					t.Fatal("expected error response")
				}
			} else {
				if resp.Error != "" {
					t.Fatalf("unexpected error: %s", resp.Error)
				}
			}

			if tt.checkFunc != nil {
				tt.checkFunc(t, resp)
			}
		})
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
