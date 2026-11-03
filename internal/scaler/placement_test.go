package scaler

import (
	"context"
	"slices"
	"testing"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/fleet"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/naming"
	"github.com/actions/scaleset"
)

func TestLocalReservationsUseTheLocalSegment(t *testing.T) {
	// The scaler mints synchronously only for the local member; that rests on
	// the fleet naming it naming.LocalWorkerSegment.
	_, fl := newFleetScaler(t, 1, &fakeJITConfigGenerator{}, blockingExecutor())
	res, ok := fl.Reserve(testRequest())
	if !ok {
		t.Fatal("no local slot")
	}
	defer res.Release()
	if res.Member() != naming.LocalWorkerSegment {
		t.Fatalf("local member = %q, want %q", res.Member(), naming.LocalWorkerSegment)
	}
	if seg, _ := naming.RunnerWorkerSegment(res.RunnerName()); seg != naming.LocalWorkerSegment {
		t.Fatalf("runner %q is not named for the local member", res.RunnerName())
	}
}

func TestNoMintWithoutAFreeSlot(t *testing.T) {
	gen := &fakeJITConfigGenerator{}
	s, fl := newFleetScaler(t, 1, gen, blockingExecutor())
	held, ok := fl.Reserve(testRequest()) // the only slot is taken
	if !ok {
		t.Fatal("no local slot")
	}
	defer held.Release()
	if err := s.Scale(context.Background(), statsMessage(1)); err != nil {
		t.Fatal(err)
	}
	if got := gen.Calls(); got != 0 {
		t.Fatalf("minted %d JIT configs with no free slot, want 0", got)
	}
}

func TestLocalMintFailureFreesTheSlotAndTracksNothing(t *testing.T) {
	s, fl := newFleetScaler(t, 1, failingJITConfigGenerator{}, blockingExecutor())
	if err := s.Scale(context.Background(), statsMessage(1)); err != nil {
		t.Fatal(err)
	}
	if got := len(s.Runners()); got != 0 {
		t.Fatalf("runners = %d after a failed mint, want 0", got)
	}
	if got := fl.Free(testRequest()); got != 1 {
		t.Fatalf("free = %d after a failed mint, want 1", got)
	}
}

func TestAdoptTracksARunnerWithoutMinting(t *testing.T) {
	gen := &fakeJITConfigGenerator{}
	s, fl := newFleetScaler(t, 2, gen, blockingExecutor())
	res, ok := fl.Reserve(testRequest())
	if !ok {
		t.Fatal("no slot")
	}
	if !s.Adopt(res) {
		t.Fatal("Adopt refused a matching reservation")
	}
	if !slices.Contains(s.Runners(), res.RunnerName()) {
		t.Fatalf("runners = %v", s.Runners())
	}
	if gen.Calls() != 0 {
		t.Fatal("adoption minted a JIT config")
	}
	other, ok := fl.Reserve(fleet.PlacementRequest{Repo: "org/other", Class: testClass, Cache: "none"})
	if !ok {
		t.Fatal("no second slot")
	}
	if s.Adopt(other) {
		t.Fatal("Adopt accepted another repo's runner")
	}
	other.Release()

	done := &scaleset.RunnerScaleSetMessage{
		MessageID:            2,
		Statistics:           &scaleset.RunnerScaleSetStatistic{},
		JobCompletedMessages: []*scaleset.JobCompleted{{Result: "succeeded", RunnerName: res.RunnerName(), JobMessageBase: scaleset.JobMessageBase{JobID: "job-1"}}},
	}
	if err := s.Scale(context.Background(), done); err != nil {
		t.Fatal(err)
	}
	waitPlacement(t, "the adopted runner's slot to be freed", func() bool {
		return len(s.Runners()) == 0 && fl.Free(testRequest()) == 2
	})
}

