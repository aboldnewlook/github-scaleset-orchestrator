package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// DockerWorker starts each JIT runner in a sibling Docker container. The
// manager's environment and mounts are never copied into the job container.
type DockerWorker struct {
	image      string
	instanceID string
	memoryMB   int64
	cpus       float64
	repo       string
	cache      string
	cacheRoot  string
	cacheUID   int
	cacheGID   int
	logger     *slog.Logger
	client     *http.Client
	endpoint   string
}

func NewDockerWorker(image, instanceID string, memoryMB int64, cpus float64, cacheRoot string, cacheUID, cacheGID int, logger *slog.Logger) *DockerWorker {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", "/var/run/docker.sock")
	}}
	return &DockerWorker{
		image: image, instanceID: instanceID, memoryMB: memoryMB, cpus: cpus, cacheRoot: cacheRoot, cacheUID: cacheUID, cacheGID: cacheGID, logger: logger,
		client: &http.Client{Transport: transport}, endpoint: "http://docker/v1.43",
	}
}

// ForRepo binds a repository identity and its cache policy to an executor.
func (w *DockerWorker) ForRepo(repo, cache string) *DockerWorker {
	copy := *w
	copy.repo, copy.cache = repo, cache
	return &copy
}

type dockerMount struct {
	Type   string `json:"Type"`
	Source string `json:"Source"`
	Target string `json:"Target"`
}

type dockerHostConfig struct {
	Memory      int64         `json:"Memory,omitempty"`
	NanoCPUs    int64         `json:"NanoCpus,omitempty"`
	PidsLimit   int64         `json:"PidsLimit"`
	CapDrop     []string      `json:"CapDrop"`
	SecurityOpt []string      `json:"SecurityOpt"`
	Mounts      []dockerMount `json:"Mounts,omitempty"`
}

type dockerCreateRequest struct {
	Image      string            `json:"Image"`
	Entrypoint []string          `json:"Entrypoint"`
	Env        []string          `json:"Env"`
	Labels     map[string]string `json:"Labels"`
	HostConfig dockerHostConfig  `json:"HostConfig"`
}

func cacheVolume(repo string) string {
	sum := sha256.Sum256([]byte(repo))
	return "gso-cache-" + hex.EncodeToString(sum[:12])
}

func (w *DockerWorker) createRequest(jit string) dockerCreateRequest {
	env := []string{"ACTIONS_RUNNER_INPUT_JITCONFIG=" + jit}
	host := dockerHostConfig{
		Memory: w.memoryMB * 1024 * 1024, NanoCPUs: int64(w.cpus * 1e9),
		PidsLimit: 512, CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges"},
	}
	if w.cache == "repo" {
		mount := dockerMount{Type: "volume", Source: cacheVolume(w.repo), Target: "/cache"}
		if w.cacheRoot != "" {
			mount.Type = "bind"
			mount.Source = filepath.Join(w.cacheRoot, cacheVolume(w.repo))
		}
		host.Mounts = []dockerMount{mount}
		env = append(env, "GOCACHE=/cache/go-build", "GOMODCACHE=/cache/go-mod", "NPM_CONFIG_CACHE=/cache/npm")
	}
	return dockerCreateRequest{
		Image: w.image, Entrypoint: []string{"/usr/local/bin/gso-job"}, Env: env,
		Labels: map[string]string{"gso.managed": "true", "gso.instance": w.instanceID, "gso.repo": w.repo}, HostConfig: host,
	}
}

// Reconcile removes containers left by a previous manager process with the
// same stable instance ID. A distinct ID is required for each manager.
func (w *DockerWorker) Reconcile(ctx context.Context) error {
	filters, err := json.Marshal(map[string][]string{"label": {"gso.managed=true", "gso.instance=" + w.instanceID}})
	if err != nil {
		return err
	}
	var containers []struct {
		ID string `json:"Id"`
	}
	path := "/containers/json?all=1&filters=" + url.QueryEscape(string(filters))
	if err := w.request(ctx, http.MethodGet, path, nil, &containers); err != nil {
		return err
	}
	for _, container := range containers {
		if container.ID == "" {
			continue
		}
		w.logger.Warn("removing stale runner container", "container", container.ID, "instance", w.instanceID)
		if err := w.request(ctx, http.MethodDelete, "/containers/"+url.PathEscape(container.ID)+"?force=true&v=true", nil, nil); err != nil {
			return fmt.Errorf("removing stale runner container %s: %w", container.ID, err)
		}
	}
	return nil
}

