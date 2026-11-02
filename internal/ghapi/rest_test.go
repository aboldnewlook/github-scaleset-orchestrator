package ghapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testToken = "ghp_SECRETtoken123"

type route struct {
	status int
	body   string
}

// restServer serves canned responses by "METHOD path" and checks the headers
// every GitHub REST call must send.
func restServer(t *testing.T, routes map[string]route) *restClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept = %q", got)
		}
		if got := r.Header.Get("X-GitHub-Api-Version"); got != "2022-11-28" {
			t.Errorf("X-GitHub-Api-Version = %q", got)
		}
		route, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(route.status)
		_, _ = io.WriteString(w, route.body)
	}))
	t.Cleanup(srv.Close)
	return &restClient{base: srv.URL, repo: "jrschumacher/prune", token: testToken, http: srv.Client()}
}

func TestRepoReadsVisibility(t *testing.T) {
	r := restServer(t, map[string]route{
		"GET /repos/jrschumacher/prune": {200, `{"full_name":"jrschumacher/prune","private":false,"visibility":"public"}`},
	})
	info, err := r.Repo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info != (RepoInfo{FullName: "jrschumacher/prune", Private: false, Visibility: "public"}) {
		t.Fatalf("Repo() = %+v", info)
	}
}

func TestWorkflowRunReadsHeadRepository(t *testing.T) {
	r := restServer(t, map[string]route{
		"GET /repos/jrschumacher/prune/actions/runs/42": {200, `{"id":42,"event":"pull_request","repository":{"full_name":"jrschumacher/prune"},"head_repository":{"full_name":"mallory/prune"}}`},
		"GET /repos/jrschumacher/prune/actions/runs/43": {200, `{"id":43,"event":"pull_request","repository":{"full_name":"jrschumacher/prune"},"head_repository":null}`},
	})
	run, err := r.WorkflowRun(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if run != (WorkflowRun{ID: 42, Event: "pull_request", RepoFullName: "jrschumacher/prune", HeadRepoFullName: "mallory/prune"}) {
		t.Fatalf("WorkflowRun(42) = %+v", run)
	}
	deleted, err := r.WorkflowRun(context.Background(), 43)
	if err != nil || deleted.HeadRepoFullName != "" {
		t.Fatalf("WorkflowRun(43) = %+v, %v; want an empty head repository", deleted, err)
	}
}

func TestCancelWorkflowRun(t *testing.T) {
	r := restServer(t, map[string]route{
		"POST /repos/jrschumacher/prune/actions/runs/42/cancel": {202, `{}`},
		"POST /repos/jrschumacher/prune/actions/runs/43/cancel": {409, `{"message":"Cannot cancel a workflow run that is completed."}`},
	})
	if err := r.CancelWorkflowRun(context.Background(), 42); err != nil {
		t.Fatalf("CancelWorkflowRun(42) = %v", err)
	}
	err := r.CancelWorkflowRun(context.Background(), 43)
	if err == nil || !strings.Contains(err.Error(), "409") || !strings.Contains(err.Error(), "Cannot cancel") {
		t.Fatalf("CancelWorkflowRun(43) = %v, want the 409 and GitHub's message", err)
	}
}

func TestRESTErrorsNeverContainTheToken(t *testing.T) {
	r := restServer(t, map[string]route{
		"GET /repos/jrschumacher/prune": {401, `{"message":"Bad credentials"}`},
	})
	_, err := r.Repo(context.Background())
	if err == nil {
		t.Fatal("Repo() succeeded on a 401")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("error leaks the token: %v", err)
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "Bad credentials") {
		t.Fatalf("error = %v, want the status and GitHub's message", err)
	}
}

func TestRepoFailsClosedOnIncompleteOrMismatchedResponse(t *testing.T) {
	tests := map[string]string{
		"empty object":      `{}`,
		"no visibility":     `{"full_name":"jrschumacher/prune","private":false}`,
		"mismatched name":   `{"full_name":"other/prune","private":false,"visibility":"public"}`,
		"missing full_name": `{"private":false,"visibility":"public"}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			r := restServer(t, map[string]route{"GET /repos/jrschumacher/prune": {200, body}})
			if info, err := r.Repo(context.Background()); err == nil {
				t.Fatalf("Repo() = %+v, nil; want an error", info)
			}
		})
	}
	// Case differences are not a mismatch.
	r := restServer(t, map[string]route{
		"GET /repos/jrschumacher/prune": {200, `{"full_name":"JRSchumacher/Prune","private":true,"visibility":"private"}`},
	})
	if _, err := r.Repo(context.Background()); err != nil {
		t.Fatalf("case-different full_name: %v", err)
	}
}
