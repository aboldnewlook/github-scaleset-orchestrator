//go:build !windows

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghsim"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/naming"
)

var (
	imageOnce sync.Once
	imageErr  error
)

const e2eImage = "gso:e2e"

func buildImage(t *testing.T) string {
	t.Helper()
	imageOnce.Do(func() {
		wd, _ := os.Getwd()
		out, err := exec.Command("docker", "build", "-t", e2eImage, filepath.Join(wd, "..", "..")).CombinedOutput()
		if err != nil {
			imageErr = fmt.Errorf("docker build: %w\n%s", err, out)
		}
	})
	if imageErr != nil {
		t.Fatal(imageErr)
	}
	return e2eImage
}

func removeContainers(instance string) {
	out, _ := exec.Command("docker", "ps", "-aq", "--filter", "label=gso.instance="+instance).Output()
	if ids := strings.Fields(string(out)); len(ids) > 0 {
		_ = exec.Command("docker", append([]string{"rm", "-f"}, ids...)...).Run()
	}
}

// Scenarios (1) and (5) with the docker executor: the job container runs the
// image's gso-job, which hands the simulator JIT to the fake runner.
func TestDockerExecutorRunsAndReattaches(t *testing.T) {
	if os.Getenv("GSO_DOCKER_IT") != "1" {
		t.Skip("set GSO_DOCKER_IT=1 to run docker-backed tests")
	}
	image := buildImage(t)
	host := os.Getenv("GSO_E2E_HOST_ADDR") // the host as seen from a container
	if host == "" {
		host = "host.docker.internal"
	}
	port := freePort(t)
	sim := startSimAt(t, fmt.Sprintf("0.0.0.0:%d", port), fmt.Sprintf("http://%s:%d", host, port))
	r := startRouter(t, sim, defaultRepos)
	args := []string{"--executor", "docker", "--image", image}
	if h := os.Getenv("GSO_DOCKER_HOST"); h != "" {
		args = append(args, "--docker-host", h)
	}
	w := startWorker(t, r, "dock", r.addWorker(t, "dock", simClass(), 1, ""), args...)
	t.Cleanup(func() { removeContainers("gsow-" + naming.WorkerSegment("dock")) })
	waitWorkerConnected(t, r, "dock")

	first := waitJob(t, sim, injectJob(t, sim, "--repo", "o/a", "--duration", "2s"), ghsim.JobCompleted, 3*time.Minute)
	if first.Result != "succeeded" {
		t.Fatalf("containerised job result = %q", first.Result)
	}

	id := injectJob(t, sim, "--repo", "o/a", "--duration", "8s")
	waitJob(t, sim, id, ghsim.JobRunning, 2*time.Minute)
	w.Restart(t, true) // the container keeps running
	waitWorkerConnected(t, r, "dock")
	waitJob(t, sim, id, ghsim.JobCompleted, time.Minute)
	waitIdle(t, r, w, "dock")
	if n := len(simRunners(t, sim, "o/a")); n != 2 {
		t.Fatalf("minted %d runners for two jobs, want 2", n)
	}
}
