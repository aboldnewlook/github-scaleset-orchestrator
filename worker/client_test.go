package worker_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	workerv1 "github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
	"github.com/aboldnewlook/github-scaleset-orchestrator/worker"
	"github.com/aboldnewlook/github-scaleset-orchestrator/worker/workertest"
)

const (
	phaseRunning = workerv1.RunnerPhase_RUNNER_PHASE_RUNNING
	phaseExited  = workerv1.RunnerPhase_RUNNER_PHASE_EXITED
	waitTimeout  = 3 * time.Second
)

func TestNewValidatesConfig(t *testing.T) {
	h := newHarness(t)
	tests := []struct {
		name   string
		mutate func(*worker.Config)
	}{
		{"no dial address", func(c *worker.Config) { c.DialAddr = "" }},
		{"no pin", func(c *worker.Config) { c.Pin = "" }},
		{"no credential store", func(c *worker.Config) { c.Credential = nil }},
		{"no record store", func(c *worker.Config) { c.Records = nil }},
		{"no launchers", func(c *worker.Config) { c.Launchers = nil }},
		{"zero slots", func(c *worker.Config) { c.Slots = 0 }},
		{"two launchers of one kind", func(c *worker.Config) { c.Launchers = []worker.Launcher{h.launch, newFakeLauncher(linuxX64)} }},
		{"unknown public mode", func(c *worker.Config) { c.AcceptPublic = "sometimes" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := worker.New(h.config(tt.mutate)); err == nil {
				t.Fatal("New() accepted an invalid config")
			}
		})
	}
}

func TestClientRunsOfferToCompletion(t *testing.T) {
	h := newHarness(t)
	c, _ := h.start(h.config())

	_, name := h.router.Offer("acme/app", "linux-x64", false)
	h.router.WaitPhase(t, name, phaseRunning, waitTimeout)

	if got := h.launch.Started(); !slices.Equal(got, []string{name}) {
		t.Fatalf("started = %v, want [%s]", got, name)
	}
	info, err := worker.ParseJIT(h.launch.JITs()[0])
	if err != nil || info.Repo != "acme/app" {
		t.Fatalf("launcher got a JIT for %+v (err %v), want acme/app", info, err)
	}
	recs := h.records()
	if len(recs) != 1 || recs[0].Name != name || recs[0].Phase != "running" || recs[0].RunnerID == 0 || recs[0].Class != "linux-x64" {
		t.Fatalf("records = %+v", recs)
	}
	if st := c.Status(); st.Busy != 1 || st.Slots != 2 || !slices.Contains(st.Classes, "linux-x64") {
		t.Fatalf("status = %+v", st)
	}

	h.launch.finish(name, nil)
	h.router.WaitPhase(t, name, phaseExited, waitTimeout)
	waitFor(t, "record removed", func() bool { return len(h.records()) == 0 })
	waitFor(t, "slot freed", func() bool { return c.Status().Busy == 0 })
	if rep := reportFor(h.router, name, phaseExited); rep.GetExitCode() != 0 || rep.GetPreempted() || rep.GetError() != "" {
		t.Fatalf("exit report = %v, want a clean exit", rep)
	}
}

func TestClientRejectsJITForAnotherRepo(t *testing.T) {
	h := newHarness(t)
	h.router.SetJITRepoOverride("evil/repo")
	h.start(h.config())

	_, name := h.router.Offer("acme/app", "linux-x64", false)
	waitFor(t, "JIT rejected", func() bool { return slices.Contains(h.router.Rejected(), name) })
	if !slices.Contains(h.router.Accepted(), name) {
		t.Fatal("the worker must accept the offer before it can see and verify the JIT")
	}
	if got := h.launch.Started(); len(got) != 0 {
		t.Fatalf("launcher started %v from a JIT for another repo", got)
	}
	if recs := h.records(); len(recs) != 0 {
		t.Fatalf("records = %+v, want none", recs)
	}
	if strings.Contains(h.logs.String(), workertest.FakeJIT("https://github.com/evil/repo", name)) {
		t.Fatal("the JIT config appeared in the worker's log")
	}
}

