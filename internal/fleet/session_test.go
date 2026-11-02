package fleet

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	workerv1 "github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/event"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

const ms = time.Millisecond

func openBox(t *testing.T, tf *testFleet, name string, slots uint32, classes ...runner.Class) (WorkerRecord, string, <-chan *workerv1.Directive) {
	t.Helper()
	w := testWorker(name, Bounds{Classes: classes, MaxSlots: int(slots)})
	res := tf.open(t, w, dockerCaps(slots, classes...))
	return w, res.SessionID, tf.directives(t, res.SessionID, w.ID)
}

func (tf *testFleet) report(t *testing.T, sid, wid, name string, phase workerv1.RunnerPhase, exitCode int32, msg string) {
	t.Helper()
	err := tf.ReportRunner(context.Background(), sid, wid, &workerv1.ReportRunnerRequest{SessionId: sid, RunnerName: name, Phase: phase, ExitCode: exitCode, Error: msg})
	if err != nil {
		t.Fatalf("ReportRunner(%s, %v): %v", name, phase, err)
	}
}

// place reserves, starts Run, receives the offer and accepts it.
func (tf *testFleet) place(t *testing.T, sid, wid string, ch <-chan *workerv1.Directive, req PlacementRequest, mint *mintRecorder) (*Reservation, <-chan error) {
	t.Helper()
	r := mustReserve(t, tf.Fleet, req)
	errc := runAsync(context.Background(), r, mint.mint)
	offer := recvDirective(t, ch).GetOffer()
	if offer == nil {
		t.Fatal("expected an offer directive")
	}
	if _, err := tf.AcceptOffer(context.Background(), sid, wid, offer.GetOfferId()); err != nil {
		t.Fatalf("AcceptOffer: %v", err)
	}
	return r, errc
}

const (
	running = workerv1.RunnerPhase_RUNNER_PHASE_RUNNING
	exited  = workerv1.RunnerPhase_RUNNER_PHASE_EXITED
)

func TestRemoteOfferAcceptRunExit(t *testing.T) {
	tf := newTestFleet(t)
	w := testWorker("box", Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 2})
	res := tf.open(t, w, dockerCaps(2, linuxX64))
	if res.HeartbeatInterval != tf.o.Timing.Heartbeat || res.OfferTTL != tf.o.Timing.OfferTTL || res.Effective.GetSlots() != 2 || len(res.RejectedRunners) != 0 {
		t.Fatalf("OpenSession = %+v", res)
	}
	if !tf.sawEvent(event.EventWorkerConnected) {
		t.Fatal("no worker.connected event")
	}
	ch := tf.directives(t, res.SessionID, w.ID)
	req := PlacementRequest{Repo: "o/prune", Class: linuxX64, Cache: "repo"}
	r := mustReserve(t, tf.Fleet, req)
	if !strings.HasPrefix(r.RunnerName(), "gso-box-prune-") || r.Member() != "box" {
		t.Fatalf("reservation %s on %s", r.RunnerName(), r.Member())
	}
	mint := &mintRecorder{}
	errc := runAsync(context.Background(), r, mint.mint)
	offer := recvDirective(t, ch).GetOffer()
	if offer == nil || offer.GetRunnerName() != r.RunnerName() || offer.GetRepo() != "o/prune" || offer.GetCacheMode() != "repo" ||
		offer.GetPublicRepo() || offer.GetExecutor() != workerv1.ExecutorKind_EXECUTOR_KIND_DOCKER || len(offer.GetOfferId()) != 32 {
		t.Fatalf("offer = %v", offer)
	}
	if !offer.GetExpiresAt().AsTime().Equal(tf.clock.Now().Add(tf.o.Timing.OfferTTL)) {
		t.Fatalf("expires_at = %v", offer.GetExpiresAt().AsTime())
	}
	if mint.calls() != 0 {
		t.Fatal("minted before the worker accepted")
	}
	resp, err := tf.AcceptOffer(context.Background(), res.SessionID, w.ID, offer.GetOfferId())
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetRunnerName() != r.RunnerName() || resp.GetRunnerId() != 1001 || resp.GetEncodedJitConfig() != "jit-"+r.RunnerName() {
		t.Fatalf("AcceptOffer = %v", resp)
	}
	if _, err := tf.AcceptOffer(context.Background(), res.SessionID, w.ID, offer.GetOfferId()); !errors.Is(err, ErrOfferNotFound) {
		t.Fatalf("second accept: %v", err)
	}
	if mint.calls() != 1 || r.RunnerID() != 1001 || tf.Free(req) != 1 {
		t.Fatalf("mint calls=%d id=%d free=%d", mint.calls(), r.RunnerID(), tf.Free(req))
	}
	tf.report(t, res.SessionID, w.ID, r.RunnerName(), running, 0, "")
	assertPending(t, errc)
	tf.report(t, res.SessionID, w.ID, r.RunnerName(), exited, 0, "")
	if err := waitErr(t, errc); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if tf.Free(req) != 2 {
		t.Fatalf("Free after exit = %d", tf.Free(req))
	}
}

