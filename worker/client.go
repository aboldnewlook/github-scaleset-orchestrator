package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	workerv1 "github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1"
	"github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1/workerv1connect"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

// Availability tells the router whether this worker takes new work.
type Availability int

const (
	// Unavailable: connected, but no new offers (for example, the desktop is in use).
	Unavailable Availability = iota
	// Available: offers welcome.
	Available
	// Draining: no new offers; running jobs finish, or are killed at the drain deadline.
	Draining
)

// String returns "available", "draining" or "unavailable".
func (a Availability) String() string {
	switch a {
	case Available:
		return "available"
	case Draining:
		return "draining"
	default:
		return "unavailable"
	}
}

func (a Availability) toProto() workerv1.Availability {
	switch a {
	case Available:
		return workerv1.Availability_AVAILABILITY_AVAILABLE
	case Draining:
		return workerv1.Availability_AVAILABILITY_DRAINING
	default:
		return workerv1.Availability_AVAILABILITY_UNAVAILABLE
	}
}

const (
	defaultHeartbeat = 10 * time.Second
	rpcTimeout       = 10 * time.Second
	closeTimeout     = 5 * time.Second
	killTimeout      = 30 * time.Second
	maxReasonLen     = 200
	maxPending       = 256
	// routerLostTimeout is the router's worker-lost timer (spec §4.5): past it
	// the router has dropped the session, so the client reconnects.
	routerLostTimeout = 30 * time.Second
)

// Config configures a Client.
type Config struct {
	Name, MachineID, Version string
	DialAddr, Pin            string // router address to dial, and its SPKI pin
	Credential               CredentialStore
	Records                  *RecordStore
	Launchers                []Launcher // at most one per executor kind
	Slots                    int        // advertised; the router clamps to the enrolled bounds
	Allowlist                []string   // owner/* or owner/repo patterns (spec §8.3)
	AcceptPublic             PublicMode
	Logger                   *slog.Logger
	BackoffMin, BackoffMax   time.Duration // reconnect backoff; defaults 1s, 15s
	// TrustedHosts is passed to OfferCheck.TrustedHosts so a worker can run
	// against a simulator router. Empty means the GitHub defaults.
	TrustedHosts []string
}

// Status is a snapshot for a UI or a status command.
type Status struct {
	Connected    bool
	SessionID    string
	Availability Availability
	Slots        int // effective, after the router's clamp
	Busy         int
	Classes      []string // effective
	AcceptPublic bool     // effective
	LastError    string
	Runners      []RunnerRecord
}

// Client keeps a session with the router, accepts offers that pass the local
// checks, verifies their JIT configs, and runs them through its Launchers.
type Client struct {
	cfg    Config
	logger *slog.Logger
	rpc    workerv1connect.WorkerServiceClient
	byKind map[runner.ExecutorKind]Launcher

	transport   *resettableTransport
	hbFailLimit time.Duration // heartbeats may fail this long before the session is dropped
	hbFailSince time.Time     // first failure of the current streak (guarded by mu)

	recMu sync.Mutex // serialises Records
	wake  chan struct{}
	wg    sync.WaitGroup

	mu            sync.Mutex
	credential    string
	availability  Availability
	drainTimer    *time.Timer
	allowlist     []string
	mode          PublicMode
	routerDefault bool
	connected     bool
	sessionID     string
	effSlots      int
	effClasses    map[runner.ExecutorKind]map[runner.Class]bool
	effNames      []string
	lastErr       string
	reserved      int
	active        map[string]*activeRunner
	pending       []*workerv1.ReportRunnerRequest
}

type activeRunner struct {
	rec       RunnerRecord
	launcher  Launcher
	preempted bool
	silent    bool // the router already dropped it: no EXITED report
	reason    string
}

