package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/event"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/orchestrator"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/service"
	"github.com/spf13/cobra"
)

var (
	listenAddr string
	startCmd   = &cobra.Command{
		Use:   "start",
		Short: "Start the orchestrator daemon",
		RunE:  runStart,
	}
)

func init() {
	rootCmd.AddCommand(startCmd)
	startCmd.Flags().StringVar(&listenAddr, "listen", "", "TCP address to listen on (e.g. :9100)")
}

func runStart(cmd *cobra.Command, args []string) error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	logger.Info("starting gso",
		"repos", len(cfg.Repos),
		"max_runners", cfg.MaxRunners,
	)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Event bus with file store
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		cacheDir = "/tmp"
	}
	eventsDir := filepath.Join(cacheDir, "gso")
	if err := os.MkdirAll(eventsDir, 0o755); err != nil {
		return fmt.Errorf("creating events directory: %w", err)
	}
	store := event.NewFileStore(filepath.Join(eventsDir, "events.jsonl"))
	bus := event.NewBus(store)

	orch := orchestrator.New(cfg, logger, bus)

	// Control socket server
	runtime := service.NewRuntime(orch, cancel, logger)
	socketPath := control.SocketPath()

	var serverOpts []control.ServerOption
	if listenAddr != "" {
		serverOpts = append(serverOpts, control.WithTCPAddr(listenAddr))
	}
	ctrlServer := control.NewServer(socketPath, runtime, logger, serverOpts...)

	go func() {
		if err := ctrlServer.Start(ctx); err != nil {
			logger.Error("control server error", "error", err)
		}
	}()
	defer func() { _ = ctrlServer.Stop() }()

	if err := orch.Run(ctx); err != nil {
		if ctx.Err() != nil {
			logger.Info("shutting down gracefully")
			return nil
		}
		return fmt.Errorf("orchestrator error: %w", err)
	}

	return nil
}
