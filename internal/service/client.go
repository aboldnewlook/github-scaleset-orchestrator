package service

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/actions/scaleset"
)

// NewScaleSetClient creates a scaleset.Client for the given repo using PAT
// authentication. Both the orchestrator and the query service use this
// helper to avoid duplicating client-creation logic.
func NewScaleSetClient(_ context.Context, cfg *config.Config, repo config.Repo, logger *slog.Logger) (*scaleset.Client, error) {
	token, err := cfg.TokenForRepo(repo)
	if err != nil {
		return nil, fmt.Errorf("resolving token for %s: %w", repo.Name, err)
	}

	configURL := fmt.Sprintf("https://github.com/%s", repo.Name)

	sysInfo := scaleset.SystemInfo{
		System:    "gso",
		Version:   "0.1.0",
		Subsystem: "client",
	}

	client, err := scaleset.NewClientWithPersonalAccessToken(
		scaleset.NewClientWithPersonalAccessTokenConfig{
			GitHubConfigURL:     configURL,
			PersonalAccessToken: token,
			SystemInfo:          sysInfo,
		},
		scaleset.WithLogger(logger),
	)
	if err != nil {
		return nil, fmt.Errorf("creating scaleset client for %s: %w", repo.Name, err)
	}

	return client, nil
}
