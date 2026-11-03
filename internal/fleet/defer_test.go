package fleet

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	workerv1 "github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

const deferredRunner = "gso-box-r-aaaaaa"

func heartbeatRunning(t *testing.T, tf *testFleet, sid, workerID string, names ...string) {
	t.Helper()
	var rs []*workerv1.RunnerState
	for _, n := range names {
		rs = append(rs, runningState(n, "o/r", linuxX64, running))
	}
	if _, err := tf.Heartbeat(context.Background(), sid, workerID, workerv1.Availability_AVAILABILITY_AVAILABLE, rs, false); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
}

// A router that restarts while GitHub is slow does not know its scale sets yet.
// That is not a verdict on the worker's runner: it stays running and is
// adopted by a later heartbeat once the scale set is known.
func TestAdoptionIsDeferredUntilTheScaleSetIsKnown(t *testing.T) {
	tf := newTestFleet(t)
	w := testWorker("box", Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 1})
	req := PlacementRequest{Repo: "o/r", Class: linuxX64}
	res := tf.open(t, w, dockerCaps(1, linuxX64), runningState(deferredRunner, "o/r", linuxX64, running))
	if len(res.RejectedRunners) != 0 {
		t.Fatalf("rejected = %v, want none: the scale set is merely unknown", res.RejectedRunners)
	}
	if len(tf.adopter.list()) != 0 || tf.gh.getCalls() != 0 {
		t.Fatalf("adopted %d, GitHub lookups %d, want 0 and 0 while the scale set is unknown", len(tf.adopter.list()), tf.gh.getCalls())
	}
	if tf.Free(req) != 0 {
		t.Fatalf("Free = %d, want 0: the deferred runner holds the worker's slot", tf.Free(req))
	}

	heartbeatRunning(t, tf, res.SessionID, w.ID, deferredRunner)
	if len(tf.adopter.list()) != 0 {
		t.Fatal("adopted while the scale set is still unknown")
	}

	tf.gh.setScaleSet("o/r", linuxX64, 7)
	tf.gh.addRunner("o/r", deferredRunner, 55, 7)
	tf.clock.Advance(tf.o.Timing.Heartbeat)
	heartbeatRunning(t, tf, res.SessionID, w.ID, deferredRunner)
	adopted := tf.adopter.list()
	if len(adopted) != 1 || adopted[0].RunnerName() != deferredRunner || adopted[0].RunnerID() != 55 || adopted[0].Member() != "box" {
		t.Fatalf("adopted = %v, want %s with ID 55", adopted, deferredRunner)
	}
	if tf.Free(req) != 0 {
		t.Fatalf("Free = %d after adoption, want 0", tf.Free(req))
	}
	heartbeatRunning(t, tf, res.SessionID, w.ID, deferredRunner)
	if len(tf.adopter.list()) != 1 {
		t.Fatal("a later heartbeat adopted the runner twice")
	}
	errc := runAsync(context.Background(), adopted[0], nil)
	assertPending(t, errc)
	tf.report(t, res.SessionID, w.ID, deferredRunner, exited, 0, "")
	if err := waitErr(t, errc); err != nil {
		t.Fatalf("adopted Run = %v", err)
	}
	if tf.Free(req) != 1 {
		t.Fatalf("Free after exit = %d, want 1", tf.Free(req))
	}
}

// Once the scale set is known a deferred runner that GitHub says is not ours
// is cancelled, and its slot comes back.
func TestDeferredRunnerThatIsNotOursIsCancelled(t *testing.T) {
	tests := []struct {
		name  string
		setup func(tf *testFleet)
	}{
		{"another scale set", func(tf *testFleet) { tf.gh.addRunner("o/r", deferredRunner, 55, 8) }},
		{"unknown to GitHub", func(tf *testFleet) {}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tf := newTestFleet(t)
			w := testWorker("box", Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 1})
			req := PlacementRequest{Repo: "o/r", Class: linuxX64}
			res := tf.open(t, w, dockerCaps(1, linuxX64), runningState(deferredRunner, "o/r", linuxX64, running))
			ch := tf.directives(t, res.SessionID, w.ID)
			tf.gh.setScaleSet("o/r", linuxX64, 7)
			tt.setup(tf)
			tf.clock.Advance(tf.o.Timing.Heartbeat)
			heartbeatRunning(t, tf, res.SessionID, w.ID, deferredRunner)
			if c := recvDirective(t, ch).GetCancel(); c == nil || c.GetRunnerName() != deferredRunner {
				t.Fatalf("want a Cancel for %s", deferredRunner)
			}
			if len(tf.adopter.list()) != 0 {
				t.Fatal("a runner GitHub does not recognise was adopted")
			}
			if tf.Free(req) != 1 {
				t.Fatalf("Free = %d, want 1 after the cancel", tf.Free(req))
			}
		})
	}
}

