package router

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/event"
	"github.com/aboldnewlook/github-scaleset-orchestrator/worker"
)

// enrollWorker adds and redeems an enrollment; it returns the credential
// store and pin a client needs to connect.
func enrollWorker(t *testing.T, ctx context.Context, r *Router, name string) (worker.CredentialStore, string) {
	t.Helper()
	res, err := r.AddWorker(ctx, control.WorkersAddParams{Name: name, Classes: []string{"linux-x64"}, Slots: 2, Advertise: r.WorkersAddr()})
	if err != nil {
		t.Fatal(err)
	}
	join, err := worker.ParseJoin(res.Join)
	if err != nil {
		t.Fatal(err)
	}
	enrollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cred, st, err := worker.Enroll(enrollCtx, worker.EnrollOptions{Join: join, Name: name, MachineID: "machine-" + name})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	store := worker.FileCredentialStore(filepath.Join(t.TempDir(), "credential"))
	if err := store.Save(cred); err != nil {
		t.Fatal(err)
	}
	return store, st.Pin
}

// countRevoked returns how many worker.revoked events arrive within window.
func countRevoked(ch <-chan event.Event, window time.Duration) int {
	n := 0
	timer := time.After(window)
	for {
		select {
		case e := <-ch:
			if e.Type == event.EventWorkerRevoked {
				n++
			}
		case <-timer:
			return n
		}
	}
}

func TestRevokeNeverConnectedWorkerPublishesOneEvent(t *testing.T) {
	bus := event.NewBus(nil)
	_, events := bus.Subscribe(16)
	r, err := New(loadTestConfig(t, workersYAML), nil, bus, Options{StateDir: t.TempDir(), FleetTiming: fastTiming()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := startWorkerSide(t, r)
	enrollWorker(t, ctx, r, "ghost")
	if err := r.RevokeWorker(ctx, "ghost"); err != nil {
		t.Fatal(err)
	}
	if n := countRevoked(events, 300*time.Millisecond); n != 1 {
		t.Fatalf("worker.revoked events = %d, want exactly 1", n)
	}
}

func TestRevokeConnectedWorkerPublishesOneEvent(t *testing.T) {
	bus := event.NewBus(nil)
	_, events := bus.Subscribe(16)
	r, err := New(loadTestConfig(t, workersYAML), nil, bus, Options{StateDir: t.TempDir(), FleetTiming: fastTiming()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := startWorkerSide(t, r)
	store, pin := enrollWorker(t, ctx, r, "live")
	records, err := worker.OpenRecordStore(filepath.Join(t.TempDir(), "runners.json"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := worker.New(worker.Config{
		Name: "live", MachineID: "machine-live", Version: "test",
		DialAddr: r.WorkersAddr(), Pin: pin, Credential: store, Records: records,
		Launchers: []worker.Launcher{idleLauncher{}}, Slots: 2,
		Allowlist: []string{"o/*"}, AcceptPublic: worker.PublicFollow,
		BackoffMin: 10 * time.Millisecond, BackoffMax: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.SetAvailability(worker.Available)
	clientCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = c.Run(clientCtx) }()
	waitFor(t, "worker connected", func() bool { return findWorker(r.ListWorkers(), "live").Connected })

	if err := r.RevokeWorker(ctx, "live"); err != nil {
		t.Fatal(err)
	}
	if n := countRevoked(events, 300*time.Millisecond); n != 1 {
		t.Fatalf("worker.revoked events = %d, want exactly 1", n)
	}
}
