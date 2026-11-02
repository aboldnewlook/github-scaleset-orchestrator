package cmd

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/timing"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
	"github.com/aboldnewlook/github-scaleset-orchestrator/worker"
)

// simLauncher runs `gso sim runner` processes in place of job containers. It
// stands in for the docker executor, so it advertises one linux class.
func simLauncher(stateDir string) (worker.Launcher, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locating the gso binary: %w", err)
	}
	class, err := runner.ClassFor("linux", runtime.GOARCH)
	if err != nil {
		return nil, fmt.Errorf("sim executor: %w", err)
	}
	return worker.NewProcessLauncher(worker.ProcessLauncherConfig{
		Command: []string{exe, "sim", "runner"},
		Dir:     filepath.Join(stateDir, "sim"),
		Kind:    runner.ExecutorDocker,
		Classes: []runner.Class{class},
		Env:     timingScaleEnv(),
	})
}

// timingScaleEnv forwards GSO_TIMING_SCALE, which runner.SanitizedEnv strips.
func timingScaleEnv() []string {
	if v := os.Getenv("GSO_TIMING_SCALE"); v != "" {
		return []string{"GSO_TIMING_SCALE=" + v}
	}
	return nil
}

// workerBackoff returns the reconnect backoff bounds (1 s to 15 s), shortened by GSO_TIMING_SCALE.
func workerBackoff() (time.Duration, time.Duration) {
	return timing.Scale(time.Second), timing.Scale(15 * time.Second)
}

// simTrustedHosts returns the hosts a worker accepts in JIT configs when it
// serves a simulated GitHub. worker.OfferCheck.TrustedHosts replaces the
// GitHub defaults, so the list carries both github.com (the simulator's
// gitHubUrl) and the simulator's own host (its serverUrl), followed by any
// explicit --trusted-host entries. It returns explicit unchanged outside sim
// mode (no simulator URL and not the sim executor).
func simTrustedHosts(simURL string, simExecutor bool, explicit []string) ([]string, error) {
	if simURL == "" && !simExecutor {
		return explicit, nil
	}
	hosts := []string{"github.com"}
	if simURL != "" {
		u, err := url.Parse(simURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
			return nil, errors.New("--github-sim must be an http(s) URL such as http://host:9300")
		}
		hosts = append(hosts, strings.ToLower(u.Hostname()))
	}
	for _, h := range explicit {
		if !slices.Contains(hosts, strings.ToLower(h)) {
			hosts = append(hosts, h)
		}
	}
	return hosts, nil
}
