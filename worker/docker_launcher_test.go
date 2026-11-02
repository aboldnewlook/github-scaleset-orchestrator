package worker_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
	"github.com/aboldnewlook/github-scaleset-orchestrator/worker"
)

// fakeEngine serves h on a unix socket and returns its unix:// host.
func fakeEngine(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix:// docker endpoints only")
	}
	dir, err := os.MkdirTemp("", "gsow") // short path: macOS caps unix socket paths at 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "unix://" + sock
}

func newDockerLauncher(t *testing.T, host string) worker.Launcher {
	t.Helper()
	dw, err := runner.NewDockerWorker(runner.DockerConfig{Image: "example/job:1", InstanceID: "gsow-box", Host: host, OnCancel: runner.DetachOnCancel}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return worker.NewDockerLauncher(dw)
}

func TestDockerLauncherClassesFollowTheEngineArchitecture(t *testing.T) {
	tests := []struct {
		arch    string
		want    string
		wantErr bool
	}{
		{"arm64", "linux-arm64", false},
		{"amd64", "linux-x64", false},
		{"s390x", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.arch, func(t *testing.T) {
			host := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1.43/version" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"Arch": tt.arch})
			})
			classes, err := newDockerLauncher(t, host).Classes(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("Classes() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && (len(classes) != 1 || classes[0].String() != tt.want) {
				t.Fatalf("Classes() = %v, want [%s]", classes, tt.want)
			}
		})
	}
}

func TestDockerLauncherStartBindsRepoAndDefaultsCacheToNone(t *testing.T) {
	var (
		mu         sync.Mutex
		gotName    string
		unexpected []string
		created    struct {
			Labels     map[string]string `json:"Labels"`
			HostConfig struct {
				Mounts []json.RawMessage `json:"Mounts"`
			} `json:"HostConfig"`
		}
	)
	host := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/v1.43/containers/create":
			gotName = r.URL.Query().Get("name")
			_ = json.NewDecoder(r.Body).Decode(&created)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"Id":"cid-1"}`)
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusNoContent)
		default:
			unexpected = append(unexpected, r.Method+" "+r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	rec := worker.RunnerRecord{Name: "gso-box-app-abc123", Repo: "acme/app", Executor: runner.ExecutorDocker}
	if err := newDockerLauncher(t, host).Start(context.Background(), rec, "jit-blob", ""); err != nil {
		t.Fatalf("Start() error: %v (unexpected engine calls: %v)", err, unexpected)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotName != rec.Name {
		t.Errorf("container name = %q, want %q", gotName, rec.Name)
	}
	if created.Labels["gso.repo"] != "acme/app" || created.Labels["gso.instance"] != "gsow-box" {
		t.Errorf("labels = %v", created.Labels)
	}
	if len(created.HostConfig.Mounts) != 0 {
		t.Errorf("cache \"\" mounted %d caches, want none", len(created.HostConfig.Mounts))
	}
}

func TestDockerLauncherKillAndRemoveOrphansUseRunnerNames(t *testing.T) {
	var (
		mu      sync.Mutex
		deleted []string
	)
	host := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1.43/containers/json":
			_, _ = io.WriteString(w, `[{"Id":"id-keep","Names":["/gso-keep"],"Labels":{"gso.managed":"true","gso.instance":"gsow-box"}},`+
				`{"Id":"id-orphan","Names":["/gso-orphan"],"Labels":{"gso.managed":"true","gso.instance":"gsow-box"}}]`)
		case r.Method == http.MethodDelete:
			deleted = append(deleted, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	l := newDockerLauncher(t, host)
	if err := l.RemoveOrphans(context.Background(), map[string]bool{"gso-keep": true}); err != nil {
		t.Fatalf("RemoveOrphans() error: %v", err)
	}
	if err := l.Kill(context.Background(), worker.RunnerRecord{Name: "gso-box-app-abc123"}); err != nil {
		t.Fatalf("Kill() error: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(deleted) != 2 || !strings.Contains(deleted[0], "orphan") || !strings.HasSuffix(deleted[1], "/gso-box-app-abc123") {
		t.Fatalf("deleted = %v, want the orphan then gso-box-app-abc123", deleted)
	}
	for _, d := range deleted {
		if strings.Contains(d, "keep") {
			t.Fatalf("removed a recorded runner: %v", deleted)
		}
	}
}
