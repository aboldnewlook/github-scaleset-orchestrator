package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/event"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghapi"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/naming"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/scaler"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

// Orchestrator supervises one GitHub scale-set listener per repo.
type Orchestrator struct {
	cfg      *config.Config
	logger   *slog.Logger
	bus      *event.Bus // may be nil
	sem      *scaler.Semaphore
	github   ghapi.Factory
	timing   SupervisorTiming
	instance string       // naming.InstanceID(cfg.Runner.InstanceID): scale-set names and session owner
	class    runner.Class // P2a: the router host's class only

	mu        sync.Mutex
	scalers   map[string]*scaler.Scaler // keyed by repo; live across listener restarts
	listeners map[string]capacitySetter // keyed by repo; only while a listener runs
	status    map[string]*control.ListenerLiveStatus
}

// capacitySetter is implemented by the scale-set listener. Keeping only its
// capacity control here lets the orchestrator update every active listener.
type capacitySetter interface {
	SetMaxRunners(int)
}

// Option configures an Orchestrator.
type Option func(*Orchestrator)

// WithGitHub replaces the per-repo GitHub client factory (the simulator, tests).
// An injected factory owns authentication: the orchestrator never resolves a
// token or builds a real scaleset client itself.
func WithGitHub(f ghapi.Factory) Option { return func(o *Orchestrator) { o.github = f } }

// WithSupervisorTiming replaces the listener supervisor's retry policy.
// Zero durations fall back to the defaults, so a zero-value SupervisorTiming
// cannot spin the retry loop.
func WithSupervisorTiming(t SupervisorTiming) Option {
	return func(o *Orchestrator) {
		def := DefaultSupervisorTiming()
		if t.BackoffMin <= 0 {
			t.BackoffMin = def.BackoffMin
		}
		if t.BackoffMax <= 0 {
			t.BackoffMax = def.BackoffMax
		}
		if t.ConflictWait <= 0 {
			t.ConflictWait = def.ConflictWait
		}
		o.timing = t
	}
}

func New(cfg *config.Config, logger *slog.Logger, bus *event.Bus, opts ...Option) *Orchestrator {
	o := &Orchestrator{
		cfg:       cfg,
		logger:    logger,
		bus:       bus,
		timing:    DefaultSupervisorTiming(),
		instance:  naming.InstanceID(cfg.Runner.InstanceID),
		class:     cfg.DefaultClass(),
		scalers:   make(map[string]*scaler.Scaler),
		listeners: make(map[string]capacitySetter),
		status:    make(map[string]*control.ListenerLiveStatus),
	}
	for _, opt := range opts {
		opt(o)
	}
	if o.github == nil {
		o.github = ghapi.NewFactory(cfg, ghapi.NewHTTPClient(), logger)
	}
	return o
}

// RunnersByRepo returns a snapshot of active runner names per repo.
func (o *Orchestrator) RunnersByRepo() map[string][]string {
	o.mu.Lock()
	defer o.mu.Unlock()

	result := make(map[string][]string, len(o.scalers))
	for repo, s := range o.scalers {
		result[repo] = s.Runners()
	}
	return result
}

// Semaphore returns the orchestrator's semaphore for capacity queries.
func (o *Orchestrator) Semaphore() *scaler.Semaphore {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.sem
}

// MaxRunners returns the configured maximum concurrent runners.
func (o *Orchestrator) MaxRunners() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.cfg.MaxRunners
}

// AvailableSlots returns the number of free runner slots.
func (o *Orchestrator) AvailableSlots() int {
	o.mu.Lock()
	sem := o.sem
	maxRunners := o.cfg.MaxRunners
	o.mu.Unlock()
	if sem == nil {
		return maxRunners
	}
	return sem.Available()
}

