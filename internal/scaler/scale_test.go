package scaler

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/event"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/naming"
	"github.com/actions/scaleset"
)

func assigned(n int) *scaleset.RunnerScaleSetMessage {
	return &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: n}}
}

func blockingWorker() workerFunc {
	return workerFunc(func(ctx context.Context, _, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	})
}

type recordingAcquirer struct {
	ids []int64
	err error
}

func (a *recordingAcquirer) AcquireJobs(_ context.Context, ids []int64) ([]int64, error) {
	a.ids = append(a.ids, ids...)
	if a.err != nil {
		return nil, a.err
	}
	return ids, nil
}

// drain returns every event currently buffered on ch.
func drain(ch <-chan event.Event) []event.Event {
	var out []event.Event
	for {
		select {
		case e := <-ch:
			out = append(out, e)
		default:
			return out
		}
	}
}

func count(events []event.Event, typ event.EventType) int {
	n := 0
	for _, e := range events {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func TestScaleCachesStatisticsForIdlePolls(t *testing.T) {
	gen := &fakeJITConfigGenerator{}
	s, _ := newFleetScaler(t, 2, gen, blockingWorker())
	initial := &scaleset.RunnerScaleSetMessage{MessageID: -1, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1}}
	if err := s.Scale(t.Context(), initial); err != nil {
		t.Fatal(err)
	}
	if err := s.Scale(t.Context(), nil); err != nil { // idle poll: v0.5 passes nil
		t.Fatal(err)
	}
	if got := gen.Calls(); got != 1 {
		t.Fatalf("minted %d runners over an initial message and an idle poll, want 1", got)
	}
	if st, ok := s.Stats(); !ok || st.TotalAssignedJobs != 1 {
		t.Fatalf("Stats() = %+v, %v; want TotalAssignedJobs 1, true", st, ok)
	}
}

func TestScaleAcquiresAvailableJobsDefensively(t *testing.T) {
	bus := event.NewBus(nil)
	id, ch := bus.Subscribe(16)
	defer bus.Unsubscribe(id)
	s, _ := newFleetScaler(t, 1, &fakeJITConfigGenerator{}, blockingWorker())
	s.bus = bus
	msg := &scaleset.RunnerScaleSetMessage{
		MessageID:  3,
		Statistics: &scaleset.RunnerScaleSetStatistic{},
		JobAvailableMessages: []*scaleset.JobAvailable{
			{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 7}},
			{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 8}},
		},
	}
	// No session bound: nothing to acquire with, and Scale must still succeed.
	if err := s.Scale(t.Context(), msg); err != nil {
		t.Fatal(err)
	}
	acq := &recordingAcquirer{}
	s.BindSession(acq)
	if err := s.Scale(t.Context(), msg); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(acq.ids, []int64{7, 8}) {
		t.Fatalf("acquired %v, want [7 8]", acq.ids)
	}
	if got := count(drain(ch), event.EventJobAvailable); got != 2 {
		t.Fatalf("job.available events = %d, want 2", got)
	}
	acq.err = errors.New("github: 500")
	if err := s.Scale(t.Context(), msg); err != nil {
		t.Fatalf("Scale() = %v after an acquire error, want nil", err)
	}
}

func TestScaleJobAssignedCallsHookButNeverPlaces(t *testing.T) {
	bus := event.NewBus(nil)
	id, ch := bus.Subscribe(8)
	defer bus.Unsubscribe(id)
	gen := &fakeJITConfigGenerator{}
	s, _ := newFleetScaler(t, 2, gen, blockingWorker())
	s.bus = bus
	var hooked []string
	s.OnAssigned(func(_ context.Context, a *scaleset.JobAssigned) { hooked = append(hooked, a.JobID) })
	msg := &scaleset.RunnerScaleSetMessage{
		MessageID:           4,
		Statistics:          &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 0},
		JobAssignedMessages: []*scaleset.JobAssigned{{JobMessageBase: scaleset.JobMessageBase{JobID: "job-1"}}},
	}
	if err := s.Scale(t.Context(), msg); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(hooked, []string{"job-1"}) {
		t.Fatalf("OnAssigned saw %v, want [job-1]", hooked)
	}
	if got := gen.Calls(); got != 0 {
		t.Fatalf("JobAssigned minted %d runners; placement follows TotalAssignedJobs only", got)
	}
	if got := count(drain(ch), event.EventJobAssigned); got != 1 {
		t.Fatalf("job.assigned events = %d, want 1", got)
	}
}

func TestScaleDedupesRedeliveredJobMessages(t *testing.T) {
	bus := event.NewBus(nil)
	id, ch := bus.Subscribe(16)
	defer bus.Unsubscribe(id)
	s, _ := newFleetScaler(t, 1, &fakeJITConfigGenerator{}, blockingWorker())
	s.bus = bus
	msg := &scaleset.RunnerScaleSetMessage{
		MessageID:            5,
		Statistics:           &scaleset.RunnerScaleSetStatistic{},
		JobStartedMessages:   []*scaleset.JobStarted{{RunnerName: "gso-local-repo-aaaaaa", JobMessageBase: scaleset.JobMessageBase{JobID: "j1"}}},
		JobCompletedMessages: []*scaleset.JobCompleted{{RunnerName: "gso-local-repo-aaaaaa", Result: "succeeded", JobMessageBase: scaleset.JobMessageBase{JobID: "j1"}}},
	}
	for range 2 { // the second delivery is a redelivery after a failed ack
		if err := s.Scale(t.Context(), msg); err != nil {
			t.Fatal(err)
		}
	}
	evs := drain(ch)
	if count(evs, event.EventJobStarted) != 1 || count(evs, event.EventJobCompleted) != 1 {
		t.Fatalf("started=%d completed=%d after a redelivery, want 1 each",
			count(evs, event.EventJobStarted), count(evs, event.EventJobCompleted))
	}
}

