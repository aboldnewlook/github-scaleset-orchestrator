// Package router assembles the gso router: the orchestrator (GitHub scale sets,
// listeners, scalers), the fleet of runner hosts, and the remote-worker listener
// with its enrollment registry (design spec §3, §4).
package router

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/event"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/fleet"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghapi"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/orchestrator"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
	"github.com/aboldnewlook/github-scaleset-orchestrator/worker"
	"golang.org/x/net/netutil"
)

// Worker listener connection limits. WriteTimeout stays 0 on purpose:
// Subscribe is a long-lived server stream. Request bodies are capped at 1 MiB
// by the fleet server (connect.WithReadMaxBytes).
const (
	workerReadHeaderTimeout = 10 * time.Second
	workerReadTimeout       = 30 * time.Second
	workerIdleTimeout       = 2 * time.Minute
)

// Options configures a Router. The zero value is production behaviour.
type Options struct {
	// StateDir holds workers.json and workers/tls. "" = config.StateDir().
	StateDir string
	// GitHub replaces the real GitHub client factory (tests, simulator). The
	// orchestrator takes it through orchestrator.WithGitHub and then never
	// resolves a token or builds a real client. nil = real GitHub.
	GitHub ghapi.Factory
	// FleetTiming overrides fleet.DefaultTiming().
	FleetTiming *fleet.Timing
	// LocalExecutor replaces the local member's executor (tests: no runner
	// download, no Docker). nil = built from runner.* when max_runners > 0.
	LocalExecutor fleet.LocalExecutor
}

// Router is one gso router process.
type Router struct {
	cfg    *config.Config
	logger *slog.Logger
	bus    *event.Bus
	opts   Options
	timing fleet.Timing

	fleet *fleet.Fleet
	orch  *orchestrator.Orchestrator
	// runOrchestrator runs the orchestrator (default r.orch.Run). Tests replace
	// it with a stub so they never contact GitHub.
	runOrchestrator func(ctx context.Context) error

	// Worker state, loaded by New when workers.listen is set.
	registry *fleet.Registry
	tlsCfg   *tls.Config
	pin      string

	mu        sync.Mutex
	addr      string
	ready     chan struct{}
	readyOnce sync.Once
	local     fleet.LocalExecutor // nil when there is no local member
	started   atomic.Bool         // prepareLocal ran
}

var (
	errWorkersDisabled = errors.New("remote workers are disabled: set workers.listen (GSO_WORKERS_LISTEN) on the router")
	errWorkerNotFound  = errors.New("worker not found")
)

