package fleet

import (
	"context"
	"errors"
	"testing"

	workerv1 "github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

// offered opens a worker for o/a, places one reservation and returns the
// offer, leaving it unaccepted.
func offered(t *testing.T, tf *testFleet) (WorkerRecord, string, *Reservation, <-chan error, *workerv1.Offer, *mintRecorder) {
	t.Helper()
	w := testWorker("box", Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 2, Repos: []string{"o/a"}})
	res := tf.open(t, w, dockerCaps(2, linuxX64))
	ch := tf.directives(t, res.SessionID, w.ID)
	r := mustReserve(t, tf.Fleet, PlacementRequest{Repo: "o/a", Class: linuxX64, Cache: "none"})
	mint := &mintRecorder{}
	errc := runAsync(context.Background(), r, mint.mint)
	offer := recvDirective(t, ch).GetOffer()
	if offer == nil {
		t.Fatal("expected an offer")
	}
	return w, res.SessionID, r, errc, offer, mint
}

func TestUpdateBoundsFinishesAnOfferThatNoLongerQualifies(t *testing.T) {
	tf := newTestFleet(t)
	w, sid, _, errc, offer, mint := offered(t, tf)
	tf.UpdateBounds(w.ID, Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 2, Repos: []string{"z/z"}})
	if _, err := tf.AcceptOffer(context.Background(), sid, w.ID, offer.GetOfferId()); err == nil {
		t.Fatal("AcceptOffer succeeded after the repo left the worker's bounds")
	}
	if err := waitErr(t, errc); !errors.Is(err, ErrOfferNotTaken) {
		t.Fatalf("Run = %v, want ErrOfferNotTaken", err)
	}
	if mint.calls() != 0 {
		t.Fatal("a JIT was minted for a repo outside the worker's bounds")
	}
}

func TestDrainFinishesOutstandingOffers(t *testing.T) {
	tf := newTestFleet(t)
	w, sid, _, errc, offer, mint := offered(t, tf)
	if err := tf.DrainWorker(w.ID, "maintenance"); err != nil {
		t.Fatal(err)
	}
	if _, err := tf.AcceptOffer(context.Background(), sid, w.ID, offer.GetOfferId()); err == nil {
		t.Fatal("AcceptOffer succeeded on a drained worker")
	}
	if err := waitErr(t, errc); !errors.Is(err, ErrOfferNotTaken) {
		t.Fatalf("Run = %v, want ErrOfferNotTaken", err)
	}
	if mint.calls() != 0 {
		t.Fatal("a JIT was minted on a drained worker")
	}
}

// Eligibility can also change without UpdateBounds or DrainWorker running
// (here: the worker reports itself unavailable). AcceptOffer re-checks.
func TestAcceptOfferRechecksEligibility(t *testing.T) {
	tf := newTestFleet(t)
	w, sid, _, errc, offer, mint := offered(t, tf)
	if _, err := tf.Heartbeat(context.Background(), sid, w.ID, workerv1.Availability_AVAILABILITY_UNAVAILABLE, nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := tf.AcceptOffer(context.Background(), sid, w.ID, offer.GetOfferId()); !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("AcceptOffer = %v, want ErrNotPermitted", err)
	}
	if err := waitErr(t, errc); !errors.Is(err, ErrOfferNotTaken) {
		t.Fatalf("Run = %v, want ErrOfferNotTaken", err)
	}
	if mint.calls() != 0 {
		t.Fatal("a JIT was minted for an ineligible worker")
	}
}

func TestPublicRequestsOnlyReachMembersThatAcceptPublic(t *testing.T) {
	tf := newTestFleet(t)
	w := testWorker("box", Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 2})
	res := tf.open(t, w, dockerCaps(2, linuxX64))
	ch := tf.directives(t, res.SessionID, w.ID)
	pub := PlacementRequest{Repo: "o/a", Class: linuxX64, Public: true, Cache: "repo"}
	if got := tf.Free(pub); got != 0 {
		t.Fatalf("a worker that did not opt in has %d free slots for public repos", got)
	}
	if _, ok := tf.Reserve(pub); ok {
		t.Fatal("a public reservation landed on a worker that did not opt in")
	}
	if _, err := tf.Heartbeat(context.Background(), res.SessionID, w.ID, workerv1.Availability_AVAILABILITY_AVAILABLE, nil, true); err != nil {
		t.Fatal(err)
	}
	r := mustReserve(t, tf.Fleet, pub)
	defer r.Release()
	errc := runAsync(context.Background(), r, (&mintRecorder{}).mint)
	offer := recvDirective(t, ch).GetOffer()
	if !offer.GetPublicRepo() || offer.GetCacheMode() != "none" {
		t.Fatalf("offer public=%v cache=%q, want public with cache none", offer.GetPublicRepo(), offer.GetCacheMode())
	}
	r.Release()
	_ = waitErr(t, errc)
}

func TestLocalMemberAcceptsPublicOnlyWhenTold(t *testing.T) {
	f := New(Options{Timing: DefaultTiming()})
	f.SetLocal(1, linuxX64, &fakeExec{})
	pub := PlacementRequest{Repo: "o/a", Class: linuxX64, Public: true, Cache: "repo"}
	if f.Free(pub) != 0 {
		t.Fatal("the local member took a public repo before being told to")
	}
	f.SetLocalAcceptPublic(true)
	r, ok := f.Reserve(pub)
	if !ok {
		t.Fatal("the local member refused a public repo after SetLocalAcceptPublic(true)")
	}
	r.Release()
}