func TestAdoptMatchesTheRepoCaseInsensitivelyAndRefusesDuplicates(t *testing.T) {
	s, fl := newFleetScaler(t, 2, &fakeJITConfigGenerator{}, blockingExecutor())
	res, ok := fl.Reserve(fleet.PlacementRequest{Repo: "ORG/Repo", Class: testClass, Cache: "none"})
	if !ok {
		t.Fatal("no slot")
	}
	if !s.Adopt(res) {
		t.Fatal("Adopt refused a runner whose repo differs only by case")
	}
	if s.Adopt(res) {
		t.Fatal("Adopt accepted a runner it already tracks; the fleet must finish the duplicate")
	}
	if got := len(s.Runners()); got != 1 {
		t.Fatalf("runners = %d, want 1", got)
	}
}

func TestPublicRepoRequestsCarryPublic(t *testing.T) {
	s, fl := newFleetScaler(t, 1, &fakeJITConfigGenerator{}, blockingExecutor())
	if s.request().Public {
		t.Fatal("a scaler is not public until told")
	}
	s.SetPublic(true)
	if !s.request().Public {
		t.Fatal("SetPublic(true) did not reach the placement request")
	}
	// The default local member does not accept public repos: nothing is placed.
	if err := s.Scale(context.Background(), statsMessage(1)); err != nil {
		t.Fatal(err)
	}
	if got := len(s.Runners()); got != 0 {
		t.Fatalf("placed %d public runners on a member that does not accept public, want 0", got)
	}
	fl.SetLocalAcceptPublic(true)
	s.Reevaluate(context.Background())
	if got := len(s.Runners()); got != 1 {
		t.Fatalf("runners = %d after the member accepted public, want 1", got)
	}
}

func TestCapacityIsPushedToTheListener(t *testing.T) {
	s, fl := newFleetScaler(t, 3, &fakeJITConfigGenerator{}, blockingExecutor())
	rec := &capacityRecorder{}
	s.SetCapacitySink(rec)
	if got := rec.last(); got != 3 {
		t.Fatalf("capacity on attach = %d, want 3", got)
	}
	fl.SetLocal(5, testClass, execFor(blockingExecutor()))
	s.Reevaluate(context.Background())
	if got := rec.last(); got != 5 {
		t.Fatalf("capacity after resize = %d, want 5", got)
	}
}

func TestCancelRunnerFreesTheSlot(t *testing.T) {
	s, fl := newFleetScaler(t, 1, &fakeJITConfigGenerator{}, blockingExecutor())
	if err := s.Scale(context.Background(), statsMessage(1)); err != nil {
		t.Fatal(err)
	}
	names := s.Runners()
	if len(names) != 1 {
		t.Fatalf("runners = %v", names)
	}
	if !s.CancelRunner(names[0]) {
		t.Fatal("CancelRunner did not find the runner")
	}
	waitPlacement(t, "the recycled runner's slot", func() bool { return fl.Free(testRequest()) == 1 })
}

func TestJobCompletedFreesTheSlotBeforeTheRunnerExits(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	slow := workerFunc(func(context.Context, string, string) error { <-gate; return nil }) // ignores cancellation, like a runner still exiting
	s, fl := newFleetScaler(t, 1, &fakeJITConfigGenerator{}, slow)
	if err := s.Scale(context.Background(), statsMessage(1)); err != nil {
		t.Fatal(err)
	}
	name := s.Runners()[0]
	done := &scaleset.RunnerScaleSetMessage{
		MessageID:            2,
		Statistics:           &scaleset.RunnerScaleSetStatistic{},
		JobCompletedMessages: []*scaleset.JobCompleted{{Result: "succeeded", RunnerName: name, JobMessageBase: scaleset.JobMessageBase{JobID: "job-1"}}},
	}
	if err := s.Scale(context.Background(), done); err != nil {
		t.Fatal(err)
	}
	if got := fl.Free(testRequest()); got != 1 {
		t.Fatalf("free = %d right after JobCompleted, want 1 (released before the process exits)", got)
	}
}
