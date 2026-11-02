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
	"strings"
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
	onCancel   CancelPolicy
}

// DockerConfig configures a DockerWorker.
type DockerConfig struct {
	Image      string  // job image; must contain /usr/local/bin/gso-job and /opt/gso-runner
	InstanceID string  // stable ID that labels this manager's containers
	MemoryMB   int64   // 0 = no limit
	CPUs       float64 // 0 = no limit
	CacheRoot  string  // optional host path for per-repo caches
	CacheUID   int
	CacheGID   int
	Host       string       // engine endpoint, e.g. unix:///var/run/docker.sock; "" = DefaultDockerHost
	OnCancel   CancelPolicy // what Run does on ctx cancel; zero value KillOnCancel
}

// DefaultDockerHost is the engine endpoint used when DockerConfig.Host is empty.
const DefaultDockerHost = "unix:///var/run/docker.sock"

// DockerSocketPath returns the socket path of a unix:// engine endpoint.
// Only unix:///absolute/path is supported; Windows named pipes arrive with
// the Windows worker.
func DockerSocketPath(host string) (string, error) {
	if host == "" {
		host = DefaultDockerHost
	}
	u, err := url.Parse(host)
	if err != nil {
		return "", fmt.Errorf("parsing docker host %q: %w", host, err)
	}
	if u.Scheme != "unix" || u.Host != "" || u.Path == "" {
		return "", fmt.Errorf("docker host %q: only unix:///absolute/path endpoints are supported", host)
	}
	return u.Path, nil
}

func NewDockerWorker(cfg DockerConfig, logger *slog.Logger) (*DockerWorker, error) {
	socket, err := DockerSocketPath(cfg.Host)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	return &DockerWorker{
		image: cfg.Image, instanceID: cfg.InstanceID, memoryMB: cfg.MemoryMB, cpus: cfg.CPUs,
		cacheRoot: cfg.CacheRoot, cacheUID: cfg.CacheUID, cacheGID: cfg.CacheGID, logger: logger, onCancel: cfg.OnCancel,
		client: &http.Client{Transport: transport}, endpoint: "http://docker/v1.43",
	}, nil
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
	_, err := w.RemoveOrphans(ctx, nil)
	return err
}

// RemoveOrphans force-removes this instance's runner containers whose name is
// not in keep, and returns the names it removed (a container's ID when it has
// no name). A worker passes the names of its runner records, so containers it
// can still re-attach to survive a restart.
func (w *DockerWorker) RemoveOrphans(ctx context.Context, keep map[string]bool) ([]string, error) {
	filters, err := json.Marshal(map[string][]string{"label": {"gso.managed=true", "gso.instance=" + w.instanceID}})
	if err != nil {
		return nil, err
	}
	var containers []struct {
		ID    string   `json:"Id"`
		Names []string `json:"Names"`
	}
	path := "/containers/json?all=1&filters=" + url.QueryEscape(string(filters))
	if err := w.request(ctx, http.MethodGet, path, nil, &containers); err != nil {
		return nil, err
	}
	var removed []string
	for _, container := range containers {
		if container.ID == "" {
			continue
		}
		name := ""
		if len(container.Names) > 0 {
			name = strings.TrimPrefix(container.Names[0], "/")
		}
		if name != "" && keep[name] {
			continue
		}
		w.logger.Warn("removing stale runner container", "container", container.ID, "name", name, "instance", w.instanceID)
		err := w.request(ctx, http.MethodDelete, "/containers/"+url.PathEscape(container.ID)+"?force=true&v=true", nil, nil)
		var apiErr *dockerAPIError
		if errors.As(err, &apiErr) && apiErr.code == http.StatusNotFound {
			continue // removed by someone else meanwhile
		}
		if err != nil {
			return removed, fmt.Errorf("removing stale runner container %s: %w", container.ID, err)
		}
		if name == "" {
			name = container.ID
		}
		removed = append(removed, name)
	}
	return removed, nil
}

// ErrContainerNotFound reports that a runner container no longer exists, for
// example because it was removed while the worker was not running.
var ErrContainerNotFound = errors.New("runner container not found")

// Run starts a runner container and blocks until it exits. It is Start
// followed by Wait, except that it follows the container by the ID Docker
// returned. Cancelling ctx removes the container (KillOnCancel, gso's local
// member) or leaves it running (DetachOnCancel).
func (w *DockerWorker) Run(ctx context.Context, name, jit string) error {
	id, err := w.start(ctx, name, jit)
	if err != nil {
		return err
	}
	return w.wait(ctx, id, name)
}

// Start creates the runner container (its Docker name is the runner name) and
// starts it, then returns without waiting. Follow it with Wait; end it early
// with Kill.
func (w *DockerWorker) Start(ctx context.Context, name, jit string) error {
	_, err := w.start(ctx, name, jit)
	return err
}

// Wait follows the runner container with the given name until it stops, then
// fetches its logs and removes it. A worker uses it both right after Start
// and to re-attach after a restart. On ctx cancel, DetachOnCancel leaves the
// container running and KillOnCancel removes it. Under DetachOnCancel a wait
// failure that is not a cancel (the engine dropping the long-poll) also leaves
// the container for the caller to re-attach to. A container that no longer
// exists returns an error wrapping ErrContainerNotFound.
func (w *DockerWorker) Wait(ctx context.Context, name string) error {
	return w.wait(ctx, name, name)
}