func TestRemoteRunnerFailureIsReturned(t *testing.T) {
	tf := newTestFleet(t)
	w, sid, ch := openBox(t, tf, "box", 1, linuxX64)
	r, errc := tf.place(t, sid, w.ID, ch, PlacementRequest{Repo: "o/r", Class: linuxX64}, &mintRecorder{})
	tf.report(t, sid, w.ID, r.RunnerName(), exited, 3, "boom")
	if err := waitErr(t, errc); err == nil || !strings.Contains(err.Error(), "code 3") {
		t.Fatalf("Run = %v, want an exit-code error", err)
	}
}

func TestOfferExpiresWithoutMinting(t *testing.T) {
	tf := newTestFleet(t)
	w, sid, ch := openBox(t, tf, "box", 2, linuxX64)
	req := PlacementRequest{Repo: "o/r", Class: linuxX64}
	r := mustReserve(t, tf.Fleet, req)
	mint := &mintRecorder{}
	errc := runAsync(context.Background(), r, mint.mint)
	offer := recvDirective(t, ch).GetOffer()
	tf.clock.Advance(tf.o.Timing.OfferTTL + ms)
	tf.sweep(context.Background())
	if err := waitErr(t, errc); !errors.Is(err, ErrOfferNotTaken) {
		t.Fatalf("Run = %v, want ErrOfferNotTaken", err)
	}
	if _, err := tf.AcceptOffer(context.Background(), sid, w.ID, offer.GetOfferId()); !errors.Is(err, ErrOfferNotFound) {
		t.Fatalf("late accept = %v", err)
	}
	if mint.calls() != 0 || len(tf.gh.Removed()) != 0 || tf.Free(req) != 2 {
		t.Fatalf("mint=%d removed=%v free=%d", mint.calls(), tf.gh.Removed(), tf.Free(req))
	}
}

func TestRejectBeforeAcceptMintsNothing(t *testing.T) {
	tf := newTestFleet(t)
	w, sid, ch := openBox(t, tf, "box", 1, linuxX64)
	r := mustReserve(t, tf.Fleet, PlacementRequest{Repo: "o/r", Class: linuxX64})
	mint := &mintRecorder{}
	errc := runAsync(context.Background(), r, mint.mint)
	offer := recvDirective(t, ch).GetOffer()
	if err := tf.RejectOffer(context.Background(), sid, w.ID, offer.GetOfferId(), "repo not on the local allowlist"); err != nil {
		t.Fatal(err)
	}
	if err := waitErr(t, errc); !errors.Is(err, ErrOfferNotTaken) {
		t.Fatalf("Run = %v", err)
	}
	if mint.calls() != 0 || len(tf.gh.Removed()) != 0 {
		t.Fatalf("mint=%d removed=%v", mint.calls(), tf.gh.Removed())
	}
	if !tf.sawEvent(event.EventOfferRejected) {
		t.Fatal("no offer.rejected event")
	}
}

