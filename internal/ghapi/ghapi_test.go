package ghapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/config"
	"github.com/actions/scaleset"
)

func TestNewHTTPClientEnablesHTTP2HealthChecks(t *testing.T) {
	c := NewHTTPClient()
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", c.Transport)
	}
	if tr.HTTP2 == nil || tr.HTTP2.SendPingTimeout != 30*time.Second || tr.HTTP2.PingTimeout != 15*time.Second {
		t.Fatalf("HTTP2 = %+v, want SendPingTimeout 30s and PingTimeout 15s (scaleset#131)", tr.HTTP2)
	}
	if !tr.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 is off")
	}
	if c.Timeout != scaleset.DefaultTimeout {
		t.Fatalf("Timeout = %v, want scaleset.DefaultTimeout", c.Timeout)
	}
}

func TestIsConflict(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{fmt.Errorf("creating session: %w", scaleset.ConflictError), true},
		{scaleset.ConflictError, true},
		{errors.New("409 Conflict"), false}, // string matching is what this replaces
		{nil, false},
	}
	for _, tt := range tests {
		if got := IsConflict(tt.err); got != tt.want {
			t.Errorf("IsConflict(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}

func TestFactoryResolvesThePerRepoToken(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{Auth: config.TokenSource{Env: "GSO_GHAPI_TEST_TOKEN"}}
	factory := NewFactory(cfg, nil, logger)

	t.Setenv("GSO_GHAPI_TEST_TOKEN", "")
	if _, err := factory(context.Background(), config.Repo{Name: "org/repo"}); err == nil || !strings.Contains(err.Error(), "org/repo") {
		t.Fatalf("factory without a token: err = %v, want one naming org/repo", err)
	}

	t.Setenv("GSO_GHAPI_TEST_TOKEN", "ghp_factorytest")
	c, err := factory(context.Background(), config.Repo{Name: "org/repo"})
	if err != nil || c == nil {
		t.Fatalf("factory = %v, %v", c, err)
	}
	for _, bad := range []string{"org", "org/", "/repo", "a/b/c"} {
		if _, err := factory(context.Background(), config.Repo{Name: bad}); err == nil {
			t.Errorf("factory accepted repo %q", bad)
		}
	}
}
