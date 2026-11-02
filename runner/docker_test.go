package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func dockerTestWorker(server *httptest.Server) *DockerWorker {
	w, err := NewDockerWorker(DockerConfig{Image: "example/job:sha", InstanceID: "test-instance", MemoryMB: 3072, CPUs: 1.5, CacheUID: 1001, CacheGID: 1001}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		panic(err)
	}
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
	w, err := NewDockerWorker(DockerConfig{Image: "job", InstanceID: "test-instance", CacheRoot: "/mnt/cache/gso-job-cache", CacheUID: 1001, CacheGID: 1001}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
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

func TestDockerSocketPath(t *testing.T) {
	tests := []struct {
		host    string
		want    string
		wantErr bool
	}{
		{"", "/var/run/docker.sock", false},
		{"unix:///var/run/docker.sock", "/var/run/docker.sock", false},
		{"unix:///Users/me/.docker/run/docker.sock", "/Users/me/.docker/run/docker.sock", false},
		{"tcp://127.0.0.1:2375", "", true},
		{"npipe:////./pipe/docker_engine", "", true},
		{"unix://", "", true},
		{"unix://docker.sock", "", true},
		{"/var/run/docker.sock", "", true},
	}
	for _, tt := range tests {
		got, err := DockerSocketPath(tt.host)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("DockerSocketPath(%q) = %q, %v; want %q, err=%v", tt.host, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestDockerWorkerDialsConfiguredSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix:// endpoints only")
	}
	dir, err := os.MkdirTemp("", "gsod") // short path: macOS caps unix socket paths at 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/v1.43/containers/json" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, "[]")
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	w, err := NewDockerWorker(DockerConfig{Image: "img", InstanceID: "i", Host: "unix://" + sock}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("configured socket saw %d requests, want 1", got)
	}
}

func TestNewDockerWorkerRejectsUnsupportedHost(t *testing.T) {
	if _, err := NewDockerWorker(DockerConfig{Image: "img", InstanceID: "i", Host: "tcp://127.0.0.1:2375"}, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("NewDockerWorker accepted a tcp:// host")
	}
}

func dockerTestWorkerWith(server *httptest.Server, cfg DockerConfig) *DockerWorker {
	w, err := NewDockerWorker(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		panic(err)
	}
	w.client = server.Client()
	w.endpoint = server.URL + "/v1.43"
	return w.ForRepo("owner/repo", "none")
}

func TestDockerWorkerDetachOnCancelLeavesContainerRunning(t *testing.T) {
	waiting := make(chan struct{})
	var deletes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1.43/containers/create":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"Id":"keep-me"}`)
		case r.URL.Path == "/v1.43/containers/keep-me/start":
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/v1.43/containers/keep-me/wait":
			close(waiting)
			<-r.Context().Done()
		case r.Method == http.MethodDelete:
			deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	w := dockerTestWorkerWith(server, DockerConfig{Image: "example/job:sha", InstanceID: "test-instance", OnCancel: DetachOnCancel})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx, "gso-detach", "jit") }()
	select {
	case <-waiting:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not reach wait")
	}
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("Run error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	// Run's deferred cleanup has finished by the time it returns.
	if n := deletes.Load(); n != 0 {
		t.Fatalf("detached container was deleted %d times, want 0", n)
	}
}

func TestDockerWorkerKill(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{"removed", http.StatusNoContent, "", false},
		{"already gone", http.StatusNotFound, `{"message":"No such container: gso-x"}`, false},
		{"engine error", http.StatusInternalServerError, `{"message":"boom"}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete || r.URL.Path != "/v1.43/containers/gso-x" || r.URL.Query().Get("force") != "true" {
					t.Errorf("request = %s %s", r.Method, r.URL.String())
				}
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()
			err := dockerTestWorker(server).Kill(context.Background(), "gso-x")
			if (err != nil) != tt.wantErr {
				t.Fatalf("Kill() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
func TestDockerWorkerStartCreatesAndStartsWithoutWaiting(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/v1.43/containers/create":
			if got := r.URL.Query().Get("name"); got != "gso-start" {
				t.Errorf("container name = %q", got)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"Id":"started-1"}`)
		case "/v1.43/containers/started-1/start":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	w := dockerTestWorkerWith(server, DockerConfig{Image: "example/job:sha", InstanceID: "test-instance", OnCancel: DetachOnCancel})
	if err := w.Start(context.Background(), "gso-start", "jit"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := "POST /v1.43/containers/create,POST /v1.43/containers/started-1/start"
	if got := strings.Join(paths, ","); got != want {
		t.Fatalf("requests = %s, want %s", got, want)
	}
}

func TestDockerWorkerStartRemovesContainerWhenStartFails(t *testing.T) {
	var deletes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1.43/containers/create":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"Id":"broken-1"}`)
		case r.URL.Path == "/v1.43/containers/broken-1/start":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"message":"cannot start"}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1.43/containers/broken-1":
			deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	w := dockerTestWorkerWith(server, DockerConfig{Image: "example/job:sha", InstanceID: "test-instance", OnCancel: DetachOnCancel})
	err := w.Start(context.Background(), "gso-broken", "jit")
	if err == nil || !strings.Contains(err.Error(), "starting runner container") {
		t.Fatalf("Start error = %v", err)
	}
	if n := deletes.Load(); n != 1 {
		t.Fatalf("container removed %d times after a failed start, want 1", n)
	}
}

func TestDockerWorkerWaitFollowsContainerByName(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/v1.43/containers/gso-attach/wait":
			_, _ = io.WriteString(w, `{"StatusCode":0}`)
		case "/v1.43/containers/gso-attach/logs":
			w.WriteHeader(http.StatusOK)
		case "/v1.43/containers/gso-attach":
			if r.Method != http.MethodDelete || r.URL.Query().Get("force") != "true" {
				t.Errorf("cleanup = %s %s", r.Method, r.URL.String())
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	w := dockerTestWorkerWith(server, DockerConfig{Image: "example/job:sha", InstanceID: "test-instance", OnCancel: DetachOnCancel})
	if err := w.Wait(context.Background(), "gso-attach"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := "POST /v1.43/containers/gso-attach/wait,GET /v1.43/containers/gso-attach/logs,DELETE /v1.43/containers/gso-attach"
	if got := strings.Join(paths, ","); got != want {
		t.Fatalf("requests = %s, want %s", got, want)
	}
}

func TestDockerWorkerWaitOnCancel(t *testing.T) {
	tests := []struct {
		name        string
		policy      CancelPolicy
		wantDeletes int32
	}{
		{"detach leaves the container", DetachOnCancel, 0},
		{"kill removes the container", KillOnCancel, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			waiting := make(chan struct{})
			var deletes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/v1.43/containers/gso-follow/wait":
					close(waiting)
					<-r.Context().Done()
				case r.Method == http.MethodDelete && r.URL.Path == "/v1.43/containers/gso-follow":
					deletes.Add(1)
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			w := dockerTestWorkerWith(server, DockerConfig{Image: "example/job:sha", InstanceID: "test-instance", OnCancel: tt.policy})
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- w.Wait(ctx, "gso-follow") }()
			select {
			case <-waiting:
			case <-time.After(2 * time.Second):
				t.Fatal("Wait did not reach the engine")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Wait error = %v, want context.Canceled", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Wait did not return after cancel")
			}
			if n := deletes.Load(); n != tt.wantDeletes {
				t.Fatalf("deletes = %d, want %d", n, tt.wantDeletes)
			}
		})
	}
}

func TestDockerWorkerWaitReportsExitStatus(t *testing.T) {
	var deletes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1.43/containers/gso-fail/wait":
			_, _ = io.WriteString(w, `{"StatusCode":3}`)
		case r.URL.Path == "/v1.43/containers/gso-fail/logs":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1.43/containers/gso-fail":
			deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	w := dockerTestWorkerWith(server, DockerConfig{Image: "example/job:sha", InstanceID: "test-instance", OnCancel: DetachOnCancel})
	err := w.Wait(context.Background(), "gso-fail")
	if err == nil || !strings.Contains(err.Error(), "status 3") {
		t.Fatalf("Wait error = %v, want exit status 3", err)
	}
	if n := deletes.Load(); n != 1 {
		t.Fatalf("exited container removed %d times, want 1", n)
	}
}

func TestDockerWorkerWaitMissingContainer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1.43/containers/gso-gone/wait" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"No such container: gso-gone"}`)
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	w := dockerTestWorkerWith(server, DockerConfig{Image: "example/job:sha", InstanceID: "test-instance", OnCancel: DetachOnCancel})
	err := w.Wait(context.Background(), "gso-gone")
	if !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("Wait error = %v, want ErrContainerNotFound", err)
	}
}

