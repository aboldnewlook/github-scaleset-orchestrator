package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func dockerTestWorker(server *httptest.Server) *DockerWorker {
	w := NewDockerWorker("example/job:sha", "test-instance", 3072, 1.5, "", 1001, 1001, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.client = server.Client()
	w.endpoint = server.URL + "/v1.43"
	return w.ForRepo("owner/repo", "repo")
}

func TestDockerWorkerJobIsolationAndCleanup(t *testing.T) {
	var mu sync.Mutex
	var created dockerCreateRequest
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/v1.43/containers/create":
			if got := r.URL.Query().Get("name"); got != "gso-test" {
				t.Errorf("container name = %q", got)
			}
			if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
				t.Errorf("decode create: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"Id":"container-123"}`)
		case "/v1.43/containers/container-123/start":
			w.WriteHeader(http.StatusNoContent)
		case "/v1.43/containers/container-123/wait":
			_, _ = io.WriteString(w, `{"StatusCode":0}`)
		case "/v1.43/containers/container-123/logs":
			w.WriteHeader(http.StatusOK)
		case "/v1.43/containers/container-123":
			if r.Method != http.MethodDelete || r.URL.Query().Get("force") != "true" {
				t.Errorf("cleanup = %s %s", r.Method, r.URL.String())
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected Docker API path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	w := dockerTestWorker(server)
	t.Setenv("GITHUB_TOKEN", "manager-secret")
	t.Setenv("GSO_CONTROL_TOKEN", "manager-control")
	if err := w.Run(context.Background(), "gso-test", "jit-secret"); err != nil {
		t.Fatal(err)
	}
	if created.Image != "example/job:sha" || len(created.Entrypoint) != 1 || created.Entrypoint[0] != "/usr/local/bin/gso-job" {
		t.Fatalf("wrong image or entrypoint: %+v", created)
	}
	joined := strings.Join(created.Env, "\n")
	if !strings.Contains(joined, "ACTIONS_RUNNER_INPUT_JITCONFIG=jit-secret") || strings.Contains(joined, "manager-secret") || strings.Contains(joined, "manager-control") {
		t.Fatalf("job environment has missing JIT config or manager credentials: %q", joined)
	}
	if created.HostConfig.Memory != 3072*1024*1024 || created.HostConfig.NanoCPUs != 1_500_000_000 {
		t.Errorf("resource limits not applied: %+v", created.HostConfig)
	}
	if len(created.HostConfig.Mounts) != 1 || created.HostConfig.Mounts[0] != (dockerMount{Type: "volume", Source: cacheVolume("owner/repo"), Target: "/cache"}) {
		t.Errorf("unexpected mounts: %+v", created.HostConfig.Mounts)
	}
	if strings.Contains(stringMustMarshal(created), "/var/run/docker.sock") {
		t.Error("job container has Docker socket")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 5 || paths[4] != "DELETE /v1.43/containers/container-123" {
		t.Errorf("Docker lifecycle requests = %v", paths)
	}
}

func TestDockerWorkerCancellationRemovesContainer(t *testing.T) {
	waiting := make(chan struct{})
	removed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1.43/containers/create":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"Id":"cancel-me"}`)
		case "/v1.43/containers/cancel-me/start":
			w.WriteHeader(http.StatusNoContent)
		case "/v1.43/containers/cancel-me/wait":
			close(waiting)
			<-r.Context().Done()
		case "/v1.43/containers/cancel-me":
			if r.Method != http.MethodDelete || r.URL.Query().Get("force") != "true" {
				t.Errorf("cleanup = %s %s", r.Method, r.URL.String())
			}
			close(removed)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	w := dockerTestWorker(server)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx, "gso-cancel", "jit") }()
	select {
	case <-waiting:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not reach wait")
	}
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("Run error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
	select {
	case <-removed:
	case <-time.After(2 * time.Second):
		t.Fatal("container not removed")
	}
}

func TestDockerCachePolicies(t *testing.T) {
	w := NewDockerWorker("job", "test-instance", 0, 0, "/mnt/cache/gso-job-cache", 1001, 1001, slog.New(slog.NewTextHandler(io.Discard, nil)))
	trusted := w.ForRepo("owner/trusted", "repo").createRequest("jit")
	untrusted := w.ForRepo("owner/untrusted", "none").createRequest("jit")
	if len(trusted.HostConfig.Mounts) != 1 || trusted.HostConfig.Mounts[0].Type != "bind" || trusted.HostConfig.Mounts[0].Source != "/mnt/cache/gso-job-cache/"+cacheVolume("owner/trusted") {
		t.Fatalf("trusted cache mount = %+v", trusted.HostConfig.Mounts)
	}
	if len(untrusted.HostConfig.Mounts) != 0 || len(untrusted.Env) != 1 {
		t.Fatalf("untrusted job got persistent cache: %+v", untrusted)
	}
	if cacheVolume("owner/trusted") == cacheVolume("other/trusted") {
		t.Fatal("cache namespace collision")
	}
}

func TestDockerWorkerReconcileOnlyOwnInstance(t *testing.T) {
	var deleted string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1.43/containers/json" {
			var filters map[string][]string
			if err := json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters); err != nil {
				t.Errorf("filters: %v", err)
			}
			if strings.Join(filters["label"], ",") != "gso.managed=true,gso.instance=test-instance" {
				t.Errorf("wrong filters: %+v", filters)
			}
			_, _ = io.WriteString(w, `[{"Id":"stale-container"}]`)
			return
		}
		if r.URL.Path == "/v1.43/containers/stale-container" && r.Method == http.MethodDelete {
			deleted = r.URL.Query().Get("force")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t.Errorf("unexpected Docker API request %s %s", r.Method, r.URL.String())
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	if err := dockerTestWorker(server).Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if deleted != "true" {
		t.Fatal("stale container was not force removed")
	}
}

func stringMustMarshal(v any) string { data, _ := json.Marshal(v); return string(data) }