// SetMaxRunners updates the concurrency cap at runtime.
// Note: this does not persist across restarts.
func (o *Orchestrator) SetMaxRunners(count int) error {
	if count < 1 {
		return fmt.Errorf("max runners must be at least 1")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.cfg.MaxRunners = count
	if o.sem != nil {
		o.sem.Resize(count)
	}
	for _, l := range o.listeners {
		l.SetMaxRunners(count)
	}
	return nil
}

// CancelRunner finds a runner by name across all repos and cancels it.
func (o *Orchestrator) CancelRunner(name string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()

	for _, s := range o.scalers {
		if s.CancelRunner(name) {
			return true
		}
	}
	return false
}

// Scalers returns the scaler map (for the Runtime service).
func (o *Orchestrator) Scalers() map[string]*scaler.Scaler {
	o.mu.Lock()
	defer o.mu.Unlock()

	// Return a shallow copy to avoid races.
	cp := make(map[string]*scaler.Scaler, len(o.scalers))
	for k, v := range o.scalers {
		cp[k] = v
	}
	return cp
}

// Run supervises one listener per configured repo and blocks until ctx is
// cancelled. One repo's failures never stop the others (design spec §5.3),
// so Run returns an error only when the local executor cannot be prepared.
func (o *Orchestrator) Run(ctx context.Context) error {
	o.publish(event.Event{Time: time.Now(), Type: event.EventDaemonStarted})

	worker, err := o.prepareExecutor(ctx)
	if err != nil {
		return err
	}

	o.mu.Lock()
	o.sem = scaler.NewSemaphore(o.cfg.MaxRunners)
	sem := o.sem
	o.mu.Unlock()

	var wg sync.WaitGroup
	for _, repo := range o.cfg.Repos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o.superviseRepo(ctx, repo, worker, sem)
		}()
	}

	<-ctx.Done()
	o.publish(event.Event{Time: time.Now(), Type: event.EventDaemonStopping})
	wg.Wait()
	return nil
}

// prepareExecutor builds the single local executor. The code moved from
// Run unchanged; B8a moves it into internal/router.
func (o *Orchestrator) prepareExecutor(ctx context.Context) (runner.Executor, error) {
	if o.cfg.Runner.Executor == "docker" {
		dockerWorker, err := runner.NewDockerWorker(runner.DockerConfig{
			Image:      o.cfg.Runner.Image,
			InstanceID: o.cfg.Runner.InstanceID,
			MemoryMB:   o.cfg.Runner.MemoryMB,
			CPUs:       o.cfg.Runner.CPUs,
			CacheRoot:  o.cfg.Runner.CacheRoot,
			CacheUID:   o.cfg.Runner.CacheUID,
			CacheGID:   o.cfg.Runner.CacheGID,
			Host:       o.cfg.Runner.DockerHost,
		}, o.logger)
		if err != nil {
			return nil, fmt.Errorf("configuring docker executor: %w", err)
		}
		if err := dockerWorker.Reconcile(ctx); err != nil {
			return nil, fmt.Errorf("reconciling stale runner containers: %w", err)
		}
		return dockerWorker, nil
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		cacheDir = "/tmp"
	}
	cacheDir += "/gso"
	mgr, err := runner.NewManager(cacheDir, o.logger)
	if err != nil {
		return nil, fmt.Errorf("creating runner manager: %w", err)
	}
	runnerDir, err := mgr.RunnerDir(ctx)
	if err != nil {
		return nil, fmt.Errorf("preparing runner binary: %w", err)
	}
	o.logger.Info("runner binary ready", "dir", runnerDir)
	return runner.NewWorker(runnerDir, o.logger), nil
}

// registerScaler records the repo's scaler once; it stays registered across
// listener restarts so RunnersByRepo and CancelRunner keep seeing its runners.
func (o *Orchestrator) registerScaler(repo string, s *scaler.Scaler) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.scalers[repo] = s
}

func (o *Orchestrator) registerListener(repo string, l capacitySetter) {
	o.mu.Lock()
	defer o.mu.Unlock()
	// Capacity may have changed while the listener was being created.
	l.SetMaxRunners(o.cfg.MaxRunners)
	o.listeners[repo] = l
}

func (o *Orchestrator) unregisterListener(repo string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.listeners, repo)
}

// publish sends an event to the bus if configured.
func (o *Orchestrator) publish(e event.Event) {
	if o.bus != nil {
		o.bus.Publish(e)
	}
}

func mustMarshal(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}
