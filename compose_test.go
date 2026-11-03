package main

import (
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

func TestComposeRouterHasNoDockerSocket(t *testing.T) {
	v := readCompose(t)
	for _, vol := range v.GetStringSlice("services.gso-router.volumes") {
		if strings.Contains(vol, "docker.sock") {
			t.Fatalf("router mounts the Docker socket: %q", vol)
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
	if !slices.Contains(v.GetStringSlice("services.gso-router.volumes"), "./config.yaml:/etc/gso/config.yaml:ro") {
		t.Fatal("router does not mount ./config.yaml at /etc/gso/config.yaml")
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

// The simulator is unauthenticated plain HTTP: it must never be published on
// every interface.
func TestComposeSimulatorIsNotPublishedOnAllInterfaces(t *testing.T) {
	v := readCompose(t)
	ports := v.GetStringSlice("services.gso-sim.ports")
	if len(ports) == 0 {
		t.Fatal("gso-sim publishes no port; job containers could not reach it")
	}
	for _, p := range ports {
		if strings.HasPrefix(p, "9300:") || strings.HasPrefix(p, ":") || strings.HasPrefix(p, "0.0.0.0:") {
			t.Fatalf("gso-sim port %q binds every interface; bind a specific address", p)
		}
	}
}
