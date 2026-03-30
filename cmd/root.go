package cmd

import (
	"os"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
	"github.com/spf13/cobra"
)

var (
	cfgFile          string
	remoteAddr       string
	trustFingerprint string
	rootCmd          = &cobra.Command{
		Use:   "gso",
		Short: "Multi-repo GitHub Actions self-hosted runner orchestrator",
	}
)

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file path")
	rootCmd.PersistentFlags().StringVar(&remoteAddr, "remote", "", "connect to remote daemon at host:port")
	rootCmd.PersistentFlags().StringVar(&trustFingerprint, "trust-fingerprint", "", "pre-trust a server TLS fingerprint (sha256:...)")
}

// connectClient creates a control client with the appropriate TLS options.
func connectClient(addr string) (*control.Client, error) {
	var opts []control.ClientOption
	if trustFingerprint != "" {
		opts = append(opts, control.WithTrustFingerprint(trustFingerprint))
	}
	return control.Connect(addr, opts...)
}
