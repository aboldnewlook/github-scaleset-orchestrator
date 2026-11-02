// Package ghapi is the router's single seam to GitHub: the scale-set API
// (through actions/scaleset) plus the few REST calls the public-repo policy
// needs. internal/ghsim implements the same Client in memory, so the router,
// scaler, fleet and workers never branch on "simulated".
package ghapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/buildinfo"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
)

// Session is one scale set's message session.
type Session interface {
	listener.Client // GetMessage, DeleteMessage, AcquireJobs, Session()
	Close(ctx context.Context) error
}

// RepoInfo is the part of GET /repos/{owner}/{repo} the router uses.
type RepoInfo struct {
	FullName   string
	Private    bool
	Visibility string
}

// WorkflowRun is the part of a workflow run the fork check uses.
// HeadRepoFullName is "" when GitHub returns no head repository.
type WorkflowRun struct {
	ID               int64
	Event            string
	RepoFullName     string
	HeadRepoFullName string
}

// Client is everything the router does with one repository on GitHub.
type Client interface {
	GetRunnerScaleSet(ctx context.Context, runnerGroupID int, name string) (*scaleset.RunnerScaleSet, error)
	CreateRunnerScaleSet(ctx context.Context, ss *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error)
	UpdateRunnerScaleSet(ctx context.Context, id int, ss *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error)
	ListRunnerScaleSets(ctx context.Context, runnerGroupID int) ([]scaleset.RunnerScaleSet, error)
	GenerateJitRunnerConfig(ctx context.Context, setting *scaleset.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
	GetRunnerByName(ctx context.Context, name string) (*scaleset.RunnerReference, error)
	RemoveRunner(ctx context.Context, runnerID int64) error
	SetSystemInfo(info scaleset.SystemInfo)
	OpenSession(ctx context.Context, scaleSetID int, owner string) (Session, error)
	Repo(ctx context.Context) (RepoInfo, error)
	WorkflowRun(ctx context.Context, runID int64) (WorkflowRun, error)
	CancelWorkflowRun(ctx context.Context, runID int64) error
}

// Factory creates the Client for one configured repo.
type Factory func(ctx context.Context, repo config.Repo) (Client, error)

// client adapts *scaleset.Client to Client and adds the REST calls.
type client struct {
	*scaleset.Client
	rest *restClient
}

var _ Client = (*client)(nil)

// OpenSession creates the scale set's message session. A session another
// owner still holds fails with an error wrapping scaleset.ConflictError
// (see IsConflict).
func (c *client) OpenSession(ctx context.Context, scaleSetID int, owner string) (Session, error) {
	s, err := c.MessageSessionClient(ctx, scaleSetID, owner)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (c *client) Repo(ctx context.Context) (RepoInfo, error) { return c.rest.Repo(ctx) }

func (c *client) WorkflowRun(ctx context.Context, runID int64) (WorkflowRun, error) {
	return c.rest.WorkflowRun(ctx, runID)
}

func (c *client) CancelWorkflowRun(ctx context.Context, runID int64) error {
	return c.rest.CancelWorkflowRun(ctx, runID)
}

// NewFactory returns a Factory for real GitHub. Each repo's PAT comes from
// cfg.TokenForRepo. httpClient is shared by every repo's listeners and REST
// calls; nil means NewHTTPClient().
func NewFactory(cfg *config.Config, httpClient *http.Client, logger *slog.Logger) Factory {
	if httpClient == nil {
		httpClient = NewHTTPClient()
	}
	return func(_ context.Context, repo config.Repo) (Client, error) {
		owner, name, ok := strings.Cut(repo.Name, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return nil, fmt.Errorf("repo %q: want owner/name", repo.Name)
		}
		token, err := cfg.TokenForRepo(repo)
		if err != nil {
			return nil, fmt.Errorf("resolving token for %s: %w", repo.Name, err)
		}
		sc, err := scaleset.NewClientWithPersonalAccessToken(
			scaleset.NewClientWithPersonalAccessTokenConfig{
				GitHubConfigURL:     "https://github.com/" + repo.Name,
				PersonalAccessToken: token,
				SystemInfo:          scaleset.SystemInfo{System: "gso", Version: buildinfo.Version, Subsystem: "client"},
			},
			scaleset.WithHTTPClient(httpClient),
			scaleset.WithLogger(logger.With("repo", repo.Name)),
		)
		if err != nil {
			return nil, fmt.Errorf("creating scaleset client for %s: %w", repo.Name, err)
		}
		return &client{
			Client: sc,
			rest:   &restClient{base: defaultAPIBase, repo: repo.Name, token: token, http: httpClient},
		}, nil
	}
}
