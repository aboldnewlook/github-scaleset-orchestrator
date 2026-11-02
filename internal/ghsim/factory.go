package ghsim

import (
	"context"

	"github.com/actions/scaleset"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghapi"
)

// Factory returns an in-process ghapi.Factory backed by the model.
func (m *Model) Factory() ghapi.Factory {
	return func(_ context.Context, repo config.Repo) (ghapi.Client, error) {
		return m.Client(repo.Name), nil
	}
}

// Client returns the in-process ghapi.Client of one repository.
func (m *Model) Client(repo string) ghapi.Client { return &repoClient{m: m, repo: repo} }

type repoClient struct {
	m    *Model
	repo string
}

var _ ghapi.Client = (*repoClient)(nil)

func (c *repoClient) GetRunnerScaleSet(_ context.Context, _ int, name string) (*scaleset.RunnerScaleSet, error) {
	return c.m.getScaleSet(c.repo, name), nil
}

func (c *repoClient) CreateRunnerScaleSet(_ context.Context, ss *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	return c.m.createScaleSet(c.repo, ss)
}

func (c *repoClient) UpdateRunnerScaleSet(_ context.Context, id int, ss *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	return c.m.updateScaleSet(c.repo, id, ss)
}

func (c *repoClient) ListRunnerScaleSets(_ context.Context, _ int) ([]scaleset.RunnerScaleSet, error) {
	return c.m.listScaleSets(c.repo), nil
}

func (c *repoClient) GenerateJitRunnerConfig(_ context.Context, setting *scaleset.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	return c.m.mint(c.repo, setting, scaleSetID)
}

func (c *repoClient) GetRunnerByName(_ context.Context, name string) (*scaleset.RunnerReference, error) {
	return c.m.runnerByName(c.repo, name), nil
}

func (c *repoClient) RemoveRunner(_ context.Context, runnerID int64) error {
	return c.m.removeRunner(c.repo, runnerID)
}

func (c *repoClient) SetSystemInfo(scaleset.SystemInfo) {}

func (c *repoClient) OpenSession(_ context.Context, scaleSetID int, owner string) (ghapi.Session, error) {
	info, err := c.m.openSession(c.repo, scaleSetID, owner)
	if err != nil {
		return nil, err
	}
	return &localSession{m: c.m, repo: c.repo, info: info}, nil
}

func (c *repoClient) Repo(context.Context) (ghapi.RepoInfo, error) { return c.m.repoInfo(c.repo), nil }

func (c *repoClient) WorkflowRun(_ context.Context, runID int64) (ghapi.WorkflowRun, error) {
	return c.m.workflowRun(c.repo, runID)
}

func (c *repoClient) CancelWorkflowRun(_ context.Context, runID int64) error {
	return c.m.CancelRun(c.repo, runID)
}

type localSession struct {
	m    *Model
	repo string
	info scaleset.RunnerScaleSetSession
}

var _ ghapi.Session = (*localSession)(nil)

func (s *localSession) GetMessage(ctx context.Context, _ int, maxCapacity int) (*scaleset.RunnerScaleSetMessage, error) {
	return s.m.getMessage(ctx, s.repo, s.info.SessionID, maxCapacity)
}

func (s *localSession) DeleteMessage(_ context.Context, messageID int) error {
	return s.m.deleteMessage(s.repo, s.info.SessionID, messageID)
}

func (s *localSession) AcquireJobs(_ context.Context, requestIDs []int64) ([]int64, error) {
	return s.m.acquireJobs(s.repo, s.info.SessionID, requestIDs)
}

func (s *localSession) Session() scaleset.RunnerScaleSetSession { return s.info }

func (s *localSession) Close(context.Context) error { return s.m.closeSession(s.repo, s.info.SessionID) }
