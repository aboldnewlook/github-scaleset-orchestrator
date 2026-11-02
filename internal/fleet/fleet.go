package fleet

import (
	"context"
	"encoding/json"
	"log/slog"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	workerv1 "github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/event"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/naming"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/timing"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

// Timing holds the fleet's timers (spec §4.5).
type Timing struct {
	Heartbeat, Lost, Keepalive, OfferTTL, StartTTL, Grace, LostUnstartedRemove, LostStartedCap, Tick time.Duration
}

// DefaultTiming returns the spec §4.5 values, each scaled by GSO_TIMING_SCALE.
func DefaultTiming() Timing {
	return Timing{
		Heartbeat:           timing.Scale(10 * time.Second),
		Lost:                timing.Scale(30 * time.Second),
		Keepalive:           timing.Scale(15 * time.Second),
		OfferTTL:            timing.Scale(15 * time.Second),
		StartTTL:            timing.Scale(10 * time.Minute),
		Grace:               timing.Scale(30 * time.Second),
		LostUnstartedRemove: timing.Scale(60 * time.Second),
		LostStartedCap:      timing.Scale(6 * time.Hour),
		Tick:                timing.Scale(time.Second),
	}
}

// RunnerRef is what the fleet needs to know about a registered runner.
type RunnerRef struct {
	ID         int64
	ScaleSetID int
}

// GitHub is what the fleet needs to verify adoption and clean up runners.
type GitHub interface {
	GetRunnerByName(ctx context.Context, repo, name string) (*RunnerRef, error) // nil, nil when absent
	RemoveRunner(ctx context.Context, repo string, runnerID int64) error
	ScaleSetID(repo string, class runner.Class) (int, bool)
}

// Adopter receives runners the fleet verified after a router restart.
type Adopter interface {
	Adopt(repo string, class runner.Class, res *Reservation) bool
}

// LocalExecutor runs local-member runners (today's subprocess or docker executor).
type LocalExecutor interface {
	ForRepo(repo, cache string) runner.Executor
}

// Options configures a Fleet.
type Options struct {
	Logger              *slog.Logger
	Bus                 *event.Bus
	Timing              Timing
	GitHub              GitHub
	Adopter             Adopter
	MaxSlotsPerWorker   int
	WorkerDefaultAccept func() bool
	RemoteEnabled       bool
	Now                 func() time.Time
}

// PlacementRequest describes one runner to place.
type PlacementRequest struct {
	Repo   string
	Class  runner.Class
	Public bool   // offers only to accept_public members; sets Offer.public_repo
	Cache  string // "none" | "repo"; forced to "none" when Public
	Member string // pin to one member (warm runners, C4); "" = any
}

// MemberStatus is a snapshot of one member for live_status.
type MemberStatus struct {
	Name, WorkerID   string
	Local, Connected bool
	Availability     string // "available" | "draining" | "unavailable" | "lost"
	Slots, Busy      int
	Classes          []string
	AcceptPublic     bool
	LastSeen         time.Time
}

type member struct {
	name, workerID, segment string
	local                   bool
	class                   runner.Class  // local member only
	exec                    LocalExecutor // local member only
	slots                   int
	busy                    map[string]*Reservation // by runner name

	// remote members only
	bounds         Bounds
	rawCaps        *workerv1.Capabilities
	caps           *workerv1.Capabilities
	classes        map[runner.Class]bool
	availability   workerv1.Availability
	drainRequested bool
	acceptPublic   bool
	sessionID      string
	directives     chan *workerv1.Directive
	connected      bool
	timedOut       bool
	lastSeen       time.Time
	disconnectedAt time.Time
}

func (m *member) free() int { return max(m.slots-len(m.busy), 0) }

func (m *member) availabilityString() string {
	switch {
	case m.local:
		return "available"
	case !m.connected && m.timedOut:
		return "lost"
	case !m.connected:
		return "unavailable"
	case m.drainRequested || m.availability == workerv1.Availability_AVAILABILITY_DRAINING:
		return "draining"
	case m.availability == workerv1.Availability_AVAILABILITY_AVAILABLE:
		return "available"
	default:
		return "unavailable"
	}
}

// Fleet tracks members and places runners on them.
type Fleet struct {
	o Options

	mu       sync.Mutex
	local    *member
	members  map[string]*member      // remote, by worker ID
	sessions map[string]*member      // by session ID
	offers   map[string]*Reservation // by offer ID
	started  time.Time
	onChange []func()
}

const directiveBuffer = 64

// New returns an empty fleet.
func New(o Options) *Fleet {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Timing == (Timing{}) {
		o.Timing = DefaultTiming()
	}
	if o.Timing.Tick <= 0 {
		o.Timing.Tick = time.Second
	}
	if o.MaxSlotsPerWorker <= 0 {
		o.MaxSlotsPerWorker = 16
	}
	return &Fleet{
		o:        o,
		members:  map[string]*member{},
		sessions: map[string]*member{},
		offers:   map[string]*Reservation{},
	}
}

// Start records the start time (for the reconnect grace) and runs the timer
// sweep every Timing.Tick until ctx ends.
func (f *Fleet) Start(ctx context.Context) {
	f.mu.Lock()
	f.started = f.o.Now()
	f.mu.Unlock()
	go func() {
		ticker := time.NewTicker(f.o.Timing.Tick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				f.sweep(ctx)
			}
		}
	}()
}

