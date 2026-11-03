package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghapi"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghsim"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/naming"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

var linuxX64 = runner.Class{OS: "linux", Arch: "x64"}

func TestCurrentScaleSetNameUsesInstanceAndHostClass(t *testing.T) {
	q := NewQuery(&config.Config{Runner: config.RunnerConfig{InstanceID: "Unraid"}}, nil)
	want := naming.ScaleSetNameFor("unraid", "jrschumacher/prune", runner.HostClass())
	if got := q.currentScaleSetName("jrschumacher/prune"); got != want {
		t.Fatalf("currentScaleSetName = %q, want %q", got, want)
	}
}

func TestClassifyScaleSets(t *testing.T) {
	linux := runner.Class{OS: "linux", Arch: "x64"}
	arm := runner.Class{OS: "linux", Arch: "arm64"}
	sets := []scaleset.RunnerScaleSet{
		{ID: 1, Name: naming.ScaleSetNameFor("unraid", "o/prune", linux)},
		{ID: 2, Name: naming.ScaleSetNameFor("unraid", "o/prune", arm)},
		{ID: 3, Name: "gso-tower-prune"},            // this host's legacy name
		{ID: 4, Name: "gso-3f2a9c1b7d4e-prune"},     // an orphan from an old container hostname
		{ID: 5, Name: "gso-laptop-prune-linux-x64"}, // another instance
		{ID: 6, Name: "arc-runners"},                // not gso's: ignored
	}
	got := classifyScaleSets("unraid", "tower", "o/prune", sets)
	want := map[int]ScaleSetKind{1: ScaleSetCurrent, 2: ScaleSetCurrent, 3: ScaleSetLegacy, 4: ScaleSetOther, 5: ScaleSetOther}
	if len(got) != len(want) {
		t.Fatalf("classified %d scale sets, want %d: %+v", len(got), len(want), got)
	}
	for _, ls := range got {
		if ls.Repo != "o/prune" || ls.Kind != want[ls.ID] {
			t.Errorf("#%d %s: repo %q kind %q, want kind %q", ls.ID, ls.Name, ls.Repo, ls.Kind, want[ls.ID])
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Name > got[i].Name {
			t.Fatalf("not sorted by name: %q before %q", got[i-1].Name, got[i].Name)
		}
	}
}

func TestResetScaleSetRejectsUnknownRepo(t *testing.T) {
	q := NewQuery(&config.Config{Repos: []config.Repo{{Name: "o/prune"}}}, nil)
	_, _, err := q.ResetScaleSet(context.Background(), "o/other", runner.Class{OS: "linux", Arch: "x64"})
	if err == nil || !strings.Contains(err.Error(), "o/other") {
		t.Fatalf("ResetScaleSet(unknown repo) = %v", err)
	}
}

// simDeleter is the CLI-only delete seam backed by the simulator model.
type simDeleter struct {
	m    *ghsim.Model
	repo string
}

func (d simDeleter) DeleteRunnerScaleSet(_ context.Context, id int) error {
	return d.m.DeleteScaleSet(d.repo, id)
}

type resetEnv struct {
	m    *ghsim.Model
	q    *Query
	name string
	id   int
	// listeners is what the fake control socket reports; listenersErr set
	// means no daemon is reachable.
	listeners    []control.ListenerLiveStatus
	listenersErr error
}

// wrapClient decorates the simulator client to inject GitHub misbehaviour.
type wrapClient func(ghapi.Client) ghapi.Client

// nilStatsClient returns scale sets without statistics (or with fixed ones).
type nilStatsClient struct {
	ghapi.Client
	stats *scaleset.RunnerScaleSetStatistic
}

func (c nilStatsClient) GetRunnerScaleSet(ctx context.Context, g int, name string) (*scaleset.RunnerScaleSet, error) {
	ss, err := c.Client.GetRunnerScaleSet(ctx, g, name)
	if ss != nil {
		cp := *ss
		cp.Statistics = c.stats
		ss = &cp
	}
	return ss, err
}

type closeFailClient struct{ ghapi.Client }

type closeFailSession struct{ listener.Client }

func (c closeFailClient) OpenSession(ctx context.Context, id int, owner string) (ghapi.Session, error) {
	s, err := c.Client.OpenSession(ctx, id, owner)
	if err != nil {
		return nil, err
	}
	return closeFailSession{s}, nil
}

func (closeFailSession) Close(context.Context) error { return errors.New("close failed") }

func newResetEnv(t *testing.T, wraps ...wrapClient) *resetEnv {
	t.Helper()
	m := ghsim.NewModel(ghsim.Options{PollWait: 20 * time.Millisecond, SessionTTL: time.Minute})
	cfg := &config.Config{
		Repos:   []config.Repo{{Name: "o/prune"}},
		Runner:  config.RunnerConfig{InstanceID: "unraid"},
		Workers: config.WorkersConfig{DefaultClass: "linux-x64"},
	}
	e := &resetEnv{m: m, listenersErr: errors.New("no daemon")}
	e.name = naming.ScaleSetNameFor("unraid", "o/prune", linuxX64)
	ss, err := m.Client("o/prune").CreateRunnerScaleSet(context.Background(), &scaleset.RunnerScaleSet{
		Name: e.name, RunnerGroupID: 1, Labels: []scaleset.Label{{Name: "self-hosted"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.id = ss.ID
	e.q = NewQueryWith(cfg, slog.New(slog.DiscardHandler), Deps{
		GitHub: func(ctx context.Context, repo config.Repo) (ghapi.Client, error) {
			c, err := m.Factory()(ctx, repo)
			for _, w := range wraps {
				c = w(c)
			}
			return c, err
		},
		Deleter: func(_ context.Context, repo config.Repo) (ScaleSetDeleter, error) {
			return simDeleter{m: m, repo: repo.Name}, nil
		},
		Listeners: func(context.Context) ([]control.ListenerLiveStatus, error) {
			return e.listeners, e.listenersErr
		},
	})
	return e
}

func TestResetScaleSetDeletesAndReportsIDForAnyRepoCase(t *testing.T) {
	e := newResetEnv(t)
	name, id, err := e.q.ResetScaleSet(context.Background(), "O/PRUNE", linuxX64)
	if err != nil {
		t.Fatalf("ResetScaleSet: %v", err)
	}
	if name != e.name || id != e.id {
		t.Fatalf("ResetScaleSet = %q #%d, want %q #%d", name, id, e.name, e.id)
	}
	if got := e.m.Deleted("o/prune"); len(got) != 1 || got[0] != e.id {
		t.Fatalf("Deleted = %v, want [%d]", got, e.id)
	}
}

func TestResetScaleSetNotFound(t *testing.T) {
	e := newResetEnv(t)
	if err := e.m.DeleteScaleSet("o/prune", e.id); err != nil {
		t.Fatal(err)
	}
	_, _, err := e.q.ResetScaleSet(context.Background(), "o/prune", linuxX64)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("ResetScaleSet(missing) = %v", err)
	}
	if len(e.m.Deleted("o/prune")) != 1 {
		t.Fatal("deleted something that was not found")
	}
}

func TestResetScaleSetRefusesNonDefaultClass(t *testing.T) {
	e := newResetEnv(t)
	_, _, err := e.q.ResetScaleSet(context.Background(), "o/prune", runner.Class{OS: "linux", Arch: "arm64"})
	if err == nil || !strings.Contains(err.Error(), "linux-x64") || !strings.Contains(err.Error(), "nothing would recreate") {
		t.Fatalf("err = %v, want a default-class refusal naming linux-x64", err)
	}
	if len(e.m.Deleted("o/prune")) != 0 {
		t.Fatal("deleted a non-default class")
	}
}

func TestResetScaleSetRefusesWithoutStatistics(t *testing.T) {
	e := newResetEnv(t, func(c ghapi.Client) ghapi.Client { return nilStatsClient{Client: c} })
	_, _, err := e.q.ResetScaleSet(context.Background(), "o/prune", linuxX64)
	if !errors.Is(err, ErrScaleSetBusy) {
		t.Fatalf("err = %v, want ErrScaleSetBusy", err)
	}
	if len(e.m.Deleted("o/prune")) != 0 {
		t.Fatal("deleted without statistics")
	}
}

func TestResetScaleSetRefusesWhenProbeStatisticsAreBusy(t *testing.T) {
	// The lookup read idle statistics; by the probe the scale set is busy.
	idle := &scaleset.RunnerScaleSetStatistic{}
	e := newResetEnv(t, func(c ghapi.Client) ghapi.Client { return nilStatsClient{Client: c, stats: idle} })
	e.m.SetStats("o/prune", e.name, scaleset.RunnerScaleSetStatistic{TotalRunningJobs: 1})
	_, _, err := e.q.ResetScaleSet(context.Background(), "o/prune", linuxX64)
	if !errors.Is(err, ErrScaleSetBusy) {
		t.Fatalf("err = %v, want ErrScaleSetBusy", err)
	}
	if len(e.m.Deleted("o/prune")) != 0 {
		t.Fatal("deleted despite busy probe statistics")
	}
}

func TestResetScaleSetRefusesOnProbeErrors(t *testing.T) {
	t.Run("non-409 open error", func(t *testing.T) {
		e := newResetEnv(t)
		e.m.FailSessions("o/prune", e.name, errors.New("boom"), 1)
		_, _, err := e.q.ResetScaleSet(context.Background(), "o/prune", linuxX64)
		if err == nil || errors.Is(err, ErrScaleSetLive) || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("err = %v, want the open error", err)
		}
		if len(e.m.Deleted("o/prune")) != 0 {
			t.Fatal("deleted after a failed probe")
		}
	})
	t.Run("close error", func(t *testing.T) {
		e := newResetEnv(t, func(c ghapi.Client) ghapi.Client { return closeFailClient{c} })
		_, _, err := e.q.ResetScaleSet(context.Background(), "o/prune", linuxX64)
		if err == nil || !strings.Contains(err.Error(), "close failed") {
			t.Fatalf("err = %v, want the close error", err)
		}
		if len(e.m.Deleted("o/prune")) != 0 {
			t.Fatal("deleted after a failed probe close")
		}
	})
}

func TestResetScaleSetDegradedListenerForAnotherScaleSetDoesNotSkipProbe(t *testing.T) {
	e := newResetEnv(t)
	if _, err := e.m.Client("o/prune").OpenSession(context.Background(), e.id, "foreign"); err != nil {
		t.Fatal(err)
	}
	e.listenersErr = nil
	e.listeners = []control.ListenerLiveStatus{{Repo: "o/prune", Class: "linux-x64", ScaleSetName: "gso-other-prune-linux-x64", State: "degraded"}}
	_, _, err := e.q.ResetScaleSet(context.Background(), "o/prune", linuxX64)
	if !errors.Is(err, ErrScaleSetLive) {
		t.Fatalf("err = %v, want ErrScaleSetLive", err)
	}
	if len(e.m.Deleted("o/prune")) != 0 {
		t.Fatal("deleted while a foreign session was held")
	}
}

func TestResetScaleSetRunningListenerOfAnotherClassDoesNotBlock(t *testing.T) {
	e := newResetEnv(t)
	e.listenersErr = nil
	e.listeners = []control.ListenerLiveStatus{{Repo: "o/prune", Class: "linux-arm64", ScaleSetName: "gso-unraid-prune-linux-arm64", State: "running"}}
	if _, _, err := e.q.ResetScaleSet(context.Background(), "o/prune", linuxX64); err != nil {
		t.Fatalf("ResetScaleSet: %v", err)
	}
}

func TestStatusRepoFilterIsCaseInsensitive(t *testing.T) {
	e := newResetEnv(t)
	st, err := e.q.Status(context.Background(), "O/PRU")
	if err != nil || len(st.Repos) != 1 {
		t.Fatalf("Status(filter O/PRU) = %+v, %v", st, err)
	}
	sets, err := e.q.ScaleSets(context.Background(), "O/PRU")
	if err != nil || len(sets) != 1 {
		t.Fatalf("ScaleSets(filter O/PRU) = %+v, %v", sets, err)
	}
}

func TestResetScaleSetRefusesBusyScaleSet(t *testing.T) {
	cases := map[string]scaleset.RunnerScaleSetStatistic{
		"busy runner":  {TotalRegisteredRunners: 1, TotalBusyRunners: 1},
		"running job":  {TotalRunningJobs: 1},
		"assigned job": {TotalAssignedJobs: 1},
		"acquired job": {TotalAcquiredJobs: 1},
	}
	for name, st := range cases {
		t.Run(name, func(t *testing.T) {
			e := newResetEnv(t)
			e.m.SetStats("o/prune", e.name, st)
			_, id, err := e.q.ResetScaleSet(context.Background(), "o/prune", linuxX64)
			if !errors.Is(err, ErrScaleSetBusy) {
				t.Fatalf("err = %v, want ErrScaleSetBusy", err)
			}
			if id != e.id {
				t.Fatalf("id = %d, want %d", id, e.id)
			}
			if got := e.m.Deleted("o/prune"); len(got) != 0 {
				t.Fatalf("deleted %v despite activity", got)
			}
		})
	}
}

func TestResetScaleSetRefusesWhileASessionIsHeldAndNoDaemonIsLocal(t *testing.T) {
	e := newResetEnv(t)
	// Another gso (possibly on another host) holds the message session.
	if _, err := e.m.Client("o/prune").OpenSession(context.Background(), e.id, "other-gso"); err != nil {
		t.Fatal(err)
	}
	_, _, err := e.q.ResetScaleSet(context.Background(), "o/prune", linuxX64)
	if !errors.Is(err, ErrScaleSetLive) {
		t.Fatalf("err = %v, want ErrScaleSetLive", err)
	}
	if got := e.m.Deleted("o/prune"); len(got) != 0 {
		t.Fatalf("deleted %v while a session was held", got)
	}
}

func TestResetScaleSetRefusesWhenLocalListenerIsRunning(t *testing.T) {
	e := newResetEnv(t)
	e.listenersErr = nil
	e.listeners = []control.ListenerLiveStatus{{Repo: "O/Prune", Class: "linux-x64", ScaleSetID: e.id, ScaleSetName: e.name, State: "running"}}
	_, _, err := e.q.ResetScaleSet(context.Background(), "o/prune", linuxX64)
	if !errors.Is(err, ErrScaleSetLive) {
		t.Fatalf("err = %v, want ErrScaleSetLive", err)
	}
	if got := e.m.Deleted("o/prune"); len(got) != 0 {
		t.Fatalf("deleted %v under a running listener", got)
	}
}

func TestResetScaleSetAllowsDegradedLocalListener(t *testing.T) {
	// The documented 409 escape hatch: the local listener is stuck because a
	// stale owner holds the session, so a held session is expected.
	for _, state := range []string{"degraded", "retrying"} {
		t.Run(state, func(t *testing.T) {
			e := newResetEnv(t)
			if _, err := e.m.Client("o/prune").OpenSession(context.Background(), e.id, "stale-owner"); err != nil {
				t.Fatal(err)
			}
			e.listenersErr = nil
			e.listeners = []control.ListenerLiveStatus{{Repo: "o/prune", Class: "linux-x64", ScaleSetID: e.id, ScaleSetName: e.name, State: state}}
			if _, _, err := e.q.ResetScaleSet(context.Background(), "o/prune", linuxX64); err != nil {
				t.Fatalf("ResetScaleSet: %v", err)
			}
			if got := e.m.Deleted("o/prune"); len(got) != 1 {
				t.Fatalf("Deleted = %v, want the scale set", got)
			}
		})
	}
}

func TestScaleSetsAllListsCurrentLegacyAndOther(t *testing.T) {
	e := newResetEnv(t)
	legacy := naming.ScaleSetName(naming.ScaleSetNamePrefix, hostnameOrUnknown(), "o/prune")
	for _, n := range []string{legacy, "gso-deadbeef-prune", "arc-runners"} {
		if _, err := e.m.Client("o/prune").CreateRunnerScaleSet(context.Background(), &scaleset.RunnerScaleSet{Name: n, RunnerGroupID: 1}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := e.q.ScaleSetsAll(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]ScaleSetKind{}
	for _, ls := range got {
		kinds[ls.Name] = ls.Kind
	}
	if len(kinds) != 3 || kinds[e.name] != ScaleSetCurrent || kinds["gso-deadbeef-prune"] != ScaleSetOther || kinds[legacy] != ScaleSetLegacy {
		t.Fatalf("kinds = %v (legacy name %q)", kinds, legacy)
	}
}

func TestStatusAndScaleSetsLookUpTheCurrentName(t *testing.T) {
	e := newResetEnv(t)
	st, err := e.q.Status(context.Background(), "")
	if err != nil || len(st.Repos) != 1 || st.Repos[0].ScaleSetID != e.id {
		t.Fatalf("Status = %+v, %v; want scale set #%d", st, err, e.id)
	}
	sets, err := e.q.ScaleSets(context.Background(), "")
	if err != nil || len(sets) != 1 || sets[0].ID != e.id {
		t.Fatalf("ScaleSets = %+v, %v", sets, err)
	}
}
