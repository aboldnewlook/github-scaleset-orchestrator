package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestDockerIntegration drives DockerWorker against a real engine. It runs only
// with GSO_DOCKER_IT=1 (task test:docker). GSO_DOCKER_HOST selects the engine
// (default unix:///var/run/docker.sock; on Docker Desktop for Mac use
// unix://$HOME/.docker/run/docker.sock).
func TestDockerIntegration(t *testing.T) {
	if os.Getenv("GSO_DOCKER_IT") != "1" {
		t.Skip("set GSO_DOCKER_IT=1 to run against a real Docker engine")
	}
	dockerBin, err := exec.LookPath("docker")
	if err != nil {
		t.Fatal("GSO_DOCKER_IT=1 but the docker CLI is not on PATH")
	}
	host := os.Getenv("GSO_DOCKER_HOST")
	dockerCmd := func(ctx context.Context, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, dockerBin, args...)
		if host != "" {
			cmd.Env = append(os.Environ(), "DOCKER_HOST="+host)
		}
		return cmd
	}

	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000_000)
	tag := "gso-it-jobimage:" + suffix
	buildCtx, cancelBuild := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelBuild()
	if out, err := dockerCmd(buildCtx, "build", "-q", "-t", tag, filepath.Join("testdata", "jobimage")).CombinedOutput(); err != nil {
		t.Fatalf("docker build: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = dockerCmd(context.Background(), "rmi", "-f", tag).Run() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	instance := "gso-it-" + suffix
	newWorker := func(policy CancelPolicy) *DockerWorker {
		w, err := NewDockerWorker(DockerConfig{Image: tag, InstanceID: instance, Host: host, OnCancel: policy}, logger)
		if err != nil {
			t.Fatal(err)
		}
		return w.ForRepo("owner/it", "none")
	}
	t.Cleanup(func() { _, _ = newWorker(KillOnCancel).RemoveOrphans(context.Background(), nil) })

	state := func(w *DockerWorker, name string) (exists, running bool) {
		t.Helper()
		var info struct {
			State struct {
				Running bool `json:"Running"`
			} `json:"State"`
		}
		err := w.request(context.Background(), http.MethodGet, "/containers/"+url.PathEscape(name)+"/json", nil, &info)
		var apiErr *dockerAPIError
		if errors.As(err, &apiErr) && apiErr.code == http.StatusNotFound {
			return false, false
		}
		if err != nil {
			t.Fatalf("inspecting %s: %v", name, err)
		}
		return true, info.State.Running
	}

	t.Run("engine arch", func(t *testing.T) {
		arch, err := newWorker(DetachOnCancel).EngineArch(context.Background())
		if err != nil || (arch != "amd64" && arch != "arm64") {
			t.Fatalf("EngineArch() = %q, %v", arch, err)
		}
	})

	t.Run("detach on cancel then re-attach", func(t *testing.T) {
		w := newWorker(DetachOnCancel)
		name := "gso-it-detach-" + suffix
		if err := w.Start(context.Background(), name, "sleep=4,exit=0"); err != nil {
			t.Fatal(err)
		}
		short, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		err := w.Wait(short, name)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("first Wait error = %v, want deadline exceeded", err)
		}
		if exists, running := state(w, name); !exists || !running {
			t.Fatalf("after detach: exists=%v running=%v, want a running container", exists, running)
		}
		long, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := w.Wait(long, name); err != nil {
			t.Fatalf("re-attached Wait error = %v", err)
		}
		if exists, _ := state(w, name); exists {
			t.Fatal("exited container was not removed")
		}
	})

	t.Run("non-zero exit", func(t *testing.T) {
		w := newWorker(DetachOnCancel)
		name := "gso-it-exit-" + suffix
		if err := w.Start(context.Background(), name, "sleep=0,exit=3"); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := w.Wait(ctx, name); err == nil || !strings.Contains(err.Error(), "status 3") {
			t.Fatalf("Wait error = %v, want exit status 3", err)
		}
		if exists, _ := state(w, name); exists {
			t.Fatal("failed container was not removed")
		}
	})

	t.Run("kill", func(t *testing.T) {
		w := newWorker(DetachOnCancel)
		name := "gso-it-kill-" + suffix
		if err := w.Start(context.Background(), name, "sleep=30"); err != nil {
			t.Fatal(err)
		}
		if err := w.Kill(context.Background(), name); err != nil {
			t.Fatal(err)
		}
		if exists, _ := state(w, name); exists {
			t.Fatal("killed container still exists")
		}
		if err := w.Kill(context.Background(), name); err != nil {
			t.Fatalf("second Kill = %v, want nil for a missing container", err)
		}
		if err := w.Wait(context.Background(), name); !errors.Is(err, ErrContainerNotFound) {
			t.Fatalf("Wait on a killed container = %v, want ErrContainerNotFound", err)
		}
	})

	t.Run("remove orphans keeps recorded names", func(t *testing.T) {
		w := newWorker(DetachOnCancel)
		keep, orphan := "gso-it-keep-"+suffix, "gso-it-orphan-"+suffix
		for _, name := range []string{keep, orphan} {
			if err := w.Start(context.Background(), name, "sleep=30"); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { _ = w.Kill(context.Background(), keep) })
		removed, err := w.RemoveOrphans(context.Background(), map[string]bool{keep: true})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(removed, orphan) || slices.Contains(removed, keep) {
			t.Fatalf("removed = %v, want %s and not %s", removed, orphan, keep)
		}
		if exists, running := state(w, keep); !exists || !running {
			t.Fatal("recorded container was removed")
		}
		if exists, _ := state(w, orphan); exists {
			t.Fatal("orphan container still exists")
		}
	})
}
