package cmd

import (
	"fmt"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/buildinfo"
	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version and build info",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println(buildinfo.String())
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
