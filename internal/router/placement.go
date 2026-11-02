package router

import (
	"context"
	"errors"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/fleet"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

var errPlacementNotWired = errors.New("runner placement is not wired to the orchestrator")

// placementGitHub gives the fleet the orchestrator's per-repo GitHub access.
// It delegates only when the target implements fleet.GitHub (B8a).
type placementGitHub struct{ target func() any }

func (p placementGitHub) github() (fleet.GitHub, bool) {
	g, ok := p.target().(fleet.GitHub)
	return g, ok
}

func (p placementGitHub) GetRunnerByName(ctx context.Context, repo, name string) (*fleet.RunnerRef, error) {
	g, ok := p.github()
	if !ok {
		return nil, errPlacementNotWired
	}
	return g.GetRunnerByName(ctx, repo, name)
}

func (p placementGitHub) RemoveRunner(ctx context.Context, repo string, runnerID int64) error {
	g, ok := p.github()
	if !ok {
		return errPlacementNotWired
	}
	return g.RemoveRunner(ctx, repo, runnerID)
}

func (p placementGitHub) ScaleSetID(repo string, class runner.Class) (int, bool) {
	g, ok := p.github()
	if !ok {
		return 0, false
	}
	return g.ScaleSetID(repo, class)
}

// placementAdopter routes verified runners to the orchestrator's scalers.
type placementAdopter struct{ target func() any }

func (p placementAdopter) Adopt(repo string, class runner.Class, res *fleet.Reservation) bool {
	a, ok := p.target().(fleet.Adopter)
	if !ok {
		return false
	}
	return a.Adopt(repo, class, res)
}
