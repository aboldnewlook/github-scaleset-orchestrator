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
  env: GITHUB_TOKEN
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
		t.Fatalf("expected auth.env GITHUB_TOKEN, got %q", cfg.Auth.Env)
	}
}

func TestLoadDefaults(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	yaml := `
auth:
  env: GH_TOK
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
			yaml:    "auth:\n  env: GH_TOK\nrepos: []\n",
			wantErr: "at least one repo",
		},
		{
			name:    "max_runners zero",
			yaml:    "auth:\n  env: GH_TOK\nmax_runners: 0\nrepos:\n  - name: org/repo\n",
			wantErr: "max_runners must be at least 1",
		},
		{
			name:    "empty repo name",
			yaml:    "auth:\n  env: GH_TOK\nrepos:\n  - name: \"\"\n",
			wantErr: "repo name is required",
		},
		{
			name:    "repo without token and no global auth",
			yaml:    "repos:\n  - name: org/repo\n",
			wantErr: "no token source",
		},
		{
			name: "per-repo token without global auth is ok",
			yaml: "repos:\n  - name: org/repo\n    token:\n      env: REPO_TOK\n",
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

// --- Viper / environment variable tests ---

func TestLoadFromEnvVarsOnly(t *testing.T) {
	t.Setenv("GSO_AUTH_ENV", "GITHUB_TOKEN")
	t.Setenv("GSO_MAX_RUNNERS", "5")
	t.Setenv("GSO_LABELS", "self-hosted,linux")
	t.Setenv("GSO_REPOS", `[{"name":"owner/repo-a"},{"name":"owner/repo-b"}]`)

	cfg, err := Load("/nonexistent/path/config.yaml")
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Auth.Env != "GITHUB_TOKEN" {
		t.Fatalf("expected auth.env GITHUB_TOKEN, got %q", cfg.Auth.Env)
	}
	if cfg.MaxRunners != 5 {
		t.Fatalf("expected max_runners 5, got %d", cfg.MaxRunners)
	}
	if len(cfg.Labels) != 2 || cfg.Labels[0] != "self-hosted" || cfg.Labels[1] != "linux" {
		t.Fatalf("expected labels [self-hosted linux], got %v", cfg.Labels)
	}
	if len(cfg.Repos) != 2 {
		t.Fatalf("expected 2 repos, got %d", len(cfg.Repos))
	}
	if cfg.Repos[0].Name != "owner/repo-a" {
		t.Fatalf("expected first repo owner/repo-a, got %q", cfg.Repos[0].Name)
	}
}

func TestLoadEnvVarsOverrideFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	yaml := `
auth:
  env: FILE_TOKEN
max_runners: 2
labels:
  - self-hosted
repos:
  - name: org/repo-file
`
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Env vars should override file values
	t.Setenv("GSO_AUTH_ENV", "ENV_TOKEN")
	t.Setenv("GSO_MAX_RUNNERS", "8")

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Auth.Env != "ENV_TOKEN" {
		t.Fatalf("expected env override for auth.env, got %q", cfg.Auth.Env)
	}
	if cfg.MaxRunners != 8 {
		t.Fatalf("expected env override for max_runners 8, got %d", cfg.MaxRunners)
	}
	// Repos should still come from file
	if len(cfg.Repos) != 1 || cfg.Repos[0].Name != "org/repo-file" {
		t.Fatalf("expected file repos preserved, got %v", cfg.Repos)
	}
}

func TestLoadEnvReposOverrideFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	yaml := `
auth:
  env: GITHUB_TOKEN
repos:
  - name: org/file-repo
`
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GSO_REPOS", `[{"name":"org/env-repo"}]`)

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if len(cfg.Repos) != 1 || cfg.Repos[0].Name != "org/env-repo" {
		t.Fatalf("expected GSO_REPOS to override file repos, got %v", cfg.Repos)
	}
}

func TestLoadEnvReposInvalidJSON(t *testing.T) {
	t.Setenv("GSO_AUTH_ENV", "GITHUB_TOKEN")
	t.Setenv("GSO_REPOS", `not-valid-json`)

	_, err := Load("/nonexistent/config.yaml")
	if err == nil {
		t.Fatal("expected error for invalid GSO_REPOS JSON")
	}
	if !containsSubstr(err.Error(), "parsing GSO_REPOS") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadEnvAuthTokenFile(t *testing.T) {
	t.Setenv("GSO_AUTH_FILE", "/path/to/token")
	t.Setenv("GSO_REPOS", `[{"name":"org/repo"}]`)

	cfg, err := Load("/nonexistent/config.yaml")
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Auth.File != "/path/to/token" {
		t.Fatalf("expected auth.file /path/to/token, got %q", cfg.Auth.File)
	}
}

func TestLoadEnvAuthTokenKeychain(t *testing.T) {
	t.Setenv("GSO_AUTH_KEYCHAIN", "my-keychain-entry")
	t.Setenv("GSO_REPOS", `[{"name":"org/repo"}]`)

	cfg, err := Load("/nonexistent/config.yaml")
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Auth.Keychain != "my-keychain-entry" {
		t.Fatalf("expected auth.keychain my-keychain-entry, got %q", cfg.Auth.Keychain)
	}
}

func TestLoadEnvOnlyDefaults(t *testing.T) {
	// Only set required env vars, let defaults handle the rest
	t.Setenv("GSO_AUTH_ENV", "GITHUB_TOKEN")
	t.Setenv("GSO_REPOS", `[{"name":"org/repo"}]`)

	cfg, err := Load("/nonexistent/config.yaml")
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

func TestLoadEmptyPathWithEnvVars(t *testing.T) {
	t.Setenv("GSO_AUTH_ENV", "GITHUB_TOKEN")
	t.Setenv("GSO_REPOS", `[{"name":"org/repo"}]`)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Auth.Env != "GITHUB_TOKEN" {
		t.Fatalf("expected auth.env GITHUB_TOKEN, got %q", cfg.Auth.Env)
	}
}

func TestLoadReposWithPerRepoToken(t *testing.T) {
	t.Setenv("GSO_REPOS", `[{"name":"org/repo","token":{"env":"REPO_TOKEN"}}]`)

	cfg, err := Load("/nonexistent/config.yaml")
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Repos[0].Token.Env != "REPO_TOKEN" {
		t.Fatalf("expected per-repo env REPO_TOKEN, got %q", cfg.Repos[0].Token.Env)
	}
}

func TestLoadControlConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	yaml := `
auth:
  env: GITHUB_TOKEN
repos:
  - name: org/repo
control:
  listen: ":9100"
  tls_cert: /path/to/cert.pem
  tls_key: /path/to/key.pem
  allow_cidrs:
    - 192.168.1.0/24
    - 10.0.0.0/8
`
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Control.Listen != ":9100" {
		t.Fatalf("expected control.listen :9100, got %q", cfg.Control.Listen)
	}
	if cfg.Control.TLSCert != "/path/to/cert.pem" {
		t.Fatalf("expected control.tls_cert /path/to/cert.pem, got %q", cfg.Control.TLSCert)
	}
	if cfg.Control.TLSKey != "/path/to/key.pem" {
		t.Fatalf("expected control.tls_key /path/to/key.pem, got %q", cfg.Control.TLSKey)
	}
	if len(cfg.Control.AllowCIDRs) != 2 {
		t.Fatalf("expected 2 allow_cidrs, got %d", len(cfg.Control.AllowCIDRs))
	}
	if cfg.Control.AllowCIDRs[0] != "192.168.1.0/24" {
		t.Fatalf("expected first CIDR 192.168.1.0/24, got %q", cfg.Control.AllowCIDRs[0])
	}
	if cfg.Control.AllowCIDRs[1] != "10.0.0.0/8" {
		t.Fatalf("expected second CIDR 10.0.0.0/8, got %q", cfg.Control.AllowCIDRs[1])
	}
}

func TestLoadControlConfigFromEnv(t *testing.T) {
	t.Setenv("GSO_AUTH_ENV", "GITHUB_TOKEN")
	t.Setenv("GSO_REPOS", `[{"name":"org/repo"}]`)
	t.Setenv("GSO_CONTROL_LISTEN", ":9200")
	t.Setenv("GSO_CONTROL_TLS_CERT", "/env/cert.pem")
	t.Setenv("GSO_CONTROL_TLS_KEY", "/env/key.pem")
	t.Setenv("GSO_CONTROL_ALLOW_CIDRS", "10.0.0.0/8, 172.16.0.0/12")

	cfg, err := Load("/nonexistent/config.yaml")
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Control.Listen != ":9200" {
		t.Fatalf("expected control.listen :9200, got %q", cfg.Control.Listen)
	}
	if cfg.Control.TLSCert != "/env/cert.pem" {
		t.Fatalf("expected control.tls_cert /env/cert.pem, got %q", cfg.Control.TLSCert)
	}
	if cfg.Control.TLSKey != "/env/key.pem" {
		t.Fatalf("expected control.tls_key /env/key.pem, got %q", cfg.Control.TLSKey)
	}
	if len(cfg.Control.AllowCIDRs) != 2 {
		t.Fatalf("expected 2 allow_cidrs, got %d", len(cfg.Control.AllowCIDRs))
	}
	if cfg.Control.AllowCIDRs[0] != "10.0.0.0/8" {
		t.Fatalf("expected first CIDR 10.0.0.0/8, got %q", cfg.Control.AllowCIDRs[0])
	}
	if cfg.Control.AllowCIDRs[1] != "172.16.0.0/12" {
		t.Fatalf("expected second CIDR 172.16.0.0/12, got %q", cfg.Control.AllowCIDRs[1])
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
