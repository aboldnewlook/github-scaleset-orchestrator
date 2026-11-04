package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

func writeWorkersConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "auth:\n  env: GITHUB_TOKEN\nrepos:\n  - name: org/repo\n" + body
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadWorkersConfigFromFile(t *testing.T) {
	cfg, err := Load(writeWorkersConfig(t, `max_runners: 0
workers:
  listen: ":9200"
  advertise: "unraid.lan:9200"
  allow_cidrs:
    - 192.168.1.0/24
    - 172.16.0.0/12
  max_slots_per_worker: 8
  default_class: linux-arm64
`))
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	w := cfg.Workers
	if w.Listen != ":9200" || w.Advertise != "unraid.lan:9200" || w.MaxSlotsPerWorker != 8 {
		t.Fatalf("workers = %+v", w)
	}
	if strings.Join(w.AllowCIDRs, ",") != "192.168.1.0/24,172.16.0.0/12" {
		t.Fatalf("allow_cidrs = %v", w.AllowCIDRs)
	}
	if !cfg.WorkersEnabled() {
		t.Fatal("WorkersEnabled() = false with workers.listen set")
	}
	if got := cfg.DefaultClass(); got != (runner.Class{OS: "linux", Arch: "arm64"}) {
		t.Fatalf("DefaultClass() = %v, want linux-arm64", got)
	}
	if cfg.MaxRunners != 0 {
		t.Fatalf("max_runners = %d, want 0", cfg.MaxRunners)
	}
}

func TestLoadWorkersDefaults(t *testing.T) {
	cfg, err := Load(writeWorkersConfig(t, "max_runners: 2\n"))
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.WorkersEnabled() {
		t.Fatal("WorkersEnabled() = true without workers.listen")
	}
	if cfg.Workers.MaxSlotsPerWorker != 16 {
		t.Fatalf("max_slots_per_worker default = %d, want 16", cfg.Workers.MaxSlotsPerWorker)
	}
	if got := cfg.DefaultClass(); got != runner.HostClass() {
		t.Fatalf("DefaultClass() = %v, want the host class %v", got, runner.HostClass())
	}
}

func TestLoadWorkersFromEnv(t *testing.T) {
	t.Setenv("GSO_MAX_RUNNERS", "0")
	t.Setenv("GSO_WORKERS_LISTEN", ":9300")
	t.Setenv("GSO_WORKERS_ADVERTISE", "10.0.0.5:9300")
	t.Setenv("GSO_WORKERS_ALLOW_CIDRS", "192.168.1.0/24, 172.16.0.0/12")
	t.Setenv("GSO_WORKERS_MAX_SLOTS_PER_WORKER", "4")
	t.Setenv("GSO_WORKERS_DEFAULT_CLASS", "linux-x64")
	cfg, err := Load(writeWorkersConfig(t, ""))
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	w := cfg.Workers
	if w.Listen != ":9300" || w.Advertise != "10.0.0.5:9300" || w.MaxSlotsPerWorker != 4 || w.DefaultClass != "linux-x64" {
		t.Fatalf("workers from env = %+v", w)
	}
	if strings.Join(w.AllowCIDRs, ",") != "192.168.1.0/24,172.16.0.0/12" {
		t.Fatalf("allow_cidrs from env = %v", w.AllowCIDRs)
	}
}

func TestLoadMaxRunnersZero(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"zero without a worker listener", "max_runners: 0\n", "max_runners: 0 requires workers.listen"},
		{"zero with a worker listener", "max_runners: 0\nworkers:\n  listen: \":9200\"\n", ""},
		{"negative", "max_runners: -1\n", "max_runners cannot be negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeWorkersConfig(t, tt.body))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Load() error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Load() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadWorkersValidation(t *testing.T) {
	tests := []struct {
		name    string
		workers string
		wantErr string
	}{
		{"bad CIDR", "  allow_cidrs: [192.168.1.0/33]\n", "workers.allow_cidrs: invalid CIDR"},
		{"advertise without port", "  advertise: unraid.lan\n", "workers.advertise must be host:port"},
		{"advertise without host", "  advertise: \":9200\"\n", "workers.advertise must be host:port"},
		{"advertise with a named port", "  advertise: unraid.lan:http\n", "workers.advertise must be host:port"},
		{"advertise with an underscore host", "  advertise: my_nas:9200\n", "workers.advertise"},
		{"advertise with a trailing dot", "  advertise: unraid.lan.:9200\n", "workers.advertise"},
		{"advertise with userinfo", "  advertise: a@unraid.lan:9200\n", "workers.advertise"},
		{"listen without colon", "  listen: \"9200\"\n", "workers.listen must be [host]:port"},
		{"zero slots per worker", "  max_slots_per_worker: 0\n", "workers.max_slots_per_worker must be between 1 and 256"},
		{"too many slots per worker", "  max_slots_per_worker: 300\n", "workers.max_slots_per_worker must be between 1 and 256"},
		{"unknown default class", "  default_class: linux-ppc64\n", "workers.default_class"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeWorkersConfig(t, "max_runners: 1\nworkers:\n"+tt.workers))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Load() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestStateDir(t *testing.T) {
	base, err := os.UserConfigDir()
	if err != nil {
		t.Skipf("no user config dir: %v", err)
	}
	got, err := StateDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(base, "gso"); got != want {
		t.Fatalf("StateDir() = %q, want %q", got, want)
	}
}

func TestLoadGitHubSim(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string // written as-is: no auth block
		env     string // GSO_GITHUB_SIM, "" = unset
		wantSim string
		wantErr string
	}{
		{"file; repos need no token", "github_sim: http://127.0.0.1:8088\nrepos:\n  - name: org/repo\n", "", "http://127.0.0.1:8088", ""},
		{"env; repos need no token", "repos:\n  - name: org/repo\n", "https://gh-sim.lan:8443", "https://gh-sim.lan:8443", ""},
		{"not http", "github_sim: ftp://sim.lan\nrepos:\n  - name: org/repo\n", "", "", "github_sim must be an http(s) URL"},
		{"no host", "github_sim: \"http://\"\nrepos:\n  - name: org/repo\n", "", "", "github_sim must be an http(s) URL"},
		{"not a URL", "github_sim: sim.lan:8088\nrepos:\n  - name: org/repo\n", "", "", "github_sim must be an http(s) URL"},
		{"unset still needs a token source", "repos:\n  - name: org/repo\n", "", "", "no token source"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env != "" {
				t.Setenv("GSO_GITHUB_SIM", tt.env)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tt.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Load() error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error: %v", err)
			}
			if cfg.GitHubSim != tt.wantSim || !cfg.Simulated() {
				t.Fatalf("GitHubSim = %q, Simulated() = %v; want %q, true", cfg.GitHubSim, cfg.Simulated(), tt.wantSim)
			}
		})
	}
}

