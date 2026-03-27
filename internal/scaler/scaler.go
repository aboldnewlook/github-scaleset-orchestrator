package scaler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/event"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/runner"
	"github.com/actions/scaleset"
	"github.com/google/uuid"
)

// Semaphore controls global concurrency across all repos.
type Semaphore chan struct{}

func NewSemaphore(size int) Semaphore {
	return make(chan struct{}, size)
}

func (s Semaphore) Acquire()       { s <- struct{}{} }
func (s Semaphore) Release()       { <-s }
func (s Semaphore) Available() int { return cap(s) - len(s) }

// Scaler implements the scaleset/listener.Scaler interface.
// One Scaler is created per repo.
type Scaler struct {
	repo       string
	scaleSetID int
	client     *scaleset.Client
	worker     *runner.Worker
	sem        Semaphore
	logger     *slog.Logger
	bus        *event.Bus // may be nil

	mu      sync.Mutex
	runners map[string]context.CancelFunc // name -> cancel
}

func New(repo string, scaleSetID int, client *scaleset.Client, worker *runner.Worker, sem Semaphore, logger *slog.Logger, bus *event.Bus) *Scaler {
	return &Scaler{
		repo:       repo,
		scaleSetID: scaleSetID,
		client:     client,
		worker:     worker,
		sem:        sem,
		logger:     logger.With("repo", repo),
		bus:        bus,
		runners:    make(map[string]context.CancelFunc),
	}
}

// HandleDesiredRunnerCount is called by the listener when GitHub has jobs
// that need runners. We spawn runners up to available capacity.
func (s *Scaler) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error) {
	available := s.sem.Available()
	toCreate := min(count, available)

	s.logger.Info("scaling", "desired", count, "available", available, "creating", toCreate)

	for range toCreate {
		if err := s.startRunner(ctx); err != nil {
			s.logger.Error("failed to start runner", "error", err)
			break
		}
	}

	s.mu.Lock()
	current := len(s.runners)
	s.mu.Unlock()

	return current, nil
}

// HandleJobStarted is called when a runner picks up a job.
func (s *Scaler) HandleJobStarted(ctx context.Context, jobInfo *scaleset.JobStarted) error {
	s.logger.Info("job started",
		"job", jobInfo.JobDisplayName,
		"runner", jobInfo.RunnerName,
		"workflow", jobInfo.WorkflowRunID,
	)

	s.publish(event.Event{
		Time: time.Now(),
		Type: event.EventJobStarted,
		Repo: s.repo,
		Payload: mustMarshal(map[string]any{
			"job":      jobInfo.JobDisplayName,
			"runner":   jobInfo.RunnerName,
			"workflow": jobInfo.WorkflowRunID,
		}),
	})

	return nil
}

// HandleJobCompleted is called when a runner finishes a job.
func (s *Scaler) HandleJobCompleted(ctx context.Context, jobInfo *scaleset.JobCompleted) error {
	s.logger.Info("job completed",
		"job", jobInfo.JobDisplayName,
		"runner", jobInfo.RunnerName,
		"result", jobInfo.Result,
	)

	s.publish(event.Event{
		Time: time.Now(),
		Type: event.EventJobCompleted,
		Repo: s.repo,
		Payload: mustMarshal(map[string]any{
			"job":    jobInfo.JobDisplayName,
			"runner": jobInfo.RunnerName,
			"result": jobInfo.Result,
		}),
	})

	s.mu.Lock()
	if cancel, ok := s.runners[jobInfo.RunnerName]; ok {
		cancel()
		delete(s.runners, jobInfo.RunnerName)
	}
	s.mu.Unlock()

	return nil
}

// Runners returns a snapshot of active runner names.
func (s *Scaler) Runners() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	names := make([]string, 0, len(s.runners))
	for name := range s.runners {
		names = append(names, name)
	}
	return names
}

// CancelRunner looks up a runner by name and calls its cancel function.
// Returns true if the runner was found and cancelled.
func (s *Scaler) CancelRunner(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if cancel, ok := s.runners[name]; ok {
		cancel()
		delete(s.runners, name)
		return true
	}
	return false
}

func (s *Scaler) startRunner(ctx context.Context) error {
	// Runner names cannot contain '/' — use only the repo short name
	repoShort := s.repo
	if i := strings.LastIndex(s.repo, "/"); i >= 0 {
		repoShort = s.repo[i+1:]
	}
	name := fmt.Sprintf("gso-%s-%s", repoShort, uuid.NewString()[:8])

	jit, err := s.client.GenerateJitRunnerConfig(ctx,
		&scaleset.RunnerScaleSetJitRunnerSetting{Name: name},
		s.scaleSetID,
	)
	if err != nil {
		return fmt.Errorf("generating jit config: %w", err)
	}

	s.sem.Acquire()

	runnerCtx, cancel := context.WithCancel(ctx)

	s.mu.Lock()
	s.runners[name] = cancel
	s.mu.Unlock()

	s.publish(event.Event{
		Time: time.Now(),
		Type: event.EventRunnerSpawned,
		Repo: s.repo,
		Payload: mustMarshal(map[string]any{
			"name": name,
		}),
	})

	go func() {
		defer s.sem.Release()
		defer cancel()
		defer func() {
			s.mu.Lock()
			delete(s.runners, name)
			s.mu.Unlock()
		}()

		if err := s.worker.Run(runnerCtx, name, jit.EncodedJITConfig); err != nil {
			s.logger.Error("runner failed", "name", name, "error", err)
			s.publish(event.Event{
				Time: time.Now(),
				Type: event.EventRunnerFailed,
				Repo: s.repo,
				Payload: mustMarshal(map[string]any{
					"name":  name,
					"error": err.Error(),
				}),
			})
		} else {
			s.publish(event.Event{
				Time: time.Now(),
				Type: event.EventRunnerCompleted,
				Repo: s.repo,
				Payload: mustMarshal(map[string]any{
					"name": name,
				}),
			})
		}
	}()

	return nil
}

// publish sends an event to the bus if configured.
func (s *Scaler) publish(e event.Event) {
	if s.bus != nil {
		s.bus.Publish(e)
	}
}

func mustMarshal(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}
