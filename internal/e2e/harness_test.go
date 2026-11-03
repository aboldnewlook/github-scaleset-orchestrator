//go:build !windows

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghsim"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
	"github.com/aboldnewlook/github-scaleset-orchestrator/worker"
)

const defaultRepos = "repos:\n  - name: o/a\n  - name: o/b\n"

const timingScale = "0.02"

// proc is one gso process whose output goes to a log file.
type proc struct {
	t       *testing.T
	name    string
	env     []string
	args    []string
	logPath string

	mu   sync.Mutex
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func newProc(t *testing.T, name string, env []string, args ...string) *proc {
	t.Helper()
	p := &proc{t: t, name: name, env: env, args: args, logPath: filepath.Join(t.TempDir(), name+".log")}
	p.start()
	t.Cleanup(func() {
		p.stop()
		if t.Failed() {
			t.Logf("---- %s log (tail) ----\n%s", p.name, tail(p.log(), 60))
		}
	})
	return p
}

func (p *proc) start() {
	p.t.Helper()
	f, err := os.OpenFile(p.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		p.t.Fatal(err)
	}
	_, _ = fmt.Fprintf(f, "=== start %s\n", p.name)
	cmd := exec.Command(gsoBin, p.args...)
	cmd.Env = p.env
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		p.t.Fatalf("starting %s: %v", p.name, err)
	}
	done := make(chan struct{})
	p.mu.Lock()
	p.cmd, p.done, p.err = cmd, done, nil
	p.mu.Unlock()
	go func() {
		err := cmd.Wait()
		_ = f.Close()
		p.mu.Lock()
		p.err = err
		p.mu.Unlock()
		close(done)
	}()
}

func (p *proc) signal(sig os.Signal) {
	p.mu.Lock()
	cmd := p.cmd
	p.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(sig)
	}
}

// waitExit returns the exit error and true once the process has exited.
func (p *proc) waitExit(timeout time.Duration) (error, bool) {
	p.mu.Lock()
	done := p.done
	p.mu.Unlock()
	select {
	case <-done:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.err, true
	case <-time.After(timeout):
		return nil, false
	}
}

func (p *proc) stop() {
	if _, exited := p.waitExit(0); exited {
		return
	}
	p.signal(syscall.SIGTERM)
	if _, ok := p.waitExit(5 * time.Second); !ok {
		p.signal(syscall.SIGKILL)
		_, _ = p.waitExit(5 * time.Second)
	}
}

func (p *proc) kill() {
	p.signal(syscall.SIGKILL)
	_, _ = p.waitExit(5 * time.Second)
}

func (p *proc) restart(kill bool) {
	if kill {
		p.kill()
	} else {
		p.stop()
	}
	p.start()
}

func (p *proc) log() string {
	b, _ := os.ReadFile(p.logPath)
	return string(b)
}

