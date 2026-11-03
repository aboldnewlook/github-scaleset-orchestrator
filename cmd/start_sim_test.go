package cmd

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
)

func TestGitHubFactoryFor(t *testing.T) {
	if f := githubFactoryFor(&config.Config{}); f != nil {
		t.Fatal("a real-GitHub config got the simulator factory")
	}
	if f := githubFactoryFor(&config.Config{GitHubSim: "http://sim:9300"}); f == nil {
		t.Fatal("a simulator config got no factory")
	}
}

func TestApplyGitHubSimFlag(t *testing.T) {
	t.Setenv("GSO_GITHUB_SIM", "")
	if err := applyGitHubSimFlag(""); err != nil || os.Getenv("GSO_GITHUB_SIM") != "" {
		t.Fatal("an empty flag changed the environment")
	}
	if err := applyGitHubSimFlag("http://sim:9300"); err != nil || os.Getenv("GSO_GITHUB_SIM") != "http://sim:9300" {
		t.Fatalf("GSO_GITHUB_SIM = %q", os.Getenv("GSO_GITHUB_SIM"))
	}
}

// `gso start --github-sim` must fail closed until the orchestrator can use the
// simulator: it exits non-zero, never reads a token and never dials the
// simulator or GitHub.
func TestStartWithGitHubSimRefusesToRun(t *testing.T) {
	var hits atomic.Int32
	sim := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(sim.Close)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("GSO_GITHUB_SIM", "") // restored on cleanup; the flag handler sets it
	cfgPath := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("repos:\n  - name: o/a\nmax_runners: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rootCmd.SetArgs([]string{"start", "--config", cfgPath, "--github-sim", sim.URL})
	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	t.Cleanup(func() {
		githubSim, cfgFile = "", ""
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	})
	err := rootCmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "refusing to start against real GitHub") {
		t.Fatalf("gso start --github-sim: error = %v, want a refusal", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the simulator was contacted %d times", n)
	}
}
