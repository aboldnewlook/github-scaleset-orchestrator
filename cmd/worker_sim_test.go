package cmd

import (
	"bytes"
	"context"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

func TestSimLauncherAdvertisesLinuxDocker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the sim executor needs process groups")
	}
	l, err := simLauncher(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if l.Kind() != runner.ExecutorDocker {
		t.Fatalf("Kind = %s, want docker (the sim executor stands in for it)", l.Kind())
	}
	classes, err := l.Classes(context.Background())
	if err != nil || len(classes) != 1 || classes[0].OS != "linux" {
		t.Fatalf("Classes = %v, %v", classes, err)
	}
}

func TestTimingScaleEnvIsForwarded(t *testing.T) {
	t.Setenv("GSO_TIMING_SCALE", "")
	if env := timingScaleEnv(); env != nil {
		t.Fatalf("env = %v, want nil when unset", env)
	}
	t.Setenv("GSO_TIMING_SCALE", "0.02")
	if env := timingScaleEnv(); len(env) != 1 || env[0] != "GSO_TIMING_SCALE=0.02" {
		t.Fatalf("env = %v", env)
	}
}

func TestResolveWorkerSettingsSimExecutor(t *testing.T) {
	f := baseWorkerFlags()
	f.executor = "sim"
	f.githubSim = "http://sim.lan:9300"
	s, err := resolveWorkerSettings(f, changedFlags("github-sim"), envOf(nil), fixedHostname("box"), fixedStateDir("/cfg/gso"))
	if err != nil {
		t.Fatalf("sim executor without --image: %v", err)
	}
	if s.Executor != runner.ExecutorDocker || s.NewLauncher == nil {
		t.Fatalf("Executor = %q, NewLauncher nil = %v", s.Executor, s.NewLauncher == nil)
	}
}

// Ruling 2: TrustedHosts replaces the GitHub defaults, so sim mode must list
// both github.com and the simulator's host.
func TestResolveWorkerSettingsTrustsGitHubAndTheSimHost(t *testing.T) {
	tests := []struct {
		name     string
		executor string
		flag     string
		env      map[string]string
		explicit []string
		want     []string
	}{
		{"flag", "sim", "http://sim.lan:9300", nil, nil, []string{"github.com", "sim.lan"}},
		{"env", "sim", "", map[string]string{"GSO_GITHUB_SIM": "http://10.0.0.5:9300/"}, nil, []string{"github.com", "10.0.0.5"}},
		{"docker executor too", "docker", "http://sim.lan:9300", nil, nil, []string{"github.com", "sim.lan"}},
		{"explicit hosts are kept", "sim", "http://sim.lan:9300", nil, []string{"extra.lan", "sim.lan"}, []string{"github.com", "sim.lan", "extra.lan"}},
		{"explicit host stands in for the URL", "sim", "", nil, []string{"127.0.0.1"}, []string{"github.com", "127.0.0.1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := baseWorkerFlags()
			f.executor, f.image, f.githubSim, f.trustedHosts = tt.executor, "img", tt.flag, tt.explicit
			changed := changedFlags("image")
			if tt.flag != "" {
				changed = changedFlags("image", "github-sim")
			}
			s, err := resolveWorkerSettings(f, changed, envOf(tt.env), fixedHostname("box"), fixedStateDir("/cfg/gso"))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(s.TrustedHosts, tt.want) {
				t.Fatalf("TrustedHosts = %v, want %v", s.TrustedHosts, tt.want)
			}
		})
	}
}

func TestResolveWorkerSettingsSimErrors(t *testing.T) {
	f := baseWorkerFlags()
	f.executor = "sim"
	if _, err := resolveWorkerSettings(f, changedFlags(), envOf(nil), fixedHostname("box"), fixedStateDir("/cfg/gso")); err == nil || !strings.Contains(err.Error(), "--github-sim") {
		t.Fatalf("sim executor without a simulator URL: error = %v", err)
	}
	f.githubSim = "not a url"
	if _, err := resolveWorkerSettings(f, changedFlags("github-sim"), envOf(nil), fixedHostname("box"), fixedStateDir("/cfg/gso")); err == nil || !strings.Contains(err.Error(), "--github-sim") {
		t.Fatalf("bad simulator URL: error = %v", err)
	}
}

func TestWorkerBackoffFollowsTimingScale(t *testing.T) {
	min, max := workerBackoff()
	if min <= 0 || max < min {
		t.Fatalf("workerBackoff() = %s, %s", min, max)
	}
}

// GSO_GITHUB_SIM only counts for --executor sim: a docker worker must not
// widen its trusted hosts because of an ambient variable (security review).
func TestSimEnvIsIgnoredByTheDockerExecutor(t *testing.T) {
	f := baseWorkerFlags()
	f.image = "img"
	env := envOf(map[string]string{"GSO_GITHUB_SIM": "http://sim.lan:9300"})
	s, err := resolveWorkerSettings(f, changedFlags("image"), env, fixedHostname("box"), fixedStateDir("/cfg/gso"))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.TrustedHosts) != 0 {
		t.Fatalf("TrustedHosts = %v, want none: the env var must be ignored with the docker executor", s.TrustedHosts)
	}
}

func TestSimExecutorIsRefusedOnWindows(t *testing.T) {
	old := workerGOOS
	workerGOOS = "windows"
	t.Cleanup(func() { workerGOOS = old })
	f := baseWorkerFlags()
	f.executor, f.githubSim = "sim", "http://sim.lan:9300"
	_, err := resolveWorkerSettings(f, changedFlags("github-sim"), envOf(nil), fixedHostname("box"), fixedStateDir("/cfg/gso"))
	if err == nil || !strings.Contains(err.Error(), "Windows") {
		t.Fatalf("error = %v, want a Windows refusal", err)
	}
}

func TestLogTrustedHostsWarnsWhenNonDefault(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logTrustedHosts(logger, workerSettings{})
	if buf.Len() != 0 {
		t.Fatalf("default trust logged: %s", buf.String())
	}
	logTrustedHosts(logger, workerSettings{TrustedHosts: []string{"github.com", "sim.lan"}, GitHubSim: "http://sim.lan:9300"})
	out := buf.String()
	for _, want := range []string{"level=WARN", "sim.lan", "http://sim.lan:9300", "github.com"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q lacks %q", out, want)
		}
	}
}