func TestLoadWorkersBootstrapFromFile(t *testing.T) {
	cfg, err := Load(writeWorkersConfig(t, `max_runners: 0
workers:
  listen: ":9200"
  advertise: "unraid.lan:9200"
  bootstrap:
    name: unraid
    classes: [linux-x64, linux-arm64]
    slots: 3
    join_file: /bootstrap/join
`))
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	b := cfg.Workers.Bootstrap
	if b.Name != "unraid" || b.Slots != 3 || b.JoinFile != "/bootstrap/join" || strings.Join(b.Classes, ",") != "linux-x64,linux-arm64" {
		t.Fatalf("bootstrap = %+v", b)
	}
	if !b.Enabled() {
		t.Fatal("Enabled() = false with name and join_file set")
	}
}

func TestLoadWorkersBootstrapFromEnv(t *testing.T) {
	t.Setenv("GSO_MAX_RUNNERS", "0")
	t.Setenv("GSO_WORKERS_LISTEN", ":9300")
	t.Setenv("GSO_WORKERS_ADVERTISE", "10.0.0.5:9300")
	t.Setenv("GSO_WORKERS_BOOTSTRAP_NAME", "unraid")
	t.Setenv("GSO_WORKERS_BOOTSTRAP_CLASSES", "linux-x64, linux-arm64")
	t.Setenv("GSO_WORKERS_BOOTSTRAP_SLOTS", "2")
	t.Setenv("GSO_WORKERS_BOOTSTRAP_JOIN_FILE", "/bootstrap/join")
	cfg, err := Load(writeWorkersConfig(t, ""))
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	b := cfg.Workers.Bootstrap
	if b.Name != "unraid" || b.Slots != 2 || b.JoinFile != "/bootstrap/join" || strings.Join(b.Classes, ",") != "linux-x64,linux-arm64" {
		t.Fatalf("bootstrap from env = %+v", b)
	}
}

func TestLoadWorkersBootstrapIsOffUnlessNameAndJoinFileAreSet(t *testing.T) {
	for name, body := range map[string]string{
		"unset":     "",
		"name only": "    name: unraid\n",
		"file only": "    join_file: /bootstrap/join\n",
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := Load(writeWorkersConfig(t, "max_runners: 1\nworkers:\n  bootstrap:\n    slots: 1\n"+body))
			if err != nil {
				t.Fatalf("Load() error: %v", err)
			}
			if cfg.Workers.Bootstrap.Enabled() {
				t.Fatalf("Enabled() = true for %+v", cfg.Workers.Bootstrap)
			}
		})
	}
}

func TestLoadWorkersBootstrapNeedsTheWorkerListener(t *testing.T) {
	_, err := Load(writeWorkersConfig(t, "max_runners: 1\nworkers:\n  bootstrap:\n    name: unraid\n    join_file: /bootstrap/join\n    classes: [linux-x64]\n"))
	if err == nil || !strings.Contains(err.Error(), "workers.bootstrap requires workers.listen") {
		t.Fatalf("Load() error = %v, want it to require workers.listen", err)
	}
}

func TestLoadCleanupStaleScaleSets(t *testing.T) {
	t.Run("defaults to on", func(t *testing.T) {
		cfg, err := Load(writeWorkersConfig(t, "max_runners: 2\n"))
		if err != nil {
			t.Fatalf("Load() error: %v", err)
		}
		if !cfg.CleanupStaleScaleSets {
			t.Fatal("cleanup_stale_scale_sets default = false, want true")
		}
	})
	t.Run("file turns it off", func(t *testing.T) {
		cfg, err := Load(writeWorkersConfig(t, "max_runners: 2\ncleanup_stale_scale_sets: false\n"))
		if err != nil {
			t.Fatalf("Load() error: %v", err)
		}
		if cfg.CleanupStaleScaleSets {
			t.Fatal("cleanup_stale_scale_sets = true, want false from the file")
		}
	})
	t.Run("env turns it off", func(t *testing.T) {
		t.Setenv("GSO_CLEANUP_STALE_SCALE_SETS", "false")
		cfg, err := Load(writeWorkersConfig(t, "max_runners: 2\n"))
		if err != nil {
			t.Fatalf("Load() error: %v", err)
		}
		if cfg.CleanupStaleScaleSets {
			t.Fatal("cleanup_stale_scale_sets = true, want false from GSO_CLEANUP_STALE_SCALE_SETS")
		}
	})
}
