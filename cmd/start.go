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
	tlsCert    string
	tlsKey     string
	allowCIDRs []string
	startCmd   = &cobra.Command{
		Use:   "start",
		Short: "Start the orchestrator daemon",
		RunE:  runStart,
	}
)

func init() {
	rootCmd.AddCommand(startCmd)
	startCmd.Flags().StringVar(&listenAddr, "listen", "", "enable TCP control listener on address (e.g. :9100), requires GSO_CONTROL_TOKEN")
	startCmd.Flags().StringVar(&tlsCert, "tls-cert", "", "path to TLS certificate PEM (overrides auto-generated)")
	startCmd.Flags().StringVar(&tlsKey, "tls-key", "", "path to TLS private key PEM (overrides auto-generated)")
	startCmd.Flags().StringSliceVar(&allowCIDRs, "allow-cidr", nil, "allowed CIDR for TCP connections (repeatable)")
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
	runtime := service.NewRuntime(orch, cancel, logger, store)
	socketPath := control.SocketPath()

	var serverOpts []control.ServerOption

	// Merge CLI flags with config (CLI wins).
	tcpAddr := cfg.Control.Listen
	if listenAddr != "" {
		tcpAddr = listenAddr
	}
	if tcpAddr != "" {
		serverOpts = append(serverOpts, control.WithTCPAddr(tcpAddr))
	}

	// TLS cert: CLI flags override config.
	certPath := cfg.Control.TLSCert
	keyPath := cfg.Control.TLSKey
	if tlsCert != "" {
		certPath = tlsCert
	}
	if tlsKey != "" {
		keyPath = tlsKey
	}
	if certPath != "" && keyPath != "" {
		tlsCfg, fp, err := control.LoadOrGenerateTLSConfig(certPath, keyPath, logger)
		if err != nil {
			return fmt.Errorf("loading TLS cert: %w", err)
		}
		serverOpts = append(serverOpts, control.WithTLSConfig(tlsCfg, fp))
	}

	// IP allowlist: CLI flags override config.
	cidrs := cfg.Control.AllowCIDRs
	if len(allowCIDRs) > 0 {
		cidrs = allowCIDRs
	}
	if len(cidrs) > 0 {
		serverOpts = append(serverOpts, control.WithAllowCIDRs(cidrs))
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
