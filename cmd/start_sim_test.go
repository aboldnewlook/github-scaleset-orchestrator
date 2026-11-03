package cmd

import (
	"os"
	"testing"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/router"
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

// The router takes the simulator's factory (it used to refuse one until the
// orchestrator could honour it). Constructing it dials nothing.
func TestRouterAcceptsTheSimulatorFactory(t *testing.T) {
	cfg := &config.Config{GitHubSim: "http://sim:9300", Repos: []config.Repo{{Name: "o/a"}}}
	r, err := router.New(cfg, nil, nil, router.Options{GitHub: githubFactoryFor(cfg), StateDir: t.TempDir()})
	if err != nil || r == nil {
		t.Fatalf("router.New with the simulator factory = %v, %v", r, err)
	}
}
