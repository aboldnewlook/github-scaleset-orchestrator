package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
	"github.com/spf13/cobra"
)

var (
	workersCmd = &cobra.Command{
		Use:   "workers",
		Short: "Enroll and manage remote workers (router host only)",
		Long: "Enroll and manage remote workers. These commands talk to the router over its\n" +
			"local unix socket only; run them on the router host, e.g.\n" +
			"  docker exec gso-router gso workers add unraid --classes linux-x64 --slots 2",
	}
	workersAddCmd = &cobra.Command{
		Use:   "add <name>",
		Short: "Print a one-time join string for a new worker",
		Args:  cobra.ExactArgs(1),
		RunE:  runWorkersAdd,
	}
	workersUpdateCmd = &cobra.Command{
		Use:   "update <name>",
		Short: "Change an enrolled worker's classes, slots or repos",
		Args:  cobra.ExactArgs(1),
		RunE:  runWorkersUpdate,
	}
	workersListCmd = &cobra.Command{
		Use:   "list",
		Short: "List the local member and enrolled workers",
		Args:  cobra.NoArgs,
		RunE:  runWorkersList,
	}
	workersDrainCmd = &cobra.Command{
		Use:   "drain <name>",
		Short: "Stop offering jobs to a worker and let its runners finish",
		Args:  cobra.ExactArgs(1),
		RunE:  runWorkersDrain,
	}
	workersRevokeCmd = &cobra.Command{
		Use:   "revoke <name>",
		Short: "Revoke a worker's credential and end its session",
		Args:  cobra.ExactArgs(1),
		RunE:  runWorkersRevoke,
	}

	workersClasses   []string
	workersSlots     int
	workersRepos     []string
	workersAdvertise string
	workersReason    string
)

func init() {
	rootCmd.AddCommand(workersCmd)
	workersCmd.AddCommand(workersAddCmd, workersUpdateCmd, workersListCmd, workersDrainCmd, workersRevokeCmd)
	for _, c := range []*cobra.Command{workersAddCmd, workersUpdateCmd} {
		c.Flags().StringSliceVar(&workersClasses, "classes", nil, "classes the worker may serve, e.g. linux-x64,linux-arm64 (required)")
		c.Flags().IntVar(&workersSlots, "slots", 1, "maximum concurrent runners on the worker")
		c.Flags().StringSliceVar(&workersRepos, "repos", nil, "limit the worker to these repos (owner/repo or owner/*); default every router repo")
	}
	workersAddCmd.Flags().StringVar(&workersAdvertise, "advertise", "", "router host:port to write into the join string (default workers.advertise)")
	workersDrainCmd.Flags().StringVar(&workersReason, "reason", "", "reason shown on the worker")
}

// workersClient connects to the local unix socket. Worker administration is
// never sent over the TCP control listener (spec §8.1, SEC-F8).
func workersClient() (*control.Client, error) {
	if remoteAddr != "" {
		return nil, errors.New("gso workers only works on the router's local unix socket and is refused over --remote; run it on the router host (e.g. docker exec gso-router gso workers …)")
	}
	return control.NewClient()
}

func callWorkers(method string, params, out any) error {
	c, err := workersClient()
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	raw, err := c.Call(context.Background(), method, params)
	if err != nil {
		return err
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func validateWorkerBounds(name string, classes []string, slots int) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("a worker name is required")
	}
	if len(classes) == 0 {
		return errors.New("--classes is required, e.g. --classes linux-x64")
	}
	if slots < 1 {
		return errors.New("--slots must be at least 1")
	}
	return nil
}

func runWorkersAdd(cmd *cobra.Command, args []string) error {
	if err := validateWorkerBounds(args[0], workersClasses, workersSlots); err != nil {
		return err
	}
	var res control.WorkersAddResult
	if err := callWorkers(control.MethodWorkersAdd, control.WorkersAddParams{
		Name: args[0], Classes: workersClasses, Slots: workersSlots, Repos: workersRepos, Advertise: workersAdvertise,
	}, &res); err != nil {
		return err
	}
	// The join string alone on stdout, so it can be captured into a stack env.
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), res.Join)
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Single use, expires %s. Move it over a trusted channel: it is the worker's root of trust.\n", res.ExpiresAt.Local().Format(time.RFC3339))
	return nil
}

func runWorkersUpdate(_ *cobra.Command, args []string) error {
	if err := validateWorkerBounds(args[0], workersClasses, workersSlots); err != nil {
		return err
	}
	return callWorkers(control.MethodWorkersUpdate, control.WorkersUpdateParams{
		Name: args[0], Classes: workersClasses, Slots: workersSlots, Repos: workersRepos,
	}, nil)
}

func runWorkersList(_ *cobra.Command, _ []string) error {
	var ws []control.WorkerLiveStatus
	if err := callWorkers(control.MethodWorkersList, nil, &ws); err != nil {
		return err
	}
	if len(ws) == 0 {
		fmt.Println("No workers")
		return nil
	}
	return printTable([]string{"NAME", "ID", "STATE", "BUSY", "CLASSES", "PUBLIC", "LAST SEEN"}, formatWorkerRows(ws))
}

func formatWorkerRows(ws []control.WorkerLiveStatus) [][]string {
	rows := make([][]string, 0, len(ws))
	for _, w := range ws {
		name, id, state := w.Name, w.ID, w.Availability
		if w.Local {
			name += " (local)"
		}
		if id == "" {
			id = "-"
		}
		switch {
		case w.Revoked:
			state = "revoked"
		case !w.Connected && !w.Local:
			state = "offline"
		}
		classes := strings.Join(w.Classes, ",")
		if classes == "" {
			classes = "-"
		}
		public := "no"
		if w.AcceptPublic {
			public = "yes"
		}
		lastSeen := "-"
		if !w.LastSeen.IsZero() {
			lastSeen = w.LastSeen.Local().Format(time.RFC3339)
		}
		rows = append(rows, []string{name, id, state, fmt.Sprintf("%d/%d", w.Busy, w.Slots), classes, public, lastSeen})
	}
	return rows
}

func runWorkersDrain(_ *cobra.Command, args []string) error {
	return callWorkers(control.MethodWorkersDrain, control.WorkerNameParams{Name: args[0], Reason: workersReason}, nil)
}

func runWorkersRevoke(_ *cobra.Command, args []string) error {
	return callWorkers(control.MethodWorkersRevoke, control.WorkerNameParams{Name: args[0]}, nil)
}
