//go:build !windows

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

const processPollInterval = 200 * time.Millisecond

type pidRecord struct {
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`
	Boot    int64     `json:"boot,omitempty"`
}

type processChild struct {
	pid  int
	done chan struct{}
	err  error
}

type processLauncher struct {
	cfg      ProcessLauncherConfig
	mu       sync.Mutex
	children map[string]*processChild
}

// NewProcessLauncher runs each job as a local process in its own process
// group, so it survives a worker restart, and re-attaches to it by PID. It is
// meant for simulations and tests: a re-attached process's exit status is
// unknown, and PID reuse after a reboot is not detected.
func NewProcessLauncher(c ProcessLauncherConfig) (Launcher, error) {
	if len(c.Command) == 0 {
		return nil, errors.New("process launcher: a command is required")
	}
	if c.Dir == "" {
		return nil, errors.New("process launcher: a directory is required")
	}
	if c.Kind == "" {
		return nil, errors.New("process launcher: an executor kind is required")
	}
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("process launcher: %w", err)
	}
	return &processLauncher{cfg: c, children: make(map[string]*processChild)}, nil
}

func (l *processLauncher) Kind() runner.ExecutorKind { return l.cfg.Kind }

func (l *processLauncher) Classes(context.Context) ([]runner.Class, error) {
	return append([]runner.Class(nil), l.cfg.Classes...), nil
}

func (l *processLauncher) pidPath(name string) string { return filepath.Join(l.cfg.Dir, name+".pid") }

var errUnsafeRunnerName = errors.New("process launcher: unsafe runner name")

// errStalePID marks a pid file written before the last reboot: its PID may now
// belong to an unrelated process.
var errStalePID = errors.New("pid record is from a previous boot")

// bootTolerance absorbs clock adjustments in the platform's boot time.
const bootTolerance = 5 // seconds

func (l *processLauncher) Start(_ context.Context, rec RunnerRecord, jit, _ string) error {
	if !validRunnerName(rec.Name) {
		return errUnsafeRunnerName
	}
	logf, err := os.OpenFile(filepath.Join(l.cfg.Dir, rec.Name+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("opening runner log: %w", err)
	}
	defer func() { _ = logf.Close() }() // the child keeps its own descriptor
	// Not CommandContext: the job outlives this call and this worker.
	cmd := exec.Command(l.cfg.Command[0], l.cfg.Command[1:]...) //nolint:gosec // the command is the operator's own configuration
	cmd.Dir = l.cfg.Dir
	cmd.Env = append(append(runner.SanitizedEnv(), l.cfg.Env...), "ACTIONS_RUNNER_INPUT_JITCONFIG="+jit)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting runner process: %w", err)
	}
	child := &processChild{pid: cmd.Process.Pid, done: make(chan struct{})}
	go func() { // the only Wait: it also reaps the process if this worker detaches
		child.err = cmd.Wait()
		close(child.done)
	}()
	data, _ := json.Marshal(pidRecord{PID: child.pid, Started: time.Now().UTC(), Boot: bootTime()})
	if err := writeProcessFile(l.pidPath(rec.Name), data); err != nil {
		_ = syscall.Kill(-child.pid, syscall.SIGKILL)
		return fmt.Errorf("recording runner pid: %w", err)
	}
	l.mu.Lock()
	l.children[rec.Name] = child
	l.mu.Unlock()
	return nil
}

func (l *processLauncher) Wait(ctx context.Context, rec RunnerRecord) error {
	if !validRunnerName(rec.Name) {
		return errUnsafeRunnerName
	}
	l.mu.Lock()
	child := l.children[rec.Name]
	l.mu.Unlock()
	if child != nil {
		select {
		case <-ctx.Done():
			return ctx.Err() // detach: the process keeps running
		case <-child.done:
			l.forget(rec.Name)
			return processExitError(child.err)
		}
	}
	pid, err := l.readPID(rec.Name)
	if err != nil {
		return fmt.Errorf("re-attaching to %s: %w", rec.Name, err)
	}
	ticker := time.NewTicker(processPollInterval)
	defer ticker.Stop()
	for processAlive(pid) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	l.forget(rec.Name)
	return nil // the exit status of a process this worker did not start is unknown
}

func (l *processLauncher) Kill(_ context.Context, rec RunnerRecord) error {
	if !validRunnerName(rec.Name) {
		return errUnsafeRunnerName
	}
	l.mu.Lock()
	child := l.children[rec.Name]
	l.mu.Unlock()
	pid := 0
	if child != nil {
		pid = child.pid
	} else if p, err := l.readPID(rec.Name); err == nil {
		pid = p
	}
	if pid == 0 {
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("killing runner %s: %w", rec.Name, err)
	}
	if child == nil {
		_ = os.Remove(l.pidPath(rec.Name))
	}
	return nil
}

func (l *processLauncher) RemoveOrphans(_ context.Context, keep map[string]bool) error {
	files, err := filepath.Glob(filepath.Join(l.cfg.Dir, "*.pid"))
	if err != nil {
		return err
	}
	var errs []error
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".pid")
		if keep[name] {
			continue
		}
		if pid, err := l.readPID(name); err == nil {
			if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				errs = append(errs, fmt.Errorf("killing orphan %s: %w", name, err))
				continue
			}
		}
		_ = os.Remove(f)
	}
	return errors.Join(errs...)
}

func (l *processLauncher) readPID(name string) (int, error) {
	data, err := os.ReadFile(l.pidPath(name))
	if err != nil {
		return 0, err
	}
	var p pidRecord
	if err := json.Unmarshal(data, &p); err != nil || p.PID <= 0 {
		return 0, fmt.Errorf("pid file for %s is unreadable", name)
	}
	if now := bootTime(); p.Boot != 0 && now != 0 && (now-p.Boot > bootTolerance || p.Boot-now > bootTolerance) {
		_ = os.Remove(l.pidPath(name)) // drop it without signalling the PID
		return 0, errStalePID
	}
	return p.PID, nil
}

func (l *processLauncher) forget(name string) {
	l.mu.Lock()
	delete(l.children, name)
	l.mu.Unlock()
	_ = os.Remove(l.pidPath(name))
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func processExitError(err error) error {
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Errorf("runner process exited with status %d", exitErr.ExitCode())
	}
	return fmt.Errorf("runner process: %w", err)
}

func writeProcessFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pid-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}
