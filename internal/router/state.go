package router

import (
	"fmt"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/service"
)

var (
	_ service.OrchestratorState = (*Router)(nil)
	_ service.WorkerAdmin       = (*Router)(nil)
	_ service.ListenerReporter  = (*Router)(nil)
)

// RunnersByRepo delegates to the orchestrator.
func (r *Router) RunnersByRepo() map[string][]string { return r.orch.RunnersByRepo() }

// MaxRunners delegates to the orchestrator.
func (r *Router) MaxRunners() int { return r.orch.MaxRunners() }

// AvailableSlots delegates to the orchestrator.
func (r *Router) AvailableSlots() int { return r.orch.AvailableSlots() }

// Listeners delegates to the orchestrator so live_status shows each
// (repo x class) listener's state through the router.
func (r *Router) Listeners() []control.ListenerLiveStatus { return r.orch.Listeners() }

// SetMaxRunners resizes the local member (spec §5.5). Zero means route-only
// and needs workers.listen. Two transitions need a restart because they would
// build or tear down the local executor, which only Run does:
//   - lowering a running local member to 0 (its runners would be orphaned);
//   - growing a router that started with max_runners: 0, which has no local
//     executor (no runner download, no Docker socket).
func (r *Router) SetMaxRunners(count int) error {
	switch {
	case count < 0:
		return fmt.Errorf("max_runners cannot be negative")
	case count == 0 && !r.cfg.WorkersEnabled():
		return fmt.Errorf("max_runners must be at least 1 (max_runners: 0 requires workers.listen)")
	case count == 0 && r.orch.MaxRunners() > 0:
		return fmt.Errorf("cannot lower max_runners to 0 at runtime; set max_runners: 0 in the config and restart")
	}
	r.mu.Lock()
	local := r.local
	r.mu.Unlock()
	if count > 0 && local == nil && r.started.Load() {
		return fmt.Errorf("the local executor is not running (gso started with max_runners: 0); set max_runners > 0 in the config and restart")
	}
	if r.started.Load() && local != nil {
		r.fleet.SetLocal(count, r.cfg.DefaultClass(), local)
	}
	return r.orch.SetMaxRunners(count)
}

// CancelRunner delegates to the orchestrator.
func (r *Router) CancelRunner(name string) bool { return r.orch.CancelRunner(name) }
