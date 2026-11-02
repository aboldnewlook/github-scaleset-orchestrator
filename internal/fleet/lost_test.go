package fleet

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	workerv1 "github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/event"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

func TestLostWorkerIsNotCountedAndUnstartedRunnersAreRemoved(t *testing.T) {
	tf := newTestFleet(t)
	w, sid, ch := openBox(t, tf, "box", 2, linuxX64)
	req := PlacementRequest{Repo: "o/r", Class: linuxX64}
	r, errc := tf.place(t, sid, w.ID, ch, req, &mintRecorder{})
	tf.report(t, sid, w.ID, r.RunnerName(), running, 0, "") // RUNNING, but no JobStarted yet
	tf.clock.Advance(tf.o.Timing.Lost + ms)
	tf.sweep(context.Background())
	if !tf.sawEvent(event.EventWorkerLost) {
		t.Fatal("no worker.lost event")
	}
	if tf.Free(req) != 0 || tf.Slots(req) != 0 {
		t.Fatal("a lost worker's slots still count")
	}
	if members := tf.Members(); len(members) != 1 || members[0].Availability != "lost" || members[0].Connected {
		t.Fatalf("Members = %+v", members)
	}
	if _, err := tf.Heartbeat(context.Background(), sid, w.ID, workerv1.Availability_AVAILABILITY_AVAILABLE, nil, false); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("heartbeat after lost = %v", err)
	}
	assertPending(t, errc)
	tf.clock.Advance(tf.o.Timing.LostUnstartedRemove + ms)
	tf.sweep(context.Background())
	if err := waitErr(t, errc); !errors.Is(err, ErrWorkerLost) {
		t.Fatalf("Run = %v, want ErrWorkerLost", err)
	}
	if got := tf.gh.Removed(); len(got) != 1 || got[0] != 1001 {
		t.Fatalf("removed = %v", got)
	}
}

func TestLostWorkerKeepsStartedRunnersUntilTheCap(t *testing.T) {
	tf := newTestFleet(t)
	w, sid, ch := openBox(t, tf, "box", 1, linuxX64)
	r, errc := tf.place(t, sid, w.ID, ch, PlacementRequest{Repo: "o/r", Class: linuxX64}, &mintRecorder{})
	tf.report(t, sid, w.ID, r.RunnerName(), running, 0, "")
	r.MarkStarted()
	tf.clock.Advance(tf.o.Timing.Lost + ms)
	tf.sweep(context.Background())
	tf.clock.Advance(2 * time.Hour)
	tf.sweep(context.Background())
	assertPending(t, errc)
	tf.clock.Advance(tf.o.Timing.LostStartedCap)
	tf.sweep(context.Background())
	if err := waitErr(t, errc); !errors.Is(err, ErrWorkerLost) {
		t.Fatalf("Run = %v", err)
	}
	if got := tf.gh.Removed(); len(got) != 0 {
		t.Fatalf("a started runner was removed: %v", got)
	}
}

func TestReconnectReconcilesKnownRunners(t *testing.T) {
	tf := newTestFleet(t)
	w, sid, ch := openBox(t, tf, "box", 3, linuxX64)
	req := PlacementRequest{Repo: "o/r", Class: linuxX64}
	mint := &mintRecorder{}
	kept, keptErr := tf.place(t, sid, w.ID, ch, req, mint)
	tf.report(t, sid, w.ID, kept.RunnerName(), running, 0, "")
	starting, startingErr := tf.place(t, sid, w.ID, ch, req, mint) // accepted, never RUNNING
	gone, goneErr := tf.place(t, sid, w.ID, ch, req, mint)
	tf.report(t, sid, w.ID, gone.RunnerName(), running, 0, "")
	tf.clock.Advance(tf.o.Timing.Lost + ms)
	tf.sweep(context.Background())

	res := tf.open(t, w, dockerCaps(3, linuxX64), runningState(kept.RunnerName(), "o/r", linuxX64, running))
	if len(res.RejectedRunners) != 0 {
		t.Fatalf("rejected = %v", res.RejectedRunners)
	}
	assertPending(t, keptErr)
	if err := waitErr(t, startingErr); !errors.Is(err, ErrWorkerLost) {
		t.Fatalf("unreported STARTING runner: %v", err)
	}
	if err := waitErr(t, goneErr); err != nil {
		t.Fatalf("unreported RUNNING runner: %v", err)
	}
	if got := tf.gh.Removed(); len(got) != 1 || got[0] != starting.RunnerID() {
		t.Fatalf("removed = %v, want [%d]", got, starting.RunnerID())
	}
	if tf.Free(req) != 2 {
		t.Fatalf("Free = %d, want 2", tf.Free(req))
	}
}

