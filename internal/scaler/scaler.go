package scaler

import (
	"context"
	"encoding/json"
	"errors"
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

// TryAcquire takes a slot if one is free and reports whether it did. Scale
// uses it so a full semaphore never blocks the listener.
func (s *Semaphore) TryAcquire() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current >= s.max {
		return false
	}
	s.current++
	return true
}

// JobAcquirer acquires JobAvailable requests. The message session
// implements it; the supervisor binds the current session with BindSession.
type JobAcquirer interface {
	AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error)
}

var errNoCapacity = errors.New("no free runner slot")

// Scaler implements listener.Scaler from actions/scaleset v0.5. There is one
// Scaler per repo. It outlives listener restarts, so in-flight runners keep
// their slots when the supervisor recreates a session.
type Scaler struct {
	repo        string
	client      jitConfigGenerator
	worker      runnerWorker
	sem         *Semaphore
	logger      *slog.Logger
	bus         *event.Bus // may be nil
	reconcileMu sync.Mutex // serialises reconcile between Scale and Reevaluate

	mu           sync.Mutex
	scaleSetID   int
	runners      map[string]context.CancelFunc // name -> cancel
	releases     map[string]func()             // name -> idempotent semaphore release
	spawnedAt    map[string]time.Time          // name -> runner creation time
	jobStartedAt map[string]time.Time          // name -> GitHub job start callback time
	lastDecision scaleDecision
	hasDecision  bool
	stats        scaleset.RunnerScaleSetStatistic // cached: v0.5 passes nil on idle polls
	hasStats     bool
	acquirer     JobAcquirer
	onAssigned   func(context.Context, *scaleset.JobAssigned)
	namer        func() string
	dedupe       *dedupe
}

type scaleDecision struct {
	desired   int
	existing  int
	available int
	created   int
}

func New(repo string, scaleSetID int, client jitConfigGenerator, worker runnerWorker, sem *Semaphore, logger *slog.Logger, bus *event.Bus) *Scaler {
	return &Scaler{
		repo:         repo,
		scaleSetID:   scaleSetID,
		client:       client,
		worker:       worker,
		sem:          sem,
		logger:       logger.With("repo", repo),
		bus:          bus,
		runners:      make(map[string]context.CancelFunc),
		releases:     make(map[string]func()),
		spawnedAt:    make(map[string]time.Time),
		jobStartedAt: make(map[string]time.Time),
		dedupe:       newDedupe(dedupeMaxEntries, dedupeTTL, time.Now),
	}
}

// Scale implements listener.Scaler (actions/scaleset v0.5). It is called once
// per poll with nil (an idle poll), the synthetic initial message, or a real
// message. The listener acks only after Scale returns nil and stops on any
// error, so Scale handles every failure itself and always returns nil. Every
// step is idempotent because a message can be redelivered (design spec §5.2,
// §7). Runner contexts derive from ctx, so the caller passes a context that
// outlives a single listener Run.
func (s *Scaler) Scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
	if msg != nil {
		if msg.Statistics != nil {
			s.mu.Lock()
			s.stats = *msg.Statistics
			s.hasStats = true
			s.mu.Unlock()
		}
		s.acquire(ctx, msg.JobAvailableMessages)
		for _, a := range msg.JobAssignedMessages {
			if a != nil {
				s.onJobAssigned(ctx, a)
			}
		}
		for _, st := range msg.JobStartedMessages {
			if st == nil || s.seen("started", st.JobID, st.RunnerName, "") {
				continue
			}
			s.onStarted(st)
		}
		for _, c := range msg.JobCompletedMessages {
			if c == nil || s.seen("completed", c.JobID, c.RunnerName, c.Result) {
				continue
			}
			if c.RunnerName == "" && c.Result == "canceled" {
				// GitHub took the job back from a runner that never claimed it
				// and will requeue it (v5/README.md:116). No runner exited.
				s.onReassigned(c)
				continue
			}
			s.onCompleted(c)
		}
	}
	s.reconcile(ctx)
	return nil
}

// Reevaluate runs placement against the cached statistics, e.g. when
// capacity changes between polls. ctx must outlive the runners it starts.
func (s *Scaler) Reevaluate(ctx context.Context) { s.reconcile(ctx) }

// BindSession sets the session used to acquire JobAvailable requests.
func (s *Scaler) BindSession(a JobAcquirer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acquirer = a
}

// SetScaleSetID changes the scale set new runners are minted for.
func (s *Scaler) SetScaleSetID(id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scaleSetID = id
}

// ScaleSetID returns the scale set new runners are minted for. B8a's
// fleet.GitHub implementation on the orchestrator reads it.
func (s *Scaler) ScaleSetID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scaleSetID
}

// Stats returns the cached statistics and whether any were received.
func (s *Scaler) Stats() (scaleset.RunnerScaleSetStatistic, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats, s.hasStats
}

// SetRunnerNamer replaces how new runners are named.
func (s *Scaler) SetRunnerNamer(f func() string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.namer = f
}

