package ghsim

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghapi"
)

func TestClaimStartCompleteLifecycle(t *testing.T) {
	m, _ := newTestModel(t)
	ctx := context.Background()
	c, ss, s := openSet(t, m, "o/a", "set", defaultLabels)
	job, _ := m.InjectJob(JobSpec{Repo: "o/a", Duration: 3 * time.Second})
	poll(t, s, 1) // JobAssigned
	sj := mintRunner(t, c, ss.ID, "gso-alpha-a-abc123")

	claim, err := m.ClaimJob(ctx, sj.RunnerName, sj.Token)
	if err != nil || claim.JobID != job.ID || claim.Duration != 3*time.Second || claim.Result != "succeeded" {
		t.Fatalf("claim = %+v, %v", claim, err)
	}
	started := poll(t, s, 1)
	if started == nil || len(started.JobStartedMessages) != 1 || started.JobStartedMessages[0].RunnerName != sj.RunnerName {
		t.Fatalf("after claim: %+v, want JobStarted for %s", started, sj.RunnerName)
	}
	if cancelled, err := m.JobStatus(sj.RunnerName, sj.Token, job.ID); err != nil || cancelled {
		t.Fatalf("JobStatus = %v, %v", cancelled, err)
	}
	if _, err := m.ClaimJob(ctx, sj.RunnerName, "wrong-token"); !errors.Is(err, ErrUnknownRunner) {
		t.Fatalf("claim with a bad token: %v, want ErrUnknownRunner", err)
	}
	if err := m.CompleteJob(sj.RunnerName, sj.Token, job.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := m.CompleteJob(sj.RunnerName, sj.Token, job.ID, ""); err != nil {
		t.Fatalf("a repeated completion must be idempotent: %v", err)
	}
	done := poll(t, s, 1)
	if done == nil || len(done.JobCompletedMessages) != 1 {
		t.Fatalf("after completion: %+v", done)
	}
	jc := done.JobCompletedMessages[0]
	if jc.Result != "succeeded" || jc.RunnerName != sj.RunnerName {
		t.Fatalf("JobCompleted = %+v", jc)
	}
	if done.Statistics.TotalAssignedJobs != 0 {
		t.Fatalf("TotalAssignedJobs = %d after completion", done.Statistics.TotalAssignedJobs)
	}
	if ref, _ := c.GetRunnerByName(ctx, sj.RunnerName); ref != nil {
		t.Fatal("an ephemeral runner is still registered after its job")
	}
	if st, _ := m.Job(job.ID); st.State != JobCompleted || st.Runner != sj.RunnerName {
		t.Fatalf("job = %+v", st)
	}
	if m.MintCount("o/a") != 1 {
		t.Fatalf("MintCount = %d", m.MintCount("o/a"))
	}
}

func TestRunnerNamesAreUnique(t *testing.T) {
	m, _ := newTestModel(t)
	c, ss, _ := openSet(t, m, "o/a", "set", defaultLabels)
	mintRunner(t, c, ss.ID, "gso-alpha-a-dup001")
	_, err := c.GenerateJitRunnerConfig(context.Background(), &scaleset.RunnerScaleSetJitRunnerSetting{Name: "gso-alpha-a-dup001"}, ss.ID)
	if !errors.Is(err, scaleset.RunnerExistsError) {
		t.Fatalf("duplicate runner name: %v, want RunnerExistsError", err)
	}
}

func TestRemoveRunner(t *testing.T) {
	m, _ := newTestModel(t)
	ctx := context.Background()
	c, ss, s := openSet(t, m, "o/a", "set", defaultLabels)

	idle := mintRunner(t, c, ss.ID, "gso-x-a-idle01")
	ref, _ := c.GetRunnerByName(ctx, idle.RunnerName)
	if ref == nil || ref.RunnerScaleSetID != ss.ID {
		t.Fatalf("GetRunnerByName = %+v", ref)
	}
	if err := c.RemoveRunner(ctx, int64(ref.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ClaimJob(ctx, idle.RunnerName, idle.Token); !errors.Is(err, ErrRunnerRemoved) {
		t.Fatalf("claim after removal: %v, want ErrRunnerRemoved", err)
	}
	if got := m.Removed("o/a"); len(got) != 1 || got[0] != int64(ref.ID) {
		t.Fatalf("Removed = %v", got)
	}

	if _, err := m.InjectJob(JobSpec{Repo: "o/a"}); err != nil {
		t.Fatal(err)
	}
	poll(t, s, 1)
	busy := mintRunner(t, c, ss.ID, "gso-x-a-busy01")
	if _, err := m.ClaimJob(ctx, busy.RunnerName, busy.Token); err != nil {
		t.Fatal(err)
	}
	bref, _ := c.GetRunnerByName(ctx, busy.RunnerName)
	if err := c.RemoveRunner(ctx, int64(bref.ID)); !errors.Is(err, scaleset.JobStillRunningError) {
		t.Fatalf("removing a busy runner: %v, want JobStillRunningError", err)
	}
	if err := c.RemoveRunner(ctx, 999); !errors.Is(err, scaleset.RunnerNotFoundError) {
		t.Fatalf("removing an unknown runner: %v, want RunnerNotFoundError", err)
	}
}

func TestClaimWaitsForAnAssignment(t *testing.T) {
	m, _ := newTestModel(t)
	c, ss, s := openSet(t, m, "o/a", "set", defaultLabels)
	sj := mintRunner(t, c, ss.ID, "gso-x-a-wait01")
	got := make(chan *Claim, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		claim, _ := m.ClaimJob(ctx, sj.RunnerName, sj.Token)
		got <- claim
	}()
	time.Sleep(50 * time.Millisecond)
	job, _ := m.InjectJob(JobSpec{Repo: "o/a"})
	poll(t, s, 1)
	select {
	case claim := <-got:
		if claim == nil || claim.JobID != job.ID {
			t.Fatalf("claim = %+v, want job %d", claim, job.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ClaimJob did not return after the job was assigned")
	}
}

func TestUnclaimedAssignmentIsRequeuedThenCancelled(t *testing.T) {
	m, clk := newTestModel(t)
	_, _, s := openSet(t, m, "o/a", "set", defaultLabels)
	job, _ := m.InjectJob(JobSpec{Repo: "o/a"})
	for round := 1; round <= 3; round++ {
		if msg := poll(t, s, 1); msg == nil || len(msg.JobAssignedMessages) != 1 {
			t.Fatalf("round %d: %+v, want JobAssigned", round, msg)
		}
		clk.Advance(time.Minute + time.Second)
		msg := poll(t, s, 1)
		if msg == nil || len(msg.JobCompletedMessages) != 1 {
			t.Fatalf("round %d: %+v, want the reassignment JobCompleted", round, msg)
		}
		if jc := msg.JobCompletedMessages[0]; jc.Result != "canceled" || jc.RunnerName != "" {
			t.Fatalf("round %d: JobCompleted = %+v, want canceled with no runner", round, jc)
		}
	}
	if msg := poll(t, s, 1); msg == nil || len(msg.JobAssignedMessages) != 1 {
		t.Fatalf("fourth assignment missing: %+v", msg)
	}
	clk.Advance(time.Minute + time.Second)
	poll(t, s, 1)
	st, _ := m.Job(job.ID)
	if st.State != JobCancelled || st.Reassigned != 4 {
		t.Fatalf("job = %+v, want cancelled after 3 requeues", st)
	}
}

func TestCancelRunningJob(t *testing.T) {
	m, _ := newTestModel(t)
	ctx := context.Background()
	c, ss, s := openSet(t, m, "o/a", "set", defaultLabels)
	job, _ := m.InjectJob(JobSpec{Repo: "o/a", Duration: time.Hour})
	poll(t, s, 1)
	sj := mintRunner(t, c, ss.ID, "gso-x-a-cncl01")
	if _, err := m.ClaimJob(ctx, sj.RunnerName, sj.Token); err != nil {
		t.Fatal(err)
	}
	if err := c.CancelWorkflowRun(ctx, job.RunID); err != nil {
		t.Fatal(err)
	}
	if cancelled, _ := m.JobStatus(sj.RunnerName, sj.Token, job.ID); !cancelled {
		t.Fatal("JobStatus does not report the cancellation")
	}
	if err := m.CompleteJob(sj.RunnerName, sj.Token, job.ID, "canceled"); err != nil {
		t.Fatal(err)
	}
	if st, _ := m.Job(job.ID); st.State != JobCancelled || st.Result != "canceled" {
		t.Fatalf("job = %+v", st)
	}
	if got := m.Cancelled("o/a"); len(got) != 1 || got[0] != job.RunID {
		t.Fatalf("Cancelled = %v", got)
	}
}

func TestRepoVisibilityAndWorkflowRuns(t *testing.T) {
	m, _ := newTestModel(t)
	ctx := context.Background()
	c := m.Client("o/a")
	if info, _ := c.Repo(ctx); !info.Private || info.FullName != "o/a" {
		t.Fatalf("default repo = %+v, want private", info)
	}
	job, _ := m.InjectJob(JobSpec{Repo: "o/a", Public: true, Fork: true, Event: "pull_request"})
	if info, _ := c.Repo(ctx); info.Private || info.Visibility != "public" {
		t.Fatalf("repo = %+v, want public after a public job", info)
	}
	run, err := c.WorkflowRun(ctx, job.RunID)
	if err != nil || run.Event != "pull_request" || run.RepoFullName != "o/a" || run.HeadRepoFullName != "fork-o/a" {
		t.Fatalf("run = %+v, %v", run, err)
	}
	m.FailRuns("o/a", 1)
	if _, err := c.WorkflowRun(ctx, job.RunID); err == nil {
		t.Fatal("FailRuns did not fail the lookup")
	}
	if _, err := c.WorkflowRun(ctx, job.RunID); err != nil {
		t.Fatalf("second lookup: %v", err)
	}
	if _, err := c.WorkflowRun(ctx, 424242); !errors.Is(err, scaleset.NotFoundError) {
		t.Fatalf("unknown run: %v, want NotFoundError", err)
	}
	m.SetRepo("o/a", ghapi.RepoInfo{FullName: "o/a", Private: true, Visibility: "private"})
	if info, _ := c.Repo(ctx); !info.Private {
		t.Fatal("SetRepo did not override visibility")
	}
}

func TestPushAndSetStatsBeforeCreation(t *testing.T) {
	m, _ := newTestModel(t)
	m.SetStats("o/a", "set", scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 2})
	m.Push("o/a", "set", &scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{{RunnerName: "r1"}}})
	_, _, s := openSet(t, m, "o/a", "set", defaultLabels)
	if st := s.Session().Statistics; st == nil || st.TotalAssignedJobs != 2 {
		t.Fatalf("session statistics = %+v, want the SetStats override", st)
	}
	msg := poll(t, s, 1)
	if msg == nil || msg.MessageID != 1 || len(msg.JobStartedMessages) != 1 || msg.Statistics.TotalAssignedJobs != 2 {
		t.Fatalf("pushed message = %+v", msg)
	}
}

func TestPushKeepsAnExplicitMessageID(t *testing.T) {
	m, _ := newTestModel(t)
	_, _, s := openSet(t, m, "o/a", "set", defaultLabels)
	m.Push("o/a", "set", &scaleset.RunnerScaleSetMessage{MessageID: 7})
	m.Push("o/a", "set", &scaleset.RunnerScaleSetMessage{})
	if first := poll(t, s, 1); first == nil || first.MessageID != 7 {
		t.Fatalf("first = %+v, want the explicit MessageID 7", first)
	}
	if second := poll(t, s, 1); second == nil || second.MessageID != 8 {
		t.Fatalf("second = %+v, want the next ID 8", second)
	}
}

func TestFailSessionsAfterCreation(t *testing.T) {
	m, _ := newTestModel(t)
	ctx := context.Background()
	c := m.Client("o/a")
	ss, err := c.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{Name: "set", Labels: defaultLabels})
	if err != nil {
		t.Fatal(err)
	}
	m.FailSessions("o/a", "set", errors.New("boom"), 1) // keyed by name, armed after creation
	if _, err := c.OpenSession(ctx, ss.ID, "x"); err == nil || err.Error() != "boom" {
		t.Fatalf("first open = %v, want the injected error", err)
	}
	if _, err := c.OpenSession(ctx, ss.ID, "x"); err != nil {
		t.Fatalf("second open: %v", err)
	}
}

func TestInjectJobValidatesRepo(t *testing.T) {
	m, _ := newTestModel(t)
	if _, err := m.InjectJob(JobSpec{Repo: "not-a-repo"}); !errors.Is(err, scaleset.BadRequestError) {
		t.Fatalf("error = %v, want BadRequestError", err)
	}
}