// New validates cfg and returns a Client. It does not dial.
func New(cfg Config) (*Client, error) {
	if err := ValidatePatterns(cfg.Allowlist); err != nil {
		return nil, fmt.Errorf("worker: %w", err)
	}
	switch {
	case cfg.DialAddr == "":
		return nil, errors.New("worker: DialAddr is required")
	case cfg.Pin == "":
		return nil, errors.New("worker: Pin is required")
	case cfg.Credential == nil:
		return nil, errors.New("worker: Credential is required")
	case cfg.Records == nil:
		return nil, errors.New("worker: Records is required")
	case len(cfg.Launchers) == 0:
		return nil, errors.New("worker: at least one Launcher is required")
	case cfg.Slots < 1 || cfg.Slots > maxSlots:
		return nil, fmt.Errorf("worker: Slots must be between 1 and %d", maxSlots)
	}
	mode, err := ParsePublicMode(string(cfg.AcceptPublic))
	if err != nil {
		return nil, fmt.Errorf("worker: %w", err)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.BackoffMin <= 0 {
		cfg.BackoffMin = time.Second
	}
	if cfg.BackoffMax <= 0 {
		cfg.BackoffMax = 15 * time.Second
	}
	if cfg.BackoffMax < cfg.BackoffMin {
		cfg.BackoffMax = cfg.BackoffMin
	}
	byKind := make(map[runner.ExecutorKind]Launcher, len(cfg.Launchers))
	for _, l := range cfg.Launchers {
		if _, dup := byKind[l.Kind()]; dup {
			return nil, fmt.Errorf("worker: two launchers for executor %q", l.Kind())
		}
		byKind[l.Kind()] = l
	}
	rt := newResettableTransport(cfg.Pin)
	return &Client{
		cfg:         cfg,
		logger:      cfg.Logger.With("worker", cfg.Name),
		rpc:         workerv1connect.NewWorkerServiceClient(&http.Client{Transport: rt}, "https://"+cfg.DialAddr),
		transport:   rt,
		hbFailLimit: routerLostTimeout,
		byKind:      byKind,
		wake:        make(chan struct{}, 1),
		allowlist:   slices.Clone(cfg.Allowlist),
		mode:        mode,
		active:      make(map[string]*activeRunner),
	}, nil
}

// Run re-attaches to the runners recorded by a previous run, then keeps a
// session with the router until ctx is cancelled, reconnecting with jittered
// backoff. Cancelling ctx closes the session and detaches from running jobs:
// they keep running and the next Run re-attaches to them. Run returns nil
// once ctx is cancelled, and an error only when the credential cannot be
// loaded or the router refuses it (revoked or unknown).
func (c *Client) Run(ctx context.Context) error {
	credential, err := c.cfg.Credential.Load()
	if err != nil {
		return fmt.Errorf("loading worker credential: %w", err)
	}
	c.mu.Lock()
	c.credential = credential
	c.mu.Unlock()

	runCtx, cancel := context.WithCancel(ctx)
	defer c.wg.Wait()
	defer cancel()
	// A drain deadline that fires after Run returns must not kill containers
	// the next Run re-attaches to.
	defer c.stopDrainTimer()

	c.reattach(runCtx)

	attempt := 0
	for {
		established, err := c.session(runCtx)
		if runCtx.Err() != nil {
			return nil
		}
		if err != nil {
			c.setErr(err)
			if isRefused(err) {
				return fmt.Errorf("router refused this worker's credential: %w", err)
			}
			c.logger.Warn("router session ended", "error", err)
		}
		if established {
			attempt = 0
		}
		delay := c.backoff(attempt)
		attempt++
		select {
		case <-runCtx.Done():
			return nil
		case <-time.After(delay):
		}
	}
}

func (c *Client) stopDrainTimer() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.drainTimer != nil {
		c.drainTimer.Stop()
		c.drainTimer = nil
	}
}

// SetAvailability changes what the next heartbeat announces. Anything other
// than Draining cancels a pending drain deadline.
func (c *Client) SetAvailability(a Availability) {
	c.mu.Lock()
	c.availability = a
	if a != Draining && c.drainTimer != nil {
		c.drainTimer.Stop()
		c.drainTimer = nil
	}
	c.mu.Unlock()
	c.poke()
}

