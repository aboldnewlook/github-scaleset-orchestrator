package ghsim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/google/uuid"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghapi"
)

type clientUnderTest struct {
	name      string
	newClient func(t *testing.T, m *Model, repo string) ghapi.Client
}

// Both clients must honour the same router-side contract.
var clientsUnderTest = []clientUnderTest{
	{"in-process", func(_ *testing.T, m *Model, repo string) ghapi.Client { return m.Client(repo) }},
	{"remote", func(t *testing.T, m *Model, repo string) ghapi.Client {
		srv := httptest.NewServer(NewServer(m, nil))
		t.Cleanup(srv.Close)
		c, err := NewRemoteFactory(srv.URL, nil)(context.Background(), config.Repo{Name: repo})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}},
}

func TestRouterContract(t *testing.T) {
	for _, cut := range clientsUnderTest {
		t.Run(cut.name, func(t *testing.T) {
			m, _ := newTestModel(t)
			c := cut.newClient(t, m, "o/a")
			ctx := context.Background()

			if ss, err := c.GetRunnerScaleSet(ctx, 1, "set"); err != nil || ss != nil {
				t.Fatalf("missing set = %v, %v", ss, err)
			}
			ss, err := c.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{Name: "set", RunnerGroupID: 1, Labels: defaultLabels})
			if err != nil || ss.ID == 0 {
				t.Fatalf("create = %+v, %v", ss, err)
			}
			if got, err := c.GetRunnerScaleSet(ctx, 1, "set"); err != nil || got.ID != ss.ID {
				t.Fatalf("get = %+v, %v", got, err)
			}
			upd, err := c.UpdateRunnerScaleSet(ctx, ss.ID, &scaleset.RunnerScaleSet{Labels: append(append([]scaleset.Label(nil), defaultLabels...), scaleset.Label{Name: "gso"})})
			if err != nil || len(upd.Labels) != 4 {
				t.Fatalf("update = %+v, %v", upd, err)
			}
			if list, err := c.ListRunnerScaleSets(ctx, 1); err != nil || len(list) != 1 {
				t.Fatalf("list = %v, %v", list, err)
			}

			s, err := c.OpenSession(ctx, ss.ID, "router-1")
			if err != nil || s.Session().SessionID == uuid.Nil || s.Session().Statistics == nil {
				t.Fatalf("open = %+v, %v", s, err)
			}
			if _, err := c.OpenSession(ctx, ss.ID, "router-2"); !ghapi.IsConflict(err) {
				t.Fatalf("second session: %v, want a conflict", err)
			}

			job, err := m.InjectJob(JobSpec{Repo: "o/a", Duration: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			msg, err := s.GetMessage(ctx, 0, 1)
			if err != nil || msg == nil || len(msg.JobAssignedMessages) != 1 || msg.JobAssignedMessages[0].RunnerRequestID != job.ID {
				t.Fatalf("message = %+v, %v", msg, err)
			}
			if err := s.DeleteMessage(ctx, msg.MessageID); err != nil {
				t.Fatal(err)
			}
			if idle, err := s.GetMessage(ctx, 0, 1); err != nil || idle != nil {
				t.Fatalf("idle poll = %+v, %v", idle, err)
			}
			if ids, err := s.AcquireJobs(ctx, []int64{7}); err != nil || len(ids) != 1 || ids[0] != 7 {
				t.Fatalf("acquire = %v, %v", ids, err)
			}

			jit, err := c.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: "gso-local-a-abc123"}, ss.ID)
			if err != nil || jit.Runner == nil {
				t.Fatalf("mint = %+v, %v", jit, err)
			}
			if _, err := c.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: "gso-local-a-abc123"}, ss.ID); !errors.Is(err, scaleset.RunnerExistsError) {
				t.Fatalf("duplicate mint: %v, want RunnerExistsError", err)
			}
			ref, err := c.GetRunnerByName(ctx, "gso-local-a-abc123")
			if err != nil || ref == nil || ref.ID != jit.Runner.ID || ref.RunnerScaleSetID != ss.ID {
				t.Fatalf("by name = %+v, %v", ref, err)
			}
			if ref, err := c.GetRunnerByName(ctx, "nope"); err != nil || ref != nil {
				t.Fatalf("unknown runner = %+v, %v", ref, err)
			}
			sj, _, _ := DecodeSimJIT(jit.EncodedJITConfig)
			if _, err := m.ClaimJob(ctx, sj.RunnerName, sj.Token); err != nil {
				t.Fatal(err)
			}
			if err := c.RemoveRunner(ctx, int64(ref.ID)); !errors.Is(err, scaleset.JobStillRunningError) {
				t.Fatalf("remove busy runner: %v, want JobStillRunningError", err)
			}

			if info, err := c.Repo(ctx); err != nil || !info.Private || info.FullName != "o/a" {
				t.Fatalf("repo = %+v, %v", info, err)
			}
			if run, err := c.WorkflowRun(ctx, job.RunID); err != nil || run.HeadRepoFullName != "o/a" {
				t.Fatalf("run = %+v, %v", run, err)
			}
			if _, err := c.WorkflowRun(ctx, 9999); !errors.Is(err, scaleset.NotFoundError) {
				t.Fatalf("unknown run: %v, want NotFoundError", err)
			}
			if err := c.CancelWorkflowRun(ctx, job.RunID); err != nil {
				t.Fatal(err)
			}
			if cancelled, _ := m.JobStatus(sj.RunnerName, sj.Token, job.ID); !cancelled {
				t.Fatal("cancel did not reach the running job")
			}

			if err := s.Close(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := s.GetMessage(ctx, 0, 1); !errors.Is(err, scaleset.MessageQueueTokenExpiredError) {
				t.Fatalf("poll after close: %v, want MessageQueueTokenExpiredError", err)
			}
		})
	}
}