// A deferred runner that stops being reported has finished: its slot returns.
func TestDeferredRunnerThatFinishesFreesItsSlot(t *testing.T) {
	tf := newTestFleet(t)
	w := testWorker("box", Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 1})
	req := PlacementRequest{Repo: "o/r", Class: linuxX64}
	res := tf.open(t, w, dockerCaps(1, linuxX64), runningState(deferredRunner, "o/r", linuxX64, running))
	heartbeatRunning(t, tf, res.SessionID, w.ID)
	if tf.Free(req) != 1 {
		t.Fatalf("Free = %d, want 1", tf.Free(req))
	}
}

func deferredNames(n int) []string {
	names := make([]string, n)
	for i := range names {
		names[i] = "gso-box-r-aaaaa" + string(rune('a'+i))
	}
	return names
}

// A lookup that errors defers the runner (GitHub is unreachable, which says
// nothing about the runner) and holds its slot until a later heartbeat can ask.
func TestLookupErrorDefersAndALaterHeartbeatAdopts(t *testing.T) {
	tf := newTestFleet(t)
	tf.gh.setScaleSet("o/r", linuxX64, 7)
	tf.gh.addRunner("o/r", deferredRunner, 55, 7)
	tf.gh.setGetErr(errors.New("github: 502"))
	w := testWorker("box", Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 1})
	req := PlacementRequest{Repo: "o/r", Class: linuxX64}
	res := tf.open(t, w, dockerCaps(1, linuxX64), runningState(deferredRunner, "o/r", linuxX64, running))
	if len(res.RejectedRunners) != 0 || len(tf.adopter.list()) != 0 {
		t.Fatalf("rejected=%v adopted=%d, want neither while lookups fail", res.RejectedRunners, len(tf.adopter.list()))
	}
	if tf.Free(req) != 0 {
		t.Fatalf("Free = %d, want 0: the deferred runner holds the slot", tf.Free(req))
	}
	tf.gh.setGetErr(nil)
	tf.clock.Advance(tf.o.Timing.Lost) // the one-slot lookup budget has refilled
	heartbeatRunning(t, tf, res.SessionID, w.ID, deferredRunner)
	if a := tf.adopter.list(); len(a) != 1 || a[0].RunnerID() != 55 {
		t.Fatalf("adopted = %v, want %s", a, deferredRunner)
	}
}

// Heartbeat RPCs are not paced by the server, so a worker reporting fake
// runners must not be able to turn them into GitHub lookups: deferred runners
// are settled at most once per heartbeat interval.
func TestSettleIsPacedByTheHeartbeatInterval(t *testing.T) {
	tf := newTestFleet(t)
	tf.gh.setScaleSet("o/r", linuxX64, 7)
	tf.gh.setGetErr(errors.New("github: 502"))
	names := deferredNames(4)
	w := testWorker("box", Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 4})
	var states []*workerv1.RunnerState
	for _, n := range names {
		states = append(states, runningState(n, "o/r", linuxX64, running))
	}
	res := tf.open(t, w, dockerCaps(4, linuxX64), states...)
	if len(res.RejectedRunners) != 0 {
		t.Fatalf("rejected = %v", res.RejectedRunners)
	}
	base := tf.gh.getCalls() // OpenSession's own round: at most one lookup per runner

	for range 50 {
		heartbeatRunning(t, tf, res.SessionID, w.ID, names...)
	}
	if got := tf.gh.getCalls(); got > 4*(0+1) {
		t.Fatalf("%d lookups after 50 rapid heartbeats, want at most 4 (open made %d)", got, base)
	}

	tf.clock.Advance(tf.o.Timing.Heartbeat)
	for range 50 {
		heartbeatRunning(t, tf, res.SessionID, w.ID, names...)
	}
	got := tf.gh.getCalls()
	if got <= base {
		t.Fatalf("no settle after the interval passed: %d lookups", got)
	}
	if got > 4*(1+1) {
		t.Fatalf("%d lookups after one interval, want at most %d", got, 4*(1+1))
	}
}

// With the scale set unknown nothing can be looked up, so nothing is.
func TestSettleSkipsWhileEveryScaleSetIsUnknown(t *testing.T) {
	tf := newTestFleet(t)
	w := testWorker("box", Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 2})
	names := deferredNames(2)
	res := tf.open(t, w, dockerCaps(2, linuxX64),
		runningState(names[0], "o/r", linuxX64, running), runningState(names[1], "o/r", linuxX64, running))
	for range 5 {
		tf.clock.Advance(tf.o.Timing.Heartbeat)
		heartbeatRunning(t, tf, res.SessionID, w.ID, names...)
	}
	if n := tf.gh.getCalls(); n != 0 {
		t.Fatalf("%d GitHub lookups while no scale set is known, want 0", n)
	}
}