// Drain stops new offers now and, at deadline, kills the runners still going
// and reports them preempted (spec §7, "Worker drains").
func (c *Client) Drain(deadline time.Time) {
	c.mu.Lock()
	c.availability = Draining
	if c.drainTimer != nil {
		c.drainTimer.Stop()
	}
	c.drainTimer = time.AfterFunc(time.Until(deadline), func() { c.drainExpired(deadline) })
	c.mu.Unlock()
	c.poke()
}

// SetPolicy replaces the local allowlist and accept-public mode. An unknown
// mode fails closed (PublicRefuse).
func (c *Client) SetPolicy(allowlist []string, mode PublicMode) {
	m, err := ParsePublicMode(string(mode))
	if err != nil {
		m = PublicRefuse
	}
	c.mu.Lock()
	c.allowlist = slices.Clone(allowlist)
	c.mode = m
	c.mu.Unlock()
	c.poke()
}

// Status returns a snapshot.
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	runners := make([]RunnerRecord, 0, len(c.active))
	for _, a := range c.active {
		runners = append(runners, a.rec)
	}
	slices.SortFunc(runners, func(a, b RunnerRecord) int { return strings.Compare(a.Name, b.Name) })
	return Status{
		Connected:    c.connected,
		SessionID:    c.sessionID,
		Availability: c.availability,
		Slots:        c.effSlots,
		Busy:         len(c.active) + c.reserved,
		Classes:      slices.Clone(c.effNames),
		AcceptPublic: EffectiveAcceptPublic(c.mode, c.routerDefault),
		LastError:    c.lastErr,
		Runners:      runners,
	}
}

// reattach waits on every recorded runner and then removes runners no record
// knows. It runs once, before the first session and therefore before any
// Start, so RemoveOrphans never races a launch.
func (c *Client) reattach(ctx context.Context) {
	keep := make(map[runner.ExecutorKind]map[string]bool, len(c.byKind))
	for kind := range c.byKind {
		keep[kind] = map[string]bool{}
	}
	for _, rec := range c.listRecords() {
		l, ok := c.byKind[rec.Executor]
		if !ok {
			c.logger.Warn("dropping runner record for a disabled executor", "runner", rec.Name, "executor", rec.Executor)
			c.deleteRecord(rec.Name)
			continue
		}
		keep[rec.Executor][rec.Name] = true
		a := &activeRunner{rec: rec, launcher: l}
		c.mu.Lock()
		c.active[rec.Name] = a
		c.mu.Unlock()
		c.logger.Info("re-attaching to runner", "runner", rec.Name, "repo", rec.Repo, "phase", rec.Phase)
		c.goWait(ctx, a)
	}
	for kind, l := range c.byKind {
		if err := l.RemoveOrphans(ctx, keep[kind]); err != nil {
			c.logger.Warn("removing orphaned runners failed", "executor", kind, "error", err)
		}
	}
}

// session opens one router session and serves it until it ends. established
// reports whether OpenSession succeeded, which resets the reconnect backoff.
func (c *Client) session(ctx context.Context) (bool, error) {
	open, execErr := c.openRequest(ctx)
	octx, cancel := context.WithTimeout(ctx, rpcTimeout)
	resp, err := c.rpc.OpenSession(octx, authed(c, open))
	cancel()
	if err != nil {
		return false, fmt.Errorf("opening session: %w", err)
	}
	msg := resp.Msg
	sessionID := msg.GetSessionId()
	interval := defaultHeartbeat
	if d := msg.GetHeartbeatInterval(); d != nil && d.AsDuration() > 0 {
		interval = d.AsDuration()
	}
	c.applySession(msg, execErr)
	defer c.disconnect()
	c.logger.Info("connected to router", "session", sessionID, "slots", msg.GetEffective().GetSlots(), "classes", c.Status().Classes)
	c.dropRejected(ctx, msg.GetRejectedRunners())
	c.flush(ctx)

	sctx, stop := context.WithCancel(ctx)
	defer stop()
	directives := make(chan *workerv1.Directive, 16)
	subErr := make(chan error, 1)
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		subErr <- c.subscribe(sctx, sessionID, interval, directives)
	}()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			c.closeSession(ctx, sessionID)
			return true, ctx.Err()
		case err := <-subErr:
			return true, err
		case d := <-directives:
			c.handleDirective(ctx, sessionID, d)
		case <-ticker.C:
			if err := c.heartbeat(ctx, sessionID, interval); err != nil {
				return true, err
			}
		case <-c.wake:
			if err := c.heartbeat(ctx, sessionID, interval); err != nil {
				return true, err
			}
		}
	}
}

