package config

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/viper"
)

// TokenSource defines how to resolve a GitHub PAT.
// Resolution order: Env, Keychain, File.
// The first non-empty value wins.
type TokenSource struct {
	Env      string `yaml:"env"      mapstructure:"env"      json:"env"`
	Keychain string `yaml:"keychain" mapstructure:"keychain" json:"keychain"`
	File     string `yaml:"file"     mapstructure:"file"     json:"file"`
}

type Repo struct {
	Name  string      `yaml:"name"            mapstructure:"name"  json:"name"`
	Token TokenSource `yaml:"token,omitempty" mapstructure:"token" json:"token"`
}

// ControlConfig defines settings for the remote TCP control server.
type ControlConfig struct {
	Listen     string   `yaml:"listen"      mapstructure:"listen"`
	TLSCert    string   `yaml:"tls_cert"    mapstructure:"tls_cert"`
	TLSKey     string   `yaml:"tls_key"     mapstructure:"tls_key"`
	AllowCIDRs []string `yaml:"allow_cidrs" mapstructure:"allow_cidrs"`
}

type Config struct {
	Auth       TokenSource   `yaml:"auth"        mapstructure:"auth"`
	MaxRunners int           `yaml:"max_runners" mapstructure:"max_runners"`
	Labels     []string      `yaml:"labels"      mapstructure:"labels"`
	Repos      []Repo        `yaml:"repos"       mapstructure:"repos"`
	Control    ControlConfig `yaml:"control"     mapstructure:"control"`
}

// Load reads configuration from the given file path, overlaid with environment
// variables using the GSO_ prefix. If the file does not exist but environment
// variables provide sufficient configuration, the file is treated as optional.
func Load(path string) (*Config, error) {
	v := viper.New()

	// Defaults
	v.SetDefault("max_runners", runtime.NumCPU())
	v.SetDefault("labels", []string{"self-hosted"})

	// Config file
	v.SetConfigType("yaml")

	fileExists := false
	if path != "" {
		if _, err := os.Stat(path); err == nil {
			fileExists = true
			v.SetConfigFile(path)
			if err := v.ReadInConfig(); err != nil {
				return nil, fmt.Errorf("parsing config: %w", err)
			}
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("reading config: %w", err)
		}
	}

	// Bind specific env vars to config keys. We intentionally do NOT use
	// AutomaticEnv because GSO_REPOS needs special handling (JSON string
	// → slice-of-structs) and AutomaticEnv would feed the raw string to
	// Unmarshal, causing a decode error.
	_ = v.BindEnv("auth.env", "GSO_AUTH_ENV")
	_ = v.BindEnv("auth.keychain", "GSO_AUTH_KEYCHAIN")
	_ = v.BindEnv("auth.file", "GSO_AUTH_FILE")
	_ = v.BindEnv("max_runners", "GSO_MAX_RUNNERS")
	_ = v.BindEnv("labels", "GSO_LABELS")
	_ = v.BindEnv("control.listen", "GSO_CONTROL_LISTEN")
	_ = v.BindEnv("control.tls_cert", "GSO_CONTROL_TLS_CERT")
	_ = v.BindEnv("control.tls_key", "GSO_CONTROL_TLS_KEY")

	// Parse GSO_REPOS before Viper unmarshal -- Viper can't map an env
	// string into a slice-of-structs, so we handle it ourselves.
	var envRepos []Repo
	if reposJSON := os.Getenv("GSO_REPOS"); reposJSON != "" {
		if err := json.Unmarshal([]byte(reposJSON), &envRepos); err != nil {
			return nil, fmt.Errorf("parsing GSO_REPOS: %w", err)
		}
		// Remove repos from Viper so Unmarshal doesn't choke on the string
		v.Set("repos", nil)
	}

	cfg := &Config{}

	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	// Apply GSO_REPOS (overrides file-based repos)
	if envRepos != nil {
		cfg.Repos = envRepos
	}

	// Handle GSO_LABELS as comma-separated when set via env
	if labelsEnv := os.Getenv("GSO_LABELS"); labelsEnv != "" {
		cfg.Labels = strings.Split(labelsEnv, ",")
		for i, l := range cfg.Labels {
			cfg.Labels[i] = strings.TrimSpace(l)
		}
	}

	// Handle GSO_CONTROL_ALLOW_CIDRS as comma-separated when set via env
	if cidrsEnv := os.Getenv("GSO_CONTROL_ALLOW_CIDRS"); cidrsEnv != "" {
		cidrs := strings.Split(cidrsEnv, ",")
		for i, c := range cidrs {
			cidrs[i] = strings.TrimSpace(c)
		}
		cfg.Control.AllowCIDRs = cidrs
	}

	// If no config file and no repos from env, report the missing file
	if !fileExists && len(cfg.Repos) == 0 && path != "" {
		return nil, fmt.Errorf("reading config: open %s: no such file or directory", path)
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

	return "", fmt.Errorf("no token configured for repo %s (set auth.env, auth.keychain, auth.file, or per-repo token)", repo.Name)
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