func TestAdoptionAfterRouterRestart(t *testing.T) {
	tf := newTestFleet(t)
	tf.gh.setScaleSet("o/r", linuxX64, 7)
	valid := "gso-box-r-abc123"
	tf.gh.addRunner("o/r", valid, 55, 7)
	tf.gh.addRunner("o/r", "gso-box-r-wrongs", 56, 8)   // registered in another scale set
	tf.gh.addRunner("o/r", "gso-other-r-aaaaaa", 57, 7) // another worker's segment
	w := testWorker("box", Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 3, Repos: []string{"o/r"}})
	res := tf.open(t, w, dockerCaps(3, linuxX64),
		runningState(valid, "o/r", linuxX64, running),
		runningState("gso-box-r-wrongs", "o/r", linuxX64, running),
		runningState("gso-other-r-aaaaaa", "o/r", linuxX64, running),
		runningState("gso-box-r-zzzzzz", "o/r", linuxX64, running),   // unknown to GitHub
		runningState("gso-box-x-bbbbbb", "o/x", linuxX64, running),   // repo outside bounds
		runningState("gso-box-r-cccccc", "o/r", linuxArm64, running), // class not served
	)
	rejected := append([]string(nil), res.RejectedRunners...)
	sort.Strings(rejected)
	want := []string{"gso-box-r-cccccc", "gso-box-r-wrongs", "gso-box-r-zzzzzz", "gso-box-x-bbbbbb", "gso-other-r-aaaaaa"}
	if !reflect.DeepEqual(rejected, want) {
		t.Fatalf("rejected = %v, want %v", rejected, want)
	}
	adopted := tf.adopter.list()
	if len(adopted) != 1 || adopted[0].RunnerName() != valid || adopted[0].RunnerID() != 55 || adopted[0].Member() != "box" {
		t.Fatalf("adopted = %v", adopted)
	}
	if !tf.sawEvent(event.EventRunnerAdopted) {
		t.Fatal("no runner.adopted event")
	}
	req := PlacementRequest{Repo: "o/r", Class: linuxX64}
	if tf.Free(req) != 2 {
		t.Fatalf("Free = %d, want 2", tf.Free(req))
	}
	errc := runAsync(context.Background(), adopted[0], nil)
	assertPending(t, errc)
	tf.report(t, res.SessionID, w.ID, valid, exited, 0, "")
	if err := waitErr(t, errc); err != nil {
		t.Fatalf("adopted Run = %v", err)
	}
	if tf.Free(req) != 3 {
		t.Fatalf("Free after exit = %d", tf.Free(req))
	}
}

func TestAdoptionIsCappedAtSlotsAndNeedsTheScaler(t *testing.T) {
	tf := newTestFleet(t)
	tf.gh.setScaleSet("o/r", linuxX64, 7)
	tf.gh.addRunner("o/r", "gso-box-r-aaaaaa", 1, 7)
	tf.gh.addRunner("o/r", "gso-box-r-bbbbbb", 2, 7)
	w := testWorker("box", Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 1})
	res := tf.open(t, w, dockerCaps(1, linuxX64),
		runningState("gso-box-r-aaaaaa", "o/r", linuxX64, running),
		runningState("gso-box-r-bbbbbb", "o/r", linuxX64, running))
	if len(res.RejectedRunners) != 1 || len(tf.adopter.list()) != 1 {
		t.Fatalf("rejected=%v adopted=%d, want 1 and 1", res.RejectedRunners, len(tf.adopter.list()))
	}

	refusing := newTestFleet(t)
	refusing.adopter.refuse = true
	refusing.gh.setScaleSet("o/r", linuxX64, 7)
	refusing.gh.addRunner("o/r", "gso-box-r-aaaaaa", 1, 7)
	res = refusing.open(t, w, dockerCaps(1, linuxX64), runningState("gso-box-r-aaaaaa", "o/r", linuxX64, running))
	if len(res.RejectedRunners) != 1 || refusing.Free(PlacementRequest{Repo: "o/r", Class: linuxX64}) != 1 {
		t.Fatalf("refused adoption: rejected=%v", res.RejectedRunners)
	}
}

func TestDrainSendsDirectiveAndStopsOffers(t *testing.T) {
	tf := newTestFleet(t)
	w, sid, ch := openBox(t, tf, "box", 1, linuxX64)
	if err := tf.DrainWorker(w.ID, "maintenance"); err != nil {
		t.Fatal(err)
	}
	if d := recvDirective(t, ch).GetDrain(); d == nil || d.GetReason() != "maintenance" {
		t.Fatalf("expected a Drain directive, got %v", d)
	}
	req := PlacementRequest{Repo: "o/r", Class: linuxX64}
	if _, err := tf.Heartbeat(context.Background(), sid, w.ID, workerv1.Availability_AVAILABILITY_AVAILABLE, nil, false); err != nil {
		t.Fatal(err)
	}
	if tf.Free(req) != 0 {
		t.Fatal("drained worker still offered work")
	}
	if members := tf.Members(); members[0].Availability != "draining" {
		t.Fatalf("availability = %s", members[0].Availability)
	}
	if err := tf.CloseSession(sid, w.ID); err != nil {
		t.Fatal(err)
	}
	tf.open(t, w, dockerCaps(1, linuxX64)) // a new session clears the router-requested drain
	if tf.Free(req) != 1 {
		t.Fatal("drain survived a new session")
	}
}

