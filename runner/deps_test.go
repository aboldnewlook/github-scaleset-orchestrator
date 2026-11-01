package runner_test

import (
	"os/exec"
	"strings"
	"testing"
)

// forbiddenImports are path prefixes the public runner package must never
// depend on, directly or transitively (design spec §4.1 import rule).
var forbiddenImports = []string{
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/",
	"github.com/spf13/cobra",
	"github.com/spf13/viper",
	"github.com/charmbracelet/bubbletea",
	"github.com/actions/scaleset",
}

func TestRunnerDependsOnNoInternalOrCLIPackages(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool not on PATH")
	}
	out, err := exec.Command(goBin, "list", "-deps", "-f", "{{.ImportPath}}", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	for _, dep := range strings.Fields(string(out)) {
		for _, bad := range forbiddenImports {
			if strings.HasPrefix(dep, bad) {
				t.Errorf("runner depends on %s (forbidden prefix %s)", dep, bad)
			}
		}
	}
}
