// Package runner runs GitHub Actions JIT runners: Manager downloads and
// verifies the runner binary, Worker runs it as a subprocess, and
// DockerWorker runs it in a sibling container.
//
// The package is public so that gso workers outside this module (the Burst
// desktop app) can import it. It must not import gso's internal packages,
// cobra, viper, bubbletea or actions/scaleset; deps_test.go enforces this.
package runner
