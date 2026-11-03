package cmd

import (
	"os"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghapi"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghsim"
)

// githubFactoryFor returns the simulator's factory when the config points at
// one, and nil (real GitHub) otherwise.
func githubFactoryFor(cfg *config.Config) ghapi.Factory {
	if !cfg.Simulated() {
		return nil
	}
	return ghsim.NewRemoteFactory(cfg.GitHubSim, nil)
}

// applyGitHubSimFlag hands --github-sim to config.Load through GSO_GITHUB_SIM,
// so token validation is skipped while the config is loaded.
func applyGitHubSimFlag(url string) error {
	if url == "" {
		return nil
	}
	return os.Setenv("GSO_GITHUB_SIM", url)
}
