package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/buildinfo"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/naming"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
	"github.com/aboldnewlook/github-scaleset-orchestrator/worker"
)

// workerFlags holds the raw `gso worker` flag values.
type workerFlags struct {
	join, router, name, stateDir, executor, image, dockerHost, acceptPublic, credentialStore, githubSim string

	slots        int
	memoryMB     int64
	cpus         float64
	allow        []string
	trustedHosts []string
	yes          bool
}

// workerSettings is the resolved configuration of `gso worker`.
type workerSettings struct {
	Join            string
	Router          string
	Name            string
	StateDir        string
	Executor        runner.ExecutorKind
	NewLauncher     func(s workerSettings, logger *slog.Logger) (worker.Launcher, error)
	Image           string
	DockerHost      string
	Slots           int
	MemoryMB        int64
	CPUs            float64
	Allow           []string
	TrustedHosts    []string
	AcceptPublic    worker.PublicMode
	CredentialStore string
	Yes             bool
	BackoffMin      time.Duration
	BackoffMax      time.Duration
}

var (
	wf        workerFlags
	workerCmd = &cobra.Command{
		Use:   "worker",
		Short: "Lend this machine to a gso router as a remote worker",
		Long: `Runs jobs for a gso router on this machine.

The first start needs the join string printed by "gso workers add <name>" on
the router (--join or GSO_WORKER_JOIN). After that the credential and the
router's pinned key are read from --state-dir.

Stopping the worker (Ctrl-C, SIGTERM, a container stop) closes its session
but leaves running job containers alone; the next start re-attaches to them.`,
		Args: cobra.NoArgs,
		RunE: runWorker,
	}
)

func init() {
	rootCmd.AddCommand(workerCmd)
	f := workerCmd.Flags()
	f.StringVar(&wf.join, "join", "", "join string from `gso workers add` (env GSO_WORKER_JOIN); used only until this worker is enrolled")
	f.StringVar(&wf.router, "router", "", "dial the router at host:port instead of the join string's address (env GSO_WORKER_ROUTER); the pinned key still applies")
	f.StringVar(&wf.name, "name", "", "worker name (env GSO_WORKER_NAME; default: this host's name)")
	f.StringVar(&wf.stateDir, "state-dir", "", "directory for the credential, enrollment state and runner records (default: <config dir>/gso/worker)")
	f.StringVar(&wf.executor, "executor", "docker", "executor: docker, or sim (testing: runs the simulator's fake runner as a process)")
	f.StringVar(&wf.githubSim, "github-sim", "", "URL of the gso GitHub simulator the router uses (env GSO_GITHUB_SIM; testing only): its host is trusted in job configs next to github.com")
	f.IntVar(&wf.slots, "slots", 1, "jobs to run at once (env GSO_WORKER_SLOTS); the router clamps this to the enrolled bounds")
	f.StringVar(&wf.image, "image", "", "job image for docker runners (env GSO_WORKER_IMAGE), e.g. ghcr.io/aboldnewlook/github-scaleset-orchestrator:<tag>")
	f.StringVar(&wf.dockerHost, "docker-host", "", "docker engine endpoint (env GSO_WORKER_DOCKER_HOST; default unix:///var/run/docker.sock)")
	f.Int64Var(&wf.memoryMB, "memory-mb", 0, "memory limit per job container in MiB (0 = none)")
	f.Float64Var(&wf.cpus, "cpus", 0, "CPU limit per job container (0 = none)")
	f.StringSliceVar(&wf.allow, "allow", nil, "repositories to run jobs for, as owner/* or owner/repo (repeatable; default: the owners granted at enrollment)")
	f.StringVar(&wf.acceptPublic, "accept-public", "follow", "public repositories: follow (the router's default), accept or refuse (env GSO_WORKER_ACCEPT_PUBLIC)")
	f.StringVar(&wf.credentialStore, "credential-store", "file", "where the worker credential lives: file (<state-dir>/credential, mode 0600) or keychain")
	f.StringSliceVar(&wf.trustedHosts, "trusted-host", nil, "GitHub host to accept in job configs instead of github.com (repeatable; for simulator routers only)")
	f.BoolVar(&wf.yes, "yes", false, "enroll without asking to confirm the router's address and key")
}