func (c *Client) openRequest(ctx context.Context) (*workerv1.OpenSessionRequest, error) {
	caps, execErr := c.capabilities(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	return &workerv1.OpenSessionRequest{
		MachineId:    c.cfg.MachineID,
		Capabilities: caps,
		Availability: c.availability.toProto(),
		Running:      c.runningLocked(),
		AcceptPublic: EffectiveAcceptPublic(c.mode, c.routerDefault),
	}, execErr
}

// capabilities lists every launcher that can report its classes; the first
// launcher error is returned so Status can show it.
func (c *Client) capabilities(ctx context.Context) (*workerv1.Capabilities, error) {
	caps := &workerv1.Capabilities{WorkerVersion: c.cfg.Version, Slots: slotsToProto(c.cfg.Slots)}
	var firstErr error
	for _, l := range c.cfg.Launchers {
		classes, err := l.Classes(ctx)
		if err != nil {
			c.logger.Warn("executor unavailable; not offering its classes", "executor", l.Kind(), "error", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("%s executor: %w", l.Kind(), err)
			}
			continue
		}
		e := &workerv1.Executor{Kind: kindToProto(l.Kind())}
		for _, cl := range classes {
			e.Classes = append(e.Classes, classToProto(cl))
		}
		caps.Executors = append(caps.Executors, e)
	}
	return caps, firstErr
}

func (c *Client) applySession(m *workerv1.OpenSessionResponse, execErr error) {
	eff := make(map[runner.ExecutorKind]map[runner.Class]bool)
	var names []string
	for _, e := range m.GetEffective().GetExecutors() {
		kind := kindFromProto(e.GetKind())
		for _, pc := range e.GetClasses() {
			cl, err := classFromProto(pc)
			if err != nil {
				continue
			}
			if eff[kind] == nil {
				eff[kind] = make(map[runner.Class]bool)
			}
			eff[kind][cl] = true
			names = append(names, cl.String())
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = true
	c.hbFailSince = time.Time{}
	c.sessionID = m.GetSessionId()
	c.effSlots = int(m.GetEffective().GetSlots())
	c.effClasses = eff
	c.effNames = names
	c.routerDefault = m.GetPolicy().GetWorkerDefaultAccept()
	c.lastErr = ""
	if execErr != nil {
		c.lastErr = execErr.Error()
	}
}

func (c *Client) disconnect() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = false
	c.sessionID = ""
}

// dropRejected kills runners the router refused to adopt after reconnecting:
// it verified them against GitHub (or its own bounds) and they are not ours,
// so a job running in them would be orphaned. A runner the router merely could
// not verify yet is not listed here; it stays running and is reported again on
// every heartbeat until the router settles it.
func (c *Client) dropRejected(ctx context.Context, names []string) {
	for _, name := range names {
		c.logger.Warn("router rejected a running runner; killing it", "runner", name)
		c.kill(ctx, name, "rejected by router", false, true)
	}
}

// subscribe streams directives into out, re-subscribing after 4.5 heartbeat
// intervals of silence (45 s at the default 10 s heartbeat, i.e. three missed
// 15 s keepalives). It returns when ctx ends or the session is gone/refused.
func (c *Client) subscribe(ctx context.Context, sessionID string, heartbeat time.Duration, out chan<- *workerv1.Directive) error {
	silence := heartbeat * 9 / 2
	for {
		sctx, cancel := context.WithCancel(ctx)
		watchdog := time.AfterFunc(silence, cancel)
		stream, err := c.rpc.Subscribe(sctx, authed(c, &workerv1.SubscribeRequest{SessionId: sessionID}))
		if err == nil {
			for stream.Receive() {
				watchdog.Reset(silence)
				select {
				case out <- stream.Msg():
				case <-sctx.Done():
				}
			}
			err = stream.Err()
			_ = stream.Close()
		}
		watchdog.Stop()
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil && (isRefused(err) || isSessionGone(err)) {
			return fmt.Errorf("subscribe: %w", err)
		}
		c.logger.Debug("re-subscribing", "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.cfg.BackoffMin):
		}
	}
}

func (c *Client) heartbeat(ctx context.Context, sessionID string, interval time.Duration) error {
	c.mu.Lock()
	req := &workerv1.HeartbeatRequest{
		SessionId:    sessionID,
		Availability: c.availability.toProto(),
		Running:      c.runningLocked(),
		AcceptPublic: EffectiveAcceptPublic(c.mode, c.routerDefault),
	}
	c.mu.Unlock()
	hctx, cancel := context.WithTimeout(ctx, min(rpcTimeout, 2*interval))
	defer cancel()
	resp, err := c.rpc.Heartbeat(hctx, authed(c, req))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if isRefused(err) || isSessionGone(err) {
			return fmt.Errorf("heartbeat: %w", err)
		}
		c.setErr(err)
		// Keep the session through brief trouble (the router's lost timer is
		// the authority), but a streak longer than that timer means the router
		// has already dropped us or the connection is dead: reconnect on a
		// fresh connection instead of waiting on it.
		c.mu.Lock()
		now := time.Now()
		if c.hbFailSince.IsZero() {
			c.hbFailSince = now
		}
		streak := now.Sub(c.hbFailSince)
		c.mu.Unlock()
		if streak >= c.hbFailLimit {
			c.transport.reset()
			return fmt.Errorf("heartbeat failing for %s: %w", streak.Round(time.Millisecond), err)
		}
		c.logger.Warn("heartbeat failed; keeping the session", "error", err, "failing_for", streak)
		return nil
	}
	c.mu.Lock()
	c.hbFailSince = time.Time{}
	c.mu.Unlock()
	if p := resp.Msg.GetPolicy(); p != nil {
		c.mu.Lock()
		changed := c.routerDefault != p.GetWorkerDefaultAccept()
		c.routerDefault = p.GetWorkerDefaultAccept()
		c.mu.Unlock()
		if changed {
			c.poke() // announce the new effective accept_public right away
		}
	}
	return nil
}

func (c *Client) handleDirective(ctx context.Context, sessionID string, d *workerv1.Directive) {
	switch {
	case d.GetOffer() != nil:
		c.startOffer(ctx, sessionID, d.GetOffer())
	case d.GetCancel() != nil:
		name := d.GetCancel().GetRunnerName()
		reason := "cancelled by router"
		if r := d.GetCancel().GetReason(); r != "" {
			reason += ": " + r
		}
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			c.kill(ctx, name, reason, false, false)
		}()
	case d.GetDrain() != nil:
		c.logger.Info("router asked this worker to drain", "reason", d.GetDrain().GetReason())
		c.mu.Lock()
		c.availability = Draining
		c.mu.Unlock()
		c.poke()
	}
	// A Keepalive needs no action: receiving it already reset the watchdog.
}