func TestClientPublicOfferPolicy(t *testing.T) {
	tests := []struct {
		name          string
		mode          worker.PublicMode
		routerDefault bool
		wantRun       bool
	}{
		{"refuse beats the router default", worker.PublicRefuse, true, false},
		{"follow, router default off", worker.PublicFollow, false, false},
		{"follow, router default on", worker.PublicFollow, true, true},
		{"accept overrides the router default", worker.PublicAccept, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.router.SetWorkerDefaultAccept(tt.routerDefault)
			h.start(h.config(func(c *worker.Config) { c.AcceptPublic = tt.mode }))

			_, name := h.router.Offer("acme/app", "linux-x64", true)
			if tt.wantRun {
				h.router.WaitPhase(t, name, phaseRunning, waitTimeout)
				return
			}
			waitFor(t, "public offer declined", func() bool { return slices.Contains(h.router.Rejected(), name) })
			if slices.Contains(h.router.Accepted(), name) {
				t.Fatal("the worker accepted a public offer it must decline")
			}
		})
	}
}

func TestClientDeclinesReposOutsideItsAllowlist(t *testing.T) {
	h := newHarness(t)
	c, _ := h.start(h.config())

	_, name := h.router.Offer("other/app", "linux-x64", false)
	waitFor(t, "offer declined", func() bool { return slices.Contains(h.router.Rejected(), name) })

	c.SetPolicy([]string{"other/*"}, worker.PublicFollow)
	_, second := h.router.Offer("acme/app", "linux-x64", false)
	waitFor(t, "offer declined after SetPolicy", func() bool { return slices.Contains(h.router.Rejected(), second) })
	if got := h.router.Accepted(); len(got) != 0 {
		t.Fatalf("accepted %v outside the allowlist", got)
	}
}

func TestClientDeclinesOffersWhileUnavailable(t *testing.T) {
	h := newHarness(t)
	c, _ := h.start(h.config())
	c.SetAvailability(worker.Unavailable)
	waitFor(t, "unavailable heartbeat", func() bool {
		return h.router.LastHeartbeat().GetAvailability() == workerv1.Availability_AVAILABILITY_UNAVAILABLE
	})
	_, name := h.router.Offer("acme/app", "linux-x64", false)
	waitFor(t, "offer declined", func() bool { return slices.Contains(h.router.Rejected(), name) })
}

func TestClientReattachesAfterRestart(t *testing.T) {
	h := newHarness(t)
	_, stop := h.start(h.config())
	_, name := h.router.Offer("acme/app", "linux-x64", false)
	h.router.WaitPhase(t, name, phaseRunning, waitTimeout)

	if err := stop(); err != nil {
		t.Fatalf("Run() = %v after cancel, want nil", err)
	}
	if got := h.launch.Killed(); len(got) != 0 {
		t.Fatalf("shutdown killed %v; it must detach", got)
	}
	if recs := h.records(); len(recs) != 1 || recs[0].Name != name {
		t.Fatalf("records after shutdown = %+v, want %s", recs, name)
	}

	h.start(h.config())
	if got := h.router.Sessions(); got != 2 {
		t.Fatalf("sessions = %d, want 2", got)
	}
	running := h.router.LastOpenSession().GetRunning()
	if len(running) != 1 || running[0].GetRunnerName() != name || running[0].GetPhase() != phaseRunning || running[0].GetRunnerId() == 0 {
		t.Fatalf("second OpenSession running[] = %v, want %s RUNNING", running, name)
	}
	waitFor(t, "re-attached", func() bool { return h.launch.WaitCount(name) == 2 })
	if got := h.launch.Started(); len(got) != 1 {
		t.Fatalf("restart started %v again; it must only re-attach", got)
	}
	keeps := h.launch.Keeps()
	if last := keeps[len(keeps)-1]; !last[name] {
		t.Fatalf("RemoveOrphans keep set = %v, want it to protect %s", last, name)
	}

	h.launch.finish(name, nil)
	h.router.WaitPhase(t, name, phaseExited, waitTimeout)
	waitFor(t, "record removed", func() bool { return len(h.records()) == 0 })
}

func TestClientKillsRunnersTheRouterRejected(t *testing.T) {
	h := newHarness(t)
	_, stop := h.start(h.config())
	_, name := h.router.Offer("acme/app", "linux-x64", false)
	h.router.WaitPhase(t, name, phaseRunning, waitTimeout)
	if err := stop(); err != nil {
		t.Fatal(err)
	}

	h.router.SetRejectedRunners(name)
	h.start(h.config())
	waitFor(t, "rejected runner killed", func() bool { return slices.Contains(h.launch.Killed(), name) })
	waitFor(t, "record removed", func() bool { return len(h.records()) == 0 })
	if rep := reportFor(h.router, name, phaseExited); rep != nil {
		t.Fatalf("reported %v for a runner the router already dropped", rep)
	}
}

