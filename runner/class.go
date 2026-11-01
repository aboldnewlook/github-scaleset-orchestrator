package runner

import (
	"fmt"
	"runtime"
	"strings"
)

// ExecutorKind is how a runner executes: in a sibling Docker container or as
// a native process on the worker.
type ExecutorKind string

const (
	ExecutorDocker ExecutorKind = "docker"
	ExecutorNative ExecutorKind = "native"
)

// Class is an (OS, arch) pair a scale set serves (design spec §5.1).
// OS is linux, macos or windows; Arch is x64 or arm64.
type Class struct {
	OS   string
	Arch string
}

// AllClasses lists every supported class.
var AllClasses = []Class{
	{"linux", "x64"}, {"linux", "arm64"},
	{"macos", "x64"}, {"macos", "arm64"},
	{"windows", "x64"}, {"windows", "arm64"},
}

// ParseClass parses "<os>-<arch>", e.g. "linux-x64". It is case-insensitive.
func ParseClass(s string) (Class, error) {
	osName, arch, ok := strings.Cut(strings.ToLower(strings.TrimSpace(s)), "-")
	if !ok {
		return Class{}, fmt.Errorf("class %q: want <os>-<arch>, e.g. linux-x64", s)
	}
	c := Class{OS: osName, Arch: arch}
	if !c.supported() {
		return Class{}, fmt.Errorf("class %q: os must be linux, macos or windows and arch x64 or arm64", s)
	}
	return c, nil
}

// ClassFor maps Go's GOOS/GOARCH names to a class.
func ClassFor(goos, goarch string) (Class, error) {
	var c Class
	switch goos {
	case "linux":
		c.OS = "linux"
	case "darwin":
		c.OS = "macos"
	case "windows":
		c.OS = "windows"
	default:
		return Class{}, fmt.Errorf("unsupported GOOS %q", goos)
	}
	switch goarch {
	case "amd64":
		c.Arch = "x64"
	case "arm64":
		c.Arch = "arm64"
	default:
		return Class{}, fmt.Errorf("unsupported GOARCH %q", goarch)
	}
	return c, nil
}

// HostClass is the class of the machine this binary runs on. gso only ships
// supported platforms; anything else falls back to linux-x64.
func HostClass() Class {
	c, err := ClassFor(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return Class{OS: "linux", Arch: "x64"}
	}
	return c
}

func (c Class) supported() bool {
	for _, k := range AllClasses {
		if k == c {
			return true
		}
	}
	return false
}

// String returns "<os>-<arch>", or "" for the zero Class.
func (c Class) String() string {
	if c.IsZero() {
		return ""
	}
	return c.OS + "-" + c.Arch
}

// IsZero reports whether c is the zero Class.
func (c Class) IsZero() bool { return c == Class{} }

// OSLabel is the GitHub runner label for the OS.
func (c Class) OSLabel() string {
	switch c.OS {
	case "linux":
		return "Linux"
	case "macos":
		return "macOS"
	case "windows":
		return "Windows"
	default:
		return c.OS
	}
}

// ArchLabel is the GitHub runner label for the architecture.
func (c Class) ArchLabel() string {
	switch c.Arch {
	case "x64":
		return "X64"
	case "arm64":
		return "ARM64"
	default:
		return c.Arch
	}
}

// Executor is the only executor kind allowed to serve the class: Linux
// classes run in Docker, macOS and Windows classes natively (design spec §5.1).
// A scale set therefore never mixes executor kinds.
func (c Class) Executor() ExecutorKind {
	if c.OS == "linux" {
		return ExecutorDocker
	}
	return ExecutorNative
}
