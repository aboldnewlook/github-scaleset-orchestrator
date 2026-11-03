package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghapi"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/naming"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
	"github.com/actions/scaleset"
)

// Query is a stateless service that creates scaleset clients on demand
// and queries GitHub for runner/scale-set information.
type Query struct {
	cfg       *config.Config
	logger    *slog.Logger
	github    ghapi.Factory
	deleter   DeleterFactory
	listeners LiveListeners
}

// ScaleSetDeleter is the one destructive scale-set call. It is deliberately
// not part of ghapi.Client (that interface has no delete, so the daemon can
// never delete a scale set, not even on a 409); only the CLI's reset uses it.
type ScaleSetDeleter interface {
	DeleteRunnerScaleSet(ctx context.Context, id int) error
}

// DeleterFactory creates the ScaleSetDeleter for one configured repo.
type DeleterFactory func(ctx context.Context, repo config.Repo) (ScaleSetDeleter, error)

// LiveListeners reports the listeners of a running daemon, or an error when
// none is reachable.
type LiveListeners func(ctx context.Context) ([]control.ListenerLiveStatus, error)

// Deps overrides how a Query reaches GitHub; nil fields use the real thing
// (and "no daemon reachable" for Listeners). Tests inject the simulator.
type Deps struct {
	GitHub    ghapi.Factory
	Deleter   DeleterFactory
	Listeners LiveListeners
}

// NewQuery creates a new Query service.
func NewQuery(cfg *config.Config, logger *slog.Logger) *Query {
	return NewQueryWith(cfg, logger, Deps{})
}

// NewQueryWith creates a Query with injected dependencies.
func NewQueryWith(cfg *config.Config, logger *slog.Logger, d Deps) *Query {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if d.GitHub == nil {
		d.GitHub = ghapi.NewFactory(cfg, nil, logger)
	}
	if d.Deleter == nil {
		d.Deleter = func(ctx context.Context, repo config.Repo) (ScaleSetDeleter, error) {
			return NewScaleSetClient(ctx, cfg, repo, logger)
		}
	}
	return &Query{cfg: cfg, logger: logger, github: d.GitHub, deleter: d.Deleter, listeners: d.Listeners}
}

// RepoStatus holds the status of a single repo's scale set.
type RepoStatus struct {
	Repo       string
	ScaleSetID int
	Statistics *scaleset.RunnerScaleSetStatistic
	Error      string // non-empty if repo had an error
}

// StatusResult aggregates status across all repos.
type StatusResult struct {
	Repos      []RepoStatus
	MaxRunners int
}

// Status queries the statistics for scale sets matching the naming pattern.
// If repoFilter is non-empty only repos whose name contains the filter string
// are included.
func (q *Query) Status(ctx context.Context, repoFilter string) (*StatusResult, error) {
	result := &StatusResult{MaxRunners: q.cfg.MaxRunners}

	for _, repo := range q.cfg.Repos {
		if repoFilter != "" && !strings.Contains(repo.Name, repoFilter) {
			continue
		}

		rs := RepoStatus{Repo: repo.Name}

		client, err := q.github(ctx, repo)
		if err != nil {
			rs.Error = err.Error()
			result.Repos = append(result.Repos, rs)
			continue
		}

		scaleSetName := q.currentScaleSetName(repo.Name)
		ss, err := client.GetRunnerScaleSet(ctx, 1, scaleSetName)
		if err != nil {
			rs.Error = err.Error()
			result.Repos = append(result.Repos, rs)
			continue
		}
		if ss == nil {
			rs.Error = "scale set not found"
			result.Repos = append(result.Repos, rs)
			continue
		}

		rs.ScaleSetID = ss.ID
		rs.Statistics = ss.Statistics
		result.Repos = append(result.Repos, rs)
	}

	return result, nil
}

// ScaleSetInfo pairs a repo name with the full RunnerScaleSet data.
type ScaleSetInfo struct {
	Repo string
	scaleset.RunnerScaleSet
}

