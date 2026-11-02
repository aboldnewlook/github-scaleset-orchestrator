package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghsim"
)

var (
	simCmd = &cobra.Command{
		Use:   "sim",
		Short: "Simulated GitHub for running a router and workers without github.com",
	}
	simServeCmd = &cobra.Command{
		Use:   "serve",
		Short: "Serve a simulated GitHub over plain HTTP (testing only; never expose it)",
		Args:  cobra.NoArgs,
		RunE:  runSimServe,
	}
	simJobCmd = &cobra.Command{
		Use:   "job",
		Short: "Inject jobs into a running simulator",
		Args:  cobra.NoArgs,
		RunE:  runSimJob,
	}
	simJobsCmd = &cobra.Command{
		Use:   "jobs",
		Short: "List the simulator's jobs",
		Args:  cobra.NoArgs,
		RunE:  runSimJobs,
	}
	simCancelCmd = &cobra.Command{
		Use:   "cancel",
		Short: "Cancel a simulated workflow run",
		Args:  cobra.NoArgs,
		RunE:  runSimCancel,
	}

	simServeFlags struct {
		listen, advertise                  string
		pollWait, claimTimeout, sessionTTL time.Duration
	}
	simJobFlags struct {
		sim, repo, labels, event, result string
		duration                         time.Duration
		count                            int
		public, fork, asJSON             bool
	}
	simJobsFlags struct {
		sim           string
		watch, asJSON bool
	}
	simCancelFlags struct {
		sim, repo string
		run       int64
	}
)

func init() {
	rootCmd.AddCommand(simCmd)
	simCmd.AddCommand(simServeCmd, simJobCmd, simJobsCmd, simCancelCmd)
	defaultSim := os.Getenv("GSO_SIM_URL")

	sf := simServeCmd.Flags()
	sf.StringVar(&simServeFlags.listen, "listen", ":9300", "address to listen on")
	sf.StringVar(&simServeFlags.advertise, "advertise", "", "base URL that fake runners in job containers dial, e.g. http://unraid.lan:9300 (default http://<listen address>)")
	sf.DurationVar(&simServeFlags.pollWait, "poll-wait", 0, "how long an idle message poll waits (default 2s; GitHub holds about 50s)")
	sf.DurationVar(&simServeFlags.claimTimeout, "claim-timeout", 0, "requeue an assigned job that no runner claims after this (default 60s)")
	sf.DurationVar(&simServeFlags.sessionTTL, "session-ttl", 0, "an un-polled session blocks new ones (409) for this long (default 2m)")

	jf := simJobCmd.Flags()
	jf.StringVar(&simJobFlags.sim, "sim", defaultSim, "simulator URL (env GSO_SIM_URL)")
	jf.StringVar(&simJobFlags.repo, "repo", "", "owner/name of the job's repository (required)")
	jf.StringVar(&simJobFlags.labels, "labels", "self-hosted", "comma-separated runs-on labels")
	jf.DurationVar(&simJobFlags.duration, "duration", 30*time.Second, "how long the fake runner works on the job")
	jf.IntVar(&simJobFlags.count, "count", 1, "number of jobs to inject (1-100)")
	jf.BoolVar(&simJobFlags.public, "public", false, "mark the repository public")
	jf.BoolVar(&simJobFlags.fork, "fork", false, "the run comes from a fork (its head repository differs)")
	jf.StringVar(&simJobFlags.event, "event", "push", "workflow event, e.g. pull_request_target")
	jf.StringVar(&simJobFlags.result, "result", "succeeded", "result the fake runner reports")
	jf.BoolVar(&simJobFlags.asJSON, "json", false, "print the injected jobs as JSON")

	lf := simJobsCmd.Flags()
	lf.StringVar(&simJobsFlags.sim, "sim", defaultSim, "simulator URL (env GSO_SIM_URL)")
	lf.BoolVar(&simJobsFlags.watch, "watch", false, "refresh every second until interrupted")
	lf.BoolVar(&simJobsFlags.asJSON, "json", false, "print JSON")

	cf := simCancelCmd.Flags()
	cf.StringVar(&simCancelFlags.sim, "sim", defaultSim, "simulator URL (env GSO_SIM_URL)")
	cf.StringVar(&simCancelFlags.repo, "repo", "", "owner/name (required)")
	cf.Int64Var(&simCancelFlags.run, "run", 0, "workflow run ID (required)")
}

