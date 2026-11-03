//go:build !windows

package e2e

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghsim"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/naming"
	"github.com/aboldnewlook/github-scaleset-orchestrator/worker"
)

func segmentOf(runnerName string) string {
	seg, _ := naming.RunnerWorkerSegment(runnerName)
	return seg
}

func waitIdle(t *testing.T, r *routerProc, w *workerProc, name string) {
	t.Helper()
	waitFor(t, 20*time.Second, name+" idle with no runner records", func() bool {
		ws, ok := tryWorkerStatus(r, name)
		return ok && ws.Connected && ws.Busy == 0 && len(w.records(t)) == 0
	})
}

// (1) inject → offer → accept → JIT verified → fake runner claims → completes → EXITED.
func TestJobRunsOnARemoteWorker(t *testing.T) {
	t.Parallel()
	sim := startSim(t)
	r := startRouter(t, sim, defaultRepos)
	w := startWorker(t, r, "alpha", r.addWorker(t, "alpha", simClass(), 1, ""))
	waitWorkerConnected(t, r, "alpha")

	id := injectJob(t, sim, "--repo", "o/a", "--duration", "1s")
	job := waitJob(t, sim, id, ghsim.JobCompleted, 30*time.Second)
	if job.Result != "succeeded" {
		t.Fatalf("result = %q", job.Result)
	}
	if seg := segmentOf(job.Runner); seg != "alpha" {
		t.Fatalf("the job ran on %q, want one of alpha's runners", job.Runner)
	}
	waitIdle(t, r, w, "alpha")
	if n := len(simRunners(t, sim, "o/a")); n != 1 {
		t.Fatalf("minted %d runners for one job, want 1", n)
	}
	if strings.Contains(strings.ToLower(w.log()), "jit verification failed") {
		t.Fatal("the worker rejected the simulator's JIT config")
	}
}

// (2) Two workers with one slot each compete for four jobs.
func TestTwoWorkersCompeteForJobs(t *testing.T) {
	t.Parallel()
	sim := startSim(t)
	r := startRouter(t, sim, defaultRepos)
	startWorker(t, r, "alpha", r.addWorker(t, "alpha", simClass(), 1, ""))
	startWorker(t, r, "beta", r.addWorker(t, "beta", simClass(), 1, ""))
	waitWorkerConnected(t, r, "alpha")
	waitWorkerConnected(t, r, "beta")

	var mu sync.Mutex
	maxBusy := map[string]int{}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(25 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				st, err := tryLiveStatus(r)
				if err != nil {
					continue
				}
				mu.Lock()
				for _, ws := range st.Workers {
					maxBusy[ws.Name] = max(maxBusy[ws.Name], ws.Busy)
				}
				mu.Unlock()
			}
		}
	}()

	ids := injectJobs(t, sim, "--repo", "o/a", "--duration", "1500ms", "--count", "4")
	ran := map[string]int{}
	for _, id := range ids {
		ran[segmentOf(waitJob(t, sim, id, ghsim.JobCompleted, 60*time.Second).Runner)]++
	}
	close(stop)
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	for _, name := range []string{"alpha", "beta"} {
		if maxBusy[name] > 1 {
			t.Errorf("%s ran %d jobs at once with 1 slot", name, maxBusy[name])
		}
		if ran[name] == 0 {
			t.Errorf("%s ran no jobs; distribution %v", name, ran)
		}
	}
}

// (3) The worker's connection drops mid-job, briefly and then past the lost threshold.
func TestWorkerSurvivesConnectionDrops(t *testing.T) {
	t.Parallel()
	sim := startSim(t)
	r := startRouter(t, sim, defaultRepos)
	p := newProxy(t, r.WorkersAddr)
	startWorker(t, r, "alpha", r.addWorker(t, "alpha", simClass(), 1, ""), "--router", p.Addr())
	waitWorkerConnected(t, r, "alpha")

	id := injectJob(t, sim, "--repo", "o/a", "--duration", "4s")
	waitJob(t, sim, id, ghsim.JobRunning, 20*time.Second)
	p.Sever(200 * time.Millisecond) // a blip shorter than the lost threshold (600 ms at scale 0.02)
	time.Sleep(400 * time.Millisecond)
	p.Sever(1500 * time.Millisecond) // past the threshold: the router marks alpha lost

	waitJob(t, sim, id, ghsim.JobCompleted, 30*time.Second)
	waitWorkerConnected(t, r, "alpha")
	waitFor(t, 20*time.Second, "alpha idle after reconnecting", func() bool {
		ws, ok := tryWorkerStatus(r, "alpha")
		return ok && ws.Busy == 0
	})
	if n := len(simRunners(t, sim, "o/a")); n != 1 {
		t.Fatalf("minted %d runners, want 1 (no duplicate after reconnecting)", n)
	}
	next := waitJob(t, sim, injectJob(t, sim, "--repo", "o/a", "--duration", "500ms"), ghsim.JobCompleted, 30*time.Second)
	if segmentOf(next.Runner) != "alpha" {
		t.Fatalf("follow-up job ran on %q", next.Runner)
	}
}