func TestDockerWorkerRemoveOrphansKeepsRecordedContainers(t *testing.T) {
	var mu sync.Mutex
	var deleted []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1.43/containers/json":
			var filters map[string][]string
			if err := json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters); err != nil {
				t.Errorf("filters: %v", err)
			}
			if got := strings.Join(filters["label"], ","); got != "gso.managed=true,gso.instance=test-instance" {
				t.Errorf("label filters = %s", got)
			}
			_, _ = io.WriteString(w, `[{"Id":"id-keep","Names":["/gso-keep"]},{"Id":"id-orphan","Names":["/gso-orphan"]},{"Id":"id-noname"},{"Id":"id-vanished","Names":["/gso-vanished"]}]`)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1.43/containers/id-vanished":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"No such container"}`)
		case r.Method == http.MethodDelete:
			if r.URL.Query().Get("force") != "true" {
				t.Errorf("orphan removal not forced: %s", r.URL.String())
			}
			mu.Lock()
			deleted = append(deleted, strings.TrimPrefix(r.URL.Path, "/v1.43/containers/"))
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	w := dockerTestWorkerWith(server, DockerConfig{Image: "example/job:sha", InstanceID: "test-instance", OnCancel: DetachOnCancel})
	removed, err := w.RemoveOrphans(context.Background(), map[string]bool{"gso-keep": true})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(removed, ","); got != "gso-orphan,id-noname" {
		t.Fatalf("removed = %s, want gso-orphan,id-noname", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(deleted, ","); got != "id-orphan,id-noname" {
		t.Fatalf("deleted IDs = %s, want id-orphan,id-noname", got)
	}
}

func TestDockerWorkerEngineArch(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    string
		wantErr bool
	}{
		{"arm64 engine", `{"Arch":"arm64","Os":"linux"}`, "arm64", false},
		{"amd64 engine", `{"Arch":"amd64","Os":"linux"}`, "amd64", false},
		{"no arch", `{}`, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1.43/version" {
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
				}
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()
			got, err := dockerTestWorker(server).EngineArch(context.Background())
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("EngineArch() = %q, %v; want %q, err=%v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestDockerWorkerRepo(t *testing.T) {
	w, err := NewDockerWorker(DockerConfig{Image: "img", InstanceID: "i"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if got := w.Repo(); got != "" {
		t.Fatalf("unbound Repo() = %q, want empty", got)
	}
	if got := w.ForRepo("owner/repo", "none").Repo(); got != "owner/repo" {
		t.Fatalf("bound Repo() = %q, want owner/repo", got)
	}
}

// A wait failure that is not a ctx cancel (dockerd dropping the long-poll) must
// not remove a live container under DetachOnCancel: the caller re-attaches.
func TestDockerWorkerWaitErrorDetachKeepsContainer(t *testing.T) {
	tests := []struct {
		name        string
		policy      CancelPolicy
		wantDeletes int32
	}{
		{"detach keeps the live container", DetachOnCancel, 0},
		{"kill removes it", KillOnCancel, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var deletes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/v1.43/containers/gso-drop/wait":
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = io.WriteString(w, `{"message":"connection dropped"}`)
				case r.URL.Path == "/v1.43/containers/gso-drop/logs":
					w.WriteHeader(http.StatusOK)
				case r.Method == http.MethodDelete && r.URL.Path == "/v1.43/containers/gso-drop":
					deletes.Add(1)
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			w := dockerTestWorkerWith(server, DockerConfig{Image: "example/job:sha", InstanceID: "test-instance", OnCancel: tt.policy})
			err := w.Wait(context.Background(), "gso-drop")
			if err == nil || !strings.Contains(err.Error(), "waiting for runner container") {
				t.Fatalf("Wait error = %v, want a wait failure", err)
			}
			if got, want := errors.Is(err, ErrWaitInterrupted), tt.policy == DetachOnCancel; got != want {
				t.Fatalf("errors.Is(err, ErrWaitInterrupted) = %v, want %v (error: %v)", got, want, err)
			}
			if n := deletes.Load(); n != tt.wantDeletes {
				t.Fatalf("deletes = %d, want %d", n, tt.wantDeletes)
			}
		})
	}
}
