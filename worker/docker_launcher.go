package worker

import (
	"context"
	"fmt"

	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

type dockerLauncher struct {
	w *runner.DockerWorker
}

// NewDockerLauncher runs Linux-class runners as sibling containers on the
// worker's Docker engine, named after the runner. w must be built with
// OnCancel: runner.DetachOnCancel so that stopping the worker leaves jobs
// running for the next start to re-attach to.
func NewDockerLauncher(w *runner.DockerWorker) Launcher {
	return &dockerLauncher{w: w}
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

func (d *dockerLauncher) Wait(ctx context.Context, rec RunnerRecord) error {
	return d.w.ForRepo(rec.Repo, "none").Wait(ctx, rec.Name)
}

func (d *dockerLauncher) Kill(ctx context.Context, rec RunnerRecord) error {
	return d.w.Kill(ctx, rec.Name)
}

func (d *dockerLauncher) RemoveOrphans(ctx context.Context, keep map[string]bool) error {
	_, err := d.w.RemoveOrphans(ctx, keep)
	return err
}
