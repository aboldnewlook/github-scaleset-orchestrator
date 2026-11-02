package worker

import (
	"fmt"
	"regexp"
	"strconv"

	workerv1 "github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

// maxSlots bounds what a worker advertises; the router clamps further.
const maxSlots = 256

func classToProto(c runner.Class) *workerv1.Class {
	pc := &workerv1.Class{}
	switch c.OS {
	case "linux":
		pc.Os = workerv1.OS_OS_LINUX
	case "macos":
		pc.Os = workerv1.OS_OS_MACOS
	case "windows":
		pc.Os = workerv1.OS_OS_WINDOWS
	}
	switch c.Arch {
	case "x64":
		pc.Arch = workerv1.Arch_ARCH_X64
	case "arm64":
		pc.Arch = workerv1.Arch_ARCH_ARM64
	}
	return pc
}

func classFromProto(pc *workerv1.Class) (runner.Class, error) {
	var c runner.Class
	switch pc.GetOs() {
	case workerv1.OS_OS_LINUX:
		c.OS = "linux"
	case workerv1.OS_OS_MACOS:
		c.OS = "macos"
	case workerv1.OS_OS_WINDOWS:
		c.OS = "windows"
	default:
		return runner.Class{}, fmt.Errorf("class has no known OS (%v)", pc.GetOs())
	}
	switch pc.GetArch() {
	case workerv1.Arch_ARCH_X64:
		c.Arch = "x64"
	case workerv1.Arch_ARCH_ARM64:
		c.Arch = "arm64"
	default:
		return runner.Class{}, fmt.Errorf("class has no known architecture (%v)", pc.GetArch())
	}
	return c, nil
}

// classNames converts proto classes to "linux-x64" strings, skipping unknown ones.
func classNames(pcs []*workerv1.Class) []string {
	names := make([]string, 0, len(pcs))
	for _, pc := range pcs {
		if c, err := classFromProto(pc); err == nil {
			names = append(names, c.String())
		}
	}
	return names
}

func kindToProto(k runner.ExecutorKind) workerv1.ExecutorKind {
	switch k {
	case runner.ExecutorDocker:
		return workerv1.ExecutorKind_EXECUTOR_KIND_DOCKER
	case runner.ExecutorNative:
		return workerv1.ExecutorKind_EXECUTOR_KIND_NATIVE
	default:
		return workerv1.ExecutorKind_EXECUTOR_KIND_UNSPECIFIED
	}
}

func kindFromProto(k workerv1.ExecutorKind) runner.ExecutorKind {
	switch k {
	case workerv1.ExecutorKind_EXECUTOR_KIND_DOCKER:
		return runner.ExecutorDocker
	case workerv1.ExecutorKind_EXECUTOR_KIND_NATIVE:
		return runner.ExecutorNative
	default:
		return ""
	}
}

func phaseToProto(phase string) workerv1.RunnerPhase {
	switch phase {
	case "starting":
		return workerv1.RunnerPhase_RUNNER_PHASE_STARTING
	case "running":
		return workerv1.RunnerPhase_RUNNER_PHASE_RUNNING
	default:
		return workerv1.RunnerPhase_RUNNER_PHASE_UNSPECIFIED
	}
}

func slotsToProto(n int) uint32 {
	if n <= 0 {
		return 0
	}
	if n > maxSlots {
		n = maxSlots
	}
	return uint32(n)
}

// exitStatus matches the runner package's "exited with status N" errors.
var exitStatus = regexp.MustCompile(`status (-?\d+)`)

// exitCode extracts a runner's exit status from its Wait error: 0 for nil,
// the reported status when the error carries one, 1 otherwise.
func exitCode(err error) int32 {
	if err == nil {
		return 0
	}
	if m := exitStatus.FindStringSubmatch(err.Error()); m != nil {
		if n, perr := strconv.ParseInt(m[1], 10, 32); perr == nil {
			return int32(n)
		}
	}
	return 1
}