func TestRejectAfterAcceptRemovesTheRunner(t *testing.T) {
	tf := newTestFleet(t)
	w, sid, ch := openBox(t, tf, "box", 1, linuxX64)
	r := mustReserve(t, tf.Fleet, PlacementRequest{Repo: "o/r", Class: linuxX64})
	errc := runAsync(context.Background(), r, (&mintRecorder{}).mint)
	offer := recvDirective(t, ch).GetOffer()
	if _, err := tf.AcceptOffer(context.Background(), sid, w.ID, offer.GetOfferId()); err != nil {
		t.Fatal(err)
	}
	if err := tf.RejectOffer(context.Background(), sid, w.ID, offer.GetOfferId(), "JIT verification failed"); err != nil {
		t.Fatal(err)
	}
	if err := waitErr(t, errc); !errors.Is(err, ErrOfferNotTaken) {
		t.Fatalf("Run = %v", err)
	}
	if got := tf.gh.Removed(); len(got) != 1 || got[0] != 1001 {
		t.Fatalf("removed = %v, want [1001]", got)
	}
}

func TestStartTimeoutRemovesAndCancels(t *testing.T) {
	tf := newTestFleet(t)
	w, sid, ch := openBox(t, tf, "box", 1, linuxX64)
	r, errc := tf.place(t, sid, w.ID, ch, PlacementRequest{Repo: "o/r", Class: linuxX64}, &mintRecorder{})
	tf.clock.Advance(tf.o.Timing.StartTTL + ms)
	tf.sweep(context.Background())
	if c := recvDirective(t, ch).GetCancel(); c == nil || c.GetRunnerName() != r.RunnerName() {
		t.Fatalf("expected a Cancel for %s, got %v", r.RunnerName(), c)
	}
	if err := waitErr(t, errc); !errors.Is(err, ErrStartTimeout) {
		t.Fatalf("Run = %v", err)
	}
	if got := tf.gh.Removed(); len(got) != 1 || got[0] != 1001 {
		t.Fatalf("removed = %v", got)
	}
}

func TestOffersAreBoundToSessionAndWorker(t *testing.T) {
	tf := newTestFleet(t)
	a, aSid, aCh := openBox(t, tf, "aaa", 1, linuxX64)
	b, bSid, _ := openBox(t, tf, "bbb", 1, linuxX64)
	r := mustReserve(t, tf.Fleet, PlacementRequest{Repo: "o/r", Class: linuxX64}) // tie → "aaa"
	if r.Member() != "aaa" {
		t.Fatalf("member = %s, want aaa (name order breaks ties)", r.Member())
	}
	errc := runAsync(context.Background(), r, (&mintRecorder{}).mint)
	offer := recvDirective(t, aCh).GetOffer()
	if _, err := tf.AcceptOffer(context.Background(), bSid, b.ID, offer.GetOfferId()); !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("other worker's accept = %v, want ErrNotPermitted", err)
	}
	if _, err := tf.AcceptOffer(context.Background(), aSid, b.ID, offer.GetOfferId()); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("mismatched worker/session = %v, want ErrUnknownSession", err)
	}
	if _, err := tf.AcceptOffer(context.Background(), aSid, a.ID, offer.GetOfferId()); err != nil {
		t.Fatal(err)
	}
	// b may not report or heartbeat a's runner.
	err := tf.ReportRunner(context.Background(), bSid, b.ID, &workerv1.ReportRunnerRequest{SessionId: bSid, RunnerName: r.RunnerName(), Phase: exited})
	if !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("foreign report = %v", err)
	}
	if _, err := tf.Heartbeat(context.Background(), bSid, b.ID, workerv1.Availability_AVAILABILITY_AVAILABLE,
		[]*workerv1.RunnerState{runningState(r.RunnerName(), "o/r", linuxX64, running)}, false); err != nil {
		t.Fatal(err)
	}
	tf.clock.Advance(tf.o.Timing.StartTTL + ms) // a never reported RUNNING; b's heartbeat must not count
	tf.sweep(context.Background())
	if err := waitErr(t, errc); !errors.Is(err, ErrStartTimeout) {
		t.Fatalf("Run = %v, want ErrStartTimeout", err)
	}
}

