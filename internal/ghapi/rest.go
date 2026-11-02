package ghapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const defaultAPIBase = "https://api.github.com"

// restClient calls the GitHub REST endpoints the scaleset client does not
// cover. The PAT travels only in the Authorization header. Errors carry the
// method, path, status and GitHub's message, never the token.
type restClient struct {
	base  string // tests point this at httptest
	repo  string // "owner/name"
	token string
	http  *http.Client
}

// Repo reads the repository's visibility (GET /repos/{owner}/{repo}).
// Fine-grained PATs always include Metadata: read.
func (r *restClient) Repo(ctx context.Context) (RepoInfo, error) {
	var out struct {
		FullName   string `json:"full_name"`
		Private    bool   `json:"private"`
		Visibility string `json:"visibility"`
	}
	if err := r.do(ctx, http.MethodGet, "/repos/"+r.repo, http.StatusOK, &out); err != nil {
		return RepoInfo{}, err
	}
	return RepoInfo{FullName: out.FullName, Private: out.Private, Visibility: out.Visibility}, nil
}

// WorkflowRun reads one run (GET …/actions/runs/{id}). Needs Actions: read.
func (r *restClient) WorkflowRun(ctx context.Context, runID int64) (WorkflowRun, error) {
	var out struct {
		ID         int64  `json:"id"`
		Event      string `json:"event"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		HeadRepository *struct {
			FullName string `json:"full_name"`
		} `json:"head_repository"`
	}
	path := fmt.Sprintf("/repos/%s/actions/runs/%d", r.repo, runID)
	if err := r.do(ctx, http.MethodGet, path, http.StatusOK, &out); err != nil {
		return WorkflowRun{}, err
	}
	run := WorkflowRun{ID: out.ID, Event: out.Event, RepoFullName: out.Repository.FullName}
	if out.HeadRepository != nil {
		run.HeadRepoFullName = out.HeadRepository.FullName
	}
	return run, nil
}

// CancelWorkflowRun cancels a whole run (POST …/runs/{id}/cancel, 202).
// Needs Actions: read and write. A completed run answers 409, which is
// returned as an error.
func (r *restClient) CancelWorkflowRun(ctx context.Context, runID int64) error {
	path := fmt.Sprintf("/repos/%s/actions/runs/%d/cancel", r.repo, runID)
	return r.do(ctx, http.MethodPost, path, http.StatusAccepted, nil)
}

func (r *restClient) do(ctx context.Context, method, path string, want int, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, r.base+path, nil)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := r.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body := io.LimitReader(resp.Body, 1<<20)
	if resp.StatusCode != want {
		var failure struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(body).Decode(&failure)
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, failure.Message)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, body)
		return nil
	}
	if err := json.NewDecoder(body).Decode(out); err != nil {
		return fmt.Errorf("%s %s: decoding response: %w", method, path, err)
	}
	return nil
}
