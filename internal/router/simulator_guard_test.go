package router

import (
	"context"
	"strings"
	"testing"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/ghapi"
)

// Until the orchestrator takes a GitHub factory (A3b), a router given one must
// refuse to start rather than drive real GitHub while claiming to simulate it.
func TestNewRefusesASimulatorFactoryItCannotHonour(t *testing.T) {
	factory := ghapi.Factory(func(context.Context, config.Repo) (ghapi.Client, error) { return nil, nil })
	r, err := New(loadTestConfig(t, workersYAML), nil, nil, Options{StateDir: t.TempDir(), FleetTiming: fastTiming(), GitHub: factory})
	if err == nil || r != nil {
		t.Fatalf("New = %v, %v; want a refusal", r, err)
	}
	if !strings.Contains(err.Error(), "refusing to start against real GitHub") {
		t.Fatalf("error = %v", err)
	}
}