func TestSecondSessionRejectedWhileLive(t *testing.T) {
	tf := newTestFleet(t)
	w, oldSid, oldCh := openBox(t, tf, "box", 1, linuxX64)
	_, err := tf.OpenSession(context.Background(), w, OpenSessionParams{Capabilities: dockerCaps(1, linuxX64), Availability: workerv1.Availability_AVAILABILITY_AVAILABLE})
	if !errors.Is(err, ErrSessionActive) {
		t.Fatalf("second OpenSession = %v, want ErrSessionActive", err)
	}
	if !tf.sawEvent(event.EventAlert) {
		t.Fatal("no alert event")
	}
	tf.clock.Advance(tf.o.Timing.Lost + ms)
	res := tf.open(t, w, dockerCaps(1, linuxX64))
	if res.SessionID == oldSid {
		t.Fatal("session ID reused")
	}
	if _, err := tf.Heartbeat(context.Background(), oldSid, w.ID, workerv1.Availability_AVAILABILITY_AVAILABLE, nil, false); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("heartbeat on the stale session = %v", err)
	}
	if _, ok := <-oldCh; ok {
		t.Fatal("stale directive stream still open")
	}
}

func TestCloseSessionStopsOffersAndAllowsReopen(t *testing.T) {
	tf := newTestFleet(t)
	w, sid, ch := openBox(t, tf, "box", 1, linuxX64)
	if err := tf.CloseSession(sid, w.ID); err != nil {
		t.Fatal(err)
	}
	if tf.Free(PlacementRequest{Repo: "o/r", Class: linuxX64}) != 0 {
		t.Fatal("closed worker still has free slots")
	}
	if _, ok := <-ch; ok {
		t.Fatal("directive stream still open")
	}
	tf.open(t, w, dockerCaps(1, linuxX64)) // no ErrSessionActive after a clean close
}

func TestPlacementPrefersMostFreeThenName(t *testing.T) {
	tf := newTestFleet(t)
	openBox(t, tf, "bbb", 3, linuxX64)
	openBox(t, tf, "aaa", 3, linuxX64)
	req := PlacementRequest{Repo: "o/r", Class: linuxX64}
	if r := mustReserve(t, tf.Fleet, req); r.Member() != "aaa" {
		t.Fatalf("first = %s, want aaa", r.Member())
	}
	if r := mustReserve(t, tf.Fleet, req); r.Member() != "bbb" {
		t.Fatalf("second = %s, want bbb (more free slots)", r.Member())
	}
	if tf.Free(req) != 4 || tf.Slots(req) != 6 {
		t.Fatalf("free=%d slots=%d", tf.Free(req), tf.Slots(req))
	}
}

