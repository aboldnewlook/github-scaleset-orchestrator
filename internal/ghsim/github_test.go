package ghsim

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
)

const testPAT = "ghp_not-a-real-token"

// realClient is the unmodified actions/scaleset client pointed at the simulator
// as if it were a GitHub Enterprise Server host.
func realClient(t *testing.T, srvURL, repo string) *scaleset.Client {
	t.Helper()
	c, err := scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL:     srvURL + "/" + repo,
		PersonalAccessToken: testPAT,
		SystemInfo:          scaleset.SystemInfo{System: "gso-test", Version: "0", Subsystem: "client"},
	}, scaleset.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRealScalesetClientAgainstServer(t *testing.T) {
	m, _ := newTestModel(t)
	srv := httptest.NewServer(NewServer(m, nil))
	defer srv.Close()
	ctx := context.Background()
	// A different spelling than the model's first-seen one: identity is case-insensitive.
	if _, err := m.InjectJob(JobSpec{Repo: "o/a", Duration: time.Second}); err != nil {
		t.Fatal(err)
	}
	sc := realClient(t, srv.URL, "O/A")

	if got, err := sc.GetRunnerScaleSet(ctx, 1, "set"); err != nil || got != nil {
		t.Fatalf("missing set = %v, %v", got, err)
	}
	ss, err := sc.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{Name: "set", RunnerGroupID: 1, Labels: defaultLabels})
	if err != nil || ss.ID == 0 || len(ss.Labels) != 3 {
		t.Fatalf("create = %+v, %v", ss, err)
	}
	if got, err := sc.GetRunnerScaleSet(ctx, 1, "set"); err != nil || got == nil || got.ID != ss.ID {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if byID, err := sc.GetRunnerScaleSetByID(ctx, ss.ID); err != nil || byID.Name != "set" {
		t.Fatalf("get by id = %+v, %v", byID, err)
	}
	upd, err := sc.UpdateRunnerScaleSet(ctx, ss.ID, &scaleset.RunnerScaleSet{Name: "set", Labels: append(append([]scaleset.Label(nil), defaultLabels...), scaleset.Label{Name: "gso"})})
	if err != nil || len(upd.Labels) != 4 {
		t.Fatalf("update = %+v, %v", upd, err)
	}
	if list, err := sc.ListRunnerScaleSets(ctx, 1); err != nil || len(list) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}
	if other, _ := realClient(t, srv.URL, "o/b").GetRunnerScaleSet(ctx, 1, "set"); other != nil {
		t.Fatal("a scale set leaked into another repository")
	}

	s, err := sc.MessageSessionClient(ctx, ss.ID, "router-1")
	if err != nil {
		t.Fatal(err)
	}
	sess := s.Session()
	if sess.Statistics == nil || sess.MessageQueueURL == "" || sess.MessageQueueAccessToken == "" {
		t.Fatalf("session = %+v", sess)
	}
	if strings.Contains(sess.MessageQueueURL, sess.MessageQueueAccessToken) {
		t.Fatal("the message queue token is in the message queue URL")
	}
	if _, err := sc.MessageSessionClient(ctx, ss.ID, "router-2"); !errors.Is(err, scaleset.ConflictError) {
		t.Fatalf("second session: %v, want ConflictError", err)
	}

	msg, err := s.GetMessage(ctx, 0, 1)
	if err != nil || msg == nil || len(msg.JobAssignedMessages) != 1 || msg.JobAssignedMessages[0].RepositoryName != "a" {
		t.Fatalf("message = %+v, %v", msg, err)
	}
	// Redelivered until acknowledged.
	if again, err := s.GetMessage(ctx, 0, 1); err != nil || again == nil || again.MessageID != msg.MessageID {
		t.Fatalf("redelivery = %+v, %v", again, err)
	}
	if err := s.DeleteMessage(ctx, msg.MessageID); err != nil {
		t.Fatal(err)
	}
	if idle, err := s.GetMessage(ctx, msg.MessageID, 1); err != nil || idle != nil {
		t.Fatalf("idle poll = %+v, %v (want nil, nil from a 202)", idle, err)
	}
	if ids, err := s.AcquireJobs(ctx, []int64{7, 8}); err != nil || len(ids) != 2 {
		t.Fatalf("acquire = %v, %v", ids, err)
	}

	jit, err := sc.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: "gso-local-a-abc123", WorkFolder: "_work"}, ss.ID)
	if err != nil || jit.Runner == nil || jit.Runner.RunnerScaleSetID != ss.ID {
		t.Fatalf("mint = %+v, %v", jit, err)
	}
	sj, ok, err := DecodeSimJIT(jit.EncodedJITConfig)
	if err != nil || !ok || sj.ServerURL != "http://sim.test:9300" {
		t.Fatalf("jit = %+v ok=%v err=%v", sj, ok, err)
	}
	if _, err := sc.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: "gso-local-a-abc123"}, ss.ID); !errors.Is(err, scaleset.RunnerExistsError) {
		t.Fatalf("duplicate mint: %v, want RunnerExistsError", err)
	}
	ref, err := sc.GetRunnerByName(ctx, "gso-local-a-abc123")
	if err != nil || ref == nil || ref.ID != jit.Runner.ID {
		t.Fatalf("by name = %+v, %v", ref, err)
	}
	if ref, err := sc.GetRunnerByName(ctx, "nope"); err != nil || ref != nil {
		t.Fatalf("unknown runner = %+v, %v", ref, err)
	}
	if _, err := m.ClaimJob(ctx, sj.RunnerName, sj.Token); err != nil {
		t.Fatal(err)
	}
	if err := sc.RemoveRunner(ctx, int64(ref.ID)); !errors.Is(err, scaleset.JobStillRunningError) {
		t.Fatalf("remove busy runner: %v, want JobStillRunningError", err)
	}
	idle, err := sc.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: "gso-local-a-idle01"}, ss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.RemoveRunner(ctx, int64(idle.Runner.ID)); err != nil {
		t.Fatalf("remove idle runner: %v", err)
	}
	if err := sc.RemoveRunner(ctx, int64(idle.Runner.ID)); !errors.Is(err, scaleset.RunnerNotFoundError) {
		t.Fatalf("remove twice: %v, want RunnerNotFoundError", err)
	}

	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetMessage(ctx, 0, 1); err == nil {
		t.Fatal("polling a closed session succeeded")
	}
	if _, err := sc.MessageSessionClient(ctx, ss.ID, "router-3"); err != nil {
		t.Fatalf("session after close: %v", err)
	}
	if err := sc.DeleteRunnerScaleSet(ctx, ss.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := sc.GetRunnerScaleSet(ctx, 1, "set"); got != nil {
		t.Fatal("deleted scale set still found")
	}
	if got := m.Deleted("o/a"); len(got) != 1 {
		t.Fatalf("deleted = %v", got)
	}
}