// Concurrent heartbeats share one settle.
func TestConcurrentHeartbeatsSettleOnce(t *testing.T) {
	tf := newTestFleet(t)
	w := testWorker("box", Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 1})
	res := tf.open(t, w, dockerCaps(1, linuxX64), runningState(deferredRunner, "o/r", linuxX64, running))
	tf.gh.setScaleSet("o/r", linuxX64, 7)
	tf.gh.setGetErr(errors.New("github: 502"))
	tf.gh.setGetDelay(200 * time.Millisecond) // keeps the first settle in flight
	tf.clock.Advance(tf.o.Timing.Heartbeat)

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			heartbeatRunning(t, tf, res.SessionID, w.ID, deferredRunner)
		}()
	}
	wg.Wait()
	if n := tf.gh.getCalls(); n != 1 {
		t.Fatalf("%d lookups from 10 concurrent heartbeats, want 1", n)
	}
}

// The settle works on what the worker reports now, not what it reported when
// the session opened.
func TestSettleUsesTheCurrentReport(t *testing.T) {
	tf := newTestFleet(t)
	w := testWorker("box", Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 1})
	res := tf.open(t, w, dockerCaps(1, linuxX64), runningState(deferredRunner, "o/r", linuxX64, workerv1.RunnerPhase_RUNNER_PHASE_STARTING))
	tf.gh.setScaleSet("o/r", linuxX64, 7)
	tf.gh.addRunner("o/r", deferredRunner, 55, 7)
	tf.clock.Advance(tf.o.Timing.Heartbeat)
	heartbeatRunning(t, tf, res.SessionID, w.ID, deferredRunner) // reports RUNNING now
	a := tf.adopter.list()
	if len(a) != 1 {
		t.Fatalf("adopted = %v", a)
	}
	// A RUNNING report adopts into the running phase, which survives a lost worker.
	tf.clock.Advance(tf.o.Timing.Lost + ms)
	tf.sweep(context.Background())
	if tf.Free(PlacementRequest{Repo: "o/r", Class: linuxX64}) != 0 {
		t.Fatal("a runner reported RUNNING at the heartbeat was adopted as not started")
	}
}

// The lookup budget belongs to the worker, not to a session: cycling sessions
// does not refill it.
func TestLookupBudgetSurvivesSessionCycling(t *testing.T) {
	tf := newTestFleet(t)
	tf.gh.setScaleSet("o/r", linuxX64, 7)
	tf.gh.setGetErr(errors.New("github: 429"))
	names := deferredNames(4)
	w := testWorker("box", Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 4})
	var states []*workerv1.RunnerState
	for _, n := range names {
		states = append(states, runningState(n, "o/r", linuxX64, running))
	}
	for range 25 {
		res := tf.open(t, w, dockerCaps(4, linuxX64), states...)
		if len(res.RejectedRunners) != 0 {
			t.Fatalf("rejected = %v: a spent budget defers, it does not reject", res.RejectedRunners)
		}
		if err := tf.CloseSession(res.SessionID, w.ID); err != nil {
			t.Fatal(err)
		}
	}
	if n := tf.gh.getCalls(); n > 4 {
		t.Fatalf("%d lookups across 25 sessions, want at most the 4-slot budget", n)
	}
	tf.clock.Advance(tf.o.Timing.Lost) // a full refill
	res := tf.open(t, w, dockerCaps(4, linuxX64), states...)
	_ = res
	if n := tf.gh.getCalls(); n <= 4 || n > 8 {
		t.Fatalf("%d lookups after a refill, want between 5 and 8", n)
	}
}

// A runner whose lookups keep failing is cancelled after maxDeferAttempts.
func TestRepeatedLookupFailuresCancelTheRunner(t *testing.T) {
	tf := newTestFleet(t)
	tf.gh.setScaleSet("o/r", linuxX64, 7)
	tf.gh.setGetErr(errors.New("github: 414 URI too long"))
	w := testWorker("box", Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 1})
	req := PlacementRequest{Repo: "o/r", Class: linuxX64}
	res := tf.open(t, w, dockerCaps(1, linuxX64), runningState(deferredRunner, "o/r", linuxX64, running)) // attempt 1
	ch := tf.directives(t, res.SessionID, w.ID)
	for attempt := 2; attempt <= maxDeferAttempts; attempt++ {
		if tf.Free(req) != 0 {
			t.Fatalf("runner dropped before attempt %d", attempt)
		}
		tf.clock.Advance(tf.o.Timing.Lost)
		heartbeatRunning(t, tf, res.SessionID, w.ID, deferredRunner)
	}
	if c := recvDirective(t, ch).GetCancel(); c == nil || c.GetRunnerName() != deferredRunner {
		t.Fatalf("want a Cancel for %s after %d failed lookups", deferredRunner, maxDeferAttempts)
	}
	if tf.Free(req) != 1 {
		t.Fatalf("Free = %d, want 1 after the cancel", tf.Free(req))
	}
	if n := tf.gh.getCalls(); n != maxDeferAttempts {
		t.Fatalf("%d lookups, want %d", n, maxDeferAttempts)
	}
}

