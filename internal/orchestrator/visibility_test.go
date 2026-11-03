package orchestrator

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghapi"
)

// repoFailClient fails Repo() while failing > 0.
type repoFailClient struct {
	ghapi.Client
	failing *atomic.Int32
}

func (c *repoFailClient) Repo(ctx context.Context) (ghapi.RepoInfo, error) {
	if c.failing.Add(-1) >= 0 {
		return ghapi.RepoInfo{}, errors.New("github: 500")
	}
	return c.Client.Repo(ctx)
}

func TestUnknownVisibilityCreatesNoScaleSetUntilTheLookupSucceeds(t *testing.T) {
	m := newModel()
	var failing atomic.Int32
	failing.Store(1 << 30)
	factory := func(ctx context.Context, repo config.Repo) (ghapi.Client, error) {
		c, err := m.Factory()(ctx, repo)
		return &repoFailClient{Client: c, failing: &failing}, err
	}
	o := newTestOrchestrator(t, factory, nil, fastTiming(), "org/repo")
	superviseWith(t, o, "org/repo", blockingExecutor{})

	eventually(t, "the visibility lookup to fail and be retried", func() bool { return listenerState(o, "org/repo").Failures >= 2 })
	if n := len(m.ScaleSets("org/repo")); n != 0 {
		t.Fatalf("%d scale sets exist while visibility is unknown, want 0", n)
	}
	failing.Store(0)
	eventually(t, "the listener to run once the lookup succeeds", func() bool { return listenerState(o, "org/repo").State == "running" })
	if n := len(m.ScaleSets("org/repo")); n != 1 {
		t.Fatalf("scale sets = %d after recovery, want 1", n)
	}
}

func TestPublicRepoPlacesOnlyOnMembersThatAcceptPublic(t *testing.T) {
	m := newModel()
	m.SetRepo("org/repo", ghapi.RepoInfo{FullName: "org/repo", Visibility: "public"})
	o := newTestOrchestrator(t, m.Factory(), nil, fastTiming(), "org/repo")
	superviseWith(t, o, "org/repo", blockingExecutor{}) // the local member does not accept public here
	eventually(t, "listener running", func() bool { return listenerState(o, "org/repo").State == "running" })

	s := o.scalerFor("org/repo", o.class)
	if s == nil {
		t.Fatal("no scaler")
	}
	if got := s.Capacity(); got != 0 {
		t.Fatalf("capacity = %d for a public repo with no member accepting public, want 0", got)
	}
	o.fleet.SetLocalAcceptPublic(true)
	if got := s.Capacity(); got != 4 {
		t.Fatalf("capacity = %d once the local member accepts public, want 4", got)
	}
}

func TestPrivateRepoIsNotMarkedPublic(t *testing.T) {
	m := newModel()
	o := newTestOrchestrator(t, m.Factory(), nil, fastTiming(), "org/repo")
	superviseWith(t, o, "org/repo", blockingExecutor{})
	eventually(t, "listener running", func() bool { return listenerState(o, "org/repo").State == "running" })
	if got := o.scalerFor("org/repo", o.class).Capacity(); got != 4 {
		t.Fatalf("capacity = %d for a private repo, want 4", got)
	}
}
