package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRunnerConfig(t *testing.T, runnerYAML string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "auth:\n  env: GITHUB_TOKEN\nmax_runners: 1\nrepos:\n  - name: org/repo\nrunner:\n" + runnerYAML
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const dockerRunnerYAML = "  executor: docker\n  image: example/job:1\n  instance_id: test\n"

func TestLoadRunnerDockerHostFromFileAndEnv(t *testing.T) {
	path := writeRunnerConfig(t, dockerRunnerYAML+"  docker_host: unix:///Users/me/.docker/run/docker.sock\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Runner.DockerHost != "unix:///Users/me/.docker/run/docker.sock" {
		t.Fatalf("DockerHost = %q", cfg.Runner.DockerHost)
	}
	t.Setenv("GSO_RUNNER_DOCKER_HOST", "unix:///run/user/1000/docker.sock")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load() with env error: %v", err)
	}
	if cfg.Runner.DockerHost != "unix:///run/user/1000/docker.sock" {
		t.Fatalf("env DockerHost = %q", cfg.Runner.DockerHost)
	}
}

func TestLoadDockerHostDefaultsToEmpty(t *testing.T) {
	cfg, err := Load(writeRunnerConfig(t, dockerRunnerYAML))
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Runner.DockerHost != "" {
		t.Fatalf("DockerHost = %q, want empty (runner.DefaultDockerHost applies)", cfg.Runner.DockerHost)
	}
}

func TestLoadRejectsNonUnixDockerHost(t *testing.T) {
	_, err := Load(writeRunnerConfig(t, dockerRunnerYAML+"  docker_host: tcp://127.0.0.1:2375\n"))
	if err == nil || !strings.Contains(err.Error(), "runner.docker_host") {
		t.Fatalf("Load() error = %v, want a runner.docker_host error", err)
	}
}
