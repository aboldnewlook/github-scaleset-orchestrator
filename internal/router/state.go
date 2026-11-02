package router

import (
	"fmt"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/service"
)

var (
	_ service.OrchestratorState = (*Router)(nil)
	_ service.WorkerAdmin       = (*Router)(nil)
)

// RunnersByRepo delegates to the orchestrator.
func (r *Router) RunnersByRepo() map[string][]string { return r.orch.RunnersByRepo() }

// MaxRunners delegates to the orchestrator.
func (r *Router) MaxRunners() int { return r.orch.MaxRunners() }

// AvailableSlots delegates to the orchestrator.
func (r *Router) AvailableSlots() int { return r.orch.AvailableSlots() }

// SetMaxRunners delegates to the orchestrator (B8a moves the local member here).
//
// Zero means route-only. The runtime allows it, so the router decides: it is
// refused without workers.listen, and with it the pre-A3b orchestrator cannot
// shrink to zero (SetMaxRunners rejects < 1), so only a router already at 0
// accepts it (a no-op) until B8a moves the local member into the fleet.
func (r *Router) SetMaxRunners(count int) error {
	switch {
	case count < 0:
		return fmt.Errorf("max_runners cannot be negative")
	case count > 0:
		return r.orch.SetMaxRunners(count)
	case !r.cfg.WorkersEnabled():
		return fmt.Errorf("max_runners must be at least 1 (max_runners: 0 requires workers.listen)")
	case r.orch.MaxRunners() == 0:
		return nil
	}
	return fmt.Errorf("cannot lower max_runners to 0 at runtime yet; set max_runners: 0 in the config and restart")
}

// CancelRunner delegates to the orchestrator.
func (r *Router) CancelRunner(name string) bool { return r.orch.CancelRunner(name) }
