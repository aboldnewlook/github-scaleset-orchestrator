package cmd

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
)

// printTable prints aligned tabular output using tabwriter.
func printTable(headers []string, rows [][]string) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, strings.Join(headers, "\t"))
	for _, row := range rows {
		_, _ = fmt.Fprintln(w, strings.Join(row, "\t"))
	}
	return w.Flush()
}