// resolveWorkerSettings turns flags and environment into settings. A flag set
// on the command line wins over its environment variable, which wins over the
// flag's default. The executor switch below is the only place that maps
// executor names to launchers.
func resolveWorkerSettings(f workerFlags, changed func(string) bool, getenv func(string) string, hostname func() (string, error), stateDir func() (string, error)) (workerSettings, error) {
	str := func(flag, env, value string) string {
		if !changed(flag) {
			if v := strings.TrimSpace(getenv(env)); v != "" {
				return v
			}
		}
		return strings.TrimSpace(value)
	}
	backoffMin, backoffMax := workerBackoff()
	s := workerSettings{
		Join:            str("join", "GSO_WORKER_JOIN", f.join),
		Router:          str("router", "GSO_WORKER_ROUTER", f.router),
		Name:            str("name", "GSO_WORKER_NAME", f.name),
		StateDir:        strings.TrimSpace(f.stateDir),
		Image:           str("image", "GSO_WORKER_IMAGE", f.image),
		DockerHost:      str("docker-host", "GSO_WORKER_DOCKER_HOST", f.dockerHost),
		Slots:           f.slots,
		MemoryMB:        f.memoryMB,
		CPUs:            f.cpus,
		CredentialStore: f.credentialStore,
		Yes:             f.yes,
		BackoffMin:      backoffMin,
		BackoffMax:      backoffMax,
	}
	for _, a := range f.allow {
		if a = strings.TrimSpace(a); a != "" {
			s.Allow = append(s.Allow, a)
		}
	}
	for _, h := range f.trustedHosts {
		if h = strings.TrimSpace(h); h != "" {
			s.TrustedHosts = append(s.TrustedHosts, h)
		}
	}
	simURL := str("github-sim", "GSO_GITHUB_SIM", f.githubSim)
	if !changed("slots") {
		if v := strings.TrimSpace(getenv("GSO_WORKER_SLOTS")); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return workerSettings{}, fmt.Errorf("GSO_WORKER_SLOTS: %w", err)
			}
			s.Slots = n
		}
	}
	if s.Slots < 1 {
		return workerSettings{}, errors.New("--slots must be at least 1")
	}
	mode, err := worker.ParsePublicMode(str("accept-public", "GSO_WORKER_ACCEPT_PUBLIC", f.acceptPublic))
	if err != nil {
		return workerSettings{}, fmt.Errorf("--accept-public: %w", err)
	}
	s.AcceptPublic = mode
	switch s.CredentialStore {
	case "file", "keychain":
	default:
		return workerSettings{}, fmt.Errorf("--credential-store %q: use file or keychain", s.CredentialStore)
	}

	switch strings.ToLower(strings.TrimSpace(f.executor)) {
	case "docker":
		if s.Image == "" {
			return workerSettings{}, errors.New("--image (GSO_WORKER_IMAGE) is required for the docker executor, e.g. ghcr.io/aboldnewlook/github-scaleset-orchestrator:<tag>")
		}
		s.Executor, s.NewLauncher = runner.ExecutorDocker, newDockerLauncher
	case "sim":
		if simURL == "" && len(s.TrustedHosts) == 0 {
			return workerSettings{}, errors.New("--executor sim needs --github-sim (GSO_GITHUB_SIM) or --trusted-host, so the worker accepts the simulator's job configs")
		}
		s.Executor, s.NewLauncher = runner.ExecutorDocker, newSimLauncher
	case "native":
		return workerSettings{}, errors.New("--executor native arrives in P5; use --executor docker")
	default:
		return workerSettings{}, fmt.Errorf("unknown --executor %q; use docker or sim", f.executor)
	}
	trusted, err := simTrustedHosts(simURL, strings.EqualFold(strings.TrimSpace(f.executor), "sim"), s.TrustedHosts)
	if err != nil {
		return workerSettings{}, err
	}
	s.TrustedHosts = trusted

	if s.Name == "" {
		host, err := hostname()
		if err != nil || host == "" {
			host = "worker"
		}
		s.Name, _, _ = strings.Cut(host, ".")
	}
	s.Name = naming.Sanitize(s.Name)
	if s.StateDir == "" {
		dir, err := stateDir()
		if err != nil {
			return workerSettings{}, fmt.Errorf("locating the state directory (pass --state-dir): %w", err)
		}
		s.StateDir = filepath.Join(dir, "worker")
	}
	return s, nil
}

// newSimLauncher runs jobs as `gso sim runner` processes (testing).
func newSimLauncher(s workerSettings, _ *slog.Logger) (worker.Launcher, error) {
	return simLauncher(s.StateDir)
}

// newDockerLauncher runs jobs as sibling containers that survive the worker's
// restart (DetachOnCancel), labelled with this worker's instance ID.
func newDockerLauncher(s workerSettings, logger *slog.Logger) (worker.Launcher, error) {
	dw, err := runner.NewDockerWorker(runner.DockerConfig{
		Image:      s.Image,
		InstanceID: "gsow-" + naming.WorkerSegment(s.Name),
		MemoryMB:   s.MemoryMB,
		CPUs:       s.CPUs,
		Host:       s.DockerHost,
		OnCancel:   runner.DetachOnCancel,
	}, logger)
	if err != nil {
		return nil, fmt.Errorf("configuring the docker executor: %w", err)
	}
	return worker.NewDockerLauncher(dw), nil
}

func credentialStoreFor(s workerSettings) worker.CredentialStore {
	if s.CredentialStore == "keychain" {
		return worker.KeychainCredentialStore("gso-worker", s.StateDir)
	}
	return worker.FileCredentialStore(filepath.Join(s.StateDir, "credential"))
}

