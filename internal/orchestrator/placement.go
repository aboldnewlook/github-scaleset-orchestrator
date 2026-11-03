package orchestrator

import (
	"context"
	"fmt"
	"strings"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/fleet"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghapi"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/scaler"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

var (
	_ fleet.GitHub  = (*Orchestrator)(nil)
	_ fleet.Adopter = (*Orchestrator)(nil)
)

// WithFleet sets the fleet runners are placed on. The router owns it.
func WithFleet(f *fleet.Fleet) Option {
	return func(o *Orchestrator) { o.fleet = f }
}

// scalerKey identifies one scale set's scaler: owner/repo (case-insensitive, as
// GitHub and ghsim fold it) plus the runner class.
func scalerKey(repo string, class runner.Class) string {
	return strings.ToLower(repo) + "|" + class.String()
}

// newScaler returns the scaler for (repo x class), creating it once. The
// supervisor reuses it across listener restarts, so in-flight runners and
// adoption state survive a session recreate (spec §5.3).
func (o *Orchestrator) newScaler(repo config.Repo, class runner.Class, client ghapi.Client) *scaler.Scaler {
	key := scalerKey(repo.Name, class)
	o.mu.Lock()
	defer o.mu.Unlock()
	if s, ok := o.byClass[key]; ok {
		return s
	}
	cache := o.cfg.Runner.Cache
	if repo.RunnerCache != "" {
		cache = repo.RunnerCache
	}
	s := scaler.New(scaler.Config{Repo: repo.Name, Class: class, Client: client, Placer: o.fleet, Cache: cache, Logger: o.logger, Bus: o.bus})
	o.byClass[key] = s
	return s
}

func (o *Orchestrator) scalerFor(repo string, class runner.Class) *scaler.Scaler {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.byClass[scalerKey(repo, class)]
}

func (o *Orchestrator) allScalers() []*scaler.Scaler {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]*scaler.Scaler, 0, len(o.byClass))
	for _, s := range o.byClass {
		out = append(out, s)
	}
	return out
}

// clientFor returns the one GitHub client for a configured repo, creating it
// on first use. The supervisor and the fleet callbacks share it, so the
// factory (and with it token resolution) runs once per repo.
func (o *Orchestrator) clientFor(ctx context.Context, repo config.Repo) (ghapi.Client, error) {
	key := strings.ToLower(repo.Name)
	o.mu.Lock()
	c, ok := o.clients[key]
	factory := o.github
	o.mu.Unlock()
	if ok {
		return c, nil
	}
	c, err := factory(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("creating GitHub client: %w", err)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if existing, ok := o.clients[key]; ok {
		return existing, nil
	}
	o.clients[key] = c
	return c, nil
}

// repoClient returns the client for a configured repo, by name.
func (o *Orchestrator) repoClient(ctx context.Context, name string) (ghapi.Client, error) {
	for _, r := range o.cfg.Repos {
		if strings.EqualFold(r.Name, name) {
			return o.clientFor(ctx, r)
		}
	}
	return nil, fmt.Errorf("repo %s is not configured", name)
}

// GetRunnerByName implements fleet.GitHub (adoption checks, spec §4.3).
func (o *Orchestrator) GetRunnerByName(ctx context.Context, repo, name string) (*fleet.RunnerRef, error) {
	c, err := o.repoClient(ctx, repo)
	if err != nil {
		return nil, err
	}
	ref, err := c.GetRunnerByName(ctx, name)
	if err != nil || ref == nil {
		return nil, err
	}
	return &fleet.RunnerRef{ID: int64(ref.ID), ScaleSetID: ref.RunnerScaleSetID}, nil
}

// RemoveRunner implements fleet.GitHub (every abandon path, spec §8.3).
func (o *Orchestrator) RemoveRunner(ctx context.Context, repo string, runnerID int64) error {
	c, err := o.repoClient(ctx, repo)
	if err != nil {
		return err
	}
	return c.RemoveRunner(ctx, runnerID)
}

// ScaleSetID implements fleet.GitHub: the scale set the scaler mints in.
func (o *Orchestrator) ScaleSetID(repo string, class runner.Class) (int, bool) {
	s := o.scalerFor(repo, class)
	if s == nil {
		return 0, false
	}
	id := s.ScaleSetID()
	return id, id != 0
}

// Adopt implements fleet.Adopter: a verified runner joins its scaler.
func (o *Orchestrator) Adopt(repo string, class runner.Class, res *fleet.Reservation) bool {
	s := o.scalerFor(repo, class)
	if s == nil {
		return false
	}
	return s.Adopt(res)
}

// signalReevaluate coalesces fleet changes; it never blocks the fleet.
func (o *Orchestrator) signalReevaluate() {
	select {
	case o.reevaluate <- struct{}{}:
	default:
	}
}

// reevaluateLoop re-runs every scaler's placement with its cached statistics
// when fleet capacity changes (spec §5.2 "Re-evaluation").
func (o *Orchestrator) reevaluateLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-o.reevaluate:
			for _, s := range o.allScalers() {
				s.Reevaluate(ctx)
			}
		}
	}
}
