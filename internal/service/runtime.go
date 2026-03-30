package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/event"
)

// OrchestratorState defines what Runtime needs from the orchestrator.
type OrchestratorState interface {
	RunnersByRepo() map[string][]string
	MaxRunners() int
	AvailableSlots() int
	SetMaxRunners(count int) error
	CancelRunner(name string) bool
}

// Runtime wraps orchestrator live state and implements control.Handler.
type Runtime struct {
	orch       OrchestratorState
	store      *event.FileStore
	cancelFunc context.CancelFunc
	logger     *slog.Logger
}

// NewRuntime creates a new Runtime service.
func NewRuntime(orch OrchestratorState, cancelFunc context.CancelFunc, logger *slog.Logger, store ...*event.FileStore) *Runtime {
	r := &Runtime{
		orch:       orch,
		cancelFunc: cancelFunc,
		logger:     logger,
	}
	if len(store) > 0 {
		r.store = store[0]
	}
	return r
}

// HandleRequest dispatches a control request to the appropriate method.
func (r *Runtime) HandleRequest(ctx context.Context, req control.Request) control.Response {
	switch req.Method {
	case control.MethodLiveStatus:
		result, err := r.LiveStatus(ctx)
		if err != nil {
			return control.Response{Error: err.Error()}
		}
		data, _ := json.Marshal(result)
		return control.Response{Result: data}

	case control.MethodLiveEvents:
		var params control.LiveEventsParams
		if req.Params != nil {
			if err := json.Unmarshal(req.Params, &params); err != nil {
				return control.Response{Error: fmt.Sprintf("invalid params: %v", err)}
			}
		}
		events, err := r.LiveEvents(ctx, params.Since)
		if err != nil {
			return control.Response{Error: err.Error()}
		}
		data, _ := json.Marshal(events)
		return control.Response{Result: data}

	case control.MethodRecycleRunner:
		var params control.RecycleRunnerParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return control.Response{Error: fmt.Sprintf("invalid params: %v", err)}
		}
		if err := r.RecycleRunner(ctx, params.Name); err != nil {
			return control.Response{Error: err.Error()}
		}
		return control.Response{}

	case control.MethodSetMaxRunners:
		var params control.SetMaxRunnersParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return control.Response{Error: fmt.Sprintf("invalid params: %v", err)}
		}
		if err := r.SetMaxRunners(params.Count); err != nil {
			return control.Response{Error: err.Error()}
		}
		return control.Response{}

	case control.MethodShutdown:
		if err := r.Shutdown(ctx); err != nil {
			return control.Response{Error: err.Error()}
		}
		return control.Response{}

	default:
		return control.Response{Error: fmt.Sprintf("unknown method: %s", req.Method)}
	}
}

// LiveStatus returns the current state of all runners.
func (r *Runtime) LiveStatus(ctx context.Context) (*control.LiveStatusResult, error) {
	byRepo := r.orch.RunnersByRepo()
	repos := make([]control.RepoLiveStatus, 0, len(byRepo))
	for repo, runners := range byRepo {
		repos = append(repos, control.RepoLiveStatus{
			Repo:    repo,
			Runners: runners,
		})
	}

	return &control.LiveStatusResult{
		Repos:      repos,
		MaxRunners: r.orch.MaxRunners(),
		Available:  r.orch.AvailableSlots(),
	}, nil
}

// LiveEvents returns events since the given timestamp from the event store.
func (r *Runtime) LiveEvents(_ context.Context, since string) ([]event.Event, error) {
	if r.store == nil {
		return nil, nil
	}

	filter := event.StoreFilter{}
	if since != "" {
		t, err := time.Parse(time.RFC3339Nano, since)
		if err != nil {
			return nil, fmt.Errorf("invalid since timestamp: %w", err)
		}
		filter.Since = t
	}

	events, err := r.store.Query(filter)
	if err != nil {
		return nil, fmt.Errorf("querying events: %w", err)
	}
	return events, nil
}

// RecycleRunner cancels a runner by name so it will be replaced.
func (r *Runtime) RecycleRunner(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("runner name is required")
	}
	if !r.orch.CancelRunner(name) {
		return fmt.Errorf("runner %q not found", name)
	}
	r.logger.Info("recycled runner", "name", name)
	return nil
}

// SetMaxRunners updates the maximum number of concurrent runners.
func (r *Runtime) SetMaxRunners(count int) error {
	if count < 1 {
		return fmt.Errorf("max_runners must be at least 1")
	}
	if err := r.orch.SetMaxRunners(count); err != nil {
		return fmt.Errorf("setting max runners: %w", err)
	}
	r.logger.Info("max runners updated", "count", count)
	return nil
}

// Shutdown triggers a graceful shutdown of the daemon.
func (r *Runtime) Shutdown(ctx context.Context) error {
	r.logger.Info("shutdown requested via control socket")
	r.cancelFunc()
	return nil
}