func (c *Client) startOffer(ctx context.Context, sessionID string, o *workerv1.Offer) {
	l, class, reason := c.admit(o)
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		if reason != "" {
			c.logger.Info("declining offer", "runner", o.GetRunnerName(), "repo", o.GetRepo(), "reason", reason)
			c.reject(ctx, sessionID, o.GetOfferId(), reason)
			return
		}
		c.runOffer(ctx, sessionID, o, l, class)
	}()
}

// admit applies the worker-local checks (spec §4.4) and, when the offer
// passes, reserves a slot that runOffer releases once the runner is tracked
// or abandoned. A non-empty reason means "reject".
func (c *Client) admit(o *workerv1.Offer) (Launcher, runner.Class, string) {
	class, err := classFromProto(o.GetClass())
	if err != nil {
		return nil, runner.Class{}, "unknown class"
	}
	kind := kindFromProto(o.GetExecutor())
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.availability != Available {
		return nil, class, "worker is " + c.availability.String()
	}
	if !MatchRepo(c.allowlist, o.GetRepo()) {
		return nil, class, "repository is not in this worker's allowlist"
	}
	if o.GetPublicRepo() && !EffectiveAcceptPublic(c.mode, c.routerDefault) {
		return nil, class, "public repositories are not accepted on this worker"
	}
	l, ok := c.byKind[kind]
	if !ok {
		return nil, class, fmt.Sprintf("executor %q is not enabled on this worker", kind)
	}
	if !c.effClasses[kind][class] {
		return nil, class, "class " + class.String() + " is not enabled for this worker"
	}
	if len(c.active)+c.reserved >= c.effSlots {
		return nil, class, "no free slot"
	}
	c.reserved++
	return l, class, ""
}

