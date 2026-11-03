package router

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/fleet"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

// localExecutor adapts the router's own executor to fleet.LocalExecutor.
type localExecutor struct {
	docker *runner.DockerWorker
	plain  runner.Executor
}

func (l localExecutor) ForRepo(repo, cache string) runner.Executor {
	if l.docker != nil {
		return l.docker.ForRepo(repo, cache)
	}
	return l.plain
}

// prepareLocal registers the in-process local member (spec §5.1, §5.5). With
// max_runners: 0 there is none: no runner download and no Docker socket.
func (r *Router) prepareLocal(ctx context.Context) error {
	exec := r.opts.LocalExecutor
	if exec == nil && r.cfg.MaxRunners > 0 {
		built, err := r.buildLocalExecutor(ctx)
		if err != nil {
			return err
		}
		exec = built
	}
	slots := r.cfg.MaxRunners
	if exec == nil {
		slots = 0
	}
	r.mu.Lock()
	r.local = exec
	r.mu.Unlock()
	r.fleet.SetLocal(slots, r.cfg.DefaultClass(), exec)
	r.started.Store(true)
	return nil
}

// buildLocalExecutor is the executor preparation orchestrator.Run did before
// B8a.
func (r *Router) buildLocalExecutor(ctx context.Context) (fleet.LocalExecutor, error) {
	if r.cfg.Runner.Executor == "docker" {
		dockerWorker, err := runner.NewDockerWorker(runner.DockerConfig{
			Image:      r.cfg.Runner.Image,
			InstanceID: r.cfg.Runner.InstanceID,
			MemoryMB:   r.cfg.Runner.MemoryMB,
			CPUs:       r.cfg.Runner.CPUs,
			CacheRoot:  r.cfg.Runner.CacheRoot,
			CacheUID:   r.cfg.Runner.CacheUID,
			CacheGID:   r.cfg.Runner.CacheGID,
			Host:       r.cfg.Runner.DockerHost,
		}, r.logger)
		if err != nil {
			return nil, fmt.Errorf("configuring docker executor: %w", err)
		}
		if err := dockerWorker.Reconcile(ctx); err != nil {
			return nil, fmt.Errorf("reconciling stale runner containers: %w", err)
		}
		return localExecutor{docker: dockerWorker}, nil
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		cacheDir = "/tmp"
	}
	mgr, err := runner.NewManager(filepath.Join(cacheDir, "gso"), r.logger)
	if err != nil {
		return nil, fmt.Errorf("creating runner manager: %w", err)
	}
	runnerDir, err := mgr.RunnerDir(ctx)
	if err != nil {
		return nil, fmt.Errorf("preparing runner binary: %w", err)
	}
	r.logger.Info("runner binary ready", "dir", runnerDir)
	return localExecutor{plain: runner.NewWorker(runnerDir, r.logger)}, nil
}