func postJSON(t *testing.T, url string, in, out any) int {
	t.Helper()
	body, _ := json.Marshal(in)
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if out != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode
}

func TestRunnerAndAdminAPI(t *testing.T) {
	m, _ := newTestModel(t)
	srv := httptest.NewServer(NewServer(m, nil))
	defer srv.Close()
	admin := NewAdminClient(srv.URL)
	ctx := context.Background()
	c, ss, s := openSet(t, m, "o/a", "set", defaultLabels)

	jobs, err := admin.InjectJob(ctx, JobSpec{Repo: "o/a", Duration: time.Second}, 2)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("inject = %v, %v", jobs, err)
	}
	if _, err := admin.InjectJob(ctx, JobSpec{Repo: "not-a-repo"}, 1); !errors.Is(err, scaleset.BadRequestError) {
		t.Fatalf("bad repo: %v, want BadRequestError", err)
	}
	poll(t, s, 1)
	sj := mintRunner(t, c, ss.ID, "gso-alpha-a-zzz111")

	var claim Claim
	if code := postJSON(t, srv.URL+"/runner/claim", runnerRequest{Name: sj.RunnerName, Token: sj.Token}, &claim); code != http.StatusOK || claim.JobID != jobs[0].ID {
		t.Fatalf("claim = %d %+v", code, claim)
	}
	var st statusResponse
	if code := postJSON(t, srv.URL+"/runner/status", runnerRequest{Name: sj.RunnerName, Token: sj.Token, JobID: claim.JobID}, &st); code != http.StatusOK || st.Cancelled {
		t.Fatalf("status = %d %+v", code, st)
	}
	if code := postJSON(t, srv.URL+"/runner/claim", runnerRequest{Name: sj.RunnerName, Token: "wrong"}, nil); code != http.StatusUnauthorized {
		t.Fatalf("claim with a wrong token = %d, want 401", code)
	}
	if code := postJSON(t, srv.URL+"/runner/complete", runnerRequest{Name: sj.RunnerName, Token: sj.Token, JobID: claim.JobID, Result: "succeeded"}, nil); code != http.StatusNoContent {
		t.Fatalf("complete = %d, want 204", code)
	}
	got, err := admin.Jobs(ctx)
	if err != nil || got[0].State != JobCompleted {
		t.Fatalf("jobs = %+v, %v", got, err)
	}
	runners, err := admin.Runners(ctx)
	if err != nil || len(runners) != 1 || runners[0].State != "finished" {
		t.Fatalf("runners = %+v, %v", runners, err)
	}
	if err := admin.CancelRun(ctx, "o/a", jobs[1].RunID); err != nil {
		t.Fatal(err)
	}
	if got, _ = admin.Jobs(ctx); got[1].State != JobCancelled {
		t.Fatalf("job 2 = %+v, want cancelled", got[1])
	}
}

func TestClaimLongPollEndsWithNoContent(t *testing.T) {
	m, _ := newTestModel(t)
	srv := httptest.NewServer(newServer(m, nil, 50*time.Millisecond))
	defer srv.Close()
	c, ss, _ := openSet(t, m, "o/a", "set", defaultLabels)
	sj := mintRunner(t, c, ss.ID, "gso-alpha-a-idle99")
	if code := postJSON(t, srv.URL+"/runner/claim", runnerRequest{Name: sj.RunnerName, Token: sj.Token}, nil); code != http.StatusNoContent {
		t.Fatalf("idle claim = %d, want 204", code)
	}
}
