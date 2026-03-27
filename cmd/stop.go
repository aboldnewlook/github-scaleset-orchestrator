package cmd

import (
	"context"
	"fmt"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
	"github.com/spf13/cobra"
)

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop running daemon",
	RunE:  runStop,
}

func init() {
	rootCmd.AddCommand(stopCmd)
}

func runStop(cmd *cobra.Command, args []string) error {
	client, err := control.NewClient()
	if err != nil {
		return fmt.Errorf("%w (is the daemon running?)", err)
	}
	defer func() { _ = client.Close() }()

	_, err = client.Call(context.Background(), control.MethodShutdown, nil)
	if err != nil {
		return fmt.Errorf("sending shutdown: %w", err)
	}

	fmt.Println("Shutdown signal sent")
	return nil
}
