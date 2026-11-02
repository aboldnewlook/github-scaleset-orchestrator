package cmd

import (
	"os"
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