// tail returns the last n lines of s, without the listener's per-poll chatter.
func tail(s string, n int) string {
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if !strings.Contains(l, "Getting next message") {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// shortDir is a temp dir with a short path (unix socket paths are limited).
func shortDir(t *testing.T, prefix string) string {
	t.Helper()
	d, err := os.MkdirTemp("", prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func isolatedEnv(t *testing.T, dir string) []string {
	t.Helper()
	for _, sub := range []string{"home", "cfg", "cache", "run", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + filepath.Join(dir, "home"),
		"XDG_CONFIG_HOME=" + filepath.Join(dir, "cfg"),
		"XDG_CACHE_HOME=" + filepath.Join(dir, "cache"),
		"XDG_RUNTIME_DIR=" + filepath.Join(dir, "run"),
		"TMPDIR=" + filepath.Join(dir, "tmp"),
		"GSO_TIMING_SCALE=" + timingScale,
	}
}

// socketFor mirrors control.SocketPath for an isolatedEnv rooted at dir.
func socketFor(dir string) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(dir, "tmp", fmt.Sprintf("gso-%d.sock", os.Getuid()))
	}
	return filepath.Join(dir, "run", "gso.sock")
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func waitLogLine(t *testing.T, p *proc, prefix string, timeout time.Duration) string {
	t.Helper()
	var found string
	waitFor(t, timeout, fmt.Sprintf("%s to log %q", p.name, prefix), func() bool {
		sc := bufio.NewScanner(strings.NewReader(p.log()))
		for sc.Scan() {
			if rest, ok := strings.CutPrefix(sc.Text(), prefix); ok {
				found = strings.TrimSpace(rest)
				return true
			}
		}
		return false
	})
	return found
}

func runGSO(t *testing.T, env []string, args ...string) string {
	t.Helper()
	stdout, stderr, err := runGSOResult(env, args...)
	if err != nil {
		t.Fatalf("gso %s: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, stdout, stderr)
	}
	return stdout
}

func runGSOResult(env []string, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, gsoBin, args...)
	cmd.Env = env // nil inherits the test's environment
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

func simClass() string {
	c, err := runner.ClassFor("linux", runtime.GOARCH)
	if err != nil {
		return "linux-x64"
	}
	return c.String()
}

// --- simulator ---

type simProc struct {
	*proc
	URL string
	// Advertise is the URL job containers dial (the host of the JIT serverUrl);
	// workers need it as --github-sim to trust the simulator's job configs.
	Advertise string
	admin     *ghsim.AdminClient
}

func startSim(t *testing.T) *simProc { return startSimAt(t, "127.0.0.1:0", "") }

// startSimAt starts `gso sim serve`; advertise is the URL job containers dial
// ("" = the listen address).
func startSimAt(t *testing.T, listen, advertise string) *simProc {
	t.Helper()
	args := []string{"sim", "serve", "--listen", listen, "--claim-timeout", "10s"}
	if advertise != "" {
		args = append(args, "--advertise", advertise)
	}
	p := newProc(t, "sim", isolatedEnv(t, shortDir(t, "gs")), args...)
	addr := waitLogLine(t, p, "listening on ", 15*time.Second)
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("sim address %q: %v", addr, err)
	}
	url := "http://127.0.0.1:" + port
	waitFor(t, 10*time.Second, "the simulator to answer", func() bool {
		resp, err := http.Get(url + "/healthz")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	if advertise == "" {
		advertise = url
	}
	return &simProc{proc: p, URL: url, Advertise: advertise, admin: ghsim.NewAdminClient(url)}
}

func injectJobs(t *testing.T, sim *simProc, args ...string) []int64 {
	t.Helper()
	out := runGSO(t, nil, append([]string{"sim", "job", "--sim", sim.URL, "--json"}, args...)...)
	var jobs []ghsim.JobState
	if err := json.Unmarshal([]byte(out), &jobs); err != nil || len(jobs) == 0 {
		t.Fatalf("gso sim job output %q: %v", out, err)
	}
	ids := make([]int64, len(jobs))
	for i, j := range jobs {
		ids[i] = j.ID
	}
	return ids
}

// injectJob works before any router exists: the job stays queued until a
// scale set with capacity polls.
func injectJob(t *testing.T, sim *simProc, args ...string) int64 {
	t.Helper()
	return injectJobs(t, sim, args...)[0]
}

func jobNow(t *testing.T, sim *simProc, id int64) ghsim.JobState {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	jobs, err := sim.admin.Jobs(ctx)
	if err != nil {
		t.Fatalf("listing simulator jobs: %v", err)
	}
	for _, j := range jobs {
		if j.ID == id {
			return j
		}
	}
	t.Fatalf("the simulator has no job %d", id)
	return ghsim.JobState{}
}

func terminal(state string) bool { return state == ghsim.JobCompleted || state == ghsim.JobCancelled }

func waitJob(t *testing.T, sim *simProc, id int64, state string, timeout time.Duration) ghsim.JobState {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		j := jobNow(t, sim, id)
		if j.State == state {
			return j
		}
		if terminal(j.State) {
			t.Fatalf("job %d ended %s (result %q) while waiting for %s", id, j.State, j.Result, state)
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %d is %s after %s, want %s", id, j.State, timeout, state)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func simRunners(t *testing.T, sim *simProc, repo string) []ghsim.RunnerInfo {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	all, err := sim.admin.Runners(ctx)
	if err != nil {
		t.Fatalf("listing simulator runners: %v", err)
	}
	var out []ghsim.RunnerInfo
	for _, r := range all {
		if r.Repo == repo {
			out = append(out, r)
		}
	}
	return out
}

// --- router ---

type routerProc struct {
	*proc
	dir         string
	Socket      string
	WorkersAddr string
	sim         *simProc
}

// startRouter supplies auth, github_sim, max_runners: 0, the instance ID and
// the whole workers: block; cfgYAML holds only top-level repos: and
// optionally public:. The two are concatenated, never re-marshalled.
func startRouter(t *testing.T, sim *simProc, cfgYAML string) *routerProc {
	t.Helper()
	dir := shortDir(t, "gr")
	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	harness := fmt.Sprintf("auth:\n  env: GSO_E2E_UNUSED_TOKEN\nmax_runners: 0\ngithub_sim: %s\nrunner:\n  instance_id: e2e\nworkers:\n  listen: %q\n  advertise: %q\n  default_class: %s\n",
		sim.URL, addr, addr, simClass())
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(harness+cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &routerProc{dir: dir, Socket: socketFor(dir), WorkersAddr: addr, sim: sim}
	r.proc = newProc(t, "router", isolatedEnv(t, dir), "start", "--config", cfgPath)
	r.waitReady(t)
	return r
}

func (r *routerProc) waitReady(t *testing.T) {
	t.Helper()
	waitFor(t, 30*time.Second, "the router to be ready", func() bool {
		st, err := tryLiveStatus(r)
		if err != nil || len(st.Listeners) == 0 {
			return false
		}
		for _, l := range st.Listeners {
			if l.State != "running" {
				return false
			}
		}
		conn, err := net.DialTimeout("tcp", r.WorkersAddr, 200*time.Millisecond)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	})
}

func (r *routerProc) Restart(t *testing.T, kill bool) {
	t.Helper()
	r.restart(kill)
	r.waitReady(t)
}

func (r *routerProc) Stop(t *testing.T) { t.Helper(); r.stop() }

func (r *routerProc) addWorker(t *testing.T, name, classes string, slots int, repos string) string {
	t.Helper()
	args := []string{"workers", "add", name, "--classes", classes, "--slots", strconv.Itoa(slots)}
	if repos != "" {
		args = append(args, "--repos", repos)
	}
	out := runGSO(t, r.env, args...)
	for _, f := range strings.Fields(out) {
		if strings.HasPrefix(f, worker.JoinPrefix) {
			return f
		}
	}
	t.Fatalf("gso workers add printed no join string:\n%s", out)
	return ""
}

func tryLiveStatus(r *routerProc) (control.LiveStatusResult, error) {
	var st control.LiveStatusResult
	c, err := control.NewClientWithPath(r.Socket)
	if err != nil {
		return st, err
	}
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	raw, err := c.Call(ctx, control.MethodLiveStatus, nil)
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(raw, &st)
	return st, err
}

func liveStatus(t *testing.T, r *routerProc) control.LiveStatusResult {
	t.Helper()
	st, err := tryLiveStatus(r)
	if err != nil {
		t.Fatalf("live_status: %v", err)
	}
	return st
}

func tryWorkerStatus(r *routerProc, name string) (control.WorkerLiveStatus, bool) {
	st, err := tryLiveStatus(r)
	if err != nil {
		return control.WorkerLiveStatus{}, false
	}
	for _, w := range st.Workers {
		if w.Name == name {
			return w, true
		}
	}
	return control.WorkerLiveStatus{}, false
}

func waitWorkerConnected(t *testing.T, r *routerProc, name string) {
	t.Helper()
	waitFor(t, 20*time.Second, name+" to be connected", func() bool {
		w, ok := tryWorkerStatus(r, name)
		return ok && w.Connected
	})
}

// --- workers ---

type workerProc struct {
	*proc
	stateDir string
}

// startWorker runs `gso worker` with the sim executor; extraArgs may override
// any flag (a later flag wins), e.g. "--executor", "docker" or "--router", proxyAddr.
// Every worker gets --github-sim <the simulator's advertise URL> so its
// TrustedHosts include the simulator (docker workers need the explicit flag).
func startWorker(t *testing.T, r *routerProc, name, join string, extraArgs ...string) *workerProc {
	t.Helper()
	dir := shortDir(t, "gw")
	state := filepath.Join(dir, "state")
	args := append([]string{"worker", "--join", join, "--yes", "--executor", "sim", "--name", name, "--state-dir", state, "--slots", "4", "--github-sim", r.sim.Advertise}, extraArgs...)
	w := &workerProc{stateDir: state}
	w.proc = newProc(t, "worker-"+name, isolatedEnv(t, dir), args...)
	t.Cleanup(w.killFakeRunners)
	return w
}

func (w *workerProc) Restart(t *testing.T, kill bool) { t.Helper(); w.restart(kill) }

func (w *workerProc) Stop(t *testing.T) { t.Helper(); w.stop() }

func (w *workerProc) records(t *testing.T) []worker.RunnerRecord {
	t.Helper()
	path := filepath.Join(w.stateDir, "runners.json")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	rs, err := worker.OpenRecordStore(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return rs.List()
}

// killFakeRunners ends fake runners that outlived their worker (own process groups).
func (w *workerProc) killFakeRunners() {
	files, _ := filepath.Glob(filepath.Join(w.stateDir, "sim", "*.pid"))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var p struct {
			PID int `json:"pid"`
		}
		if json.Unmarshal(data, &p) == nil && p.PID > 0 {
			_ = syscall.Kill(-p.PID, syscall.SIGKILL)
		}
	}
}
