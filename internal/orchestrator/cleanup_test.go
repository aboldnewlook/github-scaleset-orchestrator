package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/event"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghapi"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghsim"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/naming"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
	"github.com/actions/scaleset"
)

const (
	cleanupRepo = "org/repo"
	// An orphan of the single-container release: the hostname was a random
	// container ID, so the set shows as KIND "other", not "legacy".
	orphanName = "gso-4f9a1c2b7d3e-repo"
	// Held by a "live listener" until the test says otherwise.
	heldName = "gso-9a8b7c6d5e4f-repo"
)

// syncBuffer is a log sink the supervisor goroutines can write while the test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// cleanupModel is a simulator whose sessions never expire on their own, so a
// held session stays held until the test closes it.
func cleanupModel() *ghsim.Model {
	return ghsim.NewModel(ghsim.Options{PollWait: 20 * time.Millisecond, SessionTTL: time.Hour})
}

func seedSet(t *testing.T, m *ghsim.Model, name string) int {
	t.Helper()
	ss, err := m.Client(cleanupRepo).CreateRunnerScaleSet(context.Background(), &scaleset.RunnerScaleSet{Name: name, RunnerGroupID: 1})
	if err != nil {
		t.Fatal(err)
	}
	return ss.ID
}

func setNames(m *ghsim.Model) []string {
	var out []string
	for _, ss := range m.ScaleSets(cleanupRepo) {
		out = append(out, ss.Name)
	}
	slices.Sort(out)
	return out
}

// cleanupTiming retries quickly: 1 ms, then 2 ms, up to retries times.
func cleanupTiming(retries int) SupervisorTiming {
	tm := fastTiming()
	tm.CleanupBackoff, tm.CleanupRetries = time.Millisecond, retries
	return tm
}

func newCleanupOrchestrator(t *testing.T, gh ghapi.Factory, m *ghsim.Model, enabled bool, tm SupervisorTiming, bus *event.Bus) (*Orchestrator, *syncBuffer) {
	t.Helper()
	o := newTestOrchestrator(t, gh, bus, tm, cleanupRepo)
	logs := &syncBuffer{}
	o.logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	o.cfg.CleanupStaleScaleSets = enabled
	WithScaleSetDeleter(m.Deleter())(o)
	return o, logs
}

type stalePayload struct {
	Reason      string            `json:"reason"`
	ScaleSets   []string          `json:"scale_sets"`
	AutoCleanup bool              `json:"auto_cleanup"`
	Skipped     map[string]string `json:"skipped"`
}

// waitForStaleAlert blocks until the first-pass stale_scale_sets alert is
// published; cleanup of that pass is complete by then.
func waitForStaleAlert(t *testing.T, ch <-chan event.Event) stalePayload {
	t.Helper()
	var got stalePayload
	var seen []event.Event
	eventually(t, "a stale_scale_sets alert", func() bool {
		seen = append(seen, drainEvents(ch)...)
		for _, e := range seen {
			var p stalePayload
			if e.Type == event.EventAlert && json.Unmarshal(e.Payload, &p) == nil && p.Reason == "stale_scale_sets" {
				got = p
				return true
			}
		}
		return false
	})
	return got
}

