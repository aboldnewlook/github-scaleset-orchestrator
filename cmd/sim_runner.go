package cmd

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghsim"
)

const simJITEnv = "ACTIONS_RUNNER_INPUT_JITCONFIG"

var (
	simRunnerCmd = &cobra.Command{
		Use:   "runner",
		Short: "Fake actions runner: runs one simulated job from the JIT config in " + simJITEnv,
		Args:  cobra.NoArgs,
		RunE:  runSimRunner,
	}
	simIsSimJITCmd = &cobra.Command{
		Use:           "is-sim-jit",
		Short:         "Exit 0 when " + simJITEnv + " holds a simulator JIT config (used by gso-job)",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runIsSimJIT,
	}
	errNotSimJIT = errors.New("not a simulator JIT config")
)

func init() {
	simCmd.AddCommand(simRunnerCmd, simIsSimJITCmd)
}

func runSimRunner(_ *cobra.Command, _ []string) error {
	jit := os.Getenv(simJITEnv)
	if jit == "" {
		return errors.New(simJITEnv + " is not set")
	}
	_ = os.Unsetenv(simJITEnv)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return ghsim.RunFakeRunner(ctx, jit, logger)
}

func runIsSimJIT(_ *cobra.Command, _ []string) error {
	if _, ok, err := ghsim.DecodeSimJIT(os.Getenv(simJITEnv)); err != nil || !ok {
		return errNotSimJIT
	}
	return nil
}