func runWorker(cmd *cobra.Command, _ []string) error {
	s, err := resolveWorkerSettings(wf, cmd.Flags().Changed, os.Getenv, os.Hostname, config.StateDir)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := os.MkdirAll(s.StateDir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", s.StateDir, err)
	}
	machineID, err := loadOrCreateMachineID(s.StateDir)
	if err != nil {
		return fmt.Errorf("machine ID: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store := credentialStoreFor(s)
	interactive := term.IsTerminal(int(os.Stdin.Fd()))
	st, err := ensureEnrolled(ctx, s, store, machineID, cmd.InOrStdin(), cmd.ErrOrStderr(), interactive)
	if err != nil {
		return err
	}
	s.Name = st.Name // the enrolled name owns the instance label and runner names

	launcher, err := s.NewLauncher(s, logger)
	if err != nil {
		return err
	}
	records, err := worker.OpenRecordStore(filepath.Join(s.StateDir, "runners.json"))
	if err != nil {
		return fmt.Errorf("opening runner records: %w", err)
	}
	allow := s.Allow
	if len(allow) == 0 {
		allow = st.OwnerScopes
	}
	dial := s.Router
	if dial == "" {
		dial = st.Addr
	}
	c, err := worker.New(worker.Config{
		Name:         st.Name,
		MachineID:    machineID,
		Version:      buildinfo.Version,
		DialAddr:     dial,
		Pin:          st.Pin,
		Credential:   store,
		Records:      records,
		Launchers:    []worker.Launcher{launcher},
		Slots:        s.Slots,
		Allowlist:    allow,
		AcceptPublic: s.AcceptPublic,
		TrustedHosts: s.TrustedHosts,
		Logger:       logger,
		BackoffMin:   s.BackoffMin,
		BackoffMax:   s.BackoffMax,
	})
	if err != nil {
		return err
	}
	c.SetAvailability(worker.Available)
	logger.Info("starting gso worker", "name", st.Name, "router", dial, "executor", s.Executor, "slots", s.Slots, "allow", allow, "accept_public", s.AcceptPublic)
	if err := c.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// ensureEnrolled returns the saved enrollment, or enrolls with s.Join after
// showing the router's address and short key and, on a terminal, asking to
// confirm (spec §4.4, SEC-F9).
func ensureEnrolled(ctx context.Context, s workerSettings, store worker.CredentialStore, machineID string, in io.Reader, out io.Writer, interactive bool) (worker.State, error) {
	statePath := filepath.Join(s.StateDir, "worker.json")
	st, stateErr := worker.LoadState(statePath)
	_, credErr := store.Load()
	if stateErr == nil && credErr == nil {
		return st, nil
	}
	if credErr != nil && !errors.Is(credErr, worker.ErrNoCredential) {
		return worker.State{}, fmt.Errorf("reading the worker credential: %w", credErr)
	}
	if stateErr != nil && !errors.Is(stateErr, os.ErrNotExist) {
		return worker.State{}, fmt.Errorf("reading %s: %w", statePath, stateErr)
	}
	if s.Join == "" {
		return worker.State{}, errors.New("this worker is not enrolled: pass --join (or set GSO_WORKER_JOIN) to the string printed by `gso workers add` on the router")
	}
	j, err := worker.ParseJoin(s.Join)
	if err != nil {
		return worker.State{}, fmt.Errorf("--join: %w", err)
	}
	_, _ = fmt.Fprintf(out, "Enrolling %q with the gso router at %s\nRouter key: %s\nCheck both against the output of `gso workers add` before continuing.\n", s.Name, j.Addr, worker.ShortPin(j.Pin))
	if interactive && !s.Yes {
		_, _ = fmt.Fprint(out, "Enroll? [y/N] ")
		answer, _ := bufio.NewReader(in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			return worker.State{}, errors.New("enrollment cancelled")
		}
	}
	credential, st, err := worker.Enroll(ctx, worker.EnrollOptions{Join: j, DialAddr: s.Router, Name: s.Name, MachineID: machineID})
	if err != nil {
		return worker.State{}, err
	}
	if err := store.Save(credential); err != nil {
		return worker.State{}, fmt.Errorf("saving the worker credential: %w", err)
	}
	if err := worker.SaveState(statePath, st); err != nil {
		return worker.State{}, fmt.Errorf("saving %s: %w", statePath, err)
	}
	_, _ = fmt.Fprintf(out, "Enrolled. Credential stored (%s); state in %s\n", s.CredentialStore, s.StateDir)
	return st, nil
}

// loadOrCreateMachineID returns a random ID that identifies this installation
// to the router, creating it on first use.
func loadOrCreateMachineID(stateDir string) (string, error) {
	path := filepath.Join(stateDir, "machine-id")
	b, err := os.ReadFile(path)
	if err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	id := uuid.NewString()
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return "", err
	}
	return id, nil
}