func (w *DockerWorker) start(ctx context.Context, name, jit string) (string, error) {
	if w.repo == "" {
		return "", errors.New("docker worker has no repository")
	}
	if w.cache == "repo" && w.cacheRoot != "" {
		cacheDir := filepath.Join(w.cacheRoot, cacheVolume(w.repo))
		if err := os.MkdirAll(cacheDir, 0o770); err != nil {
			return "", fmt.Errorf("creating repository cache: %w", err)
		}
		// The image's non-root runner user needs write access to the host cache.
		if os.Geteuid() == 0 {
			if err := os.Chown(cacheDir, w.cacheUID, w.cacheGID); err != nil {
				return "", fmt.Errorf("owning repository cache: %w", err)
			}
		}
	}
	var created struct {
		ID string `json:"Id"`
	}
	path := "/containers/create?name=" + url.QueryEscape(name)
	if err := w.request(ctx, http.MethodPost, path, w.createRequest(jit), &created); err != nil {
		return "", fmt.Errorf("creating runner container: %w", err)
	}
	if created.ID == "" {
		return "", errors.New("docker returned no container ID")
	}
	w.logger.Info("runner container created", "name", name, "container", created.ID, "repo", w.repo)
	if err := w.request(ctx, http.MethodPost, "/containers/"+url.PathEscape(created.ID)+"/start", nil, nil); err != nil {
		w.remove(created.ID)
		return "", fmt.Errorf("starting runner container: %w", err)
	}
	return created.ID, nil
}

// wait follows the container ref (an ID or a name) until it stops.
func (w *DockerWorker) wait(ctx context.Context, ref, name string) error {
	removeOnReturn := true
	defer func() {
		if removeOnReturn {
			w.remove(ref)
		}
	}()
	containerPath := "/containers/" + url.PathEscape(ref)
	var result struct {
		StatusCode int64 `json:"StatusCode"`
		Error      *struct {
			Message string `json:"Message"`
		} `json:"Error"`
	}
	err := w.request(ctx, http.MethodPost, containerPath+"/wait?condition=not-running", nil, &result)
	if ctx.Err() != nil {
		if w.onCancel == DetachOnCancel {
			removeOnReturn = false
			w.logger.Info("runner container detached", "name", name, "container", ref)
			return ctx.Err()
		}
		w.logger.Info("runner container cancelled", "name", name, "container", ref)
		return ctx.Err()
	}
	var apiErr *dockerAPIError
	if errors.As(err, &apiErr) && apiErr.code == http.StatusNotFound {
		removeOnReturn = false
		return fmt.Errorf("waiting for runner container %s: %w", name, ErrContainerNotFound)
	}
	if err != nil && w.onCancel == DetachOnCancel {
		// The wait itself failed while the container may still be running
		// (dockerd dropped the long-poll). Leave it for the caller to re-attach.
		removeOnReturn = false
		return fmt.Errorf("waiting for runner container: %w", err)
	}
	// Fetch logs with an independent context, including after cancellation.
	logCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	if logErr := w.logs(logCtx, ref); logErr != nil {
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
	w.logger.Info("runner container completed", "name", name, "container", ref)
	return nil
}

// EngineArch returns the Docker engine's architecture ("amd64", "arm64"). A
// worker maps it to the linux class it can serve.
func (w *DockerWorker) EngineArch(ctx context.Context) (string, error) {
	var version struct {
		Arch string `json:"Arch"`
	}
	if err := w.request(ctx, http.MethodGet, "/version", nil, &version); err != nil {
		return "", fmt.Errorf("reading docker engine version: %w", err)
	}
	if version.Arch == "" {
		return "", errors.New("docker engine reported no architecture")
	}
	return version.Arch, nil
}

// Repo returns the repository bound by ForRepo, or "" for an unbound worker.
func (w *DockerWorker) Repo() string {
	return w.repo
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
		return &dockerAPIError{status: resp.Status, code: resp.StatusCode, message: failure.Message}
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
			return fmt.Errorf("docker log frame exceeds 16 MiB")
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

// CancelPolicy decides what Run does with a still-running container when
// its context is cancelled.
type CancelPolicy int

const (
	// KillOnCancel force-removes the container (the zero value). gso uses it:
	// shutdown, recycle and job completion all end the container.
	KillOnCancel CancelPolicy = iota
	// DetachOnCancel leaves the container running so a restarted worker can
	// re-attach to it (design spec §7, "Worker process restart"). The caller
	// owns the container and ends it with Kill.
	DetachOnCancel
)

// Kill force-removes the runner container with the given name. A container
// that no longer exists is not an error.
func (w *DockerWorker) Kill(ctx context.Context, name string) error {
	err := w.request(ctx, http.MethodDelete, "/containers/"+url.PathEscape(name)+"?force=true&v=true", nil, nil)
	var apiErr *dockerAPIError
	if errors.As(err, &apiErr) && apiErr.code == http.StatusNotFound {
		return nil
	}
	if err != nil {
		return fmt.Errorf("killing runner container %s: %w", name, err)
	}
	return nil
}

// dockerAPIError is a non-2xx Docker Engine response.
type dockerAPIError struct {
	status  string // e.g. "404 Not Found"
	code    int
	message string
}

func (e *dockerAPIError) Error() string { return fmt.Sprintf("docker API %s: %s", e.status, e.message) }
