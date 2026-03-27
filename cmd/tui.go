package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/event"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/tui"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

var tuiCmd = &cobra.Command{
	Use:   "tui",
	Short: "Attach to the running daemon with a live dashboard",
	RunE:  runTUI,
}

func init() {
	rootCmd.AddCommand(tuiCmd)
}

func runTUI(cmd *cobra.Command, args []string) error {
	// Verify daemon is running before launching the TUI
	client, err := control.NewClient()
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w\nStart the daemon first with: gso start", err)
	}
	client.Close()

	// Set up event store for reading history
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		cacheDir = "/tmp"
	}
	storePath := filepath.Join(cacheDir, "gso", "events.jsonl")
	store := event.NewFileStore(storePath)

	model := tui.New(store)
	p := tea.NewProgram(model, tea.WithAltScreen())

	if _, err := p.Run(); err != nil {
		return fmt.Errorf("TUI error: %w", err)
	}

	return nil
}
