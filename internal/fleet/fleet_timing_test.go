package fleet

import (
	"testing"
	"time"

	workerv1 "github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

func TestNewFillsEveryZeroTimingField(t *testing.T) {
	f := New(Options{Timing: Timing{Tick: 5 * time.Millisecond}})
	want := DefaultTiming()
	want.Tick = 5 * time.Millisecond
	if f.o.Timing != want {
		t.Fatalf("Timing = %+v, want %+v", f.o.Timing, want)
	}
	f = New(Options{Timing: Timing{Keepalive: time.Second}})
	if f.o.Timing.Keepalive != time.Second || f.o.Timing.Lost != DefaultTiming().Lost || f.o.Timing.Tick != time.Second {
		t.Fatalf("Timing = %+v", f.o.Timing)
	}
}

func TestAdoptionCallsGitHubOnlyForPlausibleCandidates(t *testing.T) {
	tf := newTestFleet(t)
	tf.gh.setScaleSet("o/r", linuxX64, 7)
	w := testWorker("box", Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 2})
	var bogus []*workerv1.RunnerState
	for _, n := range []string{"gso-box-r-aaaaaa", "gso-box-r-bbbbbb", "gso-box-r-cccccc"} {
		bogus = append(bogus, runningState(n, "evil/unknown", linuxX64, running)) // no scale set
	}
	tf.open(t, w, dockerCaps(2, linuxX64), bogus...)
	if n := tf.gh.getCalls(); n != 0 {
		t.Fatalf("GetRunnerByName called %d times for repos without a scale set", n)
	}

	w2 := testWorker("box2", Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 2})
	var unknown []*workerv1.RunnerState
	for _, n := range []string{"gso-box2-r-aaaaaa", "gso-box2-r-bbbbbb", "gso-box2-r-cccccc", "gso-box2-r-dddddd"} {
		unknown = append(unknown, runningState(n, "o/r", linuxX64, running)) // plausible, unknown to GitHub
	}
	tf.open(t, w2, dockerCaps(2, linuxX64), unknown...)
	if n := tf.gh.getCalls(); n > 2 {
		t.Fatalf("GetRunnerByName called %d times with 2 free slots, want at most 2", n)
	}
}