// SetLocal sizes the in-process local member. slots 0 leaves no capacity;
// runners already placed keep their reservations.
func (f *Fleet) SetLocal(slots int, class runner.Class, exec LocalExecutor) {
	f.mu.Lock()
	if f.local == nil {
		f.local = &member{name: naming.LocalWorkerSegment, segment: naming.LocalWorkerSegment, local: true, busy: map[string]*Reservation{}}
	}
	f.local.slots = max(slots, 0)
	f.local.class = class
	f.local.exec = exec
	f.mu.Unlock()
	f.notify()
}

// LocalSlots returns the local member's slots (0 when there is none).
func (f *Fleet) LocalSlots() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.local == nil {
		return 0
	}
	return f.local.slots
}

// OnChange registers fn; it runs in its own goroutine whenever membership,
// availability, slots or bounds change.
func (f *Fleet) OnChange(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onChange = append(f.onChange, fn)
}

func (f *Fleet) notify() {
	f.mu.Lock()
	fns := append([]func(){}, f.onChange...)
	f.mu.Unlock()
	for _, fn := range fns {
		go fn()
	}
}

func (f *Fleet) publish(typ event.EventType, repo string, payload map[string]any) {
	if f.o.Bus == nil {
		return
	}
	data, _ := json.Marshal(payload)
	f.o.Bus.Publish(event.Event{Time: f.o.Now(), Type: typ, Repo: repo, Payload: data})
}

func (f *Fleet) membersLocked() []*member {
	out := make([]*member, 0, len(f.members)+1)
	if f.local != nil {
		out = append(out, f.local)
	}
	for _, m := range f.members {
		out = append(out, m)
	}
	return out
}

func repoAllowed(patterns []string, repo string) bool {
	if len(patterns) == 0 {
		return true
	}
	repo = strings.ToLower(repo)
	for _, p := range patterns {
		p = strings.ToLower(p)
		if p == repo {
			return true
		}
		if ok, err := path.Match(p, repo); err == nil && ok {
			return true
		}
	}
	return false
}

// eligibleLocked applies spec §5.4 rule 1–2 except "has free slots".
func (f *Fleet) eligibleLocked(m *member, req PlacementRequest) bool {
	if req.Member != "" && m.name != req.Member {
		return false
	}
	if m.local {
		return m.slots > 0 && req.Class == m.class && !req.Public
	}
	if !m.connected || m.drainRequested || m.availability != workerv1.Availability_AVAILABILITY_AVAILABLE {
		return false
	}
	if !m.classes[req.Class] || !repoAllowed(m.bounds.Repos, req.Repo) {
		return false
	}
	return !req.Public || m.acceptPublic
}

// Free sums the free slots of eligible members.
func (f *Fleet) Free(req PlacementRequest) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, m := range f.membersLocked() {
		if f.eligibleLocked(m, req) {
			n += m.free()
		}
	}
	return n
}

// Slots sums the slots of eligible members.
func (f *Fleet) Slots(req PlacementRequest) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, m := range f.membersLocked() {
		if f.eligibleLocked(m, req) {
			n += m.slots
		}
	}
	return n
}

// Reserve takes a slot on the eligible member with the most free slots (ties:
// name order) and names the runner after that member. It never blocks.
func (f *Fleet) Reserve(req PlacementRequest) (*Reservation, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.o.RemoteEnabled && !f.started.IsZero() && f.o.Now().Before(f.started.Add(f.o.Timing.Grace)) {
		return nil, false // spec §7 "Router restart": place nothing new during the grace
	}
	if req.Public {
		req.Cache = "none"
	}
	var best *member
	for _, m := range f.membersLocked() {
		if !f.eligibleLocked(m, req) || m.free() == 0 {
			continue
		}
		if best == nil || m.free() > best.free() || (m.free() == best.free() && m.name < best.name) {
			best = m
		}
	}
	if best == nil {
		return nil, false
	}
	r := newReservation(f, best, naming.RunnerName(best.segment, req.Repo), req)
	best.busy[r.name] = r
	return r, true
}

