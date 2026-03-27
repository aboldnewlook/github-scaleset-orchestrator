package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/service"
	"github.com/spf13/cobra"
)

var healthCmd = &cobra.Command{
	Use:   "health",
	Short: "Health check",
	RunE:  runHealth,
}

func init() {
	rootCmd.AddCommand(healthCmd)
}

func runHealth(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	q := service.NewQuery(cfg, logger)

	result, err := q.Health(context.Background())
	if err != nil {
		return fmt.Errorf("health check: %w", err)
	}

	// Runner binary
	fmt.Println("Runner Binary:")
	if result.RunnerBinary.Cached {
		fmt.Printf("  Cached:  yes (v%s)\n", result.RunnerBinary.Version)
		fmt.Printf("  Path:    %s\n", result.RunnerBinary.Path)
	} else {
		fmt.Println("  Cached:  no (will download on first start)")
	}

	// Repos
	fmt.Printf("\nRepositories (%d):\n", len(result.Repos))
	allHealthy := true
	for _, r := range result.Repos {
		status := "OK"
		if r.Error != "" {
			status = r.Error
			allHealthy = false
		} else if !r.TokenValid {
			status = "token invalid"
			allHealthy = false
		} else if !r.APIReachable {
			status = "API unreachable"
			allHealthy = false
		}

		icon := "+"
		if status != "OK" {
			icon = "!"
		}
		fmt.Printf("  %s %-35s %s\n", icon, r.Repo, status)
	}

	if !allHealthy {
		return fmt.Errorf("some health checks failed")
	}

	fmt.Println("\nAll checks passed")
	return nil
}