func TestScaleTreatsCanceledWithoutRunnerAsReassignment(t *testing.T) {
	bus := event.NewBus(nil)
	id, ch := bus.Subscribe(16)
	defer bus.Unsubscribe(id)
	s, fl := newFleetScaler(t, 1, &fakeJITConfigGenerator{}, blockingWorker())
	s.bus = bus
	if err := s.Scale(t.Context(), assigned(1)); err != nil {
		t.Fatal(err)
	}
	requeued := &scaleset.RunnerScaleSetMessage{
		MessageID:            6,
		Statistics:           &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1},
		JobCompletedMessages: []*scaleset.JobCompleted{{Result: "canceled", JobMessageBase: scaleset.JobMessageBase{JobID: "j1"}}},
	}
	if err := s.Scale(t.Context(), requeued); err != nil {
		t.Fatal(err)
	}
	if got := len(s.Runners()); got != 1 {
		t.Fatalf("runners = %d after a reassignment, want 1 (a reassignment is not a runner exit)", got)
	}
	if got := fl.Free(testRequest()); got != 0 {
		t.Fatalf("available = %d after a reassignment, want 0", got)
	}
	evs := drain(ch)
	if count(evs, event.EventJobReassigned) != 1 || count(evs, event.EventJobCompleted) != 0 {
		t.Fatalf("reassigned=%d completed=%d, want 1 and 0", count(evs, event.EventJobReassigned), count(evs, event.EventJobCompleted))
	}
}

func TestScaleCompletionForUnknownRunnerIsNoOp(t *testing.T) {
	s, fl := newFleetScaler(t, 2, &fakeJITConfigGenerator{}, blockingWorker())
	msg := &scaleset.RunnerScaleSetMessage{
		MessageID:            7,
		Statistics:           &scaleset.RunnerScaleSetStatistic{},
		JobCompletedMessages: []*scaleset.JobCompleted{{RunnerName: "gso-other-repo-zzzzzz", Result: "succeeded", JobMessageBase: scaleset.JobMessageBase{JobID: "j9"}}},
	}
	if err := s.Scale(t.Context(), msg); err != nil {
		t.Fatal(err)
	}
	if got := fl.Free(testRequest()); got != 2 {
		t.Fatalf("available = %d, want 2", got)
	}
}

func TestScaleNeverReturnsAnErrorWhenMintingFails(t *testing.T) {
	bus := event.NewBus(nil)
	id, ch := bus.Subscribe(8)
	defer bus.Unsubscribe(id)
	s, fl := newFleetScaler(t, 1, failingJITConfigGenerator{}, blockingWorker())
	s.bus = bus
	if err := s.Scale(t.Context(), assigned(1)); err != nil {
		t.Fatalf("Scale() = %v; an error would stop the listener (spec §5.2)", err)
	}
	if got := fl.Free(testRequest()); got != 1 {
		t.Fatalf("available = %d after a failed mint, want 1", got)
	}
	if got := count(drain(ch), event.EventError); got != 1 {
		t.Fatalf("error events = %d, want 1", got)
	}
}

func TestReevaluatePlacesWithCachedStatistics(t *testing.T) {
	gen := &fakeJITConfigGenerator{}
	s, fl := newFleetScaler(t, 1, gen, blockingWorker())
	held, ok := fl.Reserve(testRequest()) // another repo holds the only slot
	if !ok {
		t.Fatal("Reserve failed")
	}
	if err := s.Scale(t.Context(), assigned(1)); err != nil {
		t.Fatal(err)
	}
	if got := gen.Calls(); got != 0 {
		t.Fatalf("minted %d without a free slot", got)
	}
	held.Release()
	s.Reevaluate(t.Context())
	if got := gen.Calls(); got != 1 {
		t.Fatalf("Reevaluate minted %d, want 1", got)
	}
}

func TestScaleUsesCurrentScaleSetIDAndTheFleetsRunnerName(t *testing.T) {
	gen := &fakeJITConfigGenerator{}
	s, _ := newFleetScaler(t, 1, gen, blockingWorker())
	s.SetScaleSetID(99)
	if got := s.ScaleSetID(); got != 99 {
		t.Fatalf("ScaleSetID() = %d, want 99", got)
	}
	if err := s.Scale(t.Context(), assigned(1)); err != nil {
		t.Fatal(err)
	}
	names := gen.Names()
	if !slices.Equal(gen.ScaleSetIDs(), []int{99}) || len(names) != 1 {
		t.Fatalf("minted for scale sets %v with names %v", gen.ScaleSetIDs(), names)
	}
	if seg, _ := naming.RunnerWorkerSegment(names[0]); seg != naming.LocalWorkerSegment {
		t.Fatalf("runner %q is not named for the local member", names[0])
	}
	if got := s.Runners(); !slices.Equal(got, names) {
		t.Fatalf("Runners() = %v, want %v", got, names)
	}
}