func (c *Client) runOffer(ctx context.Context, sessionID string, o *workerv1.Offer, l Launcher, class runner.Class) {
	reserved := true
	unreserve := func() {
		if reserved {
			reserved = false
			c.mu.Lock()
			c.reserved--
			c.mu.Unlock()
		}
	}
	defer unreserve()

	if !validRunnerName(o.GetRunnerName()) {
		// The name becomes a file and container name; never let the router pick a path.
		c.logger.Warn("refusing an offer with an unsafe runner name", "repo", o.GetRepo())
		c.reject(ctx, sessionID, o.GetOfferId(), "runner name is not a plain lowercase token")
		return
	}

	actx, cancel := context.WithTimeout(ctx, rpcTimeout)
	resp, err := c.rpc.AcceptOffer(actx, authed(c, &workerv1.AcceptOfferRequest{SessionId: sessionID, OfferId: o.GetOfferId()}))
	cancel()
	if err != nil {
		c.logger.Info("offer was not accepted", "runner", o.GetRunnerName(), "repo", o.GetRepo(), "error", err)
		return
	}
	acc := resp.Msg
	if acc.GetRunnerName() != o.GetRunnerName() {
		c.reject(ctx, sessionID, o.GetOfferId(), "runner name differs from the offer")
		return
	}
	c.mu.Lock()
	check := OfferCheck{
		Repo:         o.GetRepo(),
		Public:       o.GetPublicRepo(),
		AcceptPublic: EffectiveAcceptPublic(c.mode, c.routerDefault),
		Allowlist:    slices.Clone(c.allowlist),
		TrustedHosts: slices.Clone(c.cfg.TrustedHosts),
	}
	c.mu.Unlock()
	if _, err := VerifyJIT(acc.GetEncodedJitConfig(), check); err != nil {
		// err describes the decoded host and repo only; the JIT itself is never logged.
		c.logger.Warn("refusing JIT config", "runner", o.GetRunnerName(), "repo", o.GetRepo(), "error", err)
		c.reject(ctx, sessionID, o.GetOfferId(), truncate("jit verification failed: "+err.Error(), maxReasonLen))
		return
	}
	rec := RunnerRecord{
		Name:       acc.GetRunnerName(),
		RunnerID:   acc.GetRunnerId(),
		Repo:       o.GetRepo(),
		Class:      class.String(),
		Executor:   l.Kind(),
		Phase:      "starting",
		AcceptedAt: time.Now().UTC(),
	}
	if err := c.putRecord(rec); err != nil {
		c.logger.Error("persisting the runner record failed", "runner", rec.Name, "error", err)
		c.reject(ctx, sessionID, o.GetOfferId(), "worker could not persist the runner record")
		return
	}
	a := &activeRunner{rec: rec, launcher: l}
	c.mu.Lock()
	c.active[rec.Name] = a
	c.mu.Unlock()
	unreserve()

	cache := o.GetCacheMode()
	if cache == "" || o.GetPublicRepo() {
		cache = "none"
	}
	if err := l.Start(ctx, rec, acc.GetEncodedJitConfig(), cache); err != nil {
		c.logger.Error("starting runner failed", "runner", rec.Name, "error", err)
		c.finished(ctx, a, fmt.Errorf("start failed: %w", err))
		return
	}
	rec.Phase = "running"
	c.mu.Lock()
	a.rec = rec
	c.mu.Unlock()
	if err := c.putRecord(rec); err != nil {
		c.logger.Warn("updating the runner record failed", "runner", rec.Name, "error", err)
	}
	c.logger.Info("runner started", "runner", rec.Name, "repo", rec.Repo, "class", rec.Class)
	c.report(ctx, &workerv1.ReportRunnerRequest{RunnerName: rec.Name, Phase: workerv1.RunnerPhase_RUNNER_PHASE_RUNNING})
	c.poke()
	c.goWait(ctx, a)
}