// ScaleSets returns the scale set details for each configured repo.
func (q *Query) ScaleSets(ctx context.Context, repoFilter string) ([]ScaleSetInfo, error) {
	var results []ScaleSetInfo

	for _, repo := range q.cfg.Repos {
		if repoFilter != "" && !strings.Contains(repo.Name, repoFilter) {
			continue
		}

		client, err := q.github(ctx, repo)
		if err != nil {
			q.logger.Warn("skipping repo", "repo", repo.Name, "error", err)
			continue
		}

		scaleSetName := q.currentScaleSetName(repo.Name)
		ss, err := client.GetRunnerScaleSet(ctx, 1, scaleSetName)
		if err != nil || ss == nil {
			continue
		}

		results = append(results, ScaleSetInfo{Repo: repo.Name, RunnerScaleSet: *ss})
	}

	return results, nil
}

// RemoveRunner removes a specific runner by ID from the given repo.
func (q *Query) RemoveRunner(ctx context.Context, repo string, runnerID int64) error {
	r, err := q.findRepo(repo)
	if err != nil {
		return err
	}

	client, err := NewScaleSetClient(ctx, q.cfg, r, q.logger)
	if err != nil {
		return err
	}

	return client.RemoveRunner(ctx, runnerID)
}

// DeleteScaleSet deletes a scale set by ID from the given repo.
func (q *Query) DeleteScaleSet(ctx context.Context, repo string, scaleSetID int) error {
	r, err := q.findRepo(repo)
	if err != nil {
		return err
	}

	client, err := NewScaleSetClient(ctx, q.cfg, r, q.logger)
	if err != nil {
		return err
	}

	return client.DeleteRunnerScaleSet(ctx, scaleSetID)
}

// RepoHealth describes the health of a single repo's configuration.
type RepoHealth struct {
	Repo         string
	TokenValid   bool
	APIReachable bool
	Error        string
}

// RunnerBinaryHealth describes the state of the cached runner binary.
type RunnerBinaryHealth struct {
	Cached  bool
	Version string
	Path    string
}

// HealthResult aggregates health information.
type HealthResult struct {
	Repos        []RepoHealth
	RunnerBinary RunnerBinaryHealth
}

// Health checks token validity for each repo and runner binary cache status.
func (q *Query) Health(ctx context.Context) (*HealthResult, error) {
	result := &HealthResult{}

	// Check each repo's token by attempting to create a client and
	// fetch the scale set (which validates the token against the API).
	for _, repo := range q.cfg.Repos {
		rh := RepoHealth{Repo: repo.Name}

		// Verify token resolves
		token, err := q.cfg.TokenForRepo(repo)
		if err != nil || token == "" {
			rh.Error = "token not configured or empty"
			result.Repos = append(result.Repos, rh)
			continue
		}
		rh.TokenValid = true

		// Verify API reachability by fetching the scale set
		client, err := q.github(ctx, repo)
		if err != nil {
			rh.Error = err.Error()
			result.Repos = append(result.Repos, rh)
			continue
		}

		scaleSetName := q.currentScaleSetName(repo.Name)
		_, err = client.GetRunnerScaleSet(ctx, 1, scaleSetName)
		if err != nil {
			rh.Error = err.Error()
		} else {
			rh.APIReachable = true
		}

		result.Repos = append(result.Repos, rh)
	}

	// Check runner binary cache
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		cacheDir = "/tmp"
	}
	cacheDir = filepath.Join(cacheDir, "gso")

	result.RunnerBinary = checkRunnerBinary(cacheDir)

	return result, nil
}

func checkRunnerBinary(cacheDir string) RunnerBinaryHealth {
	rbh := RunnerBinaryHealth{Path: cacheDir}

	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return rbh
	}

	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "runner-") {
			runExe := filepath.Join(cacheDir, entry.Name(), "run.sh")
			if runtime.GOOS == "windows" {
				runExe = filepath.Join(cacheDir, entry.Name(), "run.cmd")
			}
			if _, err := os.Stat(runExe); err == nil {
				rbh.Cached = true
				rbh.Version = strings.TrimPrefix(entry.Name(), "runner-")
				rbh.Path = filepath.Join(cacheDir, entry.Name())
				break
			}
		}
	}

	return rbh
}