// A scale set that never becomes known makes no lookups, so its cap is in time.
func TestUnknownScaleSetIsCancelledAfterTheCap(t *testing.T) {
	tf := newTestFleet(t)
	w := testWorker("box", Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 1})
	req := PlacementRequest{Repo: "o/r", Class: linuxX64}
	res := tf.open(t, w, dockerCaps(1, linuxX64), runningState(deferredRunner, "o/r", linuxX64, running))
	ch := tf.directives(t, res.SessionID, w.ID)
	tf.clock.Advance(tf.o.Timing.LostStartedCap - time.Minute)
	heartbeatRunning(t, tf, res.SessionID, w.ID, deferredRunner)
	if tf.Free(req) != 0 {
		t.Fatal("cancelled before the cap")
	}
	tf.clock.Advance(2 * time.Minute)
	heartbeatRunning(t, tf, res.SessionID, w.ID, deferredRunner)
	if c := recvDirective(t, ch).GetCancel(); c == nil || c.GetRunnerName() != deferredRunner {
		t.Fatalf("want a Cancel for %s past the cap", deferredRunner)
	}
	if tf.Free(req) != 1 || tf.gh.getCalls() != 0 {
		t.Fatalf("Free = %d, lookups = %d, want 1 and 0", tf.Free(req), tf.gh.getCalls())
	}
}

func TestDeferredRunnersCountAsBusy(t *testing.T) {
	tf := newTestFleet(t)
	w := testWorker("box", Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 2})
	tf.open(t, w, dockerCaps(2, linuxX64), runningState(deferredRunner, "o/r", linuxX64, running))
	for _, m := range tf.Members() {
		if m.Name == "box" && m.Busy != 1 {
			t.Fatalf("Busy = %d, want 1 for a pending deferral", m.Busy)
		}
	}
}

func TestOverlongRunningEntriesAreInvalid(t *testing.T) {
	e := newServerEnv(t)
	cred, _ := e.enroll(t, "box", oneSlot(linuxX64))
	huge := "gso-box-" + strings.Repeat("a", 200_000) + "-aaaaaa"
	long := []*workerv1.RunnerState{runningState(huge, "o/r", linuxX64, running)}
	longRepo := []*workerv1.RunnerState{runningState(deferredRunner, strings.Repeat("o", 300)+"/r", linuxX64, running)}
	for name, rs := range map[string][]*workerv1.RunnerState{"name": long, "repo": longRepo} {
		_, err := e.client.OpenSession(context.Background(), authed(&workerv1.OpenSessionRequest{Capabilities: dockerCaps(1, linuxX64), Running: rs}, cred))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("OpenSession with an over-long %s = %v, want InvalidArgument", name, err)
		}
	}
	sid := e.openSession(t, cred, dockerCaps(1, linuxX64)).GetSessionId()
	_, err := e.client.Heartbeat(context.Background(), authed(&workerv1.HeartbeatRequest{SessionId: sid, Running: long}, cred))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("Heartbeat with an over-long name = %v, want InvalidArgument", err)
	}
}

// Deferral must not widen what a worker may claim: segment, class and bounds
// are still checked first, and a worker cannot exceed its slots.
func TestDeferralKeepsTheLocalChecks(t *testing.T) {
	tf := newTestFleet(t)
	w := testWorker("box", Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 1})
	res := tf.open(t, w, dockerCaps(1, linuxX64),
		runningState("gso-other-r-aaaaaa", "o/r", linuxX64, running), // another worker's segment
		runningState("gso-box-x-bbbbbb", "o/r", linuxArm64, running), // class not served
		runningState(deferredRunner, "o/r", linuxX64, running),
		runningState("gso-box-r-cccccc", "o/r", linuxX64, running)) // no room left
	want := map[string]bool{"gso-other-r-aaaaaa": true, "gso-box-x-bbbbbb": true, "gso-box-r-cccccc": true}
	if len(res.RejectedRunners) != len(want) {
		t.Fatalf("rejected = %v, want %v", res.RejectedRunners, want)
	}
	for _, n := range res.RejectedRunners {
		if !want[n] {
			t.Fatalf("rejected = %v, want %v", res.RejectedRunners, want)
		}
	}
}