func (c *Client) reject(ctx context.Context, sessionID, offerID, reason string) {
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	if _, err := c.rpc.RejectOffer(rctx, authed(c, &workerv1.RejectOfferRequest{SessionId: sessionID, OfferId: offerID, Reason: reason})); err != nil {
		c.logger.Warn("rejecting offer failed", "offer", offerID, "error", err)
	}
}

// goWait waits for a runner in the background. A cancelled ctx means the
// worker is stopping: detach and keep the record for the next Run.
func (c *Client) goWait(ctx context.Context, a *activeRunner) {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		err := a.launcher.Wait(ctx, a.rec)
		if ctx.Err() != nil {
			c.logger.Info("detached from runner; it keeps running", "runner", a.rec.Name)
			return
		}
		c.finished(ctx, a, err)
	}()
}

// finished forgets an exited runner and reports EXITED (spec §4.4).
func (c *Client) finished(ctx context.Context, a *activeRunner, runErr error) {
	c.deleteRecord(a.rec.Name)
	c.mu.Lock()
	delete(c.active, a.rec.Name)
	silent, preempted, reason := a.silent, a.preempted, a.reason
	c.mu.Unlock()
	c.logger.Info("runner exited", "runner", a.rec.Name, "error", runErr)
	c.poke()
	if silent {
		return
	}
	msg := ""
	if runErr != nil {
		msg = runErr.Error()
	}
	if reason != "" {
		if msg != "" {
			msg = reason + ": " + msg
		} else {
			msg = reason
		}
	}
	c.report(ctx, &workerv1.ReportRunnerRequest{
		RunnerName: a.rec.Name,
		Phase:      workerv1.RunnerPhase_RUNNER_PHASE_EXITED,
		ExitCode:   exitCode(runErr),
		Error:      truncate(msg, maxReasonLen),
		Preempted:  preempted,
	})
}

// kill ends a runner; its Wait then returns and finished reports it.
func (c *Client) kill(ctx context.Context, name, reason string, preempted, silent bool) {
	c.mu.Lock()
	a, ok := c.active[name]
	var rec RunnerRecord
	if ok {
		a.reason, a.preempted, a.silent = reason, preempted, silent
		rec = a.rec
	}
	c.mu.Unlock()
	if !ok {
		c.logger.Info("no such runner to kill", "runner", name)
		return
	}
	kctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), killTimeout)
	defer cancel()
	if err := a.launcher.Kill(kctx, rec); err != nil {
		c.logger.Warn("killing runner failed", "runner", name, "error", err)
	}
}

func (c *Client) drainExpired(deadline time.Time) {
	c.mu.Lock()
	if c.availability != Draining {
		c.mu.Unlock()
		return
	}
	names := make([]string, 0, len(c.active))
	for name := range c.active {
		names = append(names, name)
	}
	c.mu.Unlock()
	for _, name := range names {
		c.logger.Warn("drain deadline passed; killing runner", "runner", name, "deadline", deadline)
		c.kill(context.Background(), name, "drain deadline passed", true, false)
	}
}