func TestCleanupDeletesAnIdleOrphan(t *testing.T) {
	m := cleanupModel()
	orphanID := seedSet(t, m, orphanName)
	bus := event.NewBus(nil)
	id, ch := bus.Subscribe(64)
	defer bus.Unsubscribe(id)
	o, logs := newCleanupOrchestrator(t, m.Factory(), m, true, cleanupTiming(0), bus)
	supervise(t, o, cleanupRepo)

	var deletedEvent event.Event
	eventually(t, "the orphan deleted", func() bool { return !slices.Contains(setNames(m), orphanName) })
	eventually(t, "a scaleset.deleted event", func() bool {
		for _, e := range drainEvents(ch) {
			if e.Type == event.EventScaleSetDeleted {
				deletedEvent = e
				return true
			}
		}
		return false
	})
	if deletedEvent.Repo != cleanupRepo {
		t.Errorf("event repo = %q, want %q", deletedEvent.Repo, cleanupRepo)
	}
	var p struct {
		ID     int    `json:"id"`
		Name   string `json:"name"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(deletedEvent.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.ID != orphanID || p.Name != orphanName || p.Reason != "stale_cleanup" {
		t.Errorf("event payload = %+v, want id %d name %q reason stale_cleanup", p, orphanID, orphanName)
	}
	if got := m.Deleted(cleanupRepo); len(got) != 1 || got[0] != orphanID {
		t.Errorf("deleted = %v, want only [%d]", got, orphanID)
	}
	logged := logs.String()
	for _, want := range []string{"level=INFO", "deleted stale scale set", "name=" + orphanName, "id=" + strconv.Itoa(orphanID), "repo=" + cleanupRepo} {
		if !strings.Contains(logged, want) {
			t.Errorf("log lacks %q:\n%s", want, logged)
		}
	}
	// The session the router acquired to prove ownership is closed again.
	eventually(t, "current scale set running", func() bool { return listenerState(o, cleanupRepo).State == "running" })
}

func TestCleanupKeepsSetsItCannotProveOrphaned(t *testing.T) {
	otherInstance := naming.ScaleSetNameFor("someone-else", cleanupRepo, runner.Class{OS: "linux", Arch: "arm64"})
	tests := []struct {
		name       string
		setup      func(t *testing.T, m *ghsim.Model) (kept string)
		wantReason string
	}{
		{
			name: "held session (409)",
			setup: func(t *testing.T, m *ghsim.Model) string {
				id := seedSet(t, m, heldName)
				if _, err := m.Client(cleanupRepo).OpenSession(context.Background(), id, "old-listener"); err != nil {
					t.Fatal(err)
				}
				return heldName
			},
			wantReason: "session",
		},
		{
			name: "running jobs",
			setup: func(t *testing.T, m *ghsim.Model) string {
				seedSet(t, m, orphanName)
				m.SetStats(cleanupRepo, orphanName, scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1, TotalRunningJobs: 1})
				return orphanName
			},
			wantReason: "jobs",
		},
		{
			name: "assigned jobs only",
			setup: func(t *testing.T, m *ghsim.Model) string {
				seedSet(t, m, orphanName)
				m.SetStats(cleanupRepo, orphanName, scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 2})
				return orphanName
			},
			wantReason: "jobs",
		},
		{
			name: "missing statistics",
			setup: func(t *testing.T, m *ghsim.Model) string {
				seedSet(t, m, orphanName)
				m.HideStatistics(cleanupRepo, orphanName)
				return orphanName
			},
			wantReason: "statistics",
		},
		{
			name: "current-form set of another instance",
			setup: func(t *testing.T, m *ghsim.Model) string {
				seedSet(t, m, otherInstance)
				return otherInstance
			},
			wantReason: "current",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := cleanupModel()
			kept := tt.setup(t, m)
			bus := event.NewBus(nil)
			id, ch := bus.Subscribe(64)
			defer bus.Unsubscribe(id)
			o, _ := newCleanupOrchestrator(t, m.Factory(), m, true, cleanupTiming(0), bus)
			supervise(t, o, cleanupRepo)

			p := waitForStaleAlert(t, ch)
			if !p.AutoCleanup {
				t.Errorf("alert auto_cleanup = false, want true")
			}
			var entry, reason string
			for e, r := range p.Skipped {
				if strings.HasPrefix(e, kept+" ") {
					entry, reason = e, r
				}
			}
			if entry == "" || !strings.Contains(strings.ToLower(reason), tt.wantReason) {
				t.Errorf("skipped[%q...] = %q, want a reason mentioning %q (skipped: %v)", kept, reason, tt.wantReason, p.Skipped)
			}
			if !slices.Contains(setNames(m), kept) {
				t.Errorf("%s was deleted; sets now %v", kept, setNames(m))
			}
			if got := m.Deleted(cleanupRepo); len(got) != 0 {
				t.Errorf("deleted = %v, want none", got)
			}
		})
	}
}

func TestCleanupNeverTouchesNonGsoSets(t *testing.T) {
	m := cleanupModel()
	seedSet(t, m, "my-own-runners")
	seedSet(t, m, "ci-"+orphanName)
	seedSet(t, m, "gsonot-a-prefix") // "gso" without the hyphen is not ours
	seedSet(t, m, orphanName)        // sentinel: proves the pass ran
	o, _ := newCleanupOrchestrator(t, m.Factory(), m, true, cleanupTiming(0), nil)
	supervise(t, o, cleanupRepo)

	eventually(t, "the sentinel orphan deleted", func() bool { return !slices.Contains(setNames(m), orphanName) })
	eventually(t, "listener running", func() bool { return listenerState(o, cleanupRepo).State == "running" })
	for _, name := range []string{"my-own-runners", "ci-" + orphanName, "gsonot-a-prefix"} {
		if !slices.Contains(setNames(m), name) {
			t.Errorf("non-gso set %q was deleted", name)
		}
	}
	if got := m.Deleted(cleanupRepo); len(got) != 1 {
		t.Errorf("deleted = %v, want exactly the sentinel", got)
	}
}

func TestCleanupDisabledByConfigDeletesNothing(t *testing.T) {
	m := cleanupModel()
	seedSet(t, m, orphanName)
	bus := event.NewBus(nil)
	id, ch := bus.Subscribe(64)
	defer bus.Unsubscribe(id)
	o, logs := newCleanupOrchestrator(t, m.Factory(), m, false, cleanupTiming(3), bus)
	supervise(t, o, cleanupRepo)

	p := waitForStaleAlert(t, ch)
	if p.AutoCleanup {
		t.Errorf("alert auto_cleanup = true with cleanup disabled")
	}
	eventually(t, "listener running", func() bool { return listenerState(o, cleanupRepo).State == "running" })
	time.Sleep(50 * time.Millisecond) // room for retries that must not happen
	if !slices.Contains(setNames(m), orphanName) || len(m.Deleted(cleanupRepo)) != 0 {
		t.Errorf("orphan touched with cleanup disabled; sets %v, deleted %v", setNames(m), m.Deleted(cleanupRepo))
	}
	if !strings.Contains(logs.String(), "cleanup_stale_scale_sets") {
		t.Errorf("warning does not say how to enable auto-cleanup:\n%s", logs.String())
	}
}

func TestCleanupRetriesASkippedSetUntilItsSessionIsReleased(t *testing.T) {
	m := cleanupModel()
	id := seedSet(t, m, heldName)
	held, err := m.Client(cleanupRepo).OpenSession(context.Background(), id, "old-listener")
	if err != nil {
		t.Fatal(err)
	}
	bus := event.NewBus(nil)
	sub, ch := bus.Subscribe(64)
	defer bus.Unsubscribe(sub)
	o, _ := newCleanupOrchestrator(t, m.Factory(), m, true, cleanupTiming(8), bus)
	supervise(t, o, cleanupRepo)

	waitForStaleAlert(t, ch)
	if !slices.Contains(setNames(m), heldName) {
		t.Fatal("held set deleted while its session was open")
	}
	if err := held.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the released set deleted on a retry", func() bool { return !slices.Contains(setNames(m), heldName) })
}

// countingClient counts the stale-check listings, one per cleanup pass.
type countingClient struct {
	ghapi.Client
	lists *int
	mu    *sync.Mutex
}

func (c countingClient) ListRunnerScaleSets(ctx context.Context, g int) ([]scaleset.RunnerScaleSet, error) {
	c.mu.Lock()
	*c.lists++
	c.mu.Unlock()
	return c.Client.ListRunnerScaleSets(ctx, g)
}

func TestCleanupRetriesAreBounded(t *testing.T) {
	m := cleanupModel()
	seedSet(t, m, orphanName)
	m.SetStats(cleanupRepo, orphanName, scaleset.RunnerScaleSetStatistic{TotalRunningJobs: 1, TotalAssignedJobs: 1})
	var (
		mu    sync.Mutex
		lists int
	)
	gh := func(ctx context.Context, repo config.Repo) (ghapi.Client, error) {
		c, err := m.Factory()(ctx, repo)
		if err != nil {
			return nil, err
		}
		return countingClient{Client: c, lists: &lists, mu: &mu}, nil
	}
	o, _ := newCleanupOrchestrator(t, gh, m, true, cleanupTiming(2), nil)
	supervise(t, o, cleanupRepo)

	count := func() int { mu.Lock(); defer mu.Unlock(); return lists }
	eventually(t, "the initial pass and both retries", func() bool { return count() >= 3 })
	time.Sleep(100 * time.Millisecond) // a fourth pass would have run by now
	if got := count(); got != 3 {
		t.Errorf("cleanup listed %d times, want 3 (initial pass + 2 retries)", got)
	}
}
