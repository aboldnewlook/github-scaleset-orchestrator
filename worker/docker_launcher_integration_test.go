package worker_test

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
	"github.com/aboldnewlook/github-scaleset-orchestrator/worker"
)

// TestDockerLauncherReattachIntegration runs a real container, stops the
// worker mid-job, and checks that a new worker re-attaches and reports the
// exit (spec §7, "Worker process restart"). Run: GSO_DOCKER_IT=1 go test -run Integration ./worker/
func TestDockerLauncherReattachIntegration(t *testing.T) {
	if os.Getenv("GSO_DOCKER_IT") != "1" {
		t.Skip("set GSO_DOCKER_IT=1 to run against a real Docker engine")
	}
	if runtime.GOOS == "windows" {
		t.Skip("unix:// docker endpoints only")
	}
	host := os.Getenv("GSO_DOCKER_HOST")
	image := buildJobImage(t, host)
	dw, err := runner.NewDockerWorker(runner.DockerConfig{Image: image, InstanceID: "gsow-it", Host: host, OnCancel: runner.DetachOnCancel}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	launcher := worker.NewDockerLauncher(dw)
	classes, err := launcher.Classes(context.Background())
	if err != nil || len(classes) != 1 {
		t.Fatalf("Classes() = %v, %v", classes, err)
	}

	h := newHarness(t)
	useDocker := func(c *worker.Config) { c.Launchers = []worker.Launcher{launcher} }
	_, stop := h.start(h.config(useDocker))
	_, name := h.router.Offer("acme/app", classes[0].String(), false)
	t.Cleanup(func() { _ = dockerCLI(host, "rm", "-f", name).Run() })
	h.router.WaitPhase(t, name, phaseRunning, time.Minute)

	if err := stop(); err != nil {
		t.Fatal(err)
	}
	out, err := dockerCLI(host, "inspect", "-f", "{{.State.Running}}", name).Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		t.Fatalf("after the worker stopped: running=%q err=%v, want the job still running", out, err)
	}

	h.start(h.config(useDocker))
	h.router.WaitPhase(t, name, phaseExited, time.Minute)
	if rep := reportFor(h.router, name, phaseExited); rep.GetExitCode() != 0 {
		t.Fatalf("exit report = %v, want exit code 0", rep)
	}
	if err := dockerCLI(host, "inspect", name).Run(); err == nil {
		t.Fatal("the container still exists after its runner exited")
	}
}

// buildJobImage builds a throwaway job image whose gso-job entrypoint stands
// in for the GitHub runner: it lives long enough to be detached from.
func buildJobImage(t *testing.T, host string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nsleep 6\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "gso-job"), []byte(script), 0o755); err != nil { //nolint:gosec // must be executable in the image
		t.Fatal(err)
	}
	dockerfile := "FROM busybox:1.36\nCOPY gso-job /usr/local/bin/gso-job\nRUN chmod 755 /usr/local/bin/gso-job\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil { //nolint:gosec // test file
		t.Fatal(err)
	}
	const tag = "gso-worker-it:latest"
	if out, err := dockerCLI(host, "build", "-q", "-t", tag, dir).CombinedOutput(); err != nil {
		t.Fatalf("docker build: %v\n%s", err, out)
	}
	return tag
}

func dockerCLI(host string, args ...string) *exec.Cmd {
	cmd := exec.Command("docker", args...)
	if host != "" {
		cmd.Env = append(os.Environ(), "DOCKER_HOST="+host)
	}
	return cmd
}
