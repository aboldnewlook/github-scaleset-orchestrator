package ghsim

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// AdminClient injects and inspects jobs on a simulator served by NewServer.
type AdminClient struct {
	h httpDoer
}

// NewAdminClient returns a client for the simulator at baseURL.
func NewAdminClient(baseURL string) *AdminClient {
	return &AdminClient{h: httpDoer{base: strings.TrimRight(baseURL, "/"), client: &http.Client{Timeout: 30 * time.Second}}}
}

// InjectJob queues count copies of spec (1-100) and returns their states.
func (a *AdminClient) InjectJob(ctx context.Context, spec JobSpec, count int) ([]JobState, error) {
	var out []JobState
	_, err := a.h.do(ctx, http.MethodPost, "/admin/jobs", InjectRequest{JobSpec: spec, Count: count}, &out)
	return out, err
}

// Jobs returns every job in ID order.
func (a *AdminClient) Jobs(ctx context.Context) ([]JobState, error) {
	var out []JobState
	_, err := a.h.do(ctx, http.MethodGet, "/admin/jobs", nil, &out)
	return out, err
}

// Runners returns every runner the simulator minted.
func (a *AdminClient) Runners(ctx context.Context) ([]RunnerInfo, error) {
	var out []RunnerInfo
	_, err := a.h.do(ctx, http.MethodGet, "/admin/runners", nil, &out)
	return out, err
}

// CancelRun cancels a workflow run.
func (a *AdminClient) CancelRun(ctx context.Context, repo string, runID int64) error {
	_, err := a.h.do(ctx, http.MethodPost, "/admin/runs/cancel", cancelRequest{Repo: repo, RunID: runID}, nil)
	return err
}