// OnAssigned registers a hook run for every JobAssigned message. The hook
// must never place runners (design spec §5.2).
func (s *Scaler) OnAssigned(f func(ctx context.Context, a *scaleset.JobAssigned)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onAssigned = f
}

// reconcile creates runners for assignments that have none, up to the free
// slots. desired is TotalAssignedJobs: on github.com acquisition happens on
// GitHub's side and TotalAcquiredJobs reads 0 (design spec §5.2, FEAS §1).
func (s *Scaler) reconcile(ctx context.Context) {
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()

	s.mu.Lock()
	desired := s.stats.TotalAssignedJobs
	existing := len(s.runners)
	s.mu.Unlock()
	available := s.sem.Available()
	toCreate := runnersToCreate(desired, existing, available)
	created := 0
	for range toCreate {
		if err := s.startRunner(ctx); err != nil {
			if !errors.Is(err, errNoCapacity) {
				s.logger.Error("failed to start runner", "error", err)
				s.publish(event.Event{
					Time: time.Now(), Type: event.EventError, Repo: s.repo,
					Payload: mustMarshal(map[string]any{"op": "start_runner", "error": err.Error()}),
				})
			}
			break
		}
		created++
	}

	decision := scaleDecision{desired, existing, available, created}
	s.mu.Lock()
	changed := !s.hasDecision || s.lastDecision != decision
	s.lastDecision = decision
	s.hasDecision = true
	s.mu.Unlock()
	if changed {
		s.logger.Info("scaling", "desired", desired, "existing", existing, "available", available, "created", created)
		s.publish(event.Event{
			Time: time.Now(),
			Type: event.EventScaleDecision,
			Repo: s.repo,
			Payload: mustMarshal(map[string]any{
				"desired": desired, "existing": existing,
				"available": available, "created": created,
			}),
		})
	}
}

// acquire passes JobAvailable requests to the session. github.com pushes
// JobAssigned instead and is not expected to send these (FEAS §1); acquiring
// keeps gso correct if it does, and the event makes it visible.
func (s *Scaler) acquire(ctx context.Context, available []*scaleset.JobAvailable) {
	ids := make([]int64, 0, len(available))
	for _, a := range available {
		if a != nil {
			ids = append(ids, a.RunnerRequestID)
		}
	}
	if len(ids) == 0 {
		return
	}
	s.mu.Lock()
	acquirer := s.acquirer
	s.mu.Unlock()
	payload := map[string]any{"count": len(ids), "acquired": 0}
	if acquirer == nil {
		s.logger.Warn("JobAvailable received with no session bound; not acquired", "count", len(ids))
	} else {
		acquired, err := acquirer.AcquireJobs(ctx, ids)
		if err != nil {
			s.logger.Error("acquiring available jobs", "count", len(ids), "error", err)
			payload["error"] = err.Error()
		} else {
			payload["acquired"] = len(acquired)
		}
	}
	s.publish(event.Event{Time: time.Now(), Type: event.EventJobAvailable, Repo: s.repo, Payload: mustMarshal(payload)})
}

func (s *Scaler) onJobAssigned(ctx context.Context, a *scaleset.JobAssigned) {
	s.publish(event.Event{
		Time: time.Now(), Type: event.EventJobAssigned, Repo: s.repo,
		Payload: mustMarshal(map[string]any{
			"job": a.JobDisplayName, "job_id": a.JobID, "workflow": a.WorkflowRunID,
			"event": a.EventName, "labels": a.RequestLabels,
		}),
	})
	s.mu.Lock()
	hook := s.onAssigned
	s.mu.Unlock()
	if hook != nil {
		hook(ctx, a)
	}
}

func (s *Scaler) onReassigned(c *scaleset.JobCompleted) {
	s.logger.Info("job reassigned by GitHub", "job", c.JobDisplayName, "job_id", c.JobID)
	s.publish(event.Event{
		Time: time.Now(), Type: event.EventJobReassigned, Repo: s.repo,
		Payload: mustMarshal(map[string]any{"job": c.JobDisplayName, "job_id": c.JobID, "workflow": c.WorkflowRunID}),
	})
}

// seen reports whether this job message was already handled.
func (s *Scaler) seen(kind, jobID, runnerName, result string) bool {
	s.mu.Lock()
	if s.dedupe == nil {
		s.dedupe = newDedupe(dedupeMaxEntries, dedupeTTL, time.Now)
	}
	d := s.dedupe
	s.mu.Unlock()
	return d.seen(kind + "|" + jobID + "|" + runnerName + "|" + result)
}

func (s *Scaler) runnerName() string {
	s.mu.Lock()
	f := s.namer
	s.mu.Unlock()
	if f != nil {
		return f()
	}
	// Runner names cannot contain '/': use only the repo short name.
	repoShort := s.repo
	if i := strings.LastIndex(s.repo, "/"); i >= 0 {
		repoShort = s.repo[i+1:]
	}
	return fmt.Sprintf("gso-%s-%s", repoShort, uuid.NewString()[:8])
}

