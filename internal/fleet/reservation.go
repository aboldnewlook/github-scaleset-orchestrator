package fleet

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	workerv1 "github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

var (
	ErrOfferNotTaken  = errors.New("offer not taken")
	ErrStartTimeout   = errors.New("runner did not start in time")
	ErrWorkerLost     = errors.New("worker lost")
	ErrSessionActive  = errors.New("session active")
	ErrUnknownSession = errors.New("unknown session")
	ErrOfferNotFound  = errors.New("offer not found")
	ErrNotPermitted   = errors.New("not permitted for this worker")

	errPreempted       = errors.New("runner preempted by a worker drain")
	errWorkerRevoked   = errors.New("worker revoked")
	errAdoptionRefused = errors.New("adoption refused by the scaler")
	errLocalAdoption   = errors.New("local runners cannot be adopted")
	errNoExecutor      = errors.New("local member has no executor")
)

// MintFunc mints the JIT config for runnerName. The fleet calls it only once
// a slot is committed: locally right away, remotely inside AcceptOffer.
type MintFunc func(ctx context.Context, runnerName string) (runnerID int64, encodedJIT string, err error)

type phase int

const (
	phaseReserved phase = iota // reserved; local before mint
	phaseOffered               // offer sent, waiting for accept
	phaseAccepted              // minted (or minting), waiting for RUNNING
	phaseRunning
)

// Reservation is one slot on one member, from Reserve until the first
// completion signal (spec §7: "first completion signal wins").
type Reservation struct {
	f    *Fleet
	m    *member
	name string
	req  PlacementRequest
	done chan struct{}

	// guarded by f.mu
	phase        phase
	runnerID     int64
	offerID      string
	offerExpires time.Time
	acceptedAt   time.Time
	started      bool
	mint         MintFunc
	finished     bool
	err          error
}

func newReservation(f *Fleet, m *member, name string, req PlacementRequest) *Reservation {
	return &Reservation{f: f, m: m, name: name, req: req, done: make(chan struct{})}
}

func (r *Reservation) RunnerName() string  { return r.name }
func (r *Reservation) Repo() string        { return r.req.Repo }
func (r *Reservation) Class() runner.Class { return r.req.Class }
func (r *Reservation) Member() string      { return r.m.name }

// RunnerID is 0 until the runner is minted.
func (r *Reservation) RunnerID() int64 {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	return r.runnerID
}

// Run places the runner and blocks until it finishes. mint == nil means the
// runner was adopted after a router restart: Run only waits. The slot is
// released when Run returns, or earlier by Release.
func (r *Reservation) Run(ctx context.Context, mint MintFunc) error {
	if r.m.local {
		return r.runLocal(ctx, mint)
	}
	if mint != nil {
		r.f.offer(r, mint)
	}
	select {
	case <-r.done:
	case <-ctx.Done():
		r.finish(ctx.Err()) // no-op when a completion signal already won
	}
	return r.result()
}

func (r *Reservation) runLocal(ctx context.Context, mint MintFunc) error {
	if mint == nil {
		r.finish(errLocalAdoption)
		return errLocalAdoption
	}
	r.f.mu.Lock()
	exec := r.m.exec
	r.f.mu.Unlock()
	if exec == nil {
		r.finish(errNoExecutor)
		return errNoExecutor
	}
	id, jit, err := mint(ctx, r.name)
	if err != nil {
		r.finish(err)
		return err
	}
	r.f.mu.Lock()
	r.runnerID = id
	r.phase = phaseRunning
	r.f.mu.Unlock()
	runErr := exec.ForRepo(r.req.Repo, r.req.Cache).Run(ctx, r.name, jit)
	r.finish(runErr)
	return runErr
}

func (r *Reservation) result() error {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	return r.err
}

// Release frees the slot. It is idempotent: the first completion signal wins.
func (r *Reservation) Release() { r.finish(nil) }

// Cancel asks a remote worker to stop the runner. For the local member it is
// a no-op: the caller cancels Run's context.
func (r *Reservation) Cancel(reason string) {
	if r.m.local {
		return
	}
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	if !r.finished {
		r.f.sendLocked(r.m, cancelDirective(r.name, reason))
	}
}

// MarkStarted records that GitHub reported JobStarted for this runner.
func (r *Reservation) MarkStarted() {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	r.started = true
}

func (r *Reservation) finish(err error) {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	r.finishLocked(err)
}

func (r *Reservation) finishLocked(err error) {
	if r.finished {
		return
	}
	r.finished = true
	r.err = err
	if r.m.busy[r.name] == r {
		delete(r.m.busy, r.name)
	}
	if r.offerID != "" {
		delete(r.f.offers, r.offerID)
	}
	close(r.done)
}

// offer sends the Offer directive; nothing is minted until AcceptOffer.
func (f *Fleet) offer(r *Reservation, mint MintFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.finished {
		return
	}
	id, err := randomHex(16) // 128-bit, bound to (session, worker) through r.m
	if err != nil {
		r.finishLocked(fmt.Errorf("creating offer: %w", err))
		return
	}
	r.mint = mint
	r.offerID = id
	r.offerExpires = f.o.Now().Add(f.o.Timing.OfferTTL)
	r.phase = phaseOffered
	f.offers[id] = r
	d := &workerv1.Directive{Kind: &workerv1.Directive_Offer{Offer: &workerv1.Offer{
		OfferId:    id,
		RunnerName: r.name,
		Repo:       r.req.Repo,
		Class:      ClassToProto(r.req.Class),
		Executor:   executorKindToProto(r.req.Class.Executor()),
		PublicRepo: r.req.Public,
		ExpiresAt:  timestamppb.New(r.offerExpires),
		CacheMode:  r.req.Cache,
	}}}
	if !f.sendLocked(r.m, d) {
		r.finishLocked(ErrOfferNotTaken)
	}
}

func cancelDirective(runnerName, reason string) *workerv1.Directive {
	return &workerv1.Directive{Kind: &workerv1.Directive_Cancel{Cancel: &workerv1.Cancel{RunnerName: runnerName, Reason: reason}}}
}

func drainDirective(reason string) *workerv1.Directive {
	return &workerv1.Directive{Kind: &workerv1.Directive_Drain{Drain: &workerv1.Drain{Reason: reason}}}
}
