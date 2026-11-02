package fleet

import (
	"reflect"
	"sort"
	"testing"

	workerv1 "github.com/aboldnewlook/github-scaleset-orchestrator/gen/gso/worker/v1"
	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

func TestClassProtoRoundTrip(t *testing.T) {
	for _, c := range runner.AllClasses {
		got, err := ClassFromProto(ClassToProto(c))
		if err != nil || got != c {
			t.Errorf("round trip %s = %v, %v", c, got, err)
		}
	}
	for _, bad := range []*workerv1.Class{nil, {}, {Os: workerv1.OS_OS_LINUX}, {Arch: workerv1.Arch_ARCH_X64}} {
		if _, err := ClassFromProto(bad); err == nil {
			t.Errorf("ClassFromProto(%v) succeeded", bad)
		}
	}
}

func nativeExecutor(classes ...runner.Class) *workerv1.Executor {
	ex := &workerv1.Executor{Kind: workerv1.ExecutorKind_EXECUTOR_KIND_NATIVE}
	for _, c := range classes {
		ex.Classes = append(ex.Classes, ClassToProto(c))
	}
	return ex
}

func executorClasses(caps *workerv1.Capabilities) map[workerv1.ExecutorKind][]string {
	out := map[workerv1.ExecutorKind][]string{}
	for _, ex := range caps.GetExecutors() {
		for _, pc := range ex.GetClasses() {
			c, _ := ClassFromProto(pc)
			out[ex.GetKind()] = append(out[ex.GetKind()], c.String())
		}
		sort.Strings(out[ex.GetKind()])
	}
	return out
}

func TestClamp(t *testing.T) {
	docker := workerv1.ExecutorKind_EXECUTOR_KIND_DOCKER
	native := workerv1.ExecutorKind_EXECUTOR_KIND_NATIVE
	mixed := dockerCaps(4, linuxArm64, macosArm64)
	mixed.Executors = append(mixed.Executors, nativeExecutor(linuxX64, macosArm64))
	dup := dockerCaps(2, linuxX64, linuxX64)
	dup.Executors = append(dup.Executors, &workerv1.Executor{Kind: workerv1.ExecutorKind_EXECUTOR_KIND_UNSPECIFIED, Classes: []*workerv1.Class{ClassToProto(linuxX64)}})
	all := Bounds{Classes: runner.AllClasses, MaxSlots: 8}
	tests := []struct {
		name      string
		caps      *workerv1.Capabilities
		bounds    Bounds
		maxPer    int
		wantSlots uint32
		want      map[workerv1.ExecutorKind][]string
	}{
		{"slots clamped to bounds", dockerCaps(8, linuxX64), Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 2}, 16, 2, map[workerv1.ExecutorKind][]string{docker: {"linux-x64"}}},
		{"slots clamped to the router maximum", dockerCaps(8, linuxX64), Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 32}, 4, 4, map[workerv1.ExecutorKind][]string{docker: {"linux-x64"}}},
		{"fewer slots than bounds are kept", dockerCaps(1, linuxX64), Bounds{Classes: []runner.Class{linuxX64}, MaxSlots: 4}, 16, 1, map[workerv1.ExecutorKind][]string{docker: {"linux-x64"}}},
		{"classes outside bounds are dropped", dockerCaps(2, linuxX64, linuxArm64), Bounds{Classes: []runner.Class{linuxArm64}, MaxSlots: 2}, 16, 2, map[workerv1.ExecutorKind][]string{docker: {"linux-arm64"}}},
		{"docker serves linux only, native serves macOS/Windows only", mixed, all, 16, 4, map[workerv1.ExecutorKind][]string{docker: {"linux-arm64"}, native: {"macos-arm64"}}},
		{"executors left empty are dropped", dockerCaps(2, macosArm64), all, 16, 2, map[workerv1.ExecutorKind][]string{}},
		{"duplicates and unspecified executors are dropped", dup, all, 16, 2, map[workerv1.ExecutorKind][]string{docker: {"linux-x64"}}},
		{"nil capabilities", nil, all, 16, 0, map[workerv1.ExecutorKind][]string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Clamp(tt.caps, tt.bounds, tt.maxPer)
			if got == nil {
				t.Fatal("Clamp returned nil")
			}
			if got.GetSlots() != tt.wantSlots {
				t.Errorf("slots = %d, want %d", got.GetSlots(), tt.wantSlots)
			}
			if classes := executorClasses(got); !reflect.DeepEqual(classes, tt.want) {
				t.Errorf("executors = %v, want %v", classes, tt.want)
			}
		})
	}
}

func TestBoundsToProto(t *testing.T) {
	pb := boundsToProto(Bounds{Classes: []runner.Class{linuxArm64}, MaxSlots: 3, Repos: []string{"o/a"}})
	if pb.GetMaxSlots() != 3 || len(pb.GetClasses()) != 1 || pb.GetRepos()[0] != "o/a" {
		t.Fatalf("boundsToProto = %v", pb)
	}
}
