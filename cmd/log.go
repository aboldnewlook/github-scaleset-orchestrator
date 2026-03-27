package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/event"
	"github.com/spf13/cobra"
)

var logCmd = &cobra.Command{
	Use:   "log",
	Short: "Show event history",
	RunE:  runLog,
}

var (
	logSince string
	logType  string
	logRepo  string
)

func init() {
	rootCmd.AddCommand(logCmd)
	logCmd.Flags().StringVar(&logSince, "since", "1h", "show events since duration (e.g. 1h, 30m, 24h)")
	logCmd.Flags().StringVar(&logType, "type", "", "filter by event type (e.g. job.completed)")
	logCmd.Flags().StringVarP(&logRepo, "repo", "r", "", "filter by repo (owner/name)")
}

func runLog(cmd *cobra.Command, args []string) error {
	since, err := time.ParseDuration(logSince)
	if err != nil {
		return fmt.Errorf("invalid --since duration: %w", err)
	}

	cacheDir, err := os.UserCacheDir()
	if err != nil {
		cacheDir = "/tmp"
	}
	storePath := filepath.Join(cacheDir, "gso", "events.jsonl")

	store := event.NewFileStore(storePath)

	filter := event.StoreFilter{
		Since: time.Now().Add(-since),
		Type:  event.EventType(logType),
		Repo:  logRepo,
	}

	events, err := store.Query(filter)
	if err != nil {
		return fmt.Errorf("reading events: %w", err)
	}

	if len(events) == 0 {
		fmt.Println("No events found")
		return nil
	}

	for _, e := range events {
		repo := e.Repo
		if repo == "" {
			repo = "-"
		}
		fmt.Printf("%s  %-20s  %-30s  %s\n",
			e.Time.Format("15:04:05"),
			e.Type,
			repo,
			string(e.Payload),
		)
	}

	return nil
}