// (4) The router is SIGKILLed while a job runs. Its simulated sessions linger
// until their TTL, so the new router retries 409s, then adopts the runner.
func TestRouterRestartKeepsTheInFlightJob(t *testing.T) {
	t.Parallel()
	sim := startSim(t)
	r := startRouter(t, sim, defaultRepos)
	w := startWorker(t, r, "alpha", r.addWorker(t, "alpha", simClass(), 1, ""))
	waitWorkerConnected(t, r, "alpha")

	id := injectJob(t, sim, "--repo", "o/a", "--duration", "5s")
	waitJob(t, sim, id, ghsim.JobRunning, 20*time.Second)
	before := len(simRunners(t, sim, "o/a"))

	r.Restart(t, true)
	waitWorkerConnected(t, r, "alpha")
	if job := waitJob(t, sim, id, ghsim.JobCompleted, 40*time.Second); job.Result != "succeeded" {
		t.Fatalf("result = %q", job.Result)
	}
	if after := len(simRunners(t, sim, "o/a")); after != before {
		t.Fatalf("the restarted router minted %d extra runners: adoption failed", after-before)
	}
	if !strings.Contains(strings.ToLower(r.log()), "conflict") {
		t.Log("note: no session conflict was logged; the old sessions may have expired before the restart")
	}
	waitIdle(t, r, w, "alpha")
	waitJob(t, sim, injectJob(t, sim, "--repo", "o/a", "--duration", "500ms"), ghsim.JobCompleted, 30*time.Second)
}

// (5) The worker is SIGKILLed mid-job; its fake runner (own process group)
// keeps running, and the restarted worker re-attaches and reports completion.
func TestWorkerRestartReattachesTheRunningJob(t *testing.T) {
	t.Parallel()
	sim := startSim(t)
	r := startRouter(t, sim, defaultRepos)
	w := startWorker(t, r, "alpha", r.addWorker(t, "alpha", simClass(), 1, ""))
	waitWorkerConnected(t, r, "alpha")

	id := injectJob(t, sim, "--repo", "o/a", "--duration", "4s")
	waitJob(t, sim, id, ghsim.JobRunning, 20*time.Second)
	if len(w.records(t)) != 1 {
		t.Fatalf("records before the restart = %v, want 1", w.records(t))
	}
	w.Restart(t, true)
	waitWorkerConnected(t, r, "alpha")
	waitJob(t, sim, id, ghsim.JobCompleted, 30*time.Second)
	waitIdle(t, r, w, "alpha")
	if n := len(simRunners(t, sim, "o/a")); n != 1 {
		t.Fatalf("minted %d runners, want 1 (the job must not be re-placed)", n)
	}
}

// (6) A revoked worker is cut off and gets no more work.
func TestRevokedWorkerIsRefused(t *testing.T) {
	t.Parallel()
	sim := startSim(t)
	r := startRouter(t, sim, defaultRepos)
	startWorker(t, r, "alpha", r.addWorker(t, "alpha", simClass(), 1, ""))
	beta := startWorker(t, r, "beta", r.addWorker(t, "beta", simClass(), 1, ""))
	waitWorkerConnected(t, r, "alpha")
	waitWorkerConnected(t, r, "beta")

	runGSO(t, r.env, "workers", "revoke", "beta")
	err, exited := beta.waitExit(20 * time.Second)
	if !exited {
		t.Fatal("beta kept running after its credential was revoked")
	}
	if err == nil {
		t.Fatal("beta exited 0 after revocation, want a non-zero exit")
	}
	if l := strings.ToLower(beta.log()); !strings.Contains(l, "permission") && !strings.Contains(l, "revoked") {
		t.Fatalf("beta's log does not explain the revocation:\n%s", tail(beta.log(), 20))
	}
	if ws, ok := tryWorkerStatus(r, "beta"); !ok || !ws.Revoked {
		t.Fatalf("live status for beta = %+v (found %v), want revoked", ws, ok)
	}
	for _, id := range injectJobs(t, sim, "--repo", "o/a", "--duration", "300ms", "--count", "2") {
		if job := waitJob(t, sim, id, ghsim.JobCompleted, 30*time.Second); segmentOf(job.Runner) != "alpha" {
			t.Fatalf("job %d ran on %q after beta was revoked", id, job.Runner)
		}
	}
}

// (7) A tampered SPKI pin fails enrollment without spending the code; a
// spent code cannot enrol a second machine.
func TestEnrollmentRejectsAWrongPinAndAReusedCode(t *testing.T) {
	t.Parallel()
	sim := startSim(t)
	r := startRouter(t, sim, defaultRepos)
	join := r.addWorker(t, "gamma", simClass(), 1, "")
	j, err := worker.ParseJoin(join)
	if err != nil {
		t.Fatal(err)
	}
	tampered := worker.FormatJoin(j.Addr, strings.Repeat("0", 64), j.Code)

	bad := startWorker(t, r, "gamma", tampered)
	if err, exited := bad.waitExit(20 * time.Second); !exited || err == nil {
		t.Fatalf("a worker with the wrong pin: exited=%v err=%v, want a failed exit", exited, err)
	}
	if !strings.Contains(strings.ToLower(bad.log()), "pin") {
		t.Fatalf("the failure does not mention the pin:\n%s", tail(bad.log(), 20))
	}
	if _, ok := tryWorkerStatus(r, "gamma"); ok {
		t.Fatal("gamma was enrolled through a join string with the wrong pin")
	}

	startWorker(t, r, "gamma", join) // the TLS handshake failed before Enroll, so the code is unspent
	waitWorkerConnected(t, r, "gamma")

	again := startWorker(t, r, "gamma2", join)
	if err, exited := again.waitExit(20 * time.Second); !exited || err == nil {
		t.Fatalf("a reused code: exited=%v err=%v, want a failed exit", exited, err)
	}
	count := 0
	for _, ws := range liveStatus(t, r).Workers {
		if ws.Name == "gamma" || ws.Name == "gamma2" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("%d workers enrolled from one join string, want 1", count)
	}
}
