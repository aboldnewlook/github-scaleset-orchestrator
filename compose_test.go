package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// docker-compose.yaml is the Unraid deployment contract (design spec §9.2).
// These tests pin its security-relevant shape: the GitHub PAT and the Docker
// socket never share a container, and the router really loads its config.
// Viper lower-cases map keys, so environment variable names are compared in
// lower case.

func readCompose(t *testing.T) *viper.Viper {
	t.Helper()
	v := viper.New()
	v.SetConfigFile("docker-compose.yaml")
	if err := v.ReadInConfig(); err != nil {
		t.Fatalf("reading docker-compose.yaml: %v", err)
	}
	return v
}

// volumeEntries returns a service's volumes, each as written: a short-syntax
// string or a long-syntax map.
func volumeEntries(v *viper.Viper, service string) []any {
	entries, _ := v.Get("services." + service + ".volumes").([]any)
	return entries
}

func TestComposeRouterHasNoDockerSocket(t *testing.T) {
	v := readCompose(t)
	for _, vol := range volumeEntries(v, "gso-router") {
		if s := fmt.Sprint(vol); strings.Contains(s, "docker.sock") {
			t.Fatalf("router mounts the Docker socket: %q", s)
		}
	}
	env := v.GetStringMapString("services.gso-router.environment")
	if env["gso_max_runners"] != "0" {
		t.Fatalf("router GSO_MAX_RUNNERS = %q, want \"0\" (no local runners)", env["gso_max_runners"])
	}
	if _, ok := env["gso_workers_listen"]; !ok {
		t.Fatal("router does not set GSO_WORKERS_LISTEN")
	}
	if _, ok := env["gso_runner_instance_id"]; !ok {
		t.Fatal("router does not set GSO_RUNNER_INSTANCE_ID (container hostnames are not stable scale-set names)")
	}
}

func TestComposeRouterLoadsTheMountedConfig(t *testing.T) {
	v := readCompose(t)
	cmd := v.GetStringSlice("services.gso-router.command")
	i := slices.Index(cmd, "--config")
	if i < 0 || i+1 >= len(cmd) || cmd[i+1] != "/etc/gso/config.yaml" {
		t.Fatalf("router command = %q, want it to pass --config /etc/gso/config.yaml", cmd)
	}
	// Long syntax with create_host_path: false, so a missing ./config.yaml is an
	// error instead of Docker silently creating a directory at that path.
	found := false
	for _, vol := range volumeEntries(v, "gso-router") {
		m, ok := vol.(map[string]any)
		if !ok || m["target"] != "/etc/gso/config.yaml" {
			continue
		}
		found = true
		if m["type"] != "bind" || m["source"] != "./config.yaml" || m["read_only"] != true {
			t.Fatalf("config mount = %v, want a read-only bind of ./config.yaml", m)
		}
		bind, _ := m["bind"].(map[string]any)
		if bind["create_host_path"] != false {
			t.Fatalf("config mount bind = %v, want create_host_path: false", bind)
		}
	}
	if !found {
		t.Fatal("router does not mount a config at /etc/gso/config.yaml")
	}
	env := v.GetStringMapString("services.gso-router.environment")
	if got := env["gso_github_sim"]; got != "${GSO_GITHUB_SIM:-}" {
		t.Fatalf("router GSO_GITHUB_SIM = %q, want ${GSO_GITHUB_SIM:-} (empty outside the sim profile)", got)
	}
	if got := env["gso_workers_allow_cidrs"]; !strings.Contains(got, "10.0.0.0/8") {
		t.Fatalf("router GSO_WORKERS_ALLOW_CIDRS default = %q, want it to include 10.0.0.0/8 like the code default", got)
	}
}

