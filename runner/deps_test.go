package runner_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenImports are path prefixes the public packages must never depend
// on, directly or transitively (design spec §4.1 import rule).
var forbiddenImports = []string{
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/",
	"github.com/spf13/cobra",
	"github.com/spf13/viper",
	"github.com/charmbracelet/bubbletea",
	"github.com/actions/scaleset",
}

// publicTrees are the module directories whose packages other gso workers
// (the Burst desktop app) import. A tree that does not exist yet is skipped,
// so the rule can land before every tree does.
var publicTrees = []string{"runner", "worker", "gen"}

func TestRunnerDependsOnNoInternalOrCLIPackages(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool not on PATH")
	}
	checked := 0
	for _, tree := range publicTrees {
		info, err := os.Stat(filepath.Join("..", tree))
		if err != nil || !info.IsDir() {
			continue
		}
		checked++
		pattern := "../" + tree + "/..."
		out, err := exec.Command(goBin, "list", "-deps", "-f", "{{.ImportPath}}", pattern).CombinedOutput()
		if err != nil {
			t.Fatalf("go list -deps %s: %v\n%s", pattern, err, out)
		}
		for _, dep := range strings.Fields(string(out)) {
			for _, bad := range forbiddenImports {
				if strings.HasPrefix(dep, bad) {
					t.Errorf("%s/... depends on %s (forbidden prefix %s)", tree, dep, bad)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("found none of the public package trees runner/, worker/, gen/")
	}
}
