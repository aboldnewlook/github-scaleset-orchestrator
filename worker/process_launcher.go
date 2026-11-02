package worker

import "github.com/aboldnewlook/github-scaleset-orchestrator/runner"

// ProcessLauncherConfig configures NewProcessLauncher.
type ProcessLauncherConfig struct {
	Command []string            // argv; the JIT config arrives in ACTIONS_RUNNER_INPUT_JITCONFIG
	Dir     string              // holds <runner>.pid and <runner>.log
	Kind    runner.ExecutorKind // the executor kind advertised to the router
	Classes []runner.Class
	Env     []string // appended to runner.SanitizedEnv(), which strips GSO_* variables
}