func runnersToCreate(desired, existing, available int) int {
	return min(max(desired-existing, 0), max(available, 0))
}

// onStarted records the job start of a known runner (telemetry only).
func (s *Scaler) onStarted(jobInfo *scaleset.JobStarted) {
	now := time.Now()
	s.mu.Lock()
	spawnedAt, tracked := s.spawnedAt[jobInfo.RunnerName]
	if tracked {
		if s.jobStartedAt == nil {
			s.jobStartedAt = make(map[string]time.Time)
		}
		s.jobStartedAt[jobInfo.RunnerName] = now
	}
	s.mu.Unlock()
	payload := map[string]any{
		"job": jobInfo.JobDisplayName, "runner": jobInfo.RunnerName,
		"workflow": jobInfo.WorkflowRunID,
	}
	if tracked {
		payload["runner_startup_ms"] = now.Sub(spawnedAt).Milliseconds()
	}
	s.logger.Info("job started",
		"job", jobInfo.JobDisplayName,
		"runner", jobInfo.RunnerName,
		"workflow", jobInfo.WorkflowRunID,
	)

	s.publish(event.Event{
		Time:    now,
		Type:    event.EventJobStarted,
		Repo:    s.repo,
		Payload: mustMarshal(payload),
	})
}

// onCompleted ends a runner's job: it cancels the runner and frees its slot
// before the process exits, so the next assignment can use it at once. An
// unknown runner is a no-op apart from the event.
func (s *Scaler) onCompleted(jobInfo *scaleset.JobCompleted) {
	now := time.Now()
	s.mu.Lock()
	startedAt, started := s.jobStartedAt[jobInfo.RunnerName]
	delete(s.jobStartedAt, jobInfo.RunnerName)
	delete(s.spawnedAt, jobInfo.RunnerName)
	cancel := s.runners[jobInfo.RunnerName]
	release := s.releases[jobInfo.RunnerName]
	delete(s.runners, jobInfo.RunnerName)
	delete(s.releases, jobInfo.RunnerName)
	s.mu.Unlock()
	payload := map[string]any{
		"job": jobInfo.JobDisplayName, "runner": jobInfo.RunnerName,
		"result": jobInfo.Result, "workflow": jobInfo.WorkflowRunID,
	}
	if started {
		payload["job_duration_ms"] = now.Sub(startedAt).Milliseconds()
	}
	s.logger.Info("job completed",
		"job", jobInfo.JobDisplayName,
		"runner", jobInfo.RunnerName,
		"result", jobInfo.Result,
	)

	s.publish(event.Event{
		Time:    now,
		Type:    event.EventJobCompleted,
		Repo:    s.repo,
		Payload: mustMarshal(payload),
	})
	if cancel != nil {
		cancel()
	}
	// Scale reconciles right after this. Free the completed job's slot now
	// instead of waiting for its runner process to exit, which can leave the
	// next assigned job queued until a later poll.
	if release != nil {
		release()
	}
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
	name := s.runnerName()

	// Reserve the slot first: a JIT config is a live credential and must only
	// be minted for a runner that is certain to start (design spec F8, §5.2).
	// TryAcquire, not Acquire: Scale must never block the listener.
	if !s.sem.TryAcquire() {
		return errNoCapacity
	}
	s.mu.Lock()
	scaleSetID := s.scaleSetID
	s.mu.Unlock()
	jit, err := s.client.GenerateJitRunnerConfig(ctx,
		&scaleset.RunnerScaleSetJitRunnerSetting{Name: name},
		scaleSetID,
	)
	if err != nil {
		s.sem.Release()
		return fmt.Errorf("generating jit config: %w", err)
	}

	runnerCtx, cancel := context.WithCancel(ctx)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(s.sem.Release) }

	s.mu.Lock()
	s.runners[name] = cancel
	s.releases[name] = release
	if s.spawnedAt == nil {
		s.spawnedAt = make(map[string]time.Time)
	}
	spawnedAt := time.Now()
	s.spawnedAt[name] = spawnedAt
	s.mu.Unlock()

	s.publish(event.Event{
		Time: spawnedAt,
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
			delete(s.spawnedAt, name)
			delete(s.jobStartedAt, name)
			s.mu.Unlock()
		}()

		if err := s.worker.Run(runnerCtx, name, jit.EncodedJITConfig); err != nil {
			completedAt := time.Now()
			s.logger.Error("runner failed", "name", name, "error", err)
			s.publish(event.Event{
				Time: completedAt,
				Type: event.EventRunnerFailed,
				Repo: s.repo,
				Payload: mustMarshal(map[string]any{
					"name": name, "error": err.Error(),
					"runner_elapsed_ms": completedAt.Sub(spawnedAt).Milliseconds(),
				}),
			})
		} else {
			completedAt := time.Now()
			s.publish(event.Event{
				Time: completedAt,
				Type: event.EventRunnerCompleted,
				Repo: s.repo,
				Payload: mustMarshal(map[string]any{
					"name": name, "runner_elapsed_ms": completedAt.Sub(spawnedAt).Milliseconds(),
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
