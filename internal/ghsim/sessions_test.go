package ghsim

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghapi"
)

func TestScaleSetLifecycle(t *testing.T) {
	m, _ := newTestModel(t)
	ctx := context.Background()
	c := m.Client("o/a")
	if got, err := c.GetRunnerScaleSet(ctx, 1, "gso-e2e-a-linux-x64"); err != nil || got != nil {
		t.Fatalf("missing set = %v, %v; want nil, nil", got, err)
	}
	ss, err := c.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{Name: "gso-e2e-a-linux-x64", RunnerGroupID: 1, Labels: defaultLabels})
	if err != nil || ss.ID == 0 {
		t.Fatalf("create = %+v, %v", ss, err)
	}
	if _, err := c.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{Name: ss.Name}); !errors.Is(err, scaleset.BadRequestError) {
		t.Fatalf("duplicate create error = %v, want BadRequestError", err)
	}
	got, err := c.GetRunnerScaleSet(ctx, 1, ss.Name)
	if err != nil || got == nil || got.ID != ss.ID {
		t.Fatalf("get = %+v, %v", got, err)
	}
	upd, err := c.UpdateRunnerScaleSet(ctx, ss.ID, &scaleset.RunnerScaleSet{Labels: append(append([]scaleset.Label(nil), defaultLabels...), scaleset.Label{Name: "gso"})})
	if err != nil || len(upd.Labels) != 4 || upd.Name != ss.Name {
		t.Fatalf("update = %+v, %v", upd, err)
	}
	list, err := c.ListRunnerScaleSets(ctx, 1)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}
	if other, _ := m.Client("o/b").GetRunnerScaleSet(ctx, 1, ss.Name); other != nil {
		t.Fatal("a scale set leaked into another repository")
	}
}

func TestSecondSessionConflictsUntilClosedOrExpired(t *testing.T) {
	m, clk := newTestModel(t)
	ctx := context.Background()
	c, ss, s1 := openSet(t, m, "o/a", "set", defaultLabels)
	if _, err := c.OpenSession(ctx, ss.ID, "other"); !ghapi.IsConflict(err) {
		t.Fatalf("second session error = %v, want a conflict", err)
	}
	if err := s1.Close(ctx); err != nil {
		t.Fatal(err)
	}
	s2, err := c.OpenSession(ctx, ss.ID, "other")
	if err != nil {
		t.Fatalf("open after close: %v", err)
	}
	if _, err := c.OpenSession(ctx, ss.ID, "third"); !ghapi.IsConflict(err) {
		t.Fatalf("error = %v, want a conflict while s2 is live", err)
	}
	clk.Advance(2*time.Minute + time.Second) // nobody polls s2: it expires
	if _, err := c.OpenSession(ctx, ss.ID, "third"); err != nil {
		t.Fatalf("an expired session still blocks: %v", err)
	}
	if _, err := s2.GetMessage(ctx, 0, 1); !errors.Is(err, scaleset.MessageQueueTokenExpiredError) {
		t.Fatalf("polling a replaced session: %v, want MessageQueueTokenExpiredError", err)
	}
}

func TestFailSessionsAppliesBeforeCreation(t *testing.T) {
	m, _ := newTestModel(t)
	ctx := context.Background()
	m.FailSessions("o/a", "set", fmt.Errorf("simulated: %w", scaleset.ConflictError), 2)
	c := m.Client("o/a")
	ss, err := c.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{Name: "set", Labels: defaultLabels})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := c.OpenSession(ctx, ss.ID, "x"); !errors.Is(err, scaleset.ConflictError) {
			t.Fatalf("attempt %d: %v, want the injected conflict", i+1, err)
		}
	}
	if _, err := c.OpenSession(ctx, ss.ID, "x"); err != nil {
		t.Fatalf("third attempt: %v", err)
	}
}

func TestIdlePollReturnsNilAndRecordsCapacity(t *testing.T) {
	m, _ := newTestModel(t)
	_, _, s := openSet(t, m, "o/a", "set", defaultLabels)
	start := time.Now()
	msg, err := s.GetMessage(context.Background(), 0, 3)
	if err != nil || msg != nil {
		t.Fatalf("idle poll = %+v, %v; want nil, nil", msg, err)
	}
	if el := time.Since(start); el < 15*time.Millisecond || el > time.Second {
		t.Fatalf("idle poll took %s, want about PollWait (20ms)", el)
	}
	if got := m.LastCapacity("o/a", "set"); got != 3 {
		t.Fatalf("LastCapacity = %d, want 3", got)
	}
}

