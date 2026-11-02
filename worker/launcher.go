package worker

import (
	"context"

	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

// Launcher runs JIT runners of one executor kind on this worker.
//
// Start returns once the runner is launched; Wait blocks until it exits.
// Cancelling Wait's context detaches: the runner keeps running so that a
// restarted worker can Wait on it again (design spec §7, "Worker process
// restart"). Kill ends a runner; a runner that is already gone is not an error.
type Launcher interface {
	Kind() runner.ExecutorKind
	Classes(ctx context.Context) ([]runner.Class, error)
	Start(ctx context.Context, rec RunnerRecord, jit, cache string) error
	Wait(ctx context.Context, rec RunnerRecord) error
	Kill(ctx context.Context, rec RunnerRecord) error
	RemoveOrphans(ctx context.Context, keep map[string]bool) error
}