// findRepo resolves a configured repo. Repo identity is ASCII
// case-insensitive (not Unicode folding: strings.EqualFold would equate
// "K" with the Kelvin sign).
func (q *Query) findRepo(name string) (config.Repo, error) {
	for _, r := range q.cfg.Repos {
		if asciiLower(r.Name) == asciiLower(name) {
			return r, nil
		}
	}
	return config.Repo{}, fmt.Errorf("repo %q not found in configuration", name)
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

func hostnameOrUnknown() string {
	if h, _ := os.Hostname(); h != "" {
		return h
	}
	return "unknown"
}

// currentScaleSetName is the name the daemon uses for repo: instance_id
// (else the hostname) and the default class (workers.default_class, else
// the router host's class; design spec §6.1).
func (q *Query) currentScaleSetName(repo string) string {
	return naming.ScaleSetNameFor(naming.InstanceID(q.cfg.Runner.InstanceID), repo, q.cfg.DefaultClass())
}

// ScaleSetKind says how a gso-prefixed scale set relates to this configuration.
type ScaleSetKind string

const (
	ScaleSetCurrent ScaleSetKind = "current" // a name this instance uses for some class
	ScaleSetLegacy  ScaleSetKind = "legacy"  // this host's pre-rename gso-<hostname>-<repo>
	ScaleSetOther   ScaleSetKind = "other"   // another instance's, or an orphan from an old hostname
)

// ListedScaleSet is one gso-prefixed scale set found in a configured repo.
type ListedScaleSet struct {
	Repo string
	Kind ScaleSetKind
	scaleset.RunnerScaleSet
}

// ScaleSetsAll lists every gso-prefixed scale set in each configured repo,
// including legacy names and orphans that the name-based lookups cannot see
// (design spec §6.1, F4).
func (q *Query) ScaleSetsAll(ctx context.Context, repoFilter string) ([]ListedScaleSet, error) {
	hostname := hostnameOrUnknown()
	instance := naming.InstanceID(q.cfg.Runner.InstanceID)
	var out []ListedScaleSet
	for _, repo := range q.cfg.Repos {
		if repoFilter != "" && !strings.Contains(asciiLower(repo.Name), asciiLower(repoFilter)) {
			continue
		}
		client, err := q.github(ctx, repo)
		if err != nil {
			q.logger.Warn("skipping repo", "repo", repo.Name, "error", err)
			continue
		}
		sets, err := client.ListRunnerScaleSets(ctx, 1)
		if err != nil {
			return nil, fmt.Errorf("listing scale sets for %s: %w", repo.Name, err)
		}
		out = append(out, classifyScaleSets(instance, hostname, repo.Name, sets)...)
	}
	return out, nil
}

func classifyScaleSets(instance, hostname, repo string, sets []scaleset.RunnerScaleSet) []ListedScaleSet {
	current := make(map[string]bool, len(runner.AllClasses))
	for _, c := range runner.AllClasses {
		current[naming.ScaleSetNameFor(instance, repo, c)] = true
	}
	legacy := naming.ScaleSetName(naming.ScaleSetNamePrefix, hostname, repo)
	var out []ListedScaleSet
	for _, ss := range sets {
		if !strings.HasPrefix(ss.Name, naming.ScaleSetNamePrefix+"-") {
			continue
		}
		kind := ScaleSetOther
		switch {
		case current[ss.Name]:
			kind = ScaleSetCurrent
		case ss.Name == legacy:
			kind = ScaleSetLegacy
		}
		out = append(out, ListedScaleSet{Repo: repo, Kind: kind, RunnerScaleSet: ss})
	}
	slices.SortFunc(out, func(a, b ListedScaleSet) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Reset refusals. Both are fail-safe: reset never deletes a scale set that a
// live gso may own or that has work on it.
var (
	// ErrScaleSetLive means a gso daemon is serving the scale set right now.
	ErrScaleSetLive = errors.New("a gso daemon is serving this scale set")
	// ErrScaleSetBusy means the scale set has jobs or busy runners.
	ErrScaleSetBusy = errors.New("scale set has jobs in flight")
)

// resetProbeOwner is the session owner of reset's liveness probe.
const resetProbeOwner = "gso-reset-probe"

// ResetScaleSet deletes this instance's scale set for repo and class so the
// daemon's supervisor recreates it on its next listener retry. It is the only
// path that deletes a scale set (design spec §7, the 409 row), and it refuses
// rather than risk a live scale set:
//
//   - ErrScaleSetBusy when the scale set has busy runners or assigned,
//     acquired or running jobs;
//   - ErrScaleSetLive when this machine's daemon reports the listener
//     "running", or, when its listener is not stuck (degraded or retrying
//     after a 409), when the message session is held by another owner.
//
// The returned name and id are set whenever the scale set was found.
func (q *Query) ResetScaleSet(ctx context.Context, repo string, class runner.Class) (string, int, error) {
	r, err := q.findRepo(repo)
	if err != nil {
		return "", 0, err
	}
	name := naming.ScaleSetNameFor(naming.InstanceID(q.cfg.Runner.InstanceID), r.Name, class)
	client, err := q.github(ctx, r)
	if err != nil {
		return name, 0, err
	}
	ss, err := client.GetRunnerScaleSet(ctx, 1, name)
	if err != nil {
		return name, 0, fmt.Errorf("looking up %s: %w", name, err)
	}
	if ss == nil {
		return name, 0, fmt.Errorf("scale set %s not found in %s", name, r.Name)
	}

	stuck, err := q.listenerState(ctx, r.Name, class)
	if err != nil {
		return name, ss.ID, err
	}
	if st := ss.Statistics; st != nil && (st.TotalBusyRunners > 0 || st.TotalRunningJobs > 0 || st.TotalAssignedJobs > 0 || st.TotalAcquiredJobs > 0) {
		return name, ss.ID, fmt.Errorf("%w: %s has %d busy runners, %d running, %d assigned and %d acquired jobs; wait for them to finish, then run reset again",
			ErrScaleSetBusy, name, st.TotalBusyRunners, st.TotalRunningJobs, st.TotalAssignedJobs, st.TotalAcquiredJobs)
	}
	if !stuck {
		if err := probeSession(ctx, client, ss.ID, name); err != nil {
			return name, ss.ID, err
		}
	}

	del, err := q.deleter(ctx, r)
	if err != nil {
		return name, ss.ID, err
	}
	if err := del.DeleteRunnerScaleSet(ctx, ss.ID); err != nil {
		return name, ss.ID, fmt.Errorf("deleting %s: %w", name, err)
	}
	return name, ss.ID, nil
}

// listenerState inspects the local daemon. stuck is true when its listener
// for repo and class is degraded or retrying, the case reset exists for; it
// errors with ErrScaleSetLive when that listener is running.
func (q *Query) listenerState(ctx context.Context, repo string, class runner.Class) (stuck bool, err error) {
	if q.listeners == nil {
		return false, nil
	}
	live, lerr := q.listeners(ctx)
	if lerr != nil {
		return false, nil // no daemon reachable here; the session probe decides
	}
	for _, l := range live {
		if asciiLower(l.Repo) != asciiLower(repo) || l.Class != class.String() {
			continue
		}
		if l.State == "degraded" || l.State == "retrying" {
			return true, nil
		}
		return false, fmt.Errorf("%w: its %s listener for %s is %q; stop gso first", ErrScaleSetLive, class, repo, l.State)
	}
	return false, nil
}

// probeSession opens and closes a message session to learn whether another
// owner (a live gso, possibly on another host) holds the scale set. Any
// failure other than success refuses: unknown is treated as live.
func probeSession(ctx context.Context, client ghapi.Client, id int, name string) error {
	s, err := client.OpenSession(ctx, id, resetProbeOwner)
	if err != nil {
		if ghapi.IsConflict(err) {
			return fmt.Errorf("%w: %s has an open message session; stop the gso that owns it first (if none is running, the session expires on its own): %w", ErrScaleSetLive, name, err)
		}
		return fmt.Errorf("checking %s for a live listener: %w", name, err)
	}
	if err := s.Close(ctx); err != nil {
		return fmt.Errorf("closing the probe session on %s: %w", name, err)
	}
	return nil
}
