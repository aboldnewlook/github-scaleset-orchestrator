package cmd

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/service"
	"github.com/spf13/cobra"
)

var scalesetCmd = &cobra.Command{
	Use:   "scaleset",
	Short: "Scale set management",
}

var scalesetListCmd = &cobra.Command{
	Use:   "list",
	Short: "List scale sets with stats",
	RunE:  runScalesetList,
}

var scalesetInspectCmd = &cobra.Command{
	Use:   "inspect <id>",
	Short: "Detailed view of a scale set",
	Args:  cobra.ExactArgs(1),
	RunE:  runScalesetInspect,
}

var scalesetDeleteCmd = &cobra.Command{
	Use:   "delete <id>",
	Short: "Delete a scale set",
	Args:  cobra.ExactArgs(1),
	RunE:  runScalesetDelete,
}

var (
	scalesetListRepo   string
	scalesetDeleteRepo string
	scalesetForce      bool
)

func init() {
	rootCmd.AddCommand(scalesetCmd)
	scalesetCmd.AddCommand(scalesetListCmd)
	scalesetCmd.AddCommand(scalesetInspectCmd)
	scalesetCmd.AddCommand(scalesetDeleteCmd)

	scalesetListCmd.Flags().StringVarP(&scalesetListRepo, "repo", "r", "", "filter by repo (owner/name)")
	scalesetDeleteCmd.Flags().StringVarP(&scalesetDeleteRepo, "repo", "r", "", "repo for the scale set (required)")
	scalesetDeleteCmd.Flags().BoolVar(&scalesetForce, "force", false, "skip confirmation prompt")
}

func runScalesetList(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	q := service.NewQuery(cfg, logger)

	scaleSets, err := q.ScaleSets(context.Background(), scalesetListRepo)
	if err != nil {
		return fmt.Errorf("listing scale sets: %w", err)
	}

	if len(scaleSets) == 0 {
		fmt.Println("No scale sets found")
		return nil
	}

	headers := []string{"ID", "NAME", "REPO", "RUNNERS", "BUSY", "JOBS"}
	var rows [][]string
	for _, ss := range scaleSets {
		runners, busy, jobs := "-", "-", "-"
		if ss.Statistics != nil {
			runners = fmt.Sprintf("%d", ss.Statistics.TotalRegisteredRunners)
			busy = fmt.Sprintf("%d", ss.Statistics.TotalBusyRunners)
			jobs = fmt.Sprintf("%d", ss.Statistics.TotalAssignedJobs)
		}
		rows = append(rows, []string{
			fmt.Sprintf("%d", ss.ID),
			ss.Name,
			ss.Repo,
			runners,
			busy,
			jobs,
		})
	}
	return printTable(headers, rows)
}

func runScalesetInspect(cmd *cobra.Command, args []string) error {
	id, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Errorf("invalid scale set ID: %w", err)
	}

	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	q := service.NewQuery(cfg, logger)

	scaleSets, err := q.ScaleSets(context.Background(), "")
	if err != nil {
		return fmt.Errorf("querying scale sets: %w", err)
	}

	for _, ss := range scaleSets {
		if ss.ID == id {
			fmt.Printf("ID:            %d\n", ss.ID)
			fmt.Printf("Name:          %s\n", ss.Name)
			fmt.Printf("Repo:          %s\n", ss.Repo)
			fmt.Printf("Runner Group:  %d\n", ss.RunnerGroupID)
			fmt.Printf("Created:       %s\n", ss.CreatedOn.Format("2006-01-02 15:04:05"))

			labels := make([]string, len(ss.Labels))
			for i, l := range ss.Labels {
				labels[i] = l.Name
			}
			fmt.Printf("Labels:        %s\n", strings.Join(labels, ", "))

			if ss.Statistics != nil {
				s := ss.Statistics
				fmt.Printf("\nStatistics:\n")
				fmt.Printf("  Registered Runners: %d\n", s.TotalRegisteredRunners)
				fmt.Printf("  Busy Runners:       %d\n", s.TotalBusyRunners)
				fmt.Printf("  Idle Runners:       %d\n", s.TotalIdleRunners)
				fmt.Printf("  Available Jobs:     %d\n", s.TotalAvailableJobs)
				fmt.Printf("  Assigned Jobs:      %d\n", s.TotalAssignedJobs)
				fmt.Printf("  Running Jobs:       %d\n", s.TotalRunningJobs)
			}
			return nil
		}
	}

	return fmt.Errorf("scale set %d not found", id)
}

func runScalesetDelete(cmd *cobra.Command, args []string) error {
	id, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Errorf("invalid scale set ID: %w", err)
	}

	if scalesetDeleteRepo == "" {
		return fmt.Errorf("--repo is required for scaleset delete")
	}

	if !scalesetForce {
		fmt.Printf("Delete scale set %d from %s? [y/N] ", id, scalesetDeleteRepo)
		reader := bufio.NewReader(os.Stdin)
		answer, _ := reader.ReadString('\n')
		answer = strings.TrimSpace(strings.ToLower(answer))
		if answer != "y" && answer != "yes" {
			fmt.Println("Cancelled")
			return nil
		}
	}

	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	q := service.NewQuery(cfg, logger)

	if err := q.DeleteScaleSet(context.Background(), scalesetDeleteRepo, id); err != nil {
		return fmt.Errorf("deleting scale set: %w", err)
	}

	fmt.Printf("Scale set %d deleted\n", id)
	return nil
}
