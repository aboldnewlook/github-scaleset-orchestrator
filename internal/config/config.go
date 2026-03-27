package config

import (
	"fmt"
	"os"
	"runtime"

	"gopkg.in/yaml.v3"
)

// TokenSource defines how to resolve a GitHub PAT.
// Resolution order: Env, Keychain, File.
// The first non-empty value wins.
type TokenSource struct {
	Env      string `yaml:"token_env"`      // env var name (e.g. "GITHUB_TOKEN")
	Keychain string `yaml:"token_keychain"` // OS keyring service name
	File     string `yaml:"token_file"`     // path to file containing token
}

type Repo struct {
	Name  string      `yaml:"name"`
	Token TokenSource `yaml:"token,omitempty"` // per-repo override
}

type Config struct {
	Auth       TokenSource `yaml:"auth"`
	MaxRunners int         `yaml:"max_runners"`
	Labels     []string    `yaml:"labels"`
	Repos      []Repo      `yaml:"repos"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	cfg := &Config{
		MaxRunners: runtime.NumCPU(),
		Labels:     []string{"self-hosted"},
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// TokenForRepo resolves the token for a given repo.
// Per-repo token takes precedence over global auth.
func (c *Config) TokenForRepo(repo Repo) (string, error) {
	// Try per-repo token first
	if token, err := repo.Token.Resolve(); err != nil {
		return "", fmt.Errorf("repo %s: %w", repo.Name, err)
	} else if token != "" {
		return token, nil
	}

	// Fall back to global auth
	if token, err := c.Auth.Resolve(); err != nil {
		return "", fmt.Errorf("global auth: %w", err)
	} else if token != "" {
		return token, nil
	}

	return "", fmt.Errorf("no token configured for repo %s (set auth.token_env, auth.token_keychain, auth.token_file, or per-repo token)", repo.Name)
}

func (c *Config) validate() error {
	if len(c.Repos) == 0 {
		return fmt.Errorf("at least one repo is required")
	}
	if c.MaxRunners < 1 {
		return fmt.Errorf("max_runners must be at least 1")
	}

	// Validate that at least global auth or every repo has a token source
	globalHasSource := c.Auth.hasSource()
	for _, repo := range c.Repos {
		if repo.Name == "" {
			return fmt.Errorf("repo name is required")
		}
		if !globalHasSource && !repo.Token.hasSource() {
			return fmt.Errorf("repo %s has no token source and no global auth configured", repo.Name)
		}
	}

	return nil
}

func (ts *TokenSource) hasSource() bool {
	return ts.Env != "" || ts.Keychain != "" || ts.File != ""
}