func TestPlacementFilters(t *testing.T) {
	tf := newTestFleet(t)
	w := testWorker("box", Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 2, Repos: []string{"o/allowed", "team/*"}})
	res := tf.open(t, w, dockerCaps(2, linuxX64))
	check := func(name string, req PlacementRequest, want int) {
		t.Helper()
		if got := tf.Free(req); got != want {
			t.Errorf("%s: Free = %d, want %d", name, got, want)
		}
	}
	check("exact repo", PlacementRequest{Repo: "o/allowed", Class: linuxX64}, 2)
	check("case-insensitive", PlacementRequest{Repo: "O/Allowed", Class: linuxX64}, 2)
	check("owner glob", PlacementRequest{Repo: "team/x", Class: linuxX64}, 2)
	check("repo outside bounds", PlacementRequest{Repo: "o/other", Class: linuxX64}, 0)
	check("class not served", PlacementRequest{Repo: "o/allowed", Class: linuxArm64}, 0)
	check("public without accept_public", PlacementRequest{Repo: "o/allowed", Class: linuxX64, Public: true}, 0)
	check("pinned to another member", PlacementRequest{Repo: "o/allowed", Class: linuxX64, Member: "other"}, 0)
	if _, err := tf.Heartbeat(context.Background(), res.SessionID, w.ID, workerv1.Availability_AVAILABILITY_AVAILABLE, nil, true); err != nil {
		t.Fatal(err)
	}
	check("public with accept_public", PlacementRequest{Repo: "o/allowed", Class: linuxX64, Public: true}, 2)
	r := mustReserve(t, tf.Fleet, PlacementRequest{Repo: "o/allowed", Class: linuxX64, Public: true, Cache: "repo"})
	if r.req.Cache != "none" {
		t.Fatalf("public cache = %q, want none", r.req.Cache)
	}
	if _, err := tf.Heartbeat(context.Background(), res.SessionID, w.ID, workerv1.Availability_AVAILABILITY_DRAINING, nil, true); err != nil {
		t.Fatal(err)
	}
	check("draining", PlacementRequest{Repo: "o/allowed", Class: linuxX64}, 0)
}

func TestReconnectGraceBlocksPlacement(t *testing.T) {
	tf := newTestFleet(t, func(o *Options) { o.RemoteEnabled = true })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tf.Start(ctx)
	w, sid, _ := openBox(t, tf, "box", 1, linuxX64)
	req := PlacementRequest{Repo: "o/r", Class: linuxX64}
	if _, ok := tf.Reserve(req); ok {
		t.Fatal("Reserve succeeded during the reconnect grace")
	}
	tf.clock.Advance(tf.o.Timing.Grace + ms)
	// Keep the worker alive: Start's background sweep runs on the fake clock too.
	if _, err := tf.Heartbeat(context.Background(), sid, w.ID, workerv1.Availability_AVAILABILITY_AVAILABLE, nil, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := tf.Reserve(req); !ok {
		t.Fatal("Reserve failed after the grace")
	}
}

func TestPolicyFollowsWorkerDefaultAccept(t *testing.T) {
	var accept atomic.Bool
	tf := newTestFleet(t, func(o *Options) { o.WorkerDefaultAccept = accept.Load })
	w := testWorker("box", Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 1})
	res := tf.open(t, w, dockerCaps(1, linuxX64))
	if res.Policy.GetWorkerDefaultAccept() {
		t.Fatal("policy accepted public by default")
	}
	accept.Store(true)
	policy, err := tf.Heartbeat(context.Background(), res.SessionID, w.ID, workerv1.Availability_AVAILABILITY_AVAILABLE, nil, false)
	if err != nil || !policy.GetWorkerDefaultAccept() {
		t.Fatalf("Heartbeat policy = %v, %v", policy, err)
	}
}

func TestOnChangeFires(t *testing.T) {
	tf := newTestFleet(t)
	changed := make(chan struct{}, 8)
	tf.OnChange(func() { changed <- struct{}{} })
	wait := func(what string) {
		t.Helper()
		select {
		case <-changed:
		case <-time.After(2 * time.Second):
			t.Fatalf("no OnChange after %s", what)
		}
	}
	w := testWorker("box", Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 1})
	res := tf.open(t, w, dockerCaps(1, linuxX64))
	wait("OpenSession")
	if _, err := tf.Heartbeat(context.Background(), res.SessionID, w.ID, workerv1.Availability_AVAILABILITY_UNAVAILABLE, nil, false); err != nil {
		t.Fatal(err)
	}
	wait("an availability change")
}