func TestAssignmentRespectsLabelsCapacityAndRedelivery(t *testing.T) {
	m, _ := newTestModel(t)
	ctx := context.Background()
	_, _, s := openSet(t, m, "o/a", "set", defaultLabels)
	gpu, _ := m.InjectJob(JobSpec{Repo: "o/a", Labels: []string{"gpu"}})
	j1, _ := m.InjectJob(JobSpec{Repo: "o/a", Labels: []string{"Self-Hosted", "linux"}})
	j2, _ := m.InjectJob(JobSpec{Repo: "o/a"})

	msg, err := s.GetMessage(ctx, 0, 1)
	if err != nil || msg == nil || len(msg.JobAssignedMessages) != 1 {
		t.Fatalf("first poll = %+v, %v; want one JobAssigned", msg, err)
	}
	if got := msg.JobAssignedMessages[0].RunnerRequestID; got != j1.ID {
		t.Fatalf("assigned job %d, want %d (FIFO among matching jobs)", got, j1.ID)
	}
	if len(msg.JobAvailableMessages) != 0 {
		t.Fatal("the simulator must never send JobAvailable (github.com behaviour)")
	}
	if msg.Statistics == nil || msg.Statistics.TotalAssignedJobs != 1 {
		t.Fatalf("statistics = %+v, want TotalAssignedJobs 1", msg.Statistics)
	}
	again, _ := s.GetMessage(ctx, 0, 1)
	if again == nil || again.MessageID != msg.MessageID {
		t.Fatal("an unacked message was not redelivered")
	}
	if err := s.DeleteMessage(ctx, msg.MessageID); err != nil {
		t.Fatal(err)
	}
	if next, _ := s.GetMessage(ctx, 0, 1); next != nil {
		t.Fatalf("capacity 1 is used, but got %+v", next)
	}
	if st, _ := m.Job(gpu.ID); st.State != JobQueued {
		t.Fatalf("gpu job state = %s, want queued (no matching labels)", st.State)
	}
	next, _ := s.GetMessage(ctx, 0, 2)
	if next == nil || next.JobAssignedMessages[0].RunnerRequestID != j2.ID {
		t.Fatalf("after raising capacity: %+v, want job %d assigned", next, j2.ID)
	}
}

func TestJobsWaitForALiveSession(t *testing.T) {
	m, _ := newTestModel(t)
	job, _ := m.InjectJob(JobSpec{Repo: "o/a"})
	_, _, s := openSet(t, m, "o/a", "set", defaultLabels)
	if st, _ := m.Job(job.ID); st.State != JobQueued {
		t.Fatalf("state before the first poll = %s, want queued (capacity unknown)", st.State)
	}
	if msg := poll(t, s, 1); msg == nil || len(msg.JobAssignedMessages) != 1 {
		t.Fatalf("poll = %+v, want the queued job assigned", msg)
	}
}

func TestGetMessageWakesOnInject(t *testing.T) {
	clk := newFakeClock()
	m := NewModel(Options{Now: clk.Now, PollWait: 2 * time.Second})
	_, _, s := openSet(t, m, "o/a", "set", defaultLabels)
	done := make(chan *scaleset.RunnerScaleSetMessage, 1)
	go func() {
		msg, _ := s.GetMessage(context.Background(), 0, 1)
		done <- msg
	}()
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	if _, err := m.InjectJob(JobSpec{Repo: "o/a"}); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-done:
		if msg == nil || len(msg.JobAssignedMessages) != 1 {
			t.Fatalf("woken poll = %+v", msg)
		}
		if el := time.Since(start); el > 500*time.Millisecond {
			t.Fatalf("the long poll noticed the job after %s", el)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the long poll did not wake on InjectJob")
	}
}

func TestDeleteScaleSetRequeuesAssignedJobs(t *testing.T) {
	m, _ := newTestModel(t)
	_, ss, s := openSet(t, m, "o/a", "set", defaultLabels)
	job, _ := m.InjectJob(JobSpec{Repo: "o/a"})
	poll(t, s, 1)
	if err := m.DeleteScaleSet("o/a", ss.ID); err != nil {
		t.Fatal(err)
	}
	if st, _ := m.Job(job.ID); st.State != JobQueued {
		t.Fatalf("state = %s, want queued after its scale set was deleted", st.State)
	}
	if got := m.Deleted("o/a"); len(got) != 1 || got[0] != ss.ID {
		t.Fatalf("Deleted = %v", got)
	}
}
