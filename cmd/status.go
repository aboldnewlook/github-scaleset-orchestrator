package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/service"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show status dashboard",
	RunE:  runStatus,
}

var statusRepo string

func init() {
	rootCmd.AddCommand(statusCmd)
	statusCmd.Flags().StringVarP(&statusRepo, "repo", "r", "", "filter by repo")
}

func runStatus(cmd *cobra.Command, args []string) error {
	// Try live status from daemon first
	client, err := control.Connect(remoteAddr)
	if err == nil {
		defer func() { _ = client.Close() }()
		result, err := client.Call(context.Background(), control.MethodLiveStatus, nil)
		if err == nil {
			var live control.LiveStatusResult
			if json.Unmarshal(result, &live) == nil {
				return printLiveStatus(&live)
			}
		}
	}

	// Fall back to API query
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	q := service.NewQuery(cfg, logger)

	status, err := q.Status(context.Background(), statusRepo)
	if err != nil {
		return fmt.Errorf("querying status: %w", err)
	}

	return printAPIStatus(status)
}

func printLiveStatus(live *control.LiveStatusResult) error {
	fmt.Printf("Daemon running (max %d runners, %d available)\n\n", live.MaxRunners, live.Available)

	headers := []string{"REPO", "ACTIVE RUNNERS"}
	var rows [][]string
	for _, r := range live.Repos {
		rows = append(rows, []string{
			r.Repo,
			fmt.Sprintf("%d", len(r.Runners)),
		})
	}
	return printTable(headers, rows)
}

func printAPIStatus(status *service.StatusResult) error {
	fmt.Printf("Daemon not running (querying API, max %d runners)\n\n", status.MaxRunners)

	headers := []string{"REPO", "SCALE SET", "RUNNING", "ASSIGNED", "REGISTERED"}
	var rows [][]string
	for _, r := range status.Repos {
		if r.Error != "" {
			rows = append(rows, []string{r.Repo, "error", r.Error, "", ""})
			continue
		}
		if r.Statistics == nil {
			rows = append(rows, []string{r.Repo, fmt.Sprintf("#%d", r.ScaleSetID), "-", "-", "-"})
			continue
		}
		s := r.Statistics
		rows = append(rows, []string{
			r.Repo,
			fmt.Sprintf("#%d", r.ScaleSetID),
			fmt.Sprintf("%d", s.TotalRunningJobs),
			fmt.Sprintf("%d", s.TotalAssignedJobs),
			fmt.Sprintf("%d", s.TotalRegisteredRunners),
		})
	}
	return printTable(headers, rows)
}
