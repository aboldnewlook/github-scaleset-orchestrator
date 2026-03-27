package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoadValidConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	yaml := `
auth:
  token_env: GITHUB_TOKEN
max_runners: 3
labels:
  - self-hosted
  - linux
repos:
  - name: org/repo-a
  - name: org/repo-b
`
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.MaxRunners != 3 {
		t.Fatalf("expected max_runners 3, got %d", cfg.MaxRunners)
	}
	if len(cfg.Labels) != 2 {
		t.Fatalf("expected 2 labels, got %d", len(cfg.Labels))
	}
	if len(cfg.Repos) != 2 {
		t.Fatalf("expected 2 repos, got %d", len(cfg.Repos))
	}
	if cfg.Auth.Env != "GITHUB_TOKEN" {
		t.Fatalf("expected auth.token_env GITHUB_TOKEN, got %q", cfg.Auth.Env)
	}
}

func TestLoadDefaults(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	yaml := `
auth:
  token_env: GH_TOK
repos:
  - name: org/repo
`
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.MaxRunners != runtime.NumCPU() {
		t.Fatalf("expected default max_runners %d, got %d", runtime.NumCPU(), cfg.MaxRunners)
	}
	if len(cfg.Labels) != 1 || cfg.Labels[0] != "self-hosted" {
		t.Fatalf("expected default labels [self-hosted], got %v", cfg.Labels)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadBadYAML(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	if err := os.WriteFile(cfgPath, []byte("{{{{not yaml"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(cfgPath)
	if err == nil {
		t.Fatal("expected error for bad YAML")
	}
}

func TestLoadValidation(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "no repos",
			yaml:    "auth:\n  token_env: GH_TOK\nrepos: []\n",
			wantErr: "at least one repo",
		},
		{
			name:    "max_runners zero",
			yaml:    "auth:\n  token_env: GH_TOK\nmax_runners: 0\nrepos:\n  - name: org/repo\n",
			wantErr: "max_runners must be at least 1",
		},
		{
			name:    "empty repo name",
			yaml:    "auth:\n  token_env: GH_TOK\nrepos:\n  - name: \"\"\n",
			wantErr: "repo name is required",
		},
		{
			name:    "repo without token and no global auth",
			yaml:    "repos:\n  - name: org/repo\n",
			wantErr: "no token source",
		},
		{
			name: "per-repo token without global auth is ok",
			yaml: "repos:\n  - name: org/repo\n    token:\n      token_env: REPO_TOK\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(cfgPath, []byte(tt.yaml), 0o644); err != nil {
				t.Fatal(err)
			}

			_, err := Load(cfgPath)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatal("expected error")
				}
				if !containsSubstr(err.Error(), tt.wantErr) {
					t.Fatalf("error %q should contain %q", err.Error(), tt.wantErr)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestTokenForRepo_EnvVar(t *testing.T) {
	t.Setenv("TEST_TOKEN_GLOBAL", "global-tok-123")
	t.Setenv("TEST_TOKEN_REPO", "repo-tok-456")

	cfg := &Config{
		Auth:       TokenSource{Env: "TEST_TOKEN_GLOBAL"},
		MaxRunners: 1,
		Repos: []Repo{
			{Name: "org/repo-a"},
			{Name: "org/repo-b", Token: TokenSource{Env: "TEST_TOKEN_REPO"}},
		},
	}

	// Repo without per-repo token falls back to global
	tok, err := cfg.TokenForRepo(cfg.Repos[0])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "global-tok-123" {
		t.Fatalf("expected global token, got %q", tok)
	}

	// Repo with per-repo token uses that
	tok, err = cfg.TokenForRepo(cfg.Repos[1])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "repo-tok-456" {
		t.Fatalf("expected repo token, got %q", tok)
	}
}

func TestTokenForRepo_File(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("  file-tok-789\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{
		Auth:       TokenSource{File: tokenFile},
		MaxRunners: 1,
		Repos:      []Repo{{Name: "org/repo"}},
	}

	tok, err := cfg.TokenForRepo(cfg.Repos[0])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "file-tok-789" {
		t.Fatalf("expected trimmed file token, got %q", tok)
	}
}

func TestTokenForRepo_MissingFile(t *testing.T) {
	cfg := &Config{
		Auth:       TokenSource{File: "/nonexistent/token"},
		MaxRunners: 1,
		Repos:      []Repo{{Name: "org/repo"}},
	}

	_, err := cfg.TokenForRepo(cfg.Repos[0])
	if err == nil {
		t.Fatal("expected error for missing token file")
	}
}

func TestTokenForRepo_NoSource(t *testing.T) {
	cfg := &Config{
		Auth:       TokenSource{},
		MaxRunners: 1,
		Repos:      []Repo{{Name: "org/repo"}},
	}

	_, err := cfg.TokenForRepo(cfg.Repos[0])
	if err == nil {
		t.Fatal("expected error when no token source configured")
	}
	if !containsSubstr(err.Error(), "no token configured") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTokenForRepo_EnvVarPrecedence(t *testing.T) {
	// Env var takes precedence over file
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("file-tok"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PREC_TOKEN", "env-tok")

	cfg := &Config{
		Auth:       TokenSource{Env: "PREC_TOKEN", File: tokenFile},
		MaxRunners: 1,
		Repos:      []Repo{{Name: "org/repo"}},
	}

	tok, err := cfg.TokenForRepo(cfg.Repos[0])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "env-tok" {
		t.Fatalf("expected env token to take precedence, got %q", tok)
	}
}

func TestHasSource(t *testing.T) {
	tests := []struct {
		name string
		ts   TokenSource
		want bool
	}{
		{"empty", TokenSource{}, false},
		{"env only", TokenSource{Env: "TOK"}, true},
		{"keychain only", TokenSource{Keychain: "acct"}, true},
		{"file only", TokenSource{File: "/path"}, true},
		{"all set", TokenSource{Env: "TOK", Keychain: "acct", File: "/path"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.ts.hasSource()
			if got != tt.want {
				t.Fatalf("hasSource() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolve_NilReceiver(t *testing.T) {
	var ts *TokenSource
	tok, err := ts.Resolve()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "" {
		t.Fatalf("expected empty token from nil TokenSource, got %q", tok)
	}
}

func TestResolve_EmptyEnvVar(t *testing.T) {
	// Env var is set to empty string — should fall through
	t.Setenv("EMPTY_TOKEN_VAR", "")

	ts := &TokenSource{Env: "EMPTY_TOKEN_VAR"}
	tok, err := ts.Resolve()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "" {
		t.Fatalf("expected empty token for empty env var, got %q", tok)
	}
}

func TestResolve_EmptyTokenFile(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "empty_token")
	if err := os.WriteFile(tokenFile, []byte("   \n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ts := &TokenSource{File: tokenFile}
	tok, err := ts.Resolve()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "" {
		t.Fatalf("expected empty token for whitespace-only file, got %q", tok)
	}
}

func containsSubstr(s, sub string) bool {
	return len(s) >= len(sub) && searchStr(s, sub)
}

func searchStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