// New builds the router. With workers.listen set it loads (or creates) the
// worker registry and the listener key, so the SPKI pin is known before Run.
func New(cfg *config.Config, logger *slog.Logger, bus *event.Bus, o Options) (*Router, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	timing := fleet.DefaultTiming()
	if o.FleetTiming != nil {
		timing = *o.FleetTiming
	}
	r := &Router{cfg: cfg, logger: logger, bus: bus, opts: o, timing: timing, ready: make(chan struct{})}

	fo := fleet.Options{
		Logger:            logger,
		Bus:               bus,
		Timing:            timing,
		MaxSlotsPerWorker: cfg.Workers.MaxSlotsPerWorker,
		RemoteEnabled:     cfg.WorkersEnabled(),
	}
	if cfg.WorkersEnabled() {
		// The orchestrator is built after the fleet; the wrappers resolve it
		// on every call.
		fo.GitHub = placementGitHub{target: r.target}
		fo.Adopter = placementAdopter{target: r.target}
	}
	r.fleet = fleet.New(fo)

	oopts := []orchestrator.Option{orchestrator.WithFleet(r.fleet)}
	if o.GitHub != nil {
		oopts = append(oopts, orchestrator.WithGitHub(o.GitHub))
	}
	r.orch = orchestrator.New(cfg, logger, bus, oopts...)
	r.runOrchestrator = r.orch.Run

	if cfg.WorkersEnabled() {
		if err := r.openWorkerState(); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// target is what the placement wrappers delegate to.
func (r *Router) target() any {
	if r.orch == nil {
		return nil
	}
	return r.orch
}

func (r *Router) openWorkerState() error {
	dir := r.opts.StateDir
	if dir == "" {
		d, err := config.StateDir()
		if err != nil {
			return fmt.Errorf("resolving state directory: %w", err)
		}
		dir = d
	}
	if err := os.MkdirAll(filepath.Join(dir, "workers"), 0o700); err != nil {
		return fmt.Errorf("creating worker state directory: %w", err)
	}
	reg, err := fleet.OpenRegistry(filepath.Join(dir, "workers.json"))
	if err != nil {
		return fmt.Errorf("opening worker registry: %w", err)
	}
	tlsCfg, pin, err := fleet.LoadOrCreateTLS(filepath.Join(dir, "workers", "tls"), r.logger)
	if err != nil {
		return fmt.Errorf("loading the worker listener key: %w", err)
	}
	r.registry, r.tlsCfg, r.pin = reg, tlsCfg, pin
	return nil
}

// Run starts the fleet, the worker listener (when enabled) and the
// orchestrator, and blocks until ctx is cancelled or the orchestrator fails.
func (r *Router) Run(ctx context.Context) error {
	r.fleet.Start(ctx)
	if err := r.prepareLocal(ctx); err != nil {
		return err
	}
	if err := r.startWorkers(ctx); err != nil {
		return err
	}
	return r.runOrchestrator(ctx)
}

// newWorkerHTTPServer builds the worker listener's http.Server with
// connection limits. WriteTimeout is left at 0 for the Subscribe stream.
func newWorkerHTTPServer(ctx context.Context, h http.Handler, tlsCfg *tls.Config, logger *slog.Logger) *http.Server {
	srv := &http.Server{
		Handler:           h,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: workerReadHeaderTimeout,
		ReadTimeout:       workerReadTimeout,
		IdleTimeout:       workerIdleTimeout,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	if logger != nil {
		srv.ErrorLog = slog.NewLogLogger(logger.Handler(), slog.LevelWarn)
	}
	return srv
}

func (r *Router) startWorkers(ctx context.Context) error {
	if !r.cfg.WorkersEnabled() {
		// No worker listener means no reconnecting worker to hold back, so there
		// is nothing to wait for (B8 minor).
		r.markReady()
		return nil
	}
	nets, err := r.allowNets()
	if err != nil {
		return err
	}
	srv := fleet.NewServer(fleet.ServerConfig{
		Registry:    r.registry,
		Fleet:       r.fleet,
		Logger:      r.logger,
		Bus:         r.bus,
		AllowNets:   nets,
		OwnerScopes: r.ownerScopes,
	})
	ln, err := net.Listen("tcp", r.cfg.Workers.Listen)
	if err != nil {
		return fmt.Errorf("listening for workers on %s: %w", r.cfg.Workers.Listen, err)
	}
	ln = newWorkerListener(ln, nets, maxWorkerConns)
	r.mu.Lock()
	r.addr = ln.Addr().String()
	r.mu.Unlock()

	httpSrv := newWorkerHTTPServer(ctx, srv.Handler(), r.tlsCfg, r.logger)
	go func() {
		r.waitScaleSets(ctx)
		r.markReady()
		r.logger.Info("worker listener serving", "addr", ln.Addr().String(), "pin", worker.ShortPin(r.pin))
		if err := httpSrv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			r.logger.Error("worker listener stopped", "error", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			_ = httpSrv.Close()
		}
	}()
	return nil
}

// scaleSetKey identifies one scale set: owner/repo (case-insensitive, as
// GitHub and ghsim fold it) plus the runner class.
func scaleSetKey(repo string, class runner.Class) string {
	return strings.ToLower(repo) + "|" + class.String()
}

// scaleSetLookup is the part of fleet.GitHub waitKnown needs.
type scaleSetLookup interface {
	ScaleSetID(repo string, class runner.Class) (int, bool)
}

// waitKnown blocks until every distinct scale set of repos at class is known,
// for at most grace or until ctx ends. Repos that differ only by case are one
// scale set.
func waitKnown(ctx context.Context, gh scaleSetLookup, repos []string, class runner.Class, grace time.Duration) {
	pending := map[string]string{} // key -> repo name as configured
	for _, repo := range repos {
		key := scaleSetKey(repo, class)
		if _, dup := pending[key]; !dup {
			pending[key] = repo
		}
	}
	deadline := time.Now().Add(grace)
	for {
		for key, repo := range pending {
			if _, ok := gh.ScaleSetID(repo, class); ok {
				delete(pending, key)
			}
		}
		if len(pending) == 0 || !time.Now().Before(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// waitScaleSets holds the worker listener back until every repo's
// default-class scale set is known, for at most the reconnect grace.
// Adoption after a router restart checks reported runners against their
// scale set (spec §4.3); a worker reconnecting earlier would have its runners
// rejected. Before placement is wired (B8a) there is nothing to wait for.
func (r *Router) waitScaleSets(ctx context.Context) {
	gh, ok := placementGitHub{target: r.target}.github()
	if !ok {
		return
	}
	repos := make([]string, 0, len(r.cfg.Repos))
	for _, repo := range r.cfg.Repos {
		repos = append(repos, repo.Name)
	}
	waitKnown(ctx, gh, repos, r.cfg.DefaultClass(), r.timing.Grace)
}

func (r *Router) markReady() { r.readyOnce.Do(func() { close(r.ready) }) }

// Ready is closed once the worker listener serves (or workers are disabled)
// and, once placement is wired, every repo's default-class scale set is known
// or the reconnect grace has elapsed.
func (r *Router) Ready() <-chan struct{} { return r.ready }

// WorkersAddr is the bound worker listener address, "" before Run or when
// workers are disabled.
func (r *Router) WorkersAddr() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.addr
}

// WorkersPin is the SPKI pin of the worker listener key ("" when disabled).
func (r *Router) WorkersPin() string { return r.pin }

// Fleet returns the router's fleet.
func (r *Router) Fleet() *fleet.Fleet { return r.fleet }

// Orchestrator returns the router's orchestrator.
func (r *Router) Orchestrator() *orchestrator.Orchestrator { return r.orch }

func (r *Router) allowNets() ([]*net.IPNet, error) {
	if len(r.cfg.Workers.AllowCIDRs) == 0 {
		return nil, nil // fleet.DefaultAllowNets()
	}
	nets, err := control.ParseCIDRs(r.cfg.Workers.AllowCIDRs)
	if err != nil {
		return nil, fmt.Errorf("workers.allow_cidrs: %w", err)
	}
	return nets, nil
}

// ownerScopes seeds a newly enrolled worker's local allowlist: its repo
// bounds when it has them, else "owner/*" for every owner the router serves.
// Owners are folded to lower case, as GitHub and ghsim fold them.
func (r *Router) ownerScopes(b fleet.Bounds) []string {
	scopes := r.rawOwnerScopes(b)
	if len(scopes) > worker.MaxOwnerScopes {
		r.logger.Warn("truncating owner scopes sent at enrollment", "have", len(scopes), "max", worker.MaxOwnerScopes)
		scopes = scopes[:worker.MaxOwnerScopes]
	}
	return scopes
}

func (r *Router) rawOwnerScopes(b fleet.Bounds) []string {
	if len(b.Repos) > 0 {
		return append([]string(nil), b.Repos...)
	}
	seen := map[string]bool{}
	var scopes []string
	for _, repo := range r.cfg.Repos {
		owner, _, ok := strings.Cut(strings.ToLower(repo.Name), "/")
		if !ok || seen[owner] {
			continue
		}
		seen[owner] = true
		scopes = append(scopes, owner+"/*")
	}
	sort.Strings(scopes)
	return scopes
}

// maxWorkerConns caps simultaneous worker connections (each worker holds a
// few h2 connections; this leaves headroom while bounding file descriptors).
const maxWorkerConns = 256

// newWorkerListener closes peers outside nets before any TLS handshake, then
// caps the connections that remain at max. The allow filter sits inside the
// limit so rejected peers never occupy a slot.
func newWorkerListener(ln net.Listener, nets []*net.IPNet, max int) net.Listener {
	return netutil.LimitListener(fleet.AllowListener(ln, nets), max)
}