type recordingScaler struct {
	assigned chan int64
	initial  chan struct{}
}

func (r *recordingScaler) Scale(_ context.Context, msg *scaleset.RunnerScaleSetMessage) error {
	if msg == nil {
		return nil
	}
	if msg.MessageID == listener.InitialMessageID {
		close(r.initial)
		return nil
	}
	for _, a := range msg.JobAssignedMessages {
		r.assigned <- a.RunnerRequestID
	}
	return nil
}

func TestRealListenerAgainstServer(t *testing.T) {
	m, _ := newTestModel(t)
	srv := httptest.NewServer(NewServer(m, nil))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sc := realClient(t, srv.URL, "o/a")
	ss, err := sc.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{Name: "set", RunnerGroupID: 1, Labels: defaultLabels})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := sc.MessageSessionClient(ctx, ss.ID, "router")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close(context.Background()) }()
	l, err := listener.New(sess, listener.Config{ScaleSetID: ss.ID, MaxRunners: 2})
	if err != nil {
		t.Fatal(err)
	}
	rec := &recordingScaler{assigned: make(chan int64, 4), initial: make(chan struct{})}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- l.Run(runCtx, rec) }()

	select {
	case <-rec.initial:
	case <-ctx.Done():
		t.Fatal("listener never handled the initial statistics")
	}
	job, err := m.InjectJob(JobSpec{Repo: "o/a", Duration: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-rec.assigned:
		if id != job.ID {
			t.Fatalf("assigned %d, want %d", id, job.ID)
		}
	case <-ctx.Done():
		t.Fatal("the listener never saw the job")
	}
	if got := m.LastCapacity("o/a", "set"); got != 2 {
		t.Fatalf("capacity the listener advertised = %d, want 2", got)
	}
	stop()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("listener exit = %v, want context.Canceled", err)
	}
	// The listener acknowledged the message before it stopped.
	if msg, err := sess.GetMessage(ctx, 0, 1); err != nil || msg != nil {
		t.Fatalf("message after the run = %+v, %v; want it acknowledged", msg, err)
	}
}

