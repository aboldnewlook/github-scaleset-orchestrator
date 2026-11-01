package config

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/spf13/viper"

	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
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
	Name        string      `yaml:"name"            mapstructure:"name"  json:"name"`
	Token       TokenSource `yaml:"token,omitempty" mapstructure:"token" json:"token"`
	RunnerCache string      `yaml:"runner_cache,omitempty" mapstructure:"runner_cache" json:"runner_cache,omitempty"`
}

// ControlConfig defines settings for the remote TCP control server.
type ControlConfig struct {
	Listen     string   `yaml:"listen"      mapstructure:"listen"`
	TLSCert    string   `yaml:"tls_cert"    mapstructure:"tls_cert"`
	TLSKey     string   `yaml:"tls_key"     mapstructure:"tls_key"`
	AllowCIDRs []string `yaml:"allow_cidrs" mapstructure:"allow_cidrs"`
}

// RunnerConfig selects how ephemeral jobs execute. Docker jobs run in sibling
// containers and do not inherit the manager's environment or mounts.
type RunnerConfig struct {
	Executor   string  `yaml:"executor"  mapstructure:"executor"`
	Image      string  `yaml:"image"     mapstructure:"image"`
	MemoryMB   int64   `yaml:"memory_mb" mapstructure:"memory_mb"`
	CPUs       float64 `yaml:"cpus"      mapstructure:"cpus"`
	Cache      string  `yaml:"cache"     mapstructure:"cache"`
	CacheRoot  string  `yaml:"cache_root" mapstructure:"cache_root"`
	InstanceID string  `yaml:"instance_id" mapstructure:"instance_id"`
	CacheUID   int     `yaml:"cache_uid" mapstructure:"cache_uid"`
	CacheGID   int     `yaml:"cache_gid" mapstructure:"cache_gid"`
	DockerHost string  `yaml:"docker_host" mapstructure:"docker_host"`
}

// WorkersConfig enables remote workers (design spec §4.2, §9.1). Everything
// here is off unless Listen is set.
type WorkersConfig struct {
	Listen            string   `yaml:"listen"               mapstructure:"listen"`
	Advertise         string   `yaml:"advertise"            mapstructure:"advertise"`
	AllowCIDRs        []string `yaml:"allow_cidrs"          mapstructure:"allow_cidrs"`
	MaxSlotsPerWorker int      `yaml:"max_slots_per_worker" mapstructure:"max_slots_per_worker"`
	DefaultClass      string   `yaml:"default_class"        mapstructure:"default_class"`
}

type Config struct {
	Auth       TokenSource   `yaml:"auth"        mapstructure:"auth"`
	MaxRunners int           `yaml:"max_runners" mapstructure:"max_runners"`
	Labels     []string      `yaml:"labels"      mapstructure:"labels"`
	Repos      []Repo        `yaml:"repos"       mapstructure:"repos"`
	Control    ControlConfig `yaml:"control"     mapstructure:"control"`
	Runner     RunnerConfig  `yaml:"runner"      mapstructure:"runner"`
	Workers    WorkersConfig `yaml:"workers"     mapstructure:"workers"`
	// GitHubSim is the base URL of a simulated GitHub. When set, repos need
	// no token source: the simulator accepts none.
	GitHubSim string `yaml:"github_sim" mapstructure:"github_sim"`
}