// Members returns a snapshot of every member, sorted by name.
func (f *Fleet) Members() []MemberStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]MemberStatus, 0, len(f.members)+1)
	for _, m := range f.membersLocked() {
		st := MemberStatus{
			Name: m.name, WorkerID: m.workerID, Local: m.local, Connected: m.local || m.connected,
			Availability: m.availabilityString(), Slots: m.slots, Busy: len(m.busy),
			AcceptPublic: m.acceptPublic, LastSeen: m.lastSeen,
		}
		if m.local {
			st.Classes = []string{m.class.String()}
		} else {
			st.Classes = classStrings(m.classes)
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

type removal struct {
	repo     string
	runnerID int64
}

func (f *Fleet) removeRunners(ctx context.Context, rs []removal) {
	for _, r := range rs {
		f.removeRunner(ctx, r.repo, r.runnerID)
	}
}

// removeRunner deregisters a minted runner so its JIT config can no longer
// take a job (spec §7). Failures are logged; GitHub expires it within a day.
func (f *Fleet) removeRunner(ctx context.Context, repo string, runnerID int64) {
	if f.o.GitHub == nil || runnerID == 0 {
		return
	}
	if err := f.o.GitHub.RemoveRunner(ctx, repo, runnerID); err != nil {
		f.o.Logger.Warn("removing runner failed", "repo", repo, "runner_id", runnerID, "error", err)
		return
	}
	f.publish(event.EventRunnerRemoved, repo, map[string]any{"runner_id": runnerID})
}

func (f *Fleet) sendLocked(m *member, d *workerv1.Directive) bool {
	if m.directives == nil {
		return false
	}
	select {
	case m.directives <- d:
		return true
	default:
		f.o.Logger.Warn("worker directive queue full; dropping directive", "worker", m.name)
		return false
	}
}

// endSessionLocked closes the member's session and stream. Pending offers
// cannot be delivered any more, so they end as not taken.
func (f *Fleet) endSessionLocked(m *member, timedOut bool) {
	if m.sessionID != "" {
		delete(f.sessions, m.sessionID)
	}
	m.sessionID = ""
	if m.directives != nil {
		close(m.directives)
		m.directives = nil
	}
	m.connected = false
	m.timedOut = timedOut
	m.disconnectedAt = f.o.Now()
	for _, r := range snapshot(m.busy) {
		if r.phase == phaseOffered {
			r.finishLocked(ErrOfferNotTaken)
		}
	}
}

func snapshot(busy map[string]*Reservation) []*Reservation {
	out := make([]*Reservation, 0, len(busy))
	for _, r := range busy {
		out = append(out, r)
	}
	return out
}

// sweep applies every timer of spec §4.5/§7: lost workers, offer TTL, start
// TTL and the lost-runner rules. Start runs it every Timing.Tick.
func (f *Fleet) sweep(ctx context.Context) {
	var removals []removal
	var lost []string
	f.mu.Lock()
	now := f.o.Now()
	t := f.o.Timing
	for _, m := range f.members {
		// Runner timers first, so a Cancel directive is queued before a lost
		// worker's stream is closed below.
		for _, r := range snapshot(m.busy) {
			switch {
			case r.phase == phaseOffered && now.After(r.offerExpires):
				r.finishLocked(ErrOfferNotTaken)
			case r.phase == phaseAccepted && now.Sub(r.acceptedAt) > t.StartTTL:
				f.sendLocked(m, cancelDirective(r.name, "runner did not start in time"))
				removals = append(removals, removal{r.req.Repo, r.runnerID})
				r.finishLocked(ErrStartTimeout)
			case !m.connected && !r.started && now.Sub(m.disconnectedAt) > t.LostUnstartedRemove:
				removals = append(removals, removal{r.req.Repo, r.runnerID})
				r.finishLocked(ErrWorkerLost)
			case !m.connected && r.started && now.Sub(m.disconnectedAt) > t.LostStartedCap:
				r.finishLocked(ErrWorkerLost)
			}
		}
		if m.connected && now.Sub(m.lastSeen) > t.Lost {
			f.endSessionLocked(m, true)
			lost = append(lost, m.name)
		}
	}
	f.mu.Unlock()
	for _, name := range lost {
		f.o.Logger.Warn("worker lost", "worker", name)
		f.publish(event.EventWorkerLost, "", map[string]any{"worker": name})
	}
	f.removeRunners(ctx, removals)
	if len(lost) > 0 {
		f.notify()
	}
}
