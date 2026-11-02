package ghsim

import (
	"context"
	"encoding/base64"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

type scalesetJitSetting = scaleset.RunnerScaleSetJitRunnerSetting

// startSimServer serves m over HTTP on a loopback port whose URL is the
// model's ServerURL, so minted JIT configs point at it.
func startSimServer(t *testing.T) *Model {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := NewModel(Options{PollWait: 20 * time.Millisecond, ClaimTimeout: time.Minute, ServerURL: "http://" + ln.Addr().String()})
	srv := httptest.NewUnstartedServer(NewServer(m, nil))
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return m
}

func withFastStatusPoll(t *testing.T) {
	t.Helper()
	old := fakeRunnerPoll
	fakeRunnerPoll = func() time.Duration { return 20 * time.Millisecond }
	t.Cleanup(func() { fakeRunnerPoll = old })
}

// The simulator holds a claim long-poll for defaultClaimWait before answering
// 204. A client timeout at or under that would abort every idle poll, so the
// fake runner's timeout must clear it with room to spare (ruling 1).
func TestFakeRunnerHTTPTimeoutExceedsTheClaimWait(t *testing.T) {
	if fakeRunnerHTTPTimeout < 40*time.Second {
		t.Fatalf("fakeRunnerHTTPTimeout = %s, want at least 40s", fakeRunnerHTTPTimeout)
	}
	if fakeRunnerHTTPTimeout <= defaultClaimWait+10*time.Second {
		t.Fatalf("fakeRunnerHTTPTimeout = %s does not clear the %s claim wait by 10s", fakeRunnerHTTPTimeout, defaultClaimWait)
	}
}

func TestFakeRunnerRunsAJobToCompletion(t *testing.T) {
	withFastStatusPoll(t)
	m := startSimServer(t)
	c, ss, s := openSet(t, m, "o/a", "set", defaultLabels)
	job, _ := m.InjectJob(JobSpec{Repo: "o/a", Duration: 150 * time.Millisecond})
	poll(t, s, 1)
	cfg, err := c.GenerateJitRunnerConfig(context.Background(), &scalesetJitSetting{Name: "gso-alpha-a-run001"}, ss.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := RunFakeRunner(ctx, cfg.EncodedJITConfig, nil); err != nil {
		t.Fatalf("RunFakeRunner: %v", err)
	}
	st, _ := m.Job(job.ID)
	if st.State != JobCompleted || st.Result != "succeeded" || st.Runner != "gso-alpha-a-run001" {
		t.Fatalf("job = %+v", st)
	}
}

func TestFakeRunnerHonoursCancellation(t *testing.T) {
	withFastStatusPoll(t)
	m := startSimServer(t)
	c, ss, s := openSet(t, m, "o/a", "set", defaultLabels)
	job, _ := m.InjectJob(JobSpec{Repo: "o/a", Duration: time.Hour})
	poll(t, s, 1)
	cfg, err := c.GenerateJitRunnerConfig(context.Background(), &scalesetJitSetting{Name: "gso-alpha-a-run002"}, ss.ID)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- RunFakeRunner(context.Background(), cfg.EncodedJITConfig, nil) }()
	deadline := time.Now().Add(5 * time.Second)
	for st, _ := m.Job(job.ID); st.State != JobRunning; st, _ = m.Job(job.ID) {
		if time.Now().After(deadline) {
			t.Fatal("the fake runner never started the job")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := m.CancelRun("o/a", job.RunID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunFakeRunner: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the fake runner ignored the cancellation")
	}
	if st, _ := m.Job(job.ID); st.State != JobCancelled {
		t.Fatalf("job = %+v, want cancelled", st)
	}
}

func TestFakeRunnerExitsWhenRemovedBeforeAJob(t *testing.T) {
	m := startSimServer(t)
	c, ss, _ := openSet(t, m, "o/a", "set", defaultLabels)
	cfg, err := c.GenerateJitRunnerConfig(context.Background(), &scalesetJitSetting{Name: "gso-alpha-a-run003"}, ss.ID)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- RunFakeRunner(context.Background(), cfg.EncodedJITConfig, nil) }()
	time.Sleep(50 * time.Millisecond)
	if err := c.RemoveRunner(context.Background(), int64(cfg.Runner.ID)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunFakeRunner after removal: %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the fake runner kept waiting after it was removed")
	}
}

func TestFakeRunnerRefusesARealJIT(t *testing.T) {
	runnerFile := base64.StdEncoding.EncodeToString([]byte(`{"agentName":"r","gitHubUrl":"https://github.com/o/a"}`))
	realJIT := base64.StdEncoding.EncodeToString([]byte(`{".runner":"` + runnerFile + `"}`))
	if err := RunFakeRunner(context.Background(), realJIT, nil); err == nil {
		t.Fatal("the fake runner accepted a real JIT config")
	}
}