// report sends a runner report now, or queues it until the next session.
func (c *Client) report(ctx context.Context, rep *workerv1.ReportRunnerRequest) {
	c.mu.Lock()
	if !c.connected {
		c.queueLocked(rep)
		c.mu.Unlock()
		return
	}
	rep.SessionId = c.sessionID
	c.mu.Unlock()
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	if _, err := c.rpc.ReportRunner(rctx, authed(c, rep)); err != nil {
		c.logger.Warn("reporting runner state failed; will retry after reconnecting", "runner", rep.GetRunnerName(), "phase", rep.GetPhase(), "error", err)
		c.mu.Lock()
		c.queueLocked(rep)
		c.mu.Unlock()
	}
}

func (c *Client) queueLocked(rep *workerv1.ReportRunnerRequest) {
	if len(c.pending) >= maxPending {
		c.pending = c.pending[1:]
	}
	c.pending = append(c.pending, rep)
}

func (c *Client) flush(ctx context.Context) {
	c.mu.Lock()
	queued := c.pending
	c.pending = nil
	c.mu.Unlock()
	for _, rep := range queued {
		c.report(ctx, rep)
	}
}

func (c *Client) closeSession(ctx context.Context, sessionID string) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	defer cancel()
	if _, err := c.rpc.CloseSession(cctx, authed(c, &workerv1.CloseSessionRequest{SessionId: sessionID, Reason: "worker stopping"})); err != nil {
		c.logger.Debug("closing session failed", "error", err)
	}
}

func (c *Client) runningLocked() []*workerv1.RunnerState {
	states := make([]*workerv1.RunnerState, 0, len(c.active))
	for _, a := range c.active {
		st := &workerv1.RunnerState{
			RunnerName: a.rec.Name,
			RunnerId:   a.rec.RunnerID,
			Repo:       a.rec.Repo,
			Phase:      phaseToProto(a.rec.Phase),
		}
		if cl, err := runner.ParseClass(a.rec.Class); err == nil {
			st.Class = classToProto(cl)
		}
		states = append(states, st)
	}
	slices.SortFunc(states, func(a, b *workerv1.RunnerState) int { return strings.Compare(a.GetRunnerName(), b.GetRunnerName()) })
	return states
}

func (c *Client) listRecords() []RunnerRecord {
	c.recMu.Lock()
	defer c.recMu.Unlock()
	return c.cfg.Records.List()
}

func (c *Client) putRecord(r RunnerRecord) error {
	c.recMu.Lock()
	defer c.recMu.Unlock()
	return c.cfg.Records.Put(r)
}

func (c *Client) deleteRecord(name string) {
	c.recMu.Lock()
	defer c.recMu.Unlock()
	if err := c.cfg.Records.Delete(name); err != nil {
		c.logger.Warn("deleting runner record failed", "runner", name, "error", err)
	}
}

func (c *Client) setErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastErr = err.Error()
}

// poke asks the session loop for an immediate heartbeat.
func (c *Client) poke() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Client) backoff(attempt int) time.Duration {
	d := c.cfg.BackoffMin
	for i := 0; i < attempt && d < c.cfg.BackoffMax; i++ {
		d *= 2
	}
	d = min(d, c.cfg.BackoffMax)
	return d/2 + rand.N(d/2+1)
}

// authed wraps msg in a request carrying the worker credential. Never call
// it with c.mu held.
func authed[T any](c *Client, msg *T) *connect.Request[T] {
	req := connect.NewRequest(msg)
	c.mu.Lock()
	credential := c.credential
	c.mu.Unlock()
	req.Header().Set("Authorization", "Bearer "+credential)
	return req
}

func isRefused(err error) bool {
	switch connect.CodeOf(err) {
	case connect.CodeUnauthenticated, connect.CodePermissionDenied:
		return true
	default:
		return false
	}
}

// isSessionGone reports the router's "unknown session" (spec §4.6): the
// session expired or the router restarted, so a new OpenSession is needed.
func isSessionGone(err error) bool {
	switch connect.CodeOf(err) {
	case connect.CodeFailedPrecondition, connect.CodeNotFound:
		return true
	default:
		return false
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
