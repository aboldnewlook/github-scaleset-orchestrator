package router

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/fleet"
	"github.com/aboldnewlook/github-scaleset-orchestrator/worker"
)

func TestOwnerScopesTruncatesAtTheCapAndWarns(t *testing.T) {
	cfg := loadTestConfig(t, workersYAML)
	cfg.Repos = nil
	for i := 0; i < worker.MaxOwnerScopes+10; i++ {
		cfg.Repos = append(cfg.Repos, config.Repo{Name: fmt.Sprintf("owner%03d/r", i)})
	}
	var logs strings.Builder
	r := newTestRouter(t, cfg, "")
	r.logger = newBufLogger(&logs)

	if got := r.ownerScopes(fleet.Bounds{}); len(got) != worker.MaxOwnerScopes {
		t.Fatalf("unbounded scopes = %d, want %d", len(got), worker.MaxOwnerScopes)
	}
	repos := make([]string, worker.MaxOwnerScopes+5)
	for i := range repos {
		repos[i] = fmt.Sprintf("o/r%d", i)
	}
	if got := r.ownerScopes(fleet.Bounds{Repos: repos}); len(got) != worker.MaxOwnerScopes {
		t.Fatalf("bounded scopes = %d, want %d", len(got), worker.MaxOwnerScopes)
	}
	if !strings.Contains(logs.String(), "truncat") {
		t.Fatalf("no truncation warning logged: %q", logs.String())
	}
}

func newBufLogger(w *strings.Builder) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, nil))
}
