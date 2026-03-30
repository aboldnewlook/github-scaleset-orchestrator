package cmd

import (
	"fmt"
	"os"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var tokenCmd = &cobra.Command{
	Use:   "token",
	Short: "Token management",
}

var tokenSetCmd = &cobra.Command{
	Use:   "set <account>",
	Short: "Store a token in the OS keychain",
	Args:  cobra.ExactArgs(1),
	RunE:  runTokenSet,
}

var tokenDeleteCmd = &cobra.Command{
	Use:   "delete <account>",
	Short: "Remove a token from the OS keychain",
	Args:  cobra.ExactArgs(1),
	RunE:  runTokenDelete,
}

func init() {
	rootCmd.AddCommand(tokenCmd)
	tokenCmd.AddCommand(tokenSetCmd)
	tokenCmd.AddCommand(tokenDeleteCmd)
}

func runTokenSet(cmd *cobra.Command, args []string) error {
	account := args[0]

	fmt.Print("Enter token: ")
	raw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println() // ReadPassword does not emit a newline
	if err != nil {
		return fmt.Errorf("reading token: %w", err)
	}
	token := string(raw)
	if token == "" {
		return fmt.Errorf("token cannot be empty")
	}

	if err := config.StoreInKeychain(account, token); err != nil {
		return fmt.Errorf("storing token: %w", err)
	}
	fmt.Printf("Token stored in keychain for account %q\n", account)
	return nil
}

func runTokenDelete(cmd *cobra.Command, args []string) error {
	account := args[0]

	if err := config.DeleteFromKeychain(account); err != nil {
		return fmt.Errorf("deleting token: %w", err)
	}
	fmt.Printf("Token removed from keychain for account %q\n", account)
	return nil
}
