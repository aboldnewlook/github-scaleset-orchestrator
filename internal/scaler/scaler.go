// Package scaler turns GitHub scale-set messages into runner placements on
// the fleet. One Scaler serves one (repo x class) scale set (design spec §5.2).
package scaler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/event"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/fleet"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/naming"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
	"github.com/actions/scaleset"
)

// JITGenerator mints JIT runner configurations for a scale set.
type JITGenerator interface {
	GenerateJitRunnerConfig(context.Context, *scaleset.RunnerScaleSetJitRunnerSetting, int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
}

// JobAcquirer acquires JobAvailable requests. The message session
// implements it; the supervisor binds the current session with BindSession.
type JobAcquirer interface {
	AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error)
}

// Placer reserves runner capacity on the fleet. *fleet.Fleet implements it.
type Placer interface {
	Free(req fleet.PlacementRequest) int
	Slots(req fleet.PlacementRequest) int
	Reserve(req fleet.PlacementRequest) (*fleet.Reservation, bool)
}

// CapacitySetter is the listener's advertised-capacity control.
type CapacitySetter interface{ SetMaxRunners(int) }

// Config configures a Scaler.
type Config struct {
	Repo       string
	Class      runner.Class
	ScaleSetID int
	Client     JITGenerator
	Placer     Placer
	Cache      string // "none" | "repo"
	Logger     *slog.Logger
	Bus        *event.Bus // may be nil
}

// tracked is one runner this scale set owns.
type tracked struct {
	res       *fleet.Reservation
	cancel    context.CancelFunc
	spawnedAt time.Time
	startedAt time.Time // zero until JobStarted
}

// Scaler implements listener.Scaler from actions/scaleset v0.5. There is one
// Scaler per (repo x class). It outlives listener restarts, so in-flight
// runners keep their slots when the supervisor recreates a session.
type Scaler struct {
	repo        string
	class       runner.Class
	client      JITGenerator
	placer      Placer
	cache       string
	logger      *slog.Logger
	bus         *event.Bus // may be nil
	reconcileMu sync.Mutex // serialises reconcile between Scale and Reevaluate

	mu           sync.Mutex
	scaleSetID   int
	runners      map[string]*tracked
	lastDecision scaleDecision
	hasDecision  bool
	stats        scaleset.RunnerScaleSetStatistic // cached: v0.5 passes nil on idle polls
	hasStats     bool
	acquirer     JobAcquirer
	onAssigned   func(context.Context, *scaleset.JobAssigned)
	sink         CapacitySetter
	dedupe       *dedupe
}

type scaleDecision struct {
	desired   int
	existing  int
	available int
	created   int
}