func TestRevokeCancelsRemovesAndForgets(t *testing.T) {
	tf := newTestFleet(t)
	w, sid, ch := openBox(t, tf, "box", 1, linuxX64)
	r, errc := tf.place(t, sid, w.ID, ch, PlacementRequest{Repo: "o/r", Class: linuxX64}, &mintRecorder{})
	if err := tf.RevokeWorker(context.Background(), w.ID); err != nil {
		t.Fatal(err)
	}
	if c := recvDirective(t, ch).GetCancel(); c == nil || c.GetRunnerName() != r.RunnerName() {
		t.Fatalf("expected a Cancel, got %v", c)
	}
	if _, ok := <-ch; ok {
		t.Fatal("stream still open after revoke")
	}
	if err := waitErr(t, errc); !errors.Is(err, errWorkerRevoked) {
		t.Fatalf("Run = %v", err)
	}
	if got := tf.gh.Removed(); len(got) != 1 || got[0] != 1001 {
		t.Fatalf("removed = %v", got)
	}
	if len(tf.Members()) != 0 || !tf.sawEvent(event.EventWorkerRevoked) {
		t.Fatalf("members = %+v", tf.Members())
	}
	if _, err := tf.Heartbeat(context.Background(), sid, w.ID, workerv1.Availability_AVAILABILITY_AVAILABLE, nil, false); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("heartbeat after revoke = %v", err)
	}
}

func TestUpdateBoundsReclamps(t *testing.T) {
	tf := newTestFleet(t)
	w := testWorker("box", Bounds{Classes: []runner.Class{linuxX64, linuxArm64}, MaxSlots: 4})
	tf.open(t, w, dockerCaps(4, linuxX64, linuxArm64))
	arm := PlacementRequest{Repo: "o/r", Class: linuxArm64}
	x64 := PlacementRequest{Repo: "o/r", Class: linuxX64}
	if tf.Slots(arm) != 4 || tf.Slots(x64) != 4 {
		t.Fatalf("before: arm=%d x64=%d", tf.Slots(arm), tf.Slots(x64))
	}
	changed := make(chan struct{}, 1)
	tf.OnChange(func() { changed <- struct{}{} })
	tf.UpdateBounds(w.ID, Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 1})
	if tf.Slots(arm) != 0 || tf.Slots(x64) != 1 {
		t.Fatalf("after: arm=%d x64=%d", tf.Slots(arm), tf.Slots(x64))
	}
	select {
	case <-changed:
	case <-time.After(2 * time.Second):
		t.Fatal("no OnChange after UpdateBounds")
	}
}

// Controller ruling 1: only the connecting worker's own enrolled segment may
// attribute a runner to it. Legacy P1 names (gso-<repo>-<uuid8>) and the
// reserved local segment parse as segments too and must be refused.
func TestAdoptionRefusesUnknownOrForeignSegments(t *testing.T) {
	tf := newTestFleet(t)
	tf.gh.setScaleSet("o/prune", linuxX64, 7)
	tf.gh.addRunner("o/prune", "gso-prune-abcd1234", 1, 7) // legacy P1 name: segment "prune"
	tf.gh.addRunner("o/prune", "gso-local-prune-aaaaaa", 2, 7)
	tf.gh.addRunner("o/prune", "gso-box-prune-bbbbbb", 3, 7)
	w := testWorker("box", Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 3})
	res := tf.open(t, w, dockerCaps(3, linuxX64),
		runningState("gso-prune-abcd1234", "o/prune", linuxX64, running),
		runningState("gso-local-prune-aaaaaa", "o/prune", linuxX64, running),
		runningState("gso-box-prune-bbbbbb", "o/prune", linuxX64, running))
	sort.Strings(res.RejectedRunners)
	want := []string{"gso-local-prune-aaaaaa", "gso-prune-abcd1234"}
	if !reflect.DeepEqual(res.RejectedRunners, want) {
		t.Fatalf("rejected = %v, want %v", res.RejectedRunners, want)
	}
	if a := tf.adopter.list(); len(a) != 1 || a[0].RunnerName() != "gso-box-prune-bbbbbb" {
		t.Fatalf("adopted = %v", a)
	}
}