func TestClientRetriesWhileOldSessionIsActive(t *testing.T) {
	h := newHarness(t)
	h.router.FailNextOpenSessions(3)
	h.start(h.config())
	if got := h.router.Sessions(); got != 1 {
		t.Fatalf("sessions = %d, want 1 after three FailedPrecondition retries", got)
	}
}

func TestClientStopsWhenRevoked(t *testing.T) {
	h := newHarness(t)
	h.router.Revoke("box")
	c, err := worker.New(h.config())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	err = c.Run(ctx)
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("Run() = %v, want a PermissionDenied error", err)
	}
	if c.Status().LastError == "" {
		t.Fatal("Status().LastError is empty after the router refused the credential")
	}
}

func TestClientDrainDeadlineKillsAndReportsPreempted(t *testing.T) {
	h := newHarness(t)
	c, _ := h.start(h.config())
	_, name := h.router.Offer("acme/app", "linux-x64", false)
	h.router.WaitPhase(t, name, phaseRunning, waitTimeout)

	c.Drain(time.Now().Add(150 * time.Millisecond))
	waitFor(t, "draining heartbeat", func() bool {
		return h.router.LastHeartbeat().GetAvailability() == workerv1.Availability_AVAILABILITY_DRAINING
	})
	h.router.WaitPhase(t, name, phaseExited, waitTimeout)
	if !slices.Contains(h.launch.Killed(), name) {
		t.Fatal("the drain deadline did not kill the runner")
	}
	if rep := reportFor(h.router, name, phaseExited); !rep.GetPreempted() || rep.GetExitCode() != 137 {
		t.Fatalf("exit report = %v, want preempted with exit code 137", rep)
	}
}

func TestClientDrainEndsWhenAvailableAgain(t *testing.T) {
	h := newHarness(t)
	c, _ := h.start(h.config())
	_, name := h.router.Offer("acme/app", "linux-x64", false)
	h.router.WaitPhase(t, name, phaseRunning, waitTimeout)

	c.Drain(time.Now().Add(100 * time.Millisecond))
	c.SetAvailability(worker.Available) // the user left again before the deadline
	time.Sleep(250 * time.Millisecond)
	if got := h.launch.Killed(); len(got) != 0 {
		t.Fatalf("a cancelled drain still killed %v", got)
	}
}

func TestClientCancelDirectiveKillsRunner(t *testing.T) {
	h := newHarness(t)
	h.start(h.config())
	_, name := h.router.Offer("acme/app", "linux-x64", false)
	h.router.WaitPhase(t, name, phaseRunning, waitTimeout)

	h.router.SendCancel(name, "recycle")
	h.router.WaitPhase(t, name, phaseExited, waitTimeout)
	if !slices.Contains(h.launch.Killed(), name) {
		t.Fatal("Cancel did not kill the runner")
	}
	rep := reportFor(h.router, name, phaseExited)
	if rep.GetPreempted() || !strings.Contains(rep.GetError(), "cancelled by router: recycle") {
		t.Fatalf("exit report = %v", rep)
	}
}

func TestClientRouterDrainDirective(t *testing.T) {
	h := newHarness(t)
	c, _ := h.start(h.config())
	h.router.SendDrain("maintenance")
	waitFor(t, "draining", func() bool { return c.Status().Availability == worker.Draining })
	_, name := h.router.Offer("acme/app", "linux-x64", false)
	waitFor(t, "offer declined while draining", func() bool { return slices.Contains(h.router.Rejected(), name) })
}

func TestClientDeclinesClassesItDoesNotServe(t *testing.T) {
	h := newHarness(t)
	h.start(h.config())
	_, name := h.router.Offer("acme/app", runner.Class{OS: "linux", Arch: "arm64"}.String(), false)
	waitFor(t, "offer declined", func() bool { return slices.Contains(h.router.Rejected(), name) })
}

// TrustedHosts lets a simulator router's JIT (non-GitHub host) pass verification (controller ruling 2).
func TestClientPassesTrustedHostsToJITVerification(t *testing.T) {
	h := newHarness(t)
	h.router.SetJITHostOverride("http://127.0.0.1:9300")
	h.start(h.config())
	_, refused := h.router.Offer("acme/app", "linux-x64", false)
	waitFor(t, "sim host refused by default", func() bool { return slices.Contains(h.router.Rejected(), refused) })

	h2 := newHarness(t)
	h2.router.SetJITHostOverride("http://127.0.0.1:9300")
	h2.start(h2.config(func(c *worker.Config) { c.TrustedHosts = []string{"127.0.0.1"} }))
	_, name := h2.router.Offer("acme/app", "linux-x64", false)
	h2.router.WaitPhase(t, name, phaseRunning, waitTimeout)
}
