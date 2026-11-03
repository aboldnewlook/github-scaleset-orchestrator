package router

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
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

func TestParseBoundsRefusesWhatTheWorkerRefuses(t *testing.T) {
	r := newTestRouter(t, loadTestConfig(t, workersYAML), "")
	for _, repo := range []string{"my_org/*", "acme/re po", "acme", "*/x"} {
		_, err := r.parseBounds([]string{"linux-x64"}, 1, []string{repo})
		if err == nil || !strings.Contains(err.Error(), repo) {
			t.Errorf("parseBounds(%q) error = %v, want one naming the entry", repo, err)
		}
	}
	if _, err := r.parseBounds([]string{"linux-x64"}, 1, []string{"acme/*", "acme/a.b_c"}); err != nil {
		t.Errorf("valid repo bounds refused: %v", err)
	}
}

func TestAddWorkerRefusesAnAdvertiseTheWorkerCannotJoin(t *testing.T) {
	r := newTestRouter(t, loadTestConfig(t, workersYAML), "")
	for _, adv := range []string{"my_nas:9200", "unraid.lan.:9200", "a@b:9200"} {
		_, err := r.AddWorker(context.Background(), control.WorkersAddParams{Name: "x", Classes: []string{"linux-x64"}, Slots: 1, Advertise: adv})
		if err == nil || !strings.Contains(err.Error(), adv) {
			t.Errorf("AddWorker(advertise %q) error = %v, want a refusal naming it", adv, err)
		}
	}
}
