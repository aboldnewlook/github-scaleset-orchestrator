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
	"github.com/actions/scaleset"
	"github.com/google/uuid"
)

type jitConfigGenerator interface {
	GenerateJitRunnerConfig(context.Context, *scaleset.RunnerScaleSetJitRunnerSetting, int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
}

type runnerWorker interface {
	Run(context.Context, string, string) error
}

// Semaphore controls global concurrency across all repos.
// It supports context-aware Acquire and runtime Resize.
type Semaphore struct {
	mu      sync.Mutex
	cond    *sync.Cond
	max     int
	current int
}

func NewSemaphore(size int) *Semaphore {
	s := &Semaphore{max: size}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// Acquire blocks until a slot is available or ctx is cancelled.
func (s *Semaphore) Acquire(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Watch for context cancellation in a goroutine to wake us up.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			s.cond.Broadcast()
		case <-done:
		}
	}()

	for s.current >= s.max {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.cond.Wait()
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	s.current++
	return nil
}

// Release frees a semaphore slot.
func (s *Semaphore) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current--
	s.cond.Signal()
}

// Available returns the number of free slots.
func (s *Semaphore) Available() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.max - s.current
}

// Resize changes the maximum capacity of the semaphore at runtime.
func (s *Semaphore) Resize(newMax int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.max = newMax
	// Wake all waiters so they can re-check against the new max.
	s.cond.Broadcast()
}

// Max returns the current maximum capacity.
func (s *Semaphore) Max() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.max
}

// Scaler implements the scaleset/listener.Scaler interface.
// One Scaler is created per repo.
type Scaler struct {
	repo       string
	scaleSetID int
	client     jitConfigGenerator
	worker     runnerWorker
	sem        *Semaphore
	logger     *slog.Logger
	bus        *event.Bus // may be nil

	mu           sync.Mutex
	runners      map[string]context.CancelFunc // name -> cancel
	releases     map[string]func()             // name -> idempotent semaphore release
	lastDecision scaleDecision
	hasDecision  bool
}

type scaleDecision struct {
	desired   int
	existing  int
	available int
	created   int
}

func New(repo string, scaleSetID int, client jitConfigGenerator, worker runnerWorker, sem *Semaphore, logger *slog.Logger, bus *event.Bus) *Scaler {
	return &Scaler{
		repo:       repo,
		scaleSetID: scaleSetID,
		client:     client,
		worker:     worker,
		sem:        sem,
		logger:     logger.With("repo", repo),
		bus:        bus,
		runners:    make(map[string]context.CancelFunc),
		releases:   make(map[string]func()),
	}
}

// HandleDesiredRunnerCount is called by the listener when GitHub has jobs
// that need runners. We spawn runners up to available capacity.
func (s *Scaler) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error) {
	s.mu.Lock()
	existing := len(s.runners)
	s.mu.Unlock()
	available := s.sem.Available()
	// TotalAssignedJobs includes jobs already running on this scale set.
	// Only create runners for assignments that do not have one yet.
	toCreate := runnersToCreate(count, existing, available)
	created := 0
	for range toCreate {
		if err := s.startRunner(ctx); err != nil {
			s.logger.Error("failed to start runner", "error", err)
			break
		}
		created++
	}

	decision := scaleDecision{count, existing, available, created}
	s.mu.Lock()
	changed := !s.hasDecision || s.lastDecision != decision
	s.lastDecision = decision
	s.hasDecision = true
	s.mu.Unlock()
	if changed {
		s.logger.Info("scaling", "desired", count, "existing", existing, "available", available, "created", created)
		s.publish(event.Event{
			Time: time.Now(),
			Type: event.EventScaleDecision,
			Repo: s.repo,
			Payload: mustMarshal(map[string]any{
				"desired": count, "existing": existing,
				"available": available, "created": created,
			}),
		})
	}

	s.mu.Lock()
	current := len(s.runners)
	s.mu.Unlock()

	return current, nil
}

func runnersToCreate(desired, existing, available int) int {
	return min(max(desired-existing, 0), max(available, 0))
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
			"job":      jobInfo.JobDisplayName,
			"runner":   jobInfo.RunnerName,
			"result":   jobInfo.Result,
			"workflow": jobInfo.WorkflowRunID,
		}),
	})

	s.mu.Lock()
	cancel := s.runners[jobInfo.RunnerName]
	release := s.releases[jobInfo.RunnerName]
	delete(s.runners, jobInfo.RunnerName)
	delete(s.releases, jobInfo.RunnerName)
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	// The listener calls HandleDesiredRunnerCount immediately after this callback.
	// Free the completed job's slot now instead of waiting for its runner process
	// to exit, which can leave the next assigned job queued until a later poll.
	if release != nil {
		release()
	}

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

	if err := s.sem.Acquire(ctx); err != nil {
		return fmt.Errorf("acquiring semaphore: %w", err)
	}

	runnerCtx, cancel := context.WithCancel(ctx)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(s.sem.Release) }

	s.mu.Lock()
	s.runners[name] = cancel
	s.releases[name] = release
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
		defer release()
		defer cancel()
		defer func() {
			s.mu.Lock()
			delete(s.runners, name)
			delete(s.releases, name)
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
