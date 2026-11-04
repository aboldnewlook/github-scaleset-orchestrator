package naming

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

var nameCharset = regexp.MustCompile(`^[a-z0-9-]+$`)

func TestSanitize(t *testing.T) {
	tests := map[string]string{
		"unraid":              "unraid",
		"Unraid_Main":         "unraid-main",
		"Ryans-MacBook.local": "ryans-macbook-local",
		"--edge--":            "edge",
		"":                    "x",
		"___":                 "x",
		"Ünraid":              "nraid",
	}
	for in, want := range tests {
		if got := Sanitize(in); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestScaleSetNameFor(t *testing.T) {
	linux := runner.Class{OS: "linux", Arch: "x64"}
	if got := ScaleSetNameFor("unraid", "jrschumacher/prune", linux); got != "gso-unraid-prune-linux-x64" {
		t.Fatalf("ScaleSetNameFor = %q", got)
	}
	if got := ScaleSetNameFor("Unraid", "JRSchumacher/Prune", linux); got != "gso-unraid-prune-linux-x64" {
		t.Fatalf("ScaleSetNameFor is case-sensitive: %q", got)
	}
	if !strings.HasPrefix(ScaleSetNameFor("Unraid", "o/r", linux), ScaleSetPrefix("Unraid")) {
		t.Fatalf("ScaleSetPrefix(%q) = %q does not prefix the name", "Unraid", ScaleSetPrefix("Unraid"))
	}
}

func TestScaleSetNameForOverflow(t *testing.T) {
	win := runner.Class{OS: "windows", Arch: "arm64"}
	inst, repo := "a-very-long-instance-name", "owner/an-extremely-long-repository-name"
	a := ScaleSetNameFor(inst, repo, win)
	if len(a) > MaxNameLen || !nameCharset.MatchString(a) {
		t.Fatalf("overflow name %q (len %d) breaks the 45-char [a-z0-9-] envelope", a, len(a))
	}
	if !regexp.MustCompile(`^gso-a-very-lon-[a-z0-9-]+-[0-9a-f]{4}-windows-arm64$`).MatchString(a) {
		t.Fatalf("overflow name %q is not gso-<inst≤10>-<repo-trunc>-<hash4>-<class>", a)
	}
	if again := ScaleSetNameFor(inst, repo, win); again != a {
		t.Fatalf("not deterministic: %q then %q", a, again)
	}
	if b := ScaleSetNameFor(inst, repo+"2", win); b == a {
		t.Fatalf("two repos sharing a truncated prefix collide: %q", a)
	}
	for _, c := range runner.AllClasses {
		n := ScaleSetNameFor(strings.Repeat("i", 60), "o/"+strings.Repeat("r", 90), c)
		if len(n) > MaxNameLen || !nameCharset.MatchString(n) || !strings.HasSuffix(n, "-"+c.String()) {
			t.Errorf("class %s: %q", c, n)
		}
	}
}

func TestInstanceID(t *testing.T) {
	if got := InstanceID("Unraid Main"); got != "unraid-main" {
		t.Fatalf("InstanceID(configured) = %q", got)
	}
	want := "unknown"
	if h, err := os.Hostname(); err == nil && h != "" {
		want = Sanitize(h)
	}
	if got := InstanceID("  "); got != want {
		t.Fatalf("InstanceID(blank) = %q, want %q", got, want)
	}
}

func TestWorkerSegment(t *testing.T) {
	tests := map[string]string{
		"mac-m1":                 "macm1",
		"Mac-M1-Studio-Pro-2024": "macm1studiop",
		"local":                  "local",
		"---":                    "x",
	}
	for in, want := range tests {
		if got := WorkerSegment(in); got != want {
			t.Errorf("WorkerSegment(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRunnerName(t *testing.T) {
	n := RunnerName("mac-m1", "jrschumacher/prune")
	if !regexp.MustCompile(`^gso-macm1-prune-[a-z0-9]{6}$`).MatchString(n) {
		t.Fatalf("RunnerName = %q", n)
	}
	long := RunnerName("a-very-long-worker-name", "owner/an-extremely-long-repository-name")
	if len(long) > MaxNameLen || !nameCharset.MatchString(long) {
		t.Fatalf("long RunnerName = %q (len %d)", long, len(long))
	}
	if seg, ok := RunnerWorkerSegment(long); !ok || seg != "averylongwor" {
		t.Fatalf("RunnerWorkerSegment(%q) = %q, %v", long, seg, ok)
	}
	first, second := RunnerName("local", "o/r"), RunnerName("local", "o/r")
	if first == second {
		t.Fatal("two runner names are identical; the suffix must be random")
	}
}

func TestRunnerWorkerSegment(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"gso-macm1-prune-abc123", "macm1", true},
		{"gso-local-my-repo-abc123", "local", true},
		{"runner-1", "", false},
		{"gso-", "", false},
		{"gso-abc", "", false},
	}
	for _, tt := range tests {
		got, ok := RunnerWorkerSegment(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("RunnerWorkerSegment(%q) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestHasClassSuffix(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{ScaleSetNameFor("unraid", "o/myrepo", runner.Class{OS: "linux", Arch: "x64"}), true},
		{ScaleSetNameFor("a-very-long-instance-name", "o/a-very-long-repository-name", runner.Class{OS: "macos", Arch: "arm64"}), true},
		{"gso-0123456789ab-myrepo", false}, // container-ID era
		{"gso-oldhost-repo", false},
		{"gso-host-repo-linux", false},
		{"myrunners-linux-x64", true}, // shape only; the gso- prefix is the caller's check
	}
	for _, tt := range tests {
		if got := HasClassSuffix(tt.name); got != tt.want {
			t.Errorf("HasClassSuffix(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}