func TestComposeWorkerHasSocketButNoTokenOrConfig(t *testing.T) {
	v := readCompose(t)
	vols := v.GetStringSlice("services.gso-worker.volumes")
	if !slices.ContainsFunc(vols, func(s string) bool { return strings.HasPrefix(s, "/var/run/docker.sock:") }) {
		t.Fatalf("worker volumes = %q, want the Docker socket", vols)
	}
	for _, vol := range vols {
		if strings.Contains(vol, "config.yaml") {
			t.Fatalf("worker mounts the router config: %q", vol)
		}
	}
	for name := range v.GetStringMapString("services.gso-worker.environment") {
		if strings.Contains(name, "token") {
			t.Fatalf("worker environment carries %s", name)
		}
	}
}

func TestComposeWorkerShape(t *testing.T) {
	v := readCompose(t)
	// D5: two slots. The trailing --github-sim is empty in a normal deploy and
	// carries the simulator's advertise URL in the sim profile: a docker-executor
	// worker honours GSO_GITHUB_SIM only when it is given as a flag.
	want := []string{"worker", "--router", "gso-router:9200", "--executor", "docker", "--slots", "2", "--github-sim", "${GSO_SIM_ADVERTISE:-}"}
	if got := v.GetStringSlice("services.gso-worker.command"); !slices.Equal(got, want) {
		t.Fatalf("worker command = %q, want %q", got, want)
	}
	image := v.GetString("services.gso-router.image")
	if got := v.GetString("services.gso-worker.image"); got != image {
		t.Fatalf("worker image %q differs from router image %q", got, image)
	}
	if got := v.GetStringMapString("services.gso-worker.environment")["gso_worker_image"]; got != image {
		t.Fatalf("GSO_WORKER_IMAGE = %q, want the router image %q", got, image)
	}
	volumes := v.GetStringMap("volumes")
	for _, name := range []string{"gso-router-state", "gso-worker-state"} {
		if _, ok := volumes[name]; !ok {
			t.Fatalf("named volume %s is missing", name)
		}
	}
}

func TestComposeSimulatorIsOptInAndStateless(t *testing.T) {
	v := readCompose(t)
	if got := v.GetStringSlice("services.gso-sim.profiles"); !slices.Equal(got, []string{"sim"}) {
		t.Fatalf("gso-sim profiles = %q, want [sim] so a normal deploy never starts it", got)
	}
	if vols := v.GetStringSlice("services.gso-sim.volumes"); len(vols) != 0 {
		t.Fatalf("gso-sim mounts %q, want no volumes", vols)
	}
	if env := v.GetStringMapString("services.gso-sim.environment"); len(env) != 0 {
		t.Fatalf("gso-sim environment = %v, want none (it holds no secrets)", env)
	}
}

// The simulator is unauthenticated plain HTTP: it must never listen on every
// interface. It runs on the host network (so job containers, the router and
// the worker all reach one address without crossing Docker bridges) and binds
// only the bridge gateway address.
func TestComposeSimulatorListensOnOneAddressOnly(t *testing.T) {
	v := readCompose(t)
	if got := v.GetString("services.gso-sim.network_mode"); got != "host" {
		t.Fatalf("gso-sim network_mode = %q, want host", got)
	}
	if ports := v.GetStringSlice("services.gso-sim.ports"); len(ports) != 0 {
		t.Fatalf("gso-sim ports = %q, want none (host network)", ports)
	}
	cmd := v.GetStringSlice("services.gso-sim.command")
	i := slices.Index(cmd, "--listen")
	if i < 0 || i+1 >= len(cmd) {
		t.Fatalf("gso-sim command = %q, want --listen", cmd)
	}
	if got, want := cmd[i+1], "${GSO_SIM_HOST:-172.17.0.1}:9300"; got != want {
		t.Fatalf("gso-sim --listen = %q, want %q (never :9300 or 0.0.0.0)", got, want)
	}
	j := slices.Index(cmd, "--advertise")
	if j < 0 || j+1 >= len(cmd) || cmd[j+1] != "${GSO_SIM_ADVERTISE:-http://172.17.0.1:9300}" {
		t.Fatalf("gso-sim command = %q, want --advertise ${GSO_SIM_ADVERTISE:-http://172.17.0.1:9300}", cmd)
	}
}