// Controller ruling 2: state is keyed by owner/repo; the same repo name under
// two owners must not cross-verify.
func TestAdoptionIsKeyedByOwnerAndRepo(t *testing.T) {
	tf := newTestFleet(t)
	tf.gh.setScaleSet("jrschumacher/site", linuxX64, 7)
	tf.gh.setScaleSet("aboldnewlook/site", linuxX64, 9)
	tf.gh.addRunner("jrschumacher/site", "gso-box-site-aaaaaa", 1, 7)
	tf.gh.addRunner("aboldnewlook/site", "gso-box-site-bbbbbb", 2, 9)
	tf.gh.addRunner("jrschumacher/site", "gso-box-site-cccccc", 3, 9) // right name, other owner's scale set
	w := testWorker("box", Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 4})
	res := tf.open(t, w, dockerCaps(4, linuxX64),
		runningState("gso-box-site-aaaaaa", "jrschumacher/site", linuxX64, running),
		runningState("gso-box-site-bbbbbb", "aboldnewlook/site", linuxX64, running),
		runningState("gso-box-site-cccccc", "jrschumacher/site", linuxX64, running),
		runningState("gso-box-site-aaaaaa2", "aboldnewlook/site", linuxX64, running)) // registered under neither
	sort.Strings(res.RejectedRunners)
	want := []string{"gso-box-site-aaaaaa2", "gso-box-site-cccccc"}
	if !reflect.DeepEqual(res.RejectedRunners, want) {
		t.Fatalf("rejected = %v, want %v", res.RejectedRunners, want)
	}
	if len(tf.adopter.list()) != 2 {
		t.Fatalf("adopted = %d, want 2", len(tf.adopter.list()))
	}
}

// I-1: a report for a runner that was already released (JobCompleted won the
// race) is idempotent; only a runner held by another member is foreign.
func TestReportRunnerIsIdempotentAfterRelease(t *testing.T) {
	tf := newTestFleet(t)
	a, aSid, aCh := openBox(t, tf, "aaa", 1, linuxX64)
	b, bSid, _ := openBox(t, tf, "bbb", 1, linuxX64)
	r, errc := tf.place(t, aSid, a.ID, aCh, PlacementRequest{Repo: "o/r", Class: linuxX64}, &mintRecorder{})
	err := tf.ReportRunner(context.Background(), bSid, b.ID, &workerv1.ReportRunnerRequest{SessionId: bSid, RunnerName: r.RunnerName(), Phase: exited})
	if !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("foreign report of a busy runner = %v, want ErrNotPermitted", err)
	}
	r.Release()
	if err := waitErr(t, errc); err != nil {
		t.Fatalf("Run = %v", err)
	}
	tf.report(t, aSid, a.ID, r.RunnerName(), exited, 0, "") // after Release
	tf.report(t, aSid, a.ID, r.RunnerName(), exited, 0, "") // duplicate
	tf.report(t, aSid, a.ID, "gso-aaa-r-zzzzzz", running, 0, "")
}

// I-2: legacy scaler names (gso-<repo>-<uuid8>) are never adoptable, even by
// an enrolled worker whose segment equals the repo short name.
func TestAdoptionRefusesLegacyNamesForMatchingSegment(t *testing.T) {
	tf := newTestFleet(t)
	tf.gh.setScaleSet("o/prune", linuxX64, 7)
	tf.gh.addRunner("o/prune", "gso-prune-abcd1234", 1, 7)
	w := testWorker("prune", Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 2})
	res := tf.open(t, w, dockerCaps(2, linuxX64), runningState("gso-prune-abcd1234", "o/prune", linuxX64, running))
	if !reflect.DeepEqual(res.RejectedRunners, []string{"gso-prune-abcd1234"}) || len(tf.adopter.list()) != 0 {
		t.Fatalf("rejected=%v adopted=%d", res.RejectedRunners, len(tf.adopter.list()))
	}
}

// I-3: one enrolled worker may not adopt a runner named for another enrolled worker.
func TestAdoptionRefusesAnotherEnrolledWorkersSegment(t *testing.T) {
	tf := newTestFleet(t)
	tf.gh.setScaleSet("o/r", linuxX64, 7)
	tf.gh.addRunner("o/r", "gso-aaa-r-abc123", 1, 7)
	tf.open(t, testWorker("aaa", Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 1}), dockerCaps(1, linuxX64))
	b := testWorker("bbb", Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 1})
	res := tf.open(t, b, dockerCaps(1, linuxX64), runningState("gso-aaa-r-abc123", "o/r", linuxX64, running))
	if !reflect.DeepEqual(res.RejectedRunners, []string{"gso-aaa-r-abc123"}) || len(tf.adopter.list()) != 0 {
		t.Fatalf("rejected=%v adopted=%d", res.RejectedRunners, len(tf.adopter.list()))
	}
}
