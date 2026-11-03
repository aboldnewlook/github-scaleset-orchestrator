package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/event"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/fleet"
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
	fleet    *fleet.Fleet
	github   ghapi.Factory
	timing   SupervisorTiming
	instance string       // naming.InstanceID(cfg.Runner.InstanceID): scale-set names and session owner
	class    runner.Class // P2a: the router host's class only

	// reevaluate coalesces fleet changes into scaler re-evaluations. It is
	// created in New so signalReevaluate, which the fleet calls, takes no lock.
	reevaluate chan struct{}

	mu      sync.Mutex
	scalers map[string]*scaler.Scaler // keyed by repo; live across listener restarts
	byClass map[string]*scaler.Scaler // keyed by scalerKey(repo, class), created once
	clients map[string]ghapi.Client   // keyed by lower-cased repo; one GitHub client per repo
	status  map[string]*control.ListenerLiveStatus
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
		cfg:        cfg,
		logger:     logger,
		bus:        bus,
		timing:     DefaultSupervisorTiming(),
		instance:   naming.InstanceID(cfg.Runner.InstanceID),
		class:      cfg.DefaultClass(),
		reevaluate: make(chan struct{}, 1),
		scalers:    make(map[string]*scaler.Scaler),
		byClass:    make(map[string]*scaler.Scaler),
		clients:    make(map[string]ghapi.Client),
		status:     make(map[string]*control.ListenerLiveStatus),
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

// MaxRunners returns the configured maximum concurrent runners.
func (o *Orchestrator) MaxRunners() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.cfg.MaxRunners
}

// AvailableSlots returns the free slots of every available fleet member.
func (o *Orchestrator) AvailableSlots() int {
	if o.fleet == nil {
		return 0
	}
	free := 0
	for _, m := range o.fleet.Members() {
		if m.Availability == "available" && m.Slots > m.Busy {
			free += m.Slots - m.Busy
		}
	}
	return free
}

// SetMaxRunners records the local member's slot count. The router resizes
// the fleet's local member and enforces the lower bound; every scaler is
// re-evaluated so the new capacity reaches GitHub on the next poll. It does
// not persist across restarts.
func (o *Orchestrator) SetMaxRunners(count int) error {
	if count < 0 {
		return fmt.Errorf("max runners cannot be negative")
	}
	o.mu.Lock()
	o.cfg.MaxRunners = count
	o.mu.Unlock()
	o.signalReevaluate()
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
// so Run returns an error only when no fleet was configured. The local
// executor is the router's business (internal/router), not the orchestrator's.
func (o *Orchestrator) Run(ctx context.Context) error {
	if o.fleet == nil {
		return fmt.Errorf("orchestrator: no fleet configured (use WithFleet)")
	}
	o.fleet.OnChange(o.signalReevaluate)
	go o.reevaluateLoop(ctx)
	o.publish(event.Event{Time: time.Now(), Type: event.EventDaemonStarted})

	var wg sync.WaitGroup
	for _, repo := range o.cfg.Repos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o.superviseRepo(ctx, repo)
		}()
	}

	<-ctx.Done()
	o.publish(event.Event{Time: time.Now(), Type: event.EventDaemonStopping})
	wg.Wait()
	return nil
}

// registerScaler records the repo's scaler once; it stays registered across
// listener restarts so RunnersByRepo and CancelRunner keep seeing its runners.
func (o *Orchestrator) registerScaler(repo string, s *scaler.Scaler) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.scalers[repo] = s
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
