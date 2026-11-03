package ghsim

import (
	"context"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestJobImageRunsASimulatorJIT builds the gso image and runs its job
// entrypoint with a simulator JIT config: gso-job must hand it to the fake
// runner, which claims and completes the job from inside the container.
func TestJobImageRunsASimulatorJIT(t *testing.T) {
	if os.Getenv("GSO_DOCKER_IT") != "1" {
		t.Skip("set GSO_DOCKER_IT=1 to run docker-backed tests")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	tag := fmt.Sprintf("gso:simtest-%d", time.Now().UnixNano())
	build := exec.Command("docker", "build", "-t", tag, filepath.Join("..", ".."))
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("docker build: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", tag).Run() })

	host := os.Getenv("GSO_E2E_HOST_ADDR") // the host as seen from a container
	if host == "" {
		host = "host.docker.internal"
	}
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	m := NewModel(Options{PollWait: 50 * time.Millisecond, ClaimTimeout: time.Minute, ServerURL: fmt.Sprintf("http://%s:%d", host, port)})
	srv := httptest.NewUnstartedServer(NewServer(m, nil))
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	defer srv.Close()

	c, ss, s := openSet(t, m, "o/a", "set", defaultLabels)
	job, _ := m.InjectJob(JobSpec{Repo: "o/a", Duration: 2 * time.Second})
	poll(t, s, 1)
	cfg, err := c.GenerateJitRunnerConfig(context.Background(), &scalesetJitSetting{Name: "gso-dock-a-img001"}, ss.ID)
	if err != nil {
		t.Fatal(err)
	}
	run := exec.Command("docker", "run", "--rm",
		"--add-host", "host.docker.internal:host-gateway",
		"--entrypoint", "/usr/local/bin/gso-job",
		"-e", "ACTIONS_RUNNER_INPUT_JITCONFIG", // value from the environment, not argv
		tag)
	run.Env = append(os.Environ(), "ACTIONS_RUNNER_INPUT_JITCONFIG="+cfg.EncodedJITConfig)
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("job container: %v\n%s", err, out)
	}
	if st, _ := m.Job(job.ID); st.State != JobCompleted {
		t.Fatalf("job = %+v, want completed by the containerised fake runner", st)
	}
}