// New creates the scaler for one (repo x class) scale set.
func New(cfg Config) *Scaler {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	cache := cfg.Cache
	if cache == "" {
		cache = "none"
	}
	return &Scaler{
		repo:       cfg.Repo,
		class:      cfg.Class,
		scaleSetID: cfg.ScaleSetID,
		client:     cfg.Client,
		placer:     cfg.Placer,
		cache:      cache,
		logger:     logger.With("repo", cfg.Repo),
		bus:        cfg.Bus,
		runners:    make(map[string]*tracked),
		dedupe:     newDedupe(dedupeMaxEntries, dedupeTTL, time.Now),
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

// ScaleSetID returns the scale set new runners are minted for (0 until known).
// The orchestrator's fleet.GitHub implementation reads it.
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

// OnAssigned registers a hook run for every JobAssigned message. The hook
// must never place runners (design spec §5.2).
func (s *Scaler) OnAssigned(f func(ctx context.Context, a *scaleset.JobAssigned)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onAssigned = f
}

// SetCapacitySink attaches the listener; Capacity is pushed to it now and
// after every reconcile.
func (s *Scaler) SetCapacitySink(c CapacitySetter) {
	s.mu.Lock()
	s.sink = c
	s.mu.Unlock()
	s.pushCapacity()
}

// Capacity is the advertised capacity: the slots of every eligible member (the
// P2a value; C4 changes it to running + free).
func (s *Scaler) Capacity() int { return s.placer.Slots(s.request()) }

// Adopt tracks a runner the fleet verified after a router restart. Nothing is
// minted; the reservation is released by JobCompleted or the worker's EXITED.
func (s *Scaler) Adopt(res *fleet.Reservation) bool {
	if res == nil || res.Repo() != s.repo || res.Class() != s.class {
		return false
	}
	name := res.RunnerName()
	ctx, cancel := context.WithCancel(context.Background())
	t := &tracked{res: res, cancel: cancel, spawnedAt: time.Now()}
	s.mu.Lock()
	if _, exists := s.runners[name]; exists {
		s.mu.Unlock()
		cancel()
		return true
	}
	s.runners[name] = t
	s.mu.Unlock()
	s.logger.Info("adopted runner", "name", name, "member", res.Member())
	go s.run(ctx, t, nil)
	return true
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

// CancelRunner recycles a runner: a Cancel directive for remote runners, a
// cancelled context (container removed) for local ones. Returns true if the
// runner was found.
func (s *Scaler) CancelRunner(name string) bool {
	s.mu.Lock()
	t, ok := s.runners[name]
	if ok {
		delete(s.runners, name)
	}
	s.mu.Unlock()
	if !ok {
		return false
	}
	t.res.Cancel("recycled")
	t.cancel()
	return true
}

func (s *Scaler) request() fleet.PlacementRequest {
	return fleet.PlacementRequest{Repo: s.repo, Class: s.class, Cache: s.cache}
}

// reconcile creates runners for assignments that have none, up to the free
// slots. desired is TotalAssignedJobs: on github.com acquisition happens on
// GitHub's side and TotalAcquiredJobs reads 0 (design spec §5.2, FEAS §1).
func (s *Scaler) reconcile(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()

	s.mu.Lock()
	desired := s.stats.TotalAssignedJobs
	existing := len(s.runners)
	s.mu.Unlock()

	req := s.request()
	available := s.placer.Free(req)
	created := 0
	for range runnersToCreate(desired, existing, available) {
		if !s.place(ctx, req) {
			break
		}
		created++
	}
	s.recordDecision(scaleDecision{desired, existing, available, created})
	s.pushCapacity()
}

func runnersToCreate(desired, existing, available int) int {
	return min(max(desired-existing, 0), max(available, 0))
}

// place reserves one slot and starts the runner. Local runners mint here,
// synchronously, exactly as P2a did (golden parity, spec F8): the slot is
// reserved first and a failed mint releases it. Remote runners mint when the
// worker accepts the offer.
func (s *Scaler) place(ctx context.Context, req fleet.PlacementRequest) bool {
	res, ok := s.placer.Reserve(req)
	if !ok {
		return false
	}
	name := res.RunnerName()
	mint := fleet.MintFunc(s.mint)
	if res.Member() == naming.LocalWorkerSegment {
		runnerID, jit, err := s.mint(ctx, name)
		if err != nil {
			res.Release()
			s.logger.Error("failed to start runner", "error", err)
			s.publish(event.Event{
				Time: time.Now(), Type: event.EventError, Repo: s.repo,
				Payload: mustMarshal(map[string]any{"op": "start_runner", "error": err.Error()}),
			})
			return false
		}
		mint = func(context.Context, string) (int64, string, error) { return runnerID, jit, nil }
	}

	runnerCtx, cancel := context.WithCancel(ctx)
	t := &tracked{res: res, cancel: cancel, spawnedAt: time.Now()}
	s.mu.Lock()
	s.runners[name] = t
	s.mu.Unlock()
	s.publish(event.Event{
		Time: t.spawnedAt, Type: event.EventRunnerSpawned, Repo: s.repo,
		Payload: mustMarshal(map[string]any{"name": name, "member": res.Member()}),
	})
	go s.run(runnerCtx, t, mint)
	return true
}

// mint generates a runner's JIT config. The config is never logged.
func (s *Scaler) mint(ctx context.Context, runnerName string) (int64, string, error) {
	s.mu.Lock()
	id := s.scaleSetID
	s.mu.Unlock()
	jit, err := s.client.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: runnerName}, id)
	if err != nil {
		return 0, "", fmt.Errorf("generating jit config: %w", err)
	}
	if jit == nil {
		return 0, "", fmt.Errorf("generating jit config: empty response")
	}
	var runnerID int64
	if jit.Runner != nil {
		runnerID = int64(jit.Runner.ID)
	}
	return runnerID, jit.EncodedJITConfig, nil
}

// run blocks until the reservation ends; mint is nil for adopted runners.
func (s *Scaler) run(ctx context.Context, t *tracked, mint fleet.MintFunc) {
	defer t.cancel()
	name := t.res.RunnerName()
	err := t.res.Run(ctx, mint)
	s.mu.Lock()
	if cur, ok := s.runners[name]; ok && cur == t {
		delete(s.runners, name)
	}
	s.mu.Unlock()

	completedAt := time.Now()
	payload := map[string]any{"name": name, "runner_elapsed_ms": completedAt.Sub(t.spawnedAt).Milliseconds()}
	if err != nil {
		s.logger.Error("runner failed", "name", name, "member", t.res.Member(), "error", err)
		payload["error"] = err.Error()
		s.publish(event.Event{Time: completedAt, Type: event.EventRunnerFailed, Repo: s.repo, Payload: mustMarshal(payload)})
		return
	}
	s.publish(event.Event{Time: completedAt, Type: event.EventRunnerCompleted, Repo: s.repo, Payload: mustMarshal(payload)})
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

// onStarted records the job start of a known runner (telemetry only).
func (s *Scaler) onStarted(jobInfo *scaleset.JobStarted) {
	now := time.Now()
	s.mu.Lock()
	t := s.runners[jobInfo.RunnerName]
	var spawnedAt time.Time
	if t != nil {
		if t.startedAt.IsZero() {
			t.startedAt = now
		}
		spawnedAt = t.spawnedAt
	}
	s.mu.Unlock()
	if t != nil {
		t.res.MarkStarted()
	}
	payload := map[string]any{
		"job": jobInfo.JobDisplayName, "runner": jobInfo.RunnerName,
		"workflow": jobInfo.WorkflowRunID,
	}
	if t != nil {
		payload["runner_startup_ms"] = now.Sub(spawnedAt).Milliseconds()
	}
	s.logger.Info("job started",
		"job", jobInfo.JobDisplayName,
		"runner", jobInfo.RunnerName,
		"workflow", jobInfo.WorkflowRunID,
	)
	s.publish(event.Event{Time: now, Type: event.EventJobStarted, Repo: s.repo, Payload: mustMarshal(payload)})
}

// onCompleted ends a runner's job: it frees the slot before the runner process
// exits, so the next assignment can use it at once, then cancels the runner's
// context. For a remote runner that only ends Run on the router; the worker's
// runner exits by itself. An unknown runner is a no-op apart from the event.
func (s *Scaler) onCompleted(jobInfo *scaleset.JobCompleted) {
	now := time.Now()
	s.mu.Lock()
	t := s.runners[jobInfo.RunnerName]
	delete(s.runners, jobInfo.RunnerName)
	s.mu.Unlock()
	payload := map[string]any{
		"job": jobInfo.JobDisplayName, "runner": jobInfo.RunnerName,
		"result": jobInfo.Result, "workflow": jobInfo.WorkflowRunID,
	}
	if t != nil && !t.startedAt.IsZero() {
		payload["job_duration_ms"] = now.Sub(t.startedAt).Milliseconds()
	}
	s.logger.Info("job completed",
		"job", jobInfo.JobDisplayName,
		"runner", jobInfo.RunnerName,
		"result", jobInfo.Result,
	)
	s.publish(event.Event{Time: now, Type: event.EventJobCompleted, Repo: s.repo, Payload: mustMarshal(payload)})
	if t == nil {
		return
	}
	// Scale reconciles right after this. Free the slot now instead of waiting
	// for the runner process to exit.
	t.res.Release()
	t.cancel()
}

func (s *Scaler) recordDecision(d scaleDecision) {
	s.mu.Lock()
	changed := !s.hasDecision || s.lastDecision != d
	s.lastDecision = d
	s.hasDecision = true
	s.mu.Unlock()
	if !changed {
		return
	}
	s.logger.Info("scaling", "desired", d.desired, "existing", d.existing, "available", d.available, "created", d.created)
	s.publish(event.Event{
		Time: time.Now(), Type: event.EventScaleDecision, Repo: s.repo,
		Payload: mustMarshal(map[string]any{
			"desired": d.desired, "existing": d.existing,
			"available": d.available, "created": d.created,
		}),
	})
}

func (s *Scaler) pushCapacity() {
	s.mu.Lock()
	sink := s.sink
	s.mu.Unlock()
	if sink != nil {
		sink.SetMaxRunners(s.Capacity())
	}
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
