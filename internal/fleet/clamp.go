package fleet

import (
	"errors"
	"fmt"
	"sort"

	workerv1 "github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

// ClassToProto converts a class to its wire form.
func ClassToProto(c runner.Class) *workerv1.Class {
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

// ClassFromProto converts a wire class; unspecified fields are an error.
func ClassFromProto(pc *workerv1.Class) (runner.Class, error) {
	if pc == nil {
		return runner.Class{}, errors.New("class is missing")
	}
	var c runner.Class
	switch pc.GetOs() {
	case workerv1.OS_OS_LINUX:
		c.OS = "linux"
	case workerv1.OS_OS_MACOS:
		c.OS = "macos"
	case workerv1.OS_OS_WINDOWS:
		c.OS = "windows"
	default:
		return runner.Class{}, fmt.Errorf("unsupported OS %v", pc.GetOs())
	}
	switch pc.GetArch() {
	case workerv1.Arch_ARCH_X64:
		c.Arch = "x64"
	case workerv1.Arch_ARCH_ARM64:
		c.Arch = "arm64"
	default:
		return runner.Class{}, fmt.Errorf("unsupported arch %v", pc.GetArch())
	}
	return c, nil
}

func executorKindFromProto(k workerv1.ExecutorKind) (runner.ExecutorKind, bool) {
	switch k {
	case workerv1.ExecutorKind_EXECUTOR_KIND_DOCKER:
		return runner.ExecutorDocker, true
	case workerv1.ExecutorKind_EXECUTOR_KIND_NATIVE:
		return runner.ExecutorNative, true
	default:
		return "", false
	}
}

func executorKindToProto(k runner.ExecutorKind) workerv1.ExecutorKind {
	if k == runner.ExecutorDocker {
		return workerv1.ExecutorKind_EXECUTOR_KIND_DOCKER
	}
	return workerv1.ExecutorKind_EXECUTOR_KIND_NATIVE
}

// Clamp intersects advertised capabilities with the enrolled bounds (spec
// §4.3): slots ≤ min(bounds.MaxSlots, maxSlotsPerWorker); DOCKER executors may
// serve only linux classes and NATIVE executors only macOS/Windows classes,
// in both cases only classes inside the bounds. It never returns nil.
func Clamp(caps *workerv1.Capabilities, b Bounds, maxSlotsPerWorker int) *workerv1.Capabilities {
	out := &workerv1.Capabilities{}
	if caps == nil {
		return out
	}
	out.WorkerVersion = caps.GetWorkerVersion()
	limit := b.MaxSlots
	if maxSlotsPerWorker > 0 && maxSlotsPerWorker < limit {
		limit = maxSlotsPerWorker
	}
	limit = max(limit, 0)
	slots := min(int64(caps.GetSlots()), int64(limit))
	out.Slots = uint32(slots) // 0 ≤ slots ≤ limit ≤ maxBoundsSlots

	allowed := make(map[runner.Class]bool, len(b.Classes))
	for _, c := range b.Classes {
		allowed[c] = true
	}
	for _, ex := range caps.GetExecutors() {
		kind, ok := executorKindFromProto(ex.GetKind())
		if !ok {
			continue
		}
		kept := &workerv1.Executor{Kind: ex.GetKind(), Detail: ex.GetDetail()}
		seen := map[runner.Class]bool{}
		for _, pc := range ex.GetClasses() {
			c, err := ClassFromProto(pc)
			if err != nil || c.Executor() != kind || !allowed[c] || seen[c] {
				continue
			}
			seen[c] = true
			kept.Classes = append(kept.Classes, ClassToProto(c))
		}
		if len(kept.Classes) > 0 {
			out.Executors = append(out.Executors, kept)
		}
	}
	return out
}

// servedClasses lists the classes a clamped capability set can serve, each
// with the executor kind its class requires.
func servedClasses(caps *workerv1.Capabilities) map[runner.Class]bool {
	out := map[runner.Class]bool{}
	for _, ex := range caps.GetExecutors() {
		kind, ok := executorKindFromProto(ex.GetKind())
		if !ok {
			continue
		}
		for _, pc := range ex.GetClasses() {
			if c, err := ClassFromProto(pc); err == nil && c.Executor() == kind {
				out[c] = true
			}
		}
	}
	return out
}

func classStrings(m map[runner.Class]bool) []string {
	out := make([]string, 0, len(m))
	for c := range m {
		out = append(out, c.String())
	}
	sort.Strings(out)
	return out
}

func boundsToProto(b Bounds) *workerv1.Bounds {
	pb := &workerv1.Bounds{MaxSlots: uint32(min(max(b.MaxSlots, 0), maxBoundsSlots)), Repos: append([]string(nil), b.Repos...)}
	for _, c := range b.Classes {
		pb.Classes = append(pb.Classes, ClassToProto(c))
	}
	return pb
}