// Load reads configuration from the given file path, overlaid with environment
// variables using the GSO_ prefix. If the file does not exist but environment
// variables provide sufficient configuration, the file is treated as optional.
func Load(path string) (*Config, error) {
	v := viper.New()

	// Defaults
	v.SetDefault("max_runners", runtime.NumCPU())
	v.SetDefault("labels", []string{"self-hosted"})
	v.SetDefault("runner.executor", "subprocess")
	v.SetDefault("runner.cache", "none")
	v.SetDefault("runner.cache_uid", 1001)
	v.SetDefault("runner.cache_gid", 1001)
	v.SetDefault("workers.max_slots_per_worker", 16)

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
	_ = v.BindEnv("runner.executor", "GSO_RUNNER_EXECUTOR")
	_ = v.BindEnv("runner.image", "GSO_RUNNER_IMAGE")
	_ = v.BindEnv("runner.memory_mb", "GSO_RUNNER_MEMORY_MB")
	_ = v.BindEnv("runner.cpus", "GSO_RUNNER_CPUS")
	_ = v.BindEnv("runner.cache", "GSO_RUNNER_CACHE")
	_ = v.BindEnv("runner.cache_root", "GSO_RUNNER_CACHE_ROOT")
	_ = v.BindEnv("runner.instance_id", "GSO_RUNNER_INSTANCE_ID")
	_ = v.BindEnv("runner.cache_uid", "GSO_RUNNER_CACHE_UID")
	_ = v.BindEnv("runner.cache_gid", "GSO_RUNNER_CACHE_GID")
	_ = v.BindEnv("runner.docker_host", "GSO_RUNNER_DOCKER_HOST")
	_ = v.BindEnv("workers.listen", "GSO_WORKERS_LISTEN")
	_ = v.BindEnv("workers.advertise", "GSO_WORKERS_ADVERTISE")
	_ = v.BindEnv("workers.max_slots_per_worker", "GSO_WORKERS_MAX_SLOTS_PER_WORKER")
	_ = v.BindEnv("workers.default_class", "GSO_WORKERS_DEFAULT_CLASS")
	_ = v.BindEnv("github_sim", "GSO_GITHUB_SIM")

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

	// Handle GSO_WORKERS_ALLOW_CIDRS as comma-separated when set via env
	if cidrsEnv := os.Getenv("GSO_WORKERS_ALLOW_CIDRS"); cidrsEnv != "" {
		cidrs := strings.Split(cidrsEnv, ",")
		for i, c := range cidrs {
			cidrs[i] = strings.TrimSpace(c)
		}
		cfg.Workers.AllowCIDRs = cidrs
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
	if c.MaxRunners < 0 {
		return fmt.Errorf("max_runners cannot be negative")
	}
	// max_runners is the local member's slots (design spec §5.5). Zero means the
	// router runs no jobs itself, which only makes sense with remote workers.
	if c.MaxRunners == 0 && !c.WorkersEnabled() {
		return fmt.Errorf("max_runners must be at least 1 (max_runners: 0 requires workers.listen)")
	}
	if err := c.Workers.validate(); err != nil {
		return err
	}
	if err := validateGitHubSim(c.GitHubSim); err != nil {
		return err
	}
	if c.Runner.Executor != "subprocess" && c.Runner.Executor != "docker" {
		return fmt.Errorf("runner.executor must be subprocess or docker")
	}
	if c.Runner.Executor == "docker" && strings.TrimSpace(c.Runner.Image) == "" {
		return fmt.Errorf("runner.image is required for docker executor")
	}
	if c.Runner.Executor == "docker" && strings.TrimSpace(c.Runner.InstanceID) == "" {
		return fmt.Errorf("runner.instance_id is required for docker executor")
	}
	if c.Runner.MemoryMB < 0 || c.Runner.CPUs < 0 {
		return fmt.Errorf("runner resource limits cannot be negative")
	}
	if c.Runner.MemoryMB > math.MaxInt64/(1024*1024) || math.IsNaN(c.Runner.CPUs) || math.IsInf(c.Runner.CPUs, 0) || c.Runner.CPUs > float64(math.MaxInt64)/1e9 {
		return fmt.Errorf("runner resource limits are too large")
	}
	if c.Runner.Cache != "none" && c.Runner.Cache != "repo" {
		return fmt.Errorf("runner.cache must be none or repo")
	}
	if c.Runner.CacheRoot != "" && !filepath.IsAbs(c.Runner.CacheRoot) {
		return fmt.Errorf("runner.cache_root must be an absolute path")
	}
	if c.Runner.CacheUID < 0 || c.Runner.CacheGID < 0 {
		return fmt.Errorf("runner cache UID and GID cannot be negative")
	}
	if c.Runner.DockerHost != "" {
		if _, err := runner.DockerSocketPath(c.Runner.DockerHost); err != nil {
			return fmt.Errorf("runner.docker_host: %w", err)
		}
	}

	// Validate that at least global auth or every repo has a token source
	globalHasSource := c.Auth.hasSource()
	for _, repo := range c.Repos {
		if repo.Name == "" {
			return fmt.Errorf("repo name is required")
		}
		if repo.RunnerCache != "" && repo.RunnerCache != "none" && repo.RunnerCache != "repo" {
			return fmt.Errorf("repo %s runner_cache must be none or repo", repo.Name)
		}
		if !c.Simulated() && !globalHasSource && !repo.Token.hasSource() {
			return fmt.Errorf("repo %s has no token source and no global auth configured", repo.Name)
		}
	}

	return nil
}

func (ts *TokenSource) hasSource() bool {
	return ts.Env != "" || ts.Keychain != "" || ts.File != ""
}

// WorkersEnabled reports whether the router accepts remote workers.
func (c *Config) WorkersEnabled() bool {
	return c.Workers.Listen != ""
}

// DefaultClass is the class that carries the configured labels and that bare
// `runs-on: self-hosted` lands on (design spec §6.1–6.2): workers.default_class,
// or the router host's own class. validate has already parsed it.
func (c *Config) DefaultClass() runner.Class {
	if c.Workers.DefaultClass != "" {
		if class, err := runner.ParseClass(c.Workers.DefaultClass); err == nil {
			return class
		}
	}
	return runner.HostClass()
}

// StateDir is where the router keeps state that must survive restarts
// (worker TLS key, workers.json, classes.json; design spec §9.1). In the
// container it is /home/runner/.config/gso and must be on a volume.
func StateDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locating the user config directory: %w", err)
	}
	return filepath.Join(dir, "gso"), nil
}

func (w WorkersConfig) validate() error {
	if w.Listen != "" {
		if _, port, err := net.SplitHostPort(w.Listen); err != nil || !validPort(port) {
			return fmt.Errorf("workers.listen must be [host]:port, got %q", w.Listen)
		}
	}
	if w.Advertise != "" {
		host, port, err := net.SplitHostPort(w.Advertise)
		if err != nil || host == "" || !validPort(port) {
			return fmt.Errorf("workers.advertise must be host:port, got %q", w.Advertise)
		}
	}
	for _, cidr := range w.AllowCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("workers.allow_cidrs: invalid CIDR %q", cidr)
		}
	}
	if w.MaxSlotsPerWorker < 1 || w.MaxSlotsPerWorker > 256 {
		return fmt.Errorf("workers.max_slots_per_worker must be between 1 and 256")
	}
	if w.DefaultClass != "" {
		if _, err := runner.ParseClass(w.DefaultClass); err != nil {
			return fmt.Errorf("workers.default_class: %w", err)
		}
	}
	return nil
}

func validPort(port string) bool {
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}

// Simulated reports whether the router talks to a simulated GitHub
// (github_sim) instead of github.com.
func (c *Config) Simulated() bool {
	return c.GitHubSim != ""
}

func validateGitHubSim(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("github_sim must be an http(s) URL, got %q", raw)
	}
	return nil
}