func doJSON(t *testing.T, method, url string, out any) int {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("decoding %s: %v", b, err)
		}
	}
	return resp.StatusCode
}

// The shapes ghapi's REST client reads from api.github.com.
func TestGitHubRESTShapes(t *testing.T) {
	m, _ := newTestModel(t)
	srv := httptest.NewServer(NewServer(m, nil))
	defer srv.Close()
	job, err := m.InjectJob(JobSpec{Repo: "o/a", Public: true, Fork: true, Event: "pull_request_target"})
	if err != nil {
		t.Fatal(err)
	}

	var repo struct {
		FullName   string `json:"full_name"`
		Private    bool   `json:"private"`
		Visibility string `json:"visibility"`
	}
	if code := doJSON(t, http.MethodGet, srv.URL+"/api/v3/repos/O/A", &repo); code != http.StatusOK || repo.FullName != "o/a" || repo.Private || repo.Visibility != "public" {
		t.Fatalf("repo = %d %+v", code, repo)
	}
	repo.FullName = ""
	if code := doJSON(t, http.MethodGet, srv.URL+"/api/v3/repos/o/private", &repo); code != http.StatusOK || !repo.Private || repo.Visibility != "private" || repo.FullName != "o/private" {
		t.Fatalf("private repo = %d %+v", code, repo)
	}

	var run struct {
		ID         int64  `json:"id"`
		Event      string `json:"event"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		HeadRepository *struct {
			FullName string `json:"full_name"`
		} `json:"head_repository"`
	}
	runURL := srv.URL + "/api/v3/repos/o/a/actions/runs/"
	if code := doJSON(t, http.MethodGet, runURL+"1001", &run); code != http.StatusOK || run.ID != job.RunID || run.Event != "pull_request_target" ||
		run.Repository.FullName != "o/a" || run.HeadRepository == nil || run.HeadRepository.FullName == "o/a" {
		t.Fatalf("run = %d %+v", code, run)
	}
	if code := doJSON(t, http.MethodGet, runURL+"9999", nil); code != http.StatusNotFound {
		t.Fatalf("unknown run = %d, want 404", code)
	}
	if code := doJSON(t, http.MethodPost, runURL+"1001/cancel", nil); code != http.StatusAccepted {
		t.Fatalf("cancel = %d, want 202", code)
	}
	if got := m.Cancelled("o/a"); len(got) != 1 || got[0] != job.RunID {
		t.Fatalf("cancelled = %v", got)
	}
}

func TestQueueRejectsForgedTokenAndLogsNoSecrets(t *testing.T) {
	m, _ := newTestModel(t)
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := httptest.NewServer(NewServer(m, logger))
	defer srv.Close()
	ctx := context.Background()
	sc := realClient(t, srv.URL, "o/a")
	ss, err := sc.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{Name: "set", RunnerGroupID: 1, Labels: defaultLabels})
	if err != nil {
		t.Fatal(err)
	}
	s, err := sc.MessageSessionClient(ctx, ss.ID, "router")
	if err != nil {
		t.Fatal(err)
	}
	sess := s.Session()

	for _, tok := range []string{"", "Bearer nope", "Bearer " + sess.SessionID.String() + ".deadbeef"} {
		req, _ := http.NewRequest(http.MethodGet, sess.MessageQueueURL, nil)
		if tok != "" {
			req.Header.Set("Authorization", tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q: status %d, want 401", tok, resp.StatusCode)
		}
	}

	sj := mintRunner(t, m.Client("o/a"), ss.ID, "gso-x-a-secret")
	postJSON(t, srv.URL+"/runner/status", runnerRequest{Name: sj.RunnerName, Token: sj.Token, JobID: 1}, nil)
	for _, secret := range []string{sj.Token, sess.MessageQueueAccessToken, testPAT} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("a secret leaked into the server log: %s", logs.String())
		}
	}
}

func TestRunnerRegistrationRejectsMalformedURL(t *testing.T) {
	m, _ := newTestModel(t)
	srv := httptest.NewServer(NewServer(m, nil))
	defer srv.Close()
	for _, bad := range []string{"http://[::1", "%zz", "http://h/onlyowner", ""} {
		if code := postJSON(t, srv.URL+"/api/v3/actions/runner-registration", map[string]string{"url": bad}, nil); code != http.StatusBadRequest {
			t.Fatalf("url %q: status %d, want 400", bad, code)
		}
	}
	if code := doJSON(t, http.MethodGet, srv.URL+"/healthz", nil); code != http.StatusOK {
		t.Fatalf("server stopped serving: %d", code)
	}
}
