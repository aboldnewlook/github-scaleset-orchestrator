//go:build !windows

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

// TestHelperProcess is the "job" the launcher starts: its JIT config is
// "sleep=<duration>,exit=<code>".
func TestHelperProcess(t *testing.T) {
	if os.Getenv("PROCESS_LAUNCHER_HELPER") != "1" {
		return
	}
	var sleep time.Duration
	code := 0
	for _, kv := range strings.Split(os.Getenv("ACTIONS_RUNNER_INPUT_JITCONFIG"), ",") {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "sleep":
			sleep, _ = time.ParseDuration(v)
		case "exit":
			code, _ = strconv.Atoi(v)
		}
	}
	time.Sleep(sleep)
	os.Exit(code)
}

func helperLauncher(t *testing.T, dir string) Launcher {
	t.Helper()
	l, err := NewProcessLauncher(ProcessLauncherConfig{
		Command: []string{os.Args[0], "-test.run=^TestHelperProcess$"},
		Dir:     dir,
		Kind:    runner.ExecutorDocker,
		Classes: []runner.Class{{OS: "linux", Arch: "x64"}},
		Env:     []string{"PROCESS_LAUNCHER_HELPER=1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestProcessLauncherRunsToExit(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	l := helperLauncher(t, dir)
	if l.Kind() != runner.ExecutorDocker {
		t.Fatalf("Kind = %s", l.Kind())
	}
	ok := RunnerRecord{Name: "gso-sim-a-run001"}
	if err := l.Start(ctx, ok, "sleep=50ms,exit=0", "none"); err != nil {
		t.Fatal(err)
	}
	if err := l.Wait(ctx, ok); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ok.Name+".pid")); !os.IsNotExist(err) {
		t.Fatalf("pid file still present after exit: %v", err)
	}
	bad := RunnerRecord{Name: "gso-sim-a-run002"}
	if err := l.Start(ctx, bad, "sleep=10ms,exit=3", "none"); err != nil {
		t.Fatal(err)
	}
	if err := l.Wait(ctx, bad); err == nil || !strings.Contains(err.Error(), "status 3") {
		t.Fatalf("Wait = %v, want an exit-status-3 error", err)
	}
}

func TestProcessLauncherDetachAndReattach(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	first := helperLauncher(t, dir)
	rec := RunnerRecord{Name: "gso-sim-a-run003"}
	if err := first.Start(ctx, rec, "sleep=600ms,exit=0", "none"); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := first.Wait(cancelled, rec); !errors.Is(err, context.Canceled) {
		t.Fatalf("detaching Wait = %v, want context.Canceled", err)
	}
	second := helperLauncher(t, dir) // a restarted worker: it did not start this process
	start := time.Now()
	if err := second.Wait(ctx, rec); err != nil {
		t.Fatalf("re-attached Wait: %v", err)
	}
	if time.Since(start) < 200*time.Millisecond {
		t.Fatal("re-attached Wait returned before the process exited")
	}
}

func TestProcessLauncherKillAndOrphans(t *testing.T) {
	ctx := context.Background()
	l := helperLauncher(t, t.TempDir())
	keep := RunnerRecord{Name: "gso-sim-a-keep01"}
	orphan := RunnerRecord{Name: "gso-sim-a-orph01"}
	for _, r := range []RunnerRecord{keep, orphan} {
		if err := l.Start(ctx, r, "sleep=30s", "none"); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.RemoveOrphans(ctx, map[string]bool{keep.Name: true}); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := l.Wait(waitCtx, orphan); errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("the orphan survived RemoveOrphans")
	}
	if err := l.Kill(ctx, keep); err != nil {
		t.Fatal(err)
	}
	if err := l.Wait(waitCtx, keep); errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("Kill did not end the process")
	}
}

func TestProcessLauncherValidatesConfig(t *testing.T) {
	if _, err := NewProcessLauncher(ProcessLauncherConfig{Dir: t.TempDir(), Kind: runner.ExecutorDocker}); err == nil {
		t.Fatal("a launcher without a command was accepted")
	}
}

func TestProcessLauncherRefusesUnsafeRunnerNames(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	l := helperLauncher(t, dir)
	for _, bad := range []string{"../escape", "a/b", "", "UPPER"} {
		rec := RunnerRecord{Name: bad}
		if err := l.Start(ctx, rec, "sleep=1ms", "none"); err == nil {
			t.Errorf("Start(%q) succeeded", bad)
		}
		if err := l.Wait(ctx, rec); err == nil {
			t.Errorf("Wait(%q) succeeded", bad)
		}
		if err := l.Kill(ctx, rec); err == nil {
			t.Errorf("Kill(%q) succeeded", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.pid")); !os.IsNotExist(err) {
		t.Fatalf("a pid file escaped the launcher directory: %v", err)
	}
}

// startSleeper starts an unrelated long-lived process in its own group.
func startSleeper(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), "PROCESS_LAUNCHER_HELPER=1", "ACTIONS_RUNNER_INPUT_JITCONFIG=sleep=60s")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() })
	return cmd.Process.Pid
}

// A pid file written before the last reboot names a PID that may now belong
// to anything. It must be dropped without signalling or re-attaching.
func TestProcessLauncherIgnoresRecordsFromAPreviousBoot(t *testing.T) {
	if bootTime() == 0 {
		t.Skip("boot time is unknown on this platform")
	}
	ctx := context.Background()
	dir := t.TempDir()
	l := helperLauncher(t, dir)
	pid := startSleeper(t)
	stale := func(name string) {
		data, _ := json.Marshal(pidRecord{PID: pid, Started: time.Now().UTC(), Boot: bootTime() - 3600})
		if err := os.WriteFile(filepath.Join(dir, name+".pid"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	alive := func() bool { return processAlive(pid) }

	stale("gso-sim-a-stale1")
	if err := l.Kill(ctx, RunnerRecord{Name: "gso-sim-a-stale1"}); err != nil {
		t.Fatal(err)
	}
	stale("gso-sim-a-stale2")
	if err := l.RemoveOrphans(ctx, nil); err != nil {
		t.Fatal(err)
	}
	stale("gso-sim-a-stale3")
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := l.Wait(waitCtx, RunnerRecord{Name: "gso-sim-a-stale3"}); errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("Wait re-attached to a stale record")
	}
	if !alive() {
		t.Fatal("a stale record got an unrelated process killed")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.pid"))
	if len(files) != 0 {
		t.Fatalf("stale pid files left behind: %v", files)
	}
}

func TestProcessLauncherRecordsTheBootTime(t *testing.T) {
	if bootTime() == 0 {
		t.Skip("boot time is unknown on this platform")
	}
	dir := t.TempDir()
	l := helperLauncher(t, dir)
	rec := RunnerRecord{Name: "gso-sim-a-boot01"}
	if err := l.Start(context.Background(), rec, "sleep=1s", "none"); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Kill(context.Background(), rec) }()
	data, err := os.ReadFile(filepath.Join(dir, rec.Name+".pid"))
	if err != nil {
		t.Fatal(err)
	}
	var p pidRecord
	if err := json.Unmarshal(data, &p); err != nil || p.Boot != bootTime() {
		t.Fatalf("pid record = %+v (%v), want Boot %d", p, err, bootTime())
	}
}
