package router

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
	"github.com/aboldnewlook/github-scaleset-orchestrator/worker"
)

// idleLauncher serves linux-x64 and never runs anything: nothing is placed
// on workers before B8a.
type idleLauncher struct{}

func (idleLauncher) Kind() runner.ExecutorKind { return runner.ExecutorDocker }
func (idleLauncher) Classes(context.Context) ([]runner.Class, error) {
	return []runner.Class{{OS: "linux", Arch: "x64"}}, nil
}
func (idleLauncher) Start(context.Context, worker.RunnerRecord, string, string) error { return nil }
func (idleLauncher) Wait(ctx context.Context, _ worker.RunnerRecord) error {
	<-ctx.Done()
	return ctx.Err()
}
func (idleLauncher) Kill(context.Context, worker.RunnerRecord) error      { return nil }
func (idleLauncher) RemoveOrphans(context.Context, map[string]bool) error { return nil }

func findWorker(ws []control.WorkerLiveStatus, name string) control.WorkerLiveStatus {
	for _, w := range ws {
		if w.Name == name {
			return w
		}
	}
	return control.WorkerLiveStatus{}
}

func TestRealWorkerEnrollsConnectsDrainsAndIsRevoked(t *testing.T) {
	r := newTestRouter(t, loadTestConfig(t, workersYAML), "")
	ctx := startWorkerSide(t, r)

	res, err := r.AddWorker(ctx, control.WorkersAddParams{Name: "alpha", Classes: []string{"linux-x64"}, Slots: 2, Advertise: r.WorkersAddr()})
	if err != nil {
		t.Fatal(err)
	}
	join, err := worker.ParseJoin(res.Join)
	if err != nil {
		t.Fatal(err)
	}
	enrollCtx, cancelEnroll := context.WithTimeout(ctx, 5*time.Second)
	defer cancelEnroll()
	cred, st, err := worker.Enroll(enrollCtx, worker.EnrollOptions{Join: join, Name: "alpha", MachineID: "machine-alpha"})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if st.Pin != r.WorkersPin() {
		t.Fatalf("enrolled pin %q, router pin %q", st.Pin, r.WorkersPin())
	}
	waitFor(t, "registry record", func() bool { return findWorker(r.ListWorkers(), "alpha").ID != "" })
	if w := findWorker(r.ListWorkers(), "alpha"); w.Connected || w.Availability != "offline" {
		t.Fatalf("before connecting: %+v", w)
	}

	dir := t.TempDir()
	store := worker.FileCredentialStore(filepath.Join(dir, "credential"))
	if err := store.Save(cred); err != nil {
		t.Fatal(err)
	}
	records, err := worker.OpenRecordStore(filepath.Join(dir, "runners.json"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := worker.New(worker.Config{
		Name: "alpha", MachineID: "machine-alpha", Version: "test",
		DialAddr: r.WorkersAddr(), Pin: st.Pin,
		Credential: store, Records: records,
		Launchers: []worker.Launcher{idleLauncher{}},
		Slots:     4, // the enrollment says 2: the router clamps
		Allowlist: st.OwnerScopes, AcceptPublic: worker.PublicFollow,
		BackoffMin: 10 * time.Millisecond, BackoffMax: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.SetAvailability(worker.Available)
	clientCtx, stopClient := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- c.Run(clientCtx) }()
	t.Cleanup(func() {
		stopClient()
		select {
		case err := <-done:
			// A revoked worker's client exits with the router's refusal.
			if err != nil && !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "worker revoked") {
				t.Errorf("worker client stopped with %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("worker client did not stop")
		}
	})

	waitFor(t, "session", func() bool { return c.Status().Connected })
	if got := c.Status().Slots; got != 2 {
		t.Fatalf("effective slots = %d, want 2 (clamped to the enrollment)", got)
	}
	waitFor(t, "router lists the worker as available", func() bool {
		w := findWorker(r.ListWorkers(), "alpha")
		return w.Connected && w.Availability == "available" && w.Slots == 2
	})

	if err := r.DrainWorker("alpha", "maintenance"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "worker drains", func() bool { return c.Status().Availability == worker.Draining })
	waitFor(t, "router sees draining", func() bool { return findWorker(r.ListWorkers(), "alpha").Availability == "draining" })

	if err := r.RevokeWorker(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "router drops the revoked worker", func() bool {
		w := findWorker(r.ListWorkers(), "alpha")
		return w.Revoked && !w.Connected
	})
	waitFor(t, "client loses its session", func() bool { return !c.Status().Connected })
	time.Sleep(3 * fastTiming().Heartbeat) // give the client time to retry
	if c.Status().Connected {
		t.Fatal("a revoked worker opened a new session")
	}
}
