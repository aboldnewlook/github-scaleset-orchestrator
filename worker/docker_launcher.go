package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

type dockerLauncher struct {
	w          *runner.DockerWorker
	backoffMin time.Duration // Wait retry backoff while the engine is unreachable
	backoffMax time.Duration
}

// NewDockerLauncher runs Linux-class runners as sibling containers on the
// worker's Docker engine, named after the runner. w must be built with
// OnCancel: runner.DetachOnCancel so that stopping the worker leaves jobs
// running for the next start to re-attach to.
func NewDockerLauncher(w *runner.DockerWorker) Launcher {
	return &dockerLauncher{w: w, backoffMin: time.Second, backoffMax: 15 * time.Second}
}

func (d *dockerLauncher) Kind() runner.ExecutorKind { return runner.ExecutorDocker }

// Classes reports linux-<arch> for the engine's native architecture: Docker
// Desktop on Apple Silicon serves linux-arm64, on an x64 PC linux-x64.
func (d *dockerLauncher) Classes(ctx context.Context) ([]runner.Class, error) {
	arch, err := d.w.EngineArch(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the docker engine architecture: %w", err)
	}
	class, err := runner.ClassFor("linux", arch)
	if err != nil {
		return nil, fmt.Errorf("docker engine architecture %q is not supported: %w", arch, err)
	}
	return []runner.Class{class}, nil
}

func (d *dockerLauncher) Start(ctx context.Context, rec RunnerRecord, jit, cache string) error {
	if cache == "" {
		cache = "none"
	}
	return d.w.ForRepo(rec.Repo, cache).Start(ctx, rec.Name, jit)
}

// Wait follows the runner until it exits. While the engine is unreachable
// (runner.ErrWaitInterrupted: Docker Desktop not up yet, dockerd restarting,
// the laptop asleep) the container may well be running, so Wait retries with
// capped backoff instead of reporting an exit. A missing container
// (runner.ErrContainerNotFound), an exit status and ctx cancellation return at once.
func (d *dockerLauncher) Wait(ctx context.Context, rec RunnerRecord) error {
	delay := d.backoffMin
	for {
		err := d.w.ForRepo(rec.Repo, "none").Wait(ctx, rec.Name)
		if err == nil || ctx.Err() != nil || !errors.Is(err, runner.ErrWaitInterrupted) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(delay*2, d.backoffMax)
	}
}

func (d *dockerLauncher) Kill(ctx context.Context, rec RunnerRecord) error {
	return d.w.Kill(ctx, rec.Name)
}

func (d *dockerLauncher) RemoveOrphans(ctx context.Context, keep map[string]bool) error {
	_, err := d.w.RemoveOrphans(ctx, keep)
	return err
}
