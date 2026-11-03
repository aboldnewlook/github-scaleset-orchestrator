package fleet

import (
	"context"
	"testing"

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