func (w *DockerWorker) Run(ctx context.Context, name, jit string) error {
	if w.repo == "" {
		return errors.New("docker worker has no repository")
	}
	if w.cache == "repo" && w.cacheRoot != "" {
		cacheDir := filepath.Join(w.cacheRoot, cacheVolume(w.repo))
		if err := os.MkdirAll(cacheDir, 0o770); err != nil {
			return fmt.Errorf("creating repository cache: %w", err)
		}
		// The image's non-root runner user needs write access to the host cache.
		if os.Geteuid() == 0 {
			if err := os.Chown(cacheDir, w.cacheUID, w.cacheGID); err != nil {
				return fmt.Errorf("owning repository cache: %w", err)
			}
		}
	}
	var created struct {
		ID string `json:"Id"`
	}
	path := "/containers/create?name=" + url.QueryEscape(name)
	if err := w.request(ctx, http.MethodPost, path, w.createRequest(jit), &created); err != nil {
		return fmt.Errorf("creating runner container: %w", err)
	}
	if created.ID == "" {
		return errors.New("docker returned no container ID")
	}
	defer w.remove(created.ID)
	w.logger.Info("runner container created", "name", name, "container", created.ID, "repo", w.repo)

	containerPath := "/containers/" + url.PathEscape(created.ID)
	if err := w.request(ctx, http.MethodPost, containerPath+"/start", nil, nil); err != nil {
		return fmt.Errorf("starting runner container: %w", err)
	}
	var result struct {
		StatusCode int64 `json:"StatusCode"`
		Error      *struct {
			Message string `json:"Message"`
		} `json:"Error"`
	}
	err := w.request(ctx, http.MethodPost, containerPath+"/wait?condition=not-running", nil, &result)
	if ctx.Err() != nil {
		w.logger.Info("runner container cancelled", "name", name, "container", created.ID)
		return ctx.Err()
	}
	// Fetch logs with an independent context, including after cancellation.
	logCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	if logErr := w.logs(logCtx, created.ID); logErr != nil {
		w.logger.Warn("runner container logs unavailable", "name", name, "error", logErr)
	}
	cancel()
	if err != nil {
		return fmt.Errorf("waiting for runner container: %w", err)
	}
	if result.Error != nil {
		return fmt.Errorf("runner container error: %s", result.Error.Message)
	}
	if result.StatusCode != 0 {
		return fmt.Errorf("runner container exited with status %d", result.StatusCode)
	}
	w.logger.Info("runner container completed", "name", name, "container", created.ID)
	return nil
}

func (w *DockerWorker) remove(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := w.request(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id)+"?force=true&v=true", nil, nil); err != nil {
		w.logger.Warn("failed to remove runner container", "container", id, "error", err)
	}
}

func (w *DockerWorker) request(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, w.endpoint+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var failure struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&failure)
		if failure.Message == "" {
			failure.Message = resp.Status
		}
		return fmt.Errorf("docker API %s: %s", resp.Status, failure.Message)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (w *DockerWorker) logs(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		w.endpoint+"/containers/"+url.PathEscape(id)+"/logs?stdout=true&stderr=true", nil)
	if err != nil {
		return err
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docker logs: %s", resp.Status)
	}
	// Docker multiplexes stdout and stderr in 8-byte framed chunks when TTY is false.
	for {
		var header [8]byte
		if _, err := io.ReadFull(resp.Body, header[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		size := binary.BigEndian.Uint32(header[4:])
		if size > 16<<20 {
			return fmt.Errorf("Docker log frame exceeds 16 MiB")
		}
		dest := io.Writer(os.Stdout)
		if header[0] == 2 {
			dest = os.Stderr
		}
		if _, err := io.CopyN(dest, resp.Body, int64(size)); err != nil {
			return err
		}
	}
}
