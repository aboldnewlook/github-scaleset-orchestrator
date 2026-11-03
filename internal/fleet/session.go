package fleet

import (
	"context"
	"fmt"
	"strings"
	"time"

	workerv1 "github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/event"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/naming"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

// OpenSessionParams is a worker's OpenSession request after authentication.
type OpenSessionParams struct {
	MachineID    string
	Capabilities *workerv1.Capabilities
	Availability workerv1.Availability
	Running      []*workerv1.RunnerState
	AcceptPublic bool
}

// OpenSessionResult is returned to the worker.
type OpenSessionResult struct {
	SessionID         string
	Effective         *workerv1.Capabilities
	Policy            *workerv1.PublicPolicy
	HeartbeatInterval time.Duration
	OfferTTL          time.Duration
	RejectedRunners   []string
}

func (f *Fleet) policy() *workerv1.PublicPolicy {
	return &workerv1.PublicPolicy{WorkerDefaultAccept: f.o.WorkerDefaultAccept != nil && f.o.WorkerDefaultAccept()}
}

func (f *Fleet) sessionLocked(sessionID, workerID string) (*member, error) {
	m := f.sessions[sessionID]
	if m == nil || m.workerID != workerID {
		return nil, ErrUnknownSession
	}
	return m, nil
}

func (f *Fleet) applyCapsLocked(m *member) {
	m.caps = Clamp(m.rawCaps, m.bounds, f.o.MaxSlotsPerWorker)
	m.slots = int(m.caps.GetSlots())
	m.classes = servedClasses(m.caps)
}

type adoptionSnapshot struct {
	workerID string
	slots    int
	segment  string
	bounds   Bounds
	classes  map[runner.Class]bool
	room     int
}

// settleTimeout bounds the GitHub lookups of one settle. They run on their own
// context so a client-side Heartbeat timeout does not cancel them.
const settleTimeout = 10 * time.Second

// deferral is a reported runner the router could not verify yet. since is
// carried across sessions, so cycling sessions cannot reset the hold cap.
type deferral struct {
	rs    *workerv1.RunnerState
	since time.Time
}

// lookupBudget is a token bucket of GitHub lookups for one worker. It lives on
// the fleet, keyed by worker ID, so cycling sessions does not refill it.
// Capacity is the worker's slots; it refills that many tokens per Timing.Lost,
// so a worker can cost GitHub at most slots lookups per lost-interval however
// often it opens sessions or heartbeats.
type lookupBudget struct {
	tokens float64
	last   time.Time
}

func (f *Fleet) takeLookup(workerID string, slots int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	capacity := float64(max(slots, 1))
	now := f.o.Now()
	b := f.budgets[workerID]
	if b == nil {
		b = &lookupBudget{tokens: capacity, last: now}
		f.budgets[workerID] = b
	}
	if el := now.Sub(b.last); el > 0 {
		b.tokens = min(capacity, b.tokens+capacity*el.Seconds()/f.o.Timing.Lost.Seconds())
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// deferCap bounds how long a runner may stay deferred. It reuses the cap on
// how long a lost worker's started runners are kept.
func (f *Fleet) deferCap() time.Duration { return f.o.Timing.LostStartedCap }

// OpenSession starts a worker's session (spec §4.3, §7). A second session
// while the first heartbeated within Timing.Lost is rejected. Runners the
// worker reports are reconciled against this router's placements; unknown
// ones are adopted only after binding and GitHub checks.
func (f *Fleet) OpenSession(ctx context.Context, w WorkerRecord, p OpenSessionParams) (OpenSessionResult, error) {
	f.mu.Lock()
	now := f.o.Now()
	m := f.members[w.ID]
	if m != nil && m.connected && now.Sub(m.lastSeen) < f.o.Timing.Lost {
		f.mu.Unlock()
		f.o.Logger.Warn("second session rejected while one is live", "worker", w.Name)
		f.publish(event.EventAlert, "", map[string]any{"alert": "second session rejected while one is live", "worker": w.Name})
		return OpenSessionResult{}, ErrSessionActive
	}
	sid, err := randomHex(16)
	if err != nil {
		f.mu.Unlock()
		return OpenSessionResult{}, fmt.Errorf("creating session: %w", err)
	}
	if m == nil {
		m = &member{name: w.Name, workerID: w.ID, segment: w.Segment, busy: map[string]*Reservation{}}
		f.members[w.ID] = m
	}
	if m.connected {
		f.endSessionLocked(m, false) // its heartbeats stopped: the old session is stale
	}
	m.bounds = w.Bounds
	m.rawCaps = p.Capabilities
	f.applyCapsLocked(m)
	m.availability = p.Availability
	m.acceptPublic = p.AcceptPublic
	m.drainRequested = false
	m.sessionID = sid
	m.directives = make(chan *workerv1.Directive, directiveBuffer)
	m.connected, m.timedOut, m.lastSeen = true, false, now
	f.sessions[sid] = m

	reported := make(map[string]*workerv1.RunnerState, len(p.Running))
	for _, rs := range p.Running {
		reported[rs.GetRunnerName()] = rs
	}
	var removals []removal
	for _, r := range snapshot(m.busy) {
		rs, ok := reported[r.name]
		switch {
		case ok:
			delete(reported, r.name)
			if rs.GetPhase() == workerv1.RunnerPhase_RUNNER_PHASE_RUNNING && r.phase == phaseAccepted {
				r.phase = phaseRunning
			}
		case r.phase == phaseAccepted: // minted, the worker no longer has it
			removals = append(removals, removal{r.req.Repo, r.runnerID})
			r.finishLocked(ErrWorkerLost)
		case r.phase == phaseRunning: // it finished while we could not hear about it
			r.finishLocked(nil)
		}
	}
	var candidates []*workerv1.RunnerState
	for _, rs := range p.Running {
		if _, unknown := reported[rs.GetRunnerName()]; unknown {
			candidates = append(candidates, rs)
			delete(reported, rs.GetRunnerName())
		}
	}
	prevDeferred := m.deferred
	m.deferred = nil
	snap := adoptionSnapshot{workerID: m.workerID, slots: m.slots, segment: m.segment, bounds: m.bounds, classes: m.classes, room: m.free()}
	eff := m.caps
	f.mu.Unlock()

	f.removeRunners(ctx, removals)
	adopted, rejected, deferred := f.verifyAdoptions(ctx, m, snap, candidates)

	f.mu.Lock()
	var registered []*Reservation
	for _, r := range adopted {
		if m.sessionID != sid || m.free() == 0 || m.busy[r.name] != nil {
			rejected = append(rejected, r.name)
			continue
		}
		m.busy[r.name] = r
		registered = append(registered, r)
	}
	for _, d := range deferred {
		if m.sessionID != sid || m.free() == 0 {
			rejected = append(rejected, d.rs.GetRunnerName())
			continue
		}
		since := now
		if prev := prevDeferred[d.rs.GetRunnerName()]; prev != nil {
			since = prev.since // a new session does not restart the hold cap
		}
		if now.Sub(since) >= f.deferCap() {
			rejected = append(rejected, d.rs.GetRunnerName()) // reported back: the worker kills it
			continue
		}
		if m.deferred == nil {
			m.deferred = map[string]*deferral{}
		}
		m.deferred[d.rs.GetRunnerName()] = &deferral{rs: d.rs, since: since}
	}
	if len(m.deferred) > 0 {
		m.settleAt = now // this round counts as a settle
	}
	f.mu.Unlock()
	for _, r := range registered {
		if !f.o.Adopter.Adopt(r.req.Repo, r.req.Class, r) {
			r.finish(errAdoptionRefused)
			rejected = append(rejected, r.name)
			continue
		}
		f.publish(event.EventRunnerAdopted, r.req.Repo, map[string]any{"runner": r.name, "worker": m.name})
	}
	f.o.Logger.Info("worker session opened", "worker", m.name, "slots", eff.GetSlots(), "adopted", len(registered), "rejected", len(rejected), "deferred", len(deferred))
	f.publish(event.EventWorkerConnected, "", map[string]any{"worker": m.name, "slots": eff.GetSlots()})
	f.notify()
	return OpenSessionResult{
		SessionID: sid, Effective: eff, Policy: f.policy(),
		HeartbeatInterval: f.o.Timing.Heartbeat, OfferTTL: f.o.Timing.OfferTTL, RejectedRunners: rejected,
	}, nil
}

type adoptionVerdict int

const (
	verdictAdopt  adoptionVerdict = iota
	verdictReject                 // positively not ours, or outside the worker's bounds
	verdictDefer                  // cannot tell yet: scale set unknown or the worker's lookup budget is spent
)

func (f *Fleet) verifyAdoptions(ctx context.Context, m *member, s adoptionSnapshot, candidates []*workerv1.RunnerState) (adopted []*Reservation, rejected []string, deferred []*deferral) {
	for _, rs := range candidates {
		r, v := f.verifyAdoption(ctx, m, &s, rs)
		switch v {
		case verdictAdopt:
			adopted = append(adopted, r)
		case verdictDefer:
			deferred = append(deferred, &deferral{rs: rs})
		default:
			rejected = append(rejected, rs.GetRunnerName())
		}
	}
	return adopted, rejected, deferred
}

// verifyAdoption applies spec §4.3 "Binding": the runner name carries this
// worker's segment, its class and repo are inside the worker's bounds, and
// GitHub confirms it exists in that repo's scale set for the class.
//
// GitHub is asked only about plausible candidates (every local check passed,
// the repo has a scale set for the class), and at most s.room times: each
// lookup consumes a slot of room whether or not it succeeds, so a worker
// cannot make the router fan out GitHub calls.
//
// Not knowing the repo's scale set yet (the router restarted while GitHub is
// slow) or failing to reach GitHub is not a verdict: the runner is deferred,
// not rejected, and a later heartbeat settles it. Only a positive answer from
// GitHub, or a failed local check, rejects.
func (f *Fleet) verifyAdoption(ctx context.Context, m *member, s *adoptionSnapshot, rs *workerv1.RunnerState) (*Reservation, adoptionVerdict) {
	if f.o.GitHub == nil || f.o.Adopter == nil {
		return nil, verdictReject
	}
	name, repo := rs.GetRunnerName(), rs.GetRepo()
	if seg, ok := naming.RunnerWorkerSegment(name); !ok || seg != s.segment || !hasRunnerSuffix(name) {
		return nil, verdictReject
	}
	class, err := ClassFromProto(rs.GetClass())
	if err != nil || !s.classes[class] || !repoAllowed(s.bounds.Repos, repo) {
		return nil, verdictReject
	}
	scaleSetID, ok := f.o.GitHub.ScaleSetID(repo, class)
	if !ok {
		return nil, verdictDefer
	}
	if s.room <= 0 {
		return nil, verdictReject
	}
	if !f.takeLookup(s.workerID, s.slots) {
		return nil, verdictDefer // this worker has used its share of GitHub lookups
	}
	s.room--
	ref, err := f.o.GitHub.GetRunnerByName(ctx, repo, name)
	if err != nil {
		return nil, verdictDefer
	}
	if ref == nil || ref.ScaleSetID != scaleSetID {
		return nil, verdictReject
	}
	r := newReservation(f, m, name, PlacementRequest{Repo: repo, Class: class})
	r.runnerID = ref.ID
	if rs.GetPhase() == workerv1.RunnerPhase_RUNNER_PHASE_RUNNING {
		r.phase, r.started = phaseRunning, true // keep it if the worker is lost again
	} else {
		r.phase, r.acceptedAt = phaseAccepted, f.o.Now()
	}
	return r, verdictAdopt
}

// hasRunnerSuffix reports whether the name ends in the 6-character [a-z0-9]
// suffix naming.RunnerName appends. Legacy scaler names (gso-<repo>-<uuid8>)
// carry 8 hex characters and are never adoptable by a worker.
func hasRunnerSuffix(name string) bool {
	i := strings.LastIndex(name, "-")
	if i < 0 || len(name)-i-1 != 6 {
		return false
	}
	for _, c := range name[i+1:] {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// Directives returns the session's directive stream; it is closed when the
// session ends.
func (f *Fleet) Directives(sessionID, workerID string) (<-chan *workerv1.Directive, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.sessionLocked(sessionID, workerID)
	if err != nil {
		return nil, err
	}
	return m.directives, nil
}

// Heartbeat records liveness, availability and accept_public. Running
// entries are honoured only for runners placed on this worker.
func (f *Fleet) Heartbeat(ctx context.Context, sessionID, workerID string, availability workerv1.Availability, running []*workerv1.RunnerState, acceptPublic bool) (*workerv1.PublicPolicy, error) {
	f.mu.Lock()
	m, err := f.sessionLocked(sessionID, workerID)
	if err != nil {
		f.mu.Unlock()
		return nil, err
	}
	changed := m.availability != availability || m.acceptPublic != acceptPublic
	m.availability, m.acceptPublic, m.lastSeen = availability, acceptPublic, f.o.Now()
	for _, rs := range running {
		if r := m.busy[rs.GetRunnerName()]; r != nil && rs.GetPhase() == workerv1.RunnerPhase_RUNNER_PHASE_RUNNING && r.phase == phaseAccepted {
			r.phase = phaseRunning
		}
	}
	var pending []*workerv1.RunnerState
	if len(m.deferred) > 0 {
		current := make(map[string]*workerv1.RunnerState, len(running))
		for _, rs := range running {
			current[rs.GetRunnerName()] = rs
		}
		now := f.o.Now()
		for name, d := range m.deferred {
			cur, reported := current[name]
			switch {
			case !reported:
				delete(m.deferred, name) // it finished
				changed = true
			case now.Sub(d.since) >= f.deferCap():
				// Still unverifiable after the cap (unknown scale set, an
				// unserved repo, GitHub down for hours): stop holding it.
				delete(m.deferred, name)
				f.sendLocked(m, cancelDirective(name, "could not be verified within the hold limit"))
				changed = true
			default:
				d.rs = cur // settle what the worker reports now
				pending = append(pending, cur)
			}
		}
	}
	f.mu.Unlock()
	// Settling is paced, single-flight and skipped while no scale set is
	// known: Heartbeat RPCs are not rate limited, so they must not be a way to
	// make the router query GitHub.
	if len(pending) > 0 && f.anyScaleSetKnown(pending) {
		if snap, ok := f.claimSettle(m, sessionID); ok {
			snap.room = len(pending)
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
			if f.settleDeferred(sctx, sessionID, m, snap, pending) {
				changed = true
			}
			cancel()
		}
	}
	if changed {
		f.notify()
	}
	return f.policy(), nil
}

// anyScaleSetKnown reports whether a lookup could happen for any of the runners.
func (f *Fleet) anyScaleSetKnown(pending []*workerv1.RunnerState) bool {
	if f.o.GitHub == nil {
		return false
	}
	for _, rs := range pending {
		class, err := ClassFromProto(rs.GetClass())
		if err != nil {
			continue
		}
		if _, ok := f.o.GitHub.ScaleSetID(rs.GetRepo(), class); ok {
			return true
		}
	}
	return false
}

// claimSettle admits one settle per member at a time and at most one per
// heartbeat interval.
func (f *Fleet) claimSettle(m *member, sid string) (adoptionSnapshot, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.o.Now()
	if m.sessionID != sid || m.settling || now.Sub(m.settleAt) < f.o.Timing.Heartbeat {
		return adoptionSnapshot{}, false
	}
	m.settling, m.settleAt = true, now
	return adoptionSnapshot{workerID: m.workerID, slots: m.slots, segment: m.segment, bounds: m.bounds, classes: m.classes}, true
}

// settleDeferred retries the adoption of runners deferred at OpenSession. A
// runner still unverifiable stays deferred; one GitHub says is not ours is
// cancelled on the worker; a verified one is adopted. It reports whether any
// slot accounting changed.
func (f *Fleet) settleDeferred(ctx context.Context, sid string, m *member, snap adoptionSnapshot, pending []*workerv1.RunnerState) bool {
	// Lookup errors are GitHub-wide (5xx, network, 429, an expired token), not
	// a verdict on the runner, so failures never promote a deferral to a
	// rejection: the lookup budget bounds the cost and deferCap the hold time.
	adopted, rejected, _ := f.verifyAdoptions(ctx, m, snap, pending)
	f.mu.Lock()
	m.settling = false
	var registered []*Reservation
	for _, r := range adopted {
		if m.sessionID != sid || m.deferred[r.name] == nil {
			continue // the session or the runner moved on while we asked GitHub
		}
		delete(m.deferred, r.name)
		m.busy[r.name] = r // the deferred entry already held this slot
		registered = append(registered, r)
	}
	for _, name := range rejected {
		if m.sessionID != sid || m.deferred[name] == nil {
			continue
		}
		delete(m.deferred, name)
		f.sendLocked(m, cancelDirective(name, "not recognised by GitHub for this router"))
	}
	f.mu.Unlock()
	for _, r := range registered {
		if !f.o.Adopter.Adopt(r.req.Repo, r.req.Class, r) {
			r.finish(errAdoptionRefused)
			f.mu.Lock()
			f.sendLocked(m, cancelDirective(r.name, "adoption refused"))
			f.mu.Unlock()
			continue
		}
		f.publish(event.EventRunnerAdopted, r.req.Repo, map[string]any{"runner": r.name, "worker": m.name})
	}
	return len(registered) > 0 || len(rejected) > 0
}

// AcceptOffer mints the runner for an offer issued to this session and
// returns the JIT config (spec §4.3: "minted only once a specific worker has
// committed to a slot").
func (f *Fleet) AcceptOffer(ctx context.Context, sessionID, workerID, offerID string) (*workerv1.AcceptOfferResponse, error) {
	f.mu.Lock()
	m, err := f.sessionLocked(sessionID, workerID)
	if err != nil {
		f.mu.Unlock()
		return nil, err
	}
	r := f.offers[offerID]
	if r == nil || r.finished || r.phase != phaseOffered || f.o.Now().After(r.offerExpires) {
		f.mu.Unlock()
		return nil, ErrOfferNotFound
	}
	if r.m != m {
		f.mu.Unlock()
		return nil, ErrNotPermitted
	}
	// The worker may have been drained, narrowed or marked unavailable since
	// the offer went out: no JIT is minted for a repo it may no longer take.
	if !f.eligibleLocked(m, r.req) {
		r.finishLocked(ErrOfferNotTaken)
		f.mu.Unlock()
		return nil, ErrNotPermitted
	}
	r.phase, r.acceptedAt = phaseAccepted, f.o.Now()
	mint := r.mint
	f.mu.Unlock()

	id, jit, err := mint(ctx, r.name)
	if err != nil {
		r.finish(fmt.Errorf("minting JIT config: %w", err))
		return nil, fmt.Errorf("minting JIT config: %w", err)
	}
	f.mu.Lock()
	if r.finished { // released while minting: the runner must not stay usable
		f.mu.Unlock()
		f.removeRunner(ctx, r.req.Repo, id)
		return nil, ErrOfferNotFound
	}
	r.runnerID = id
	f.mu.Unlock()
	return &workerv1.AcceptOfferResponse{RunnerName: r.name, RunnerId: id, EncodedJitConfig: jit}, nil
}

// RejectOffer ends an offer. After an accept (JIT verification failed on the
// worker) the minted runner is removed.
func (f *Fleet) RejectOffer(ctx context.Context, sessionID, workerID, offerID, reason string) error {
	f.mu.Lock()
	m, err := f.sessionLocked(sessionID, workerID)
	if err != nil {
		f.mu.Unlock()
		return err
	}
	r := f.offers[offerID]
	if r == nil || r.finished {
		f.mu.Unlock()
		return ErrOfferNotFound
	}
	if r.m != m {
		f.mu.Unlock()
		return ErrNotPermitted
	}
	id := r.runnerID
	r.finishLocked(fmt.Errorf("%w: rejected by the worker", ErrOfferNotTaken))
	f.mu.Unlock()
	f.removeRunner(ctx, r.req.Repo, id)
	f.o.Logger.Info("offer rejected", "worker", m.name, "runner", r.name, "reason", reason)
	f.publish(event.EventOfferRejected, r.req.Repo, map[string]any{"worker": m.name, "runner": r.name, "reason": reason})
	return nil
}

// ReportRunner records a runner phase. Only runners this router placed on
// (or adopted for) this worker are accepted.
func (f *Fleet) ReportRunner(_ context.Context, sessionID, workerID string, rep *workerv1.ReportRunnerRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.sessionLocked(sessionID, workerID)
	if err != nil {
		return err
	}
	r := m.busy[rep.GetRunnerName()]
	if r == nil {
		// Already released (e.g. JobCompleted won the race): idempotent. Only a
		// runner currently held by another member is a foreign report.
		for _, other := range f.membersLocked() {
			if other != m && other.busy[rep.GetRunnerName()] != nil {
				return ErrNotPermitted
			}
		}
		return nil
	}
	switch rep.GetPhase() {
	case workerv1.RunnerPhase_RUNNER_PHASE_RUNNING:
		if r.phase == phaseAccepted {
			r.phase = phaseRunning
		}
	case workerv1.RunnerPhase_RUNNER_PHASE_EXITED:
		var exitErr error
		switch {
		case rep.GetPreempted():
			exitErr = errPreempted
		case rep.GetExitCode() != 0 || rep.GetError() != "":
			exitErr = fmt.Errorf("runner exited with code %d: %s", rep.GetExitCode(), rep.GetError())
		}
		r.finishLocked(exitErr)
	}
	return nil
}

// CloseSession ends a session cleanly. Runners keep their reservations under
// the lost-worker rules until the worker reconnects and reports them.
func (f *Fleet) CloseSession(sessionID, workerID string) error {
	f.mu.Lock()
	m, err := f.sessionLocked(sessionID, workerID)
	if err != nil {
		f.mu.Unlock()
		return err
	}
	f.endSessionLocked(m, false)
	f.mu.Unlock()
	f.notify()
	return nil
}

// DrainWorker sends a Drain directive and stops offers until the worker opens
// a new session. A worker that is not connected has nothing to drain.
func (f *Fleet) DrainWorker(workerID, reason string) error {
	f.mu.Lock()
	m := f.members[workerID]
	if m == nil {
		f.mu.Unlock()
		return nil
	}
	m.drainRequested = true
	f.sendLocked(m, drainDirective(reason))
	f.dropIneligibleOffersLocked(m)
	f.mu.Unlock()
	f.notify()
	return nil
}

// RevokeWorker cancels the worker's runners, removes the minted ones where
// GitHub allows it, closes its session and forgets it. The registry refuses
// the credential from then on (B6).
func (f *Fleet) RevokeWorker(ctx context.Context, workerID string) error {
	f.mu.Lock()
	m := f.members[workerID]
	if m == nil {
		f.mu.Unlock()
		return nil
	}
	var removals []removal
	for _, r := range snapshot(m.busy) {
		f.sendLocked(m, cancelDirective(r.name, "worker revoked"))
		removals = append(removals, removal{r.req.Repo, r.runnerID})
		r.finishLocked(errWorkerRevoked)
	}
	if m.connected {
		f.endSessionLocked(m, false)
	}
	delete(f.members, workerID)
	delete(f.budgets, workerID)
	f.mu.Unlock()
	f.removeRunners(ctx, removals) // JobStillRunning is logged: an assigned job runs to completion (spec §7)
	f.publish(event.EventWorkerRevoked, "", map[string]any{"worker": m.name})
	f.notify()
	return nil
}

// UpdateBounds re-clamps a connected worker after `gso workers update`.
func (f *Fleet) UpdateBounds(workerID string, b Bounds) {
	f.mu.Lock()
	m := f.members[workerID]
	if m == nil {
		f.mu.Unlock()
		return
	}
	m.bounds = b
	f.applyCapsLocked(m)
	f.dropIneligibleOffersLocked(m)
	f.mu.Unlock()
	f.notify()
}
