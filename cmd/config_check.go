package cmd

import (
	"fmt"
	"strings"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/spf13/cobra"
)

var configCheckCmd = &cobra.Command{
	Use:   "config-check",
	Short: "Validate configuration",
	RunE:  runConfigCheck,
}

func init() {
	rootCmd.AddCommand(configCheckCmd)
}

func runConfigCheck(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("config validation failed: %w", err)
	}

	fmt.Printf("Config file: %s\n", cfgFile)
	fmt.Printf("Max runners: %d\n", cfg.MaxRunners)
	fmt.Printf("Labels:      %s\n", strings.Join(cfg.Labels, ", "))
	fmt.Println()

	// Show global auth with masked tokens
	fmt.Println("Global auth:")
	printTokenSource("  ", cfg.Auth)
	fmt.Println()

	// Show repos
	fmt.Printf("Repos (%d):\n", len(cfg.Repos))
	for _, repo := range cfg.Repos {
		fmt.Printf("  - %s\n", repo.Name)

		// Check if token resolves
		token, err := cfg.TokenForRepo(repo)
		if err != nil {
			fmt.Printf("    token: ERROR: %v\n", err)
		} else {
			fmt.Printf("    token: %s\n", maskToken(token))
		}

		if repo.Token.Env != "" || repo.Token.Keychain != "" || repo.Token.File != "" {
			fmt.Println("    override:")
			printTokenSource("      ", repo.Token)
		}
	}

	fmt.Println()
	fmt.Println("Config OK")
	return nil
}

func printTokenSource(prefix string, ts config.TokenSource) {
	if ts.Env != "" {
		fmt.Printf("%senv: %s\n", prefix, ts.Env)
	}
	if ts.Keychain != "" {
		fmt.Printf("%skeychain: %s\n", prefix, ts.Keychain)
	}
	if ts.File != "" {
		fmt.Printf("%sfile: %s\n", prefix, ts.File)
	}
	if ts.Env == "" && ts.Keychain == "" && ts.File == "" {
		fmt.Printf("%s(none)\n", prefix)
	}
}

func maskToken(token string) string {
	if len(token) <= 8 {
		return strings.Repeat("*", len(token))
	}
	return token[:4] + strings.Repeat("*", len(token)-8) + token[len(token)-4:]
}