// simJobSpec validates the job flags and builds the spec.
func simJobSpec(repo, labels, event, result string, duration time.Duration, count int, public, fork bool) (ghsim.JobSpec, error) {
	if repo == "" {
		return ghsim.JobSpec{}, errors.New("--repo is required")
	}
	if owner, name, ok := strings.Cut(repo, "/"); !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return ghsim.JobSpec{}, fmt.Errorf("--repo %q must be owner/name", repo)
	}
	if count < 1 || count > 100 {
		return ghsim.JobSpec{}, fmt.Errorf("--count must be 1-100, got %d", count)
	}
	if duration < 0 {
		return ghsim.JobSpec{}, errors.New("--duration cannot be negative")
	}
	var ls []string
	for _, l := range strings.Split(labels, ",") {
		if l = strings.TrimSpace(l); l != "" {
			ls = append(ls, l)
		}
	}
	return ghsim.JobSpec{Repo: repo, Labels: ls, Duration: duration, Public: public, Fork: fork, Event: event, Result: result}, nil
}

func requireSimURL(u string) (string, error) {
	if u == "" {
		return "", errors.New("--sim is required (or set GSO_SIM_URL)")
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return "", fmt.Errorf("--sim %q must be an http(s) URL", u)
	}
	return strings.TrimRight(u, "/"), nil
}

func runSimServe(cmd *cobra.Command, _ []string) error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ln, err := net.Listen("tcp", simServeFlags.listen)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", simServeFlags.listen, err)
	}
	advertise := strings.TrimRight(simServeFlags.advertise, "/")
	if advertise == "" {
		advertise = "http://" + ln.Addr().String()
		if host, _, _ := net.SplitHostPort(ln.Addr().String()); host == "::" || host == "0.0.0.0" {
			logger.Warn("no --advertise given; job containers cannot dial an unspecified address", "advertise", advertise)
		}
	}
	m := ghsim.NewModel(ghsim.Options{
		PollWait:     simServeFlags.pollWait,
		ClaimTimeout: simServeFlags.claimTimeout,
		SessionTTL:   simServeFlags.sessionTTL,
		ServerURL:    advertise,
	})
	logger.Warn("SIMULATED GITHUB: this is not github.com; jobs are fake and the API is plain HTTP without authentication; never expose this port",
		"listen", ln.Addr().String(), "advertise", advertise)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "listening on %s\n", ln.Addr())

	srv := &http.Server{Handler: ghsim.NewServer(m, logger), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func runSimJob(cmd *cobra.Command, _ []string) error {
	base, err := requireSimURL(simJobFlags.sim)
	if err != nil {
		return err
	}
	f := simJobFlags
	spec, err := simJobSpec(f.repo, f.labels, f.event, f.result, f.duration, f.count, f.public, f.fork)
	if err != nil {
		return err
	}
	jobs, err := ghsim.NewAdminClient(base).InjectJob(cmd.Context(), spec, f.count)
	if err != nil {
		return err
	}
	return printSimJobs(cmd.OutOrStdout(), jobs, f.asJSON)
}

func runSimJobs(cmd *cobra.Command, _ []string) error {
	base, err := requireSimURL(simJobsFlags.sim)
	if err != nil {
		return err
	}
	admin := ghsim.NewAdminClient(base)
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for {
		jobs, err := admin.Jobs(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if simJobsFlags.watch {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\n%s\n", time.Now().Format(time.TimeOnly))
		}
		if err := printSimJobs(cmd.OutOrStdout(), jobs, simJobsFlags.asJSON); err != nil {
			return err
		}
		if !simJobsFlags.watch {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
}

func runSimCancel(cmd *cobra.Command, _ []string) error {
	base, err := requireSimURL(simCancelFlags.sim)
	if err != nil {
		return err
	}
	if simCancelFlags.repo == "" || simCancelFlags.run == 0 {
		return errors.New("--repo and --run are required")
	}
	return ghsim.NewAdminClient(base).CancelRun(cmd.Context(), simCancelFlags.repo, simCancelFlags.run)
}

func printSimJobs(out io.Writer, jobs []ghsim.JobState, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(jobs)
	}
	headers := []string{"ID", "RUN", "REPO", "STATE", "RESULT", "RUNNER", "LABELS"}
	rows := make([][]string, 0, len(jobs))
	for _, j := range jobs {
		rows = append(rows, []string{
			strconv.FormatInt(j.ID, 10), strconv.FormatInt(j.RunID, 10), j.Spec.Repo, j.State,
			j.Result, j.Runner, strings.Join(j.Spec.Labels, ","),
		})
	}
	return printTable(headers, rows)
}
