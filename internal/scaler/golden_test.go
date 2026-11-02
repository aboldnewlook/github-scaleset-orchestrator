package scaler

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/actions/scaleset"
)

// The golden cases pin the scale decisions gso makes with the semaphore. The
// fleet's local member must reproduce them exactly (design spec §14, P2b).
// B8a replaces only newGoldenScaler's body. Never edit goldenCases or the
// assertions in TestGoldenScaleDecisions.

type goldenCompletion struct {
	runner int // index into the runner names minted so far; -1 = no runner (a reassignment)
	jobID  string
	result string
}

type goldenStep struct {
	name     string
	idle     bool // nil message: an idle poll
	id       int
	assigned int
	complete []goldenCompletion
	want     scaleDecision
	mints    int // JIT configs minted so far
	runners  int
}

type goldenCase struct {
	name  string
	slots int
	steps []goldenStep
}

var goldenCases = []goldenCase{
	{name: "initial statistics fill free slots", slots: 2, steps: []goldenStep{
		{name: "initial", id: -1, assigned: 3, want: scaleDecision{3, 0, 2, 2}, mints: 2, runners: 2},
		{name: "idle", idle: true, want: scaleDecision{3, 2, 0, 0}, mints: 2, runners: 2},
	}},
	{name: "idle poll converges on cached statistics", slots: 2, steps: []goldenStep{
		{name: "initial", id: -1, assigned: 0, want: scaleDecision{0, 0, 2, 0}},
		{name: "assigned", id: 1, assigned: 1, want: scaleDecision{1, 0, 2, 1}, mints: 1, runners: 1},
		{name: "idle", idle: true, want: scaleDecision{1, 1, 1, 0}, mints: 1, runners: 1},
	}},
	{name: "completion frees its slot for the next assignment", slots: 1, steps: []goldenStep{
		{name: "initial", id: -1, assigned: 2, want: scaleDecision{2, 0, 1, 1}, mints: 1, runners: 1},
		{name: "first done", id: 1, assigned: 1, complete: []goldenCompletion{{0, "j1", "succeeded"}},
			want: scaleDecision{1, 0, 1, 1}, mints: 2, runners: 1},
	}},
	{name: "redelivered completion releases once", slots: 2, steps: []goldenStep{
		{name: "initial", id: -1, assigned: 2, want: scaleDecision{2, 0, 2, 2}, mints: 2, runners: 2},
		{name: "first done", id: 1, assigned: 1, complete: []goldenCompletion{{0, "j1", "succeeded"}},
			want: scaleDecision{1, 1, 1, 0}, mints: 2, runners: 1},
		{name: "redelivered", id: 1, assigned: 1, complete: []goldenCompletion{{0, "j1", "succeeded"}},
			want: scaleDecision{1, 1, 1, 0}, mints: 2, runners: 1},
	}},
	{name: "reassignment is not a runner exit", slots: 1, steps: []goldenStep{
		{name: "initial", id: -1, assigned: 1, want: scaleDecision{1, 0, 1, 1}, mints: 1, runners: 1},
		{name: "requeued", id: 1, assigned: 1, complete: []goldenCompletion{{-1, "j1", "canceled"}},
			want: scaleDecision{1, 1, 0, 0}, mints: 1, runners: 1},
	}},
	{name: "no demand creates nothing", slots: 2, steps: []goldenStep{
		{name: "initial", id: -1, assigned: 0, want: scaleDecision{0, 0, 2, 0}},
	}},
}

// newGoldenScaler builds a scaler with `slots` free slots whose runners block
// until cancelled. B8a replaces this body with the fleet's local member.
func newGoldenScaler(t *testing.T, slots int) (*Scaler, *fakeJITConfigGenerator) {
	t.Helper()
	gen := &fakeJITConfigGenerator{}
	blocking := workerFunc(func(ctx context.Context, _, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	})
	return New("org/repo", 42, gen, blocking, NewSemaphore(slots), slog.New(slog.NewTextHandler(io.Discard, nil)), nil), gen
}

func goldenMessage(step goldenStep, names []string) *scaleset.RunnerScaleSetMessage {
	if step.idle {
		return nil
	}
	msg := &scaleset.RunnerScaleSetMessage{
		MessageID:  step.id,
		Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: step.assigned},
	}
	for _, c := range step.complete {
		name := ""
		if c.runner >= 0 {
			name = names[c.runner]
		}
		msg.JobCompletedMessages = append(msg.JobCompletedMessages, &scaleset.JobCompleted{
			RunnerName: name, Result: c.result, JobMessageBase: scaleset.JobMessageBase{JobID: c.jobID},
		})
	}
	return msg
}

func TestGoldenScaleDecisions(t *testing.T) {
	for _, c := range goldenCases {
		t.Run(c.name, func(t *testing.T) {
			s, gen := newGoldenScaler(t, c.slots)
			for _, step := range c.steps {
				if err := s.Scale(t.Context(), goldenMessage(step, gen.Names())); err != nil {
					t.Fatalf("%s: Scale() = %v", step.name, err)
				}
				s.mu.Lock()
				got := s.lastDecision
				s.mu.Unlock()
				if got != step.want {
					t.Errorf("%s: decision = %+v, want %+v", step.name, got, step.want)
				}
				if n := gen.Calls(); n != step.mints {
					t.Errorf("%s: minted %d, want %d", step.name, n, step.mints)
				}
				if n := len(s.Runners()); n != step.runners {
					t.Errorf("%s: runners = %d, want %d", step.name, n, step.runners)
				}
			}
		})
	}
}
