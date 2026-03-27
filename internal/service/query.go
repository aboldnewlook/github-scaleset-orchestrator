package service

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/actions/scaleset"
)

const scaleSetNamePrefix = "gso"

// Query is a stateless service that creates scaleset clients on demand
// and queries GitHub for runner/scale-set information.
type Query struct {
	cfg    *config.Config
	logger *slog.Logger
}

// NewQuery creates a new Query service.
func NewQuery(cfg *config.Config, logger *slog.Logger) *Query {
	return &Query{cfg: cfg, logger: logger}
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
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "unknown"
	}

	result := &StatusResult{MaxRunners: q.cfg.MaxRunners}

	for _, repo := range q.cfg.Repos {
		if repoFilter != "" && !strings.Contains(repo.Name, repoFilter) {
			continue
		}

		rs := RepoStatus{Repo: repo.Name}

		client, err := NewScaleSetClient(ctx, q.cfg, repo, q.logger)
		if err != nil {
			rs.Error = err.Error()
			result.Repos = append(result.Repos, rs)
			continue
		}

		scaleSetName := fmt.Sprintf("%s-%s-%s", scaleSetNamePrefix, hostname, repoShortName(repo.Name))
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
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "unknown"
	}

	var results []ScaleSetInfo

	for _, repo := range q.cfg.Repos {
		if repoFilter != "" && !strings.Contains(repo.Name, repoFilter) {
			continue
		}

		client, err := NewScaleSetClient(ctx, q.cfg, repo, q.logger)
		if err != nil {
			q.logger.Warn("skipping repo", "repo", repo.Name, "error", err)
			continue
		}

		scaleSetName := fmt.Sprintf("%s-%s-%s", scaleSetNamePrefix, hostname, repoShortName(repo.Name))
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
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "unknown"
	}

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
		client, err := NewScaleSetClient(ctx, q.cfg, repo, q.logger)
		if err != nil {
			rh.Error = err.Error()
			result.Repos = append(result.Repos, rh)
			continue
		}

		scaleSetName := fmt.Sprintf("%s-%s-%s", scaleSetNamePrefix, hostname, repoShortName(repo.Name))
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

func (q *Query) findRepo(name string) (config.Repo, error) {
	for _, r := range q.cfg.Repos {
		if r.Name == name {
			return r, nil
		}
	}
	return config.Repo{}, fmt.Errorf("repo %q not found in configuration", name)
}

func repoShortName(repo string) string {
	parts := strings.Split(repo, "/")
	if len(parts) == 2 {
		return parts[1]
	}
	return repo
}
