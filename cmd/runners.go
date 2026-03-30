package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/service"
	"github.com/spf13/cobra"
)

var runnerCmd = &cobra.Command{
	Use:   "runner",
	Short: "Runner management",
}

var runnerListCmd = &cobra.Command{
	Use:   "list",
	Short: "List runners",
	RunE:  runRunnerList,
}

var runnerRecycleCmd = &cobra.Command{
	Use:   "recycle <name>",
	Short: "Recycle a runner via control socket",
	Args:  cobra.ExactArgs(1),
	RunE:  runRunnerRecycle,
}

var runnerRemoveCmd = &cobra.Command{
	Use:   "remove <id>",
	Short: "Remove a runner via API",
	Args:  cobra.ExactArgs(1),
	RunE:  runRunnerRemove,
}

var (
	runnerListRepo   string
	runnerRemoveRepo string
)

func init() {
	rootCmd.AddCommand(runnerCmd)
	runnerCmd.AddCommand(runnerListCmd)
	runnerCmd.AddCommand(runnerRecycleCmd)
	runnerCmd.AddCommand(runnerRemoveCmd)

	runnerListCmd.Flags().StringVarP(&runnerListRepo, "repo", "r", "", "filter by repo (owner/name)")
	runnerRemoveCmd.Flags().StringVarP(&runnerRemoveRepo, "repo", "r", "", "repo for the runner (required)")
}

func runRunnerList(cmd *cobra.Command, args []string) error {
	// Try live status from daemon first
	client, err := connectClient(remoteAddr)
	if err == nil {
		defer func() { _ = client.Close() }()
		result, err := client.Call(context.Background(), control.MethodLiveStatus, nil)
		if err == nil {
			var live control.LiveStatusResult
			if json.Unmarshal(result, &live) == nil {
				return printLiveRunners(&live, runnerListRepo)
			}
		}
	}

	// Fall back to API — show scale set runner counts
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	q := service.NewQuery(cfg, logger)

	status, err := q.Status(context.Background(), runnerListRepo)
	if err != nil {
		return fmt.Errorf("querying status: %w", err)
	}

	headers := []string{"REPO", "REGISTERED", "BUSY", "IDLE"}
	var rows [][]string
	for _, r := range status.Repos {
		if r.Error != "" {
			rows = append(rows, []string{r.Repo, "error", r.Error, ""})
			continue
		}
		if r.Statistics == nil {
			rows = append(rows, []string{r.Repo, "-", "-", "-"})
			continue
		}
		s := r.Statistics
		rows = append(rows, []string{
			r.Repo,
			fmt.Sprintf("%d", s.TotalRegisteredRunners),
			fmt.Sprintf("%d", s.TotalBusyRunners),
			fmt.Sprintf("%d", s.TotalIdleRunners),
		})
	}
	return printTable(headers, rows)
}

func printLiveRunners(live *control.LiveStatusResult, repoFilter string) error {
	headers := []string{"REPO", "RUNNER"}
	var rows [][]string
	for _, r := range live.Repos {
		if repoFilter != "" && r.Repo != repoFilter {
			continue
		}
		if len(r.Runners) == 0 {
			rows = append(rows, []string{r.Repo, "(none)"})
			continue
		}
		for _, name := range r.Runners {
			rows = append(rows, []string{r.Repo, name})
		}
	}
	return printTable(headers, rows)
}

func runRunnerRecycle(cmd *cobra.Command, args []string) error {
	name := args[0]

	client, err := connectClient(remoteAddr)
	if err != nil {
		return fmt.Errorf("%w (is the daemon running?)", err)
	}
	defer func() { _ = client.Close() }()

	_, err = client.Call(context.Background(), control.MethodRecycleRunner, control.RecycleRunnerParams{
		Name: name,
	})
	if err != nil {
		return fmt.Errorf("recycling runner: %w", err)
	}

	fmt.Printf("Runner %s recycled\n", name)
	return nil
}

func runRunnerRemove(cmd *cobra.Command, args []string) error {
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid runner ID: %w", err)
	}

	if runnerRemoveRepo == "" {
		return fmt.Errorf("--repo is required for runner remove")
	}

	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	q := service.NewQuery(cfg, logger)

	if err := q.RemoveRunner(context.Background(), runnerRemoveRepo, id); err != nil {
		return fmt.Errorf("removing runner: %w", err)
	}

	fmt.Printf("Runner %d removed\n", id)
	return nil
}
