package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
	"github.com/aboldnewlook/github-scaleset-orchestrator/worker"
	"github.com/aboldnewlook/github-scaleset-orchestrator/worker/workertest"
)

func baseWorkerFlags() workerFlags {
	return workerFlags{executor: "docker", slots: 1, acceptPublic: "follow", credentialStore: "file"}
}

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func changedFlags(names ...string) func(string) bool {
	return func(n string) bool { return slices.Contains(names, n) }
}

func fixedHostname(name string) func() (string, error) {
	return func() (string, error) { return name, nil }
}

func fixedStateDir(dir string) func() (string, error) {
	return func() (string, error) { return dir, nil }
}

func TestResolveWorkerSettingsDefaults(t *testing.T) {
	f := baseWorkerFlags()
	f.image = "ghcr.io/aboldnewlook/github-scaleset-orchestrator:v0.9.0"
	s, err := resolveWorkerSettings(f, changedFlags("image"), envOf(nil), fixedHostname("Mac-Mini.local"), fixedStateDir("/cfg/gso"))
	if err != nil {
		t.Fatalf("resolveWorkerSettings() error: %v", err)
	}
	if s.Name != "mac-mini" {
		t.Errorf("Name = %q, want mac-mini", s.Name)
	}
	if s.StateDir != filepath.Join("/cfg/gso", "worker") {
		t.Errorf("StateDir = %q", s.StateDir)
	}
	if s.Executor != runner.ExecutorDocker || s.NewLauncher == nil {
		t.Errorf("Executor = %q, NewLauncher nil = %v", s.Executor, s.NewLauncher == nil)
	}
	if s.Slots != 1 || s.AcceptPublic != worker.PublicFollow || s.CredentialStore != "file" {
		t.Errorf("settings = %+v", s)
	}
	if s.BackoffMin != time.Second || s.BackoffMax != 15*time.Second {
		t.Errorf("backoff = %s..%s, want 1s..15s", s.BackoffMin, s.BackoffMax)
	}
}

func TestResolveWorkerSettingsReadsTheEnvironment(t *testing.T) {
	env := envOf(map[string]string{
		"GSO_WORKER_JOIN":          "gso-join:v1:unraid.lan:9200:" + strings.Repeat("a", 64) + ":" + strings.Repeat("b", 32),
		"GSO_WORKER_ROUTER":        "gso-router:9200",
		"GSO_WORKER_NAME":          "Unraid",
		"GSO_WORKER_IMAGE":         "ghcr.io/aboldnewlook/github-scaleset-orchestrator:v0.9.0",
		"GSO_WORKER_DOCKER_HOST":   "unix:///var/run/docker.sock",
		"GSO_WORKER_SLOTS":         "2",
		"GSO_WORKER_ACCEPT_PUBLIC": "refuse",
	})
	s, err := resolveWorkerSettings(baseWorkerFlags(), changedFlags(), env, fixedHostname("ignored"), fixedStateDir("/cfg/gso"))
	if err != nil {
		t.Fatalf("resolveWorkerSettings() error: %v", err)
	}
	if !strings.HasPrefix(s.Join, "gso-join:v1:unraid.lan:9200:") || s.Router != "gso-router:9200" || s.Name != "unraid" {
		t.Errorf("join/router/name = %q, %q, %q", s.Join, s.Router, s.Name)
	}
	if s.Image == "" || s.DockerHost != "unix:///var/run/docker.sock" || s.Slots != 2 || s.AcceptPublic != worker.PublicRefuse {
		t.Errorf("settings = %+v", s)
	}
}

func TestResolveWorkerSettingsFlagsBeatTheEnvironment(t *testing.T) {
	f := baseWorkerFlags()
	f.slots, f.image = 5, "flag-image"
	env := envOf(map[string]string{"GSO_WORKER_SLOTS": "2", "GSO_WORKER_IMAGE": "env-image"})
	s, err := resolveWorkerSettings(f, changedFlags("slots", "image"), env, fixedHostname("box"), fixedStateDir("/cfg/gso"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Slots != 5 || s.Image != "flag-image" {
		t.Fatalf("slots, image = %d, %q; want the flag values 5, flag-image", s.Slots, s.Image)
	}
}

func TestResolveWorkerSettingsErrors(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*workerFlags)
		env     map[string]string
		wantErr string
	}{
		{"native executor", func(f *workerFlags) { f.executor = "native" }, nil, "P5"},
		{"unknown executor", func(f *workerFlags) { f.executor = "bogus" }, nil, "unknown --executor"},
		{"missing image", func(f *workerFlags) { f.image = "" }, nil, "--image"},
		{"zero slots", func(f *workerFlags) { f.slots = 0 }, nil, "--slots"},
		{"bad slots env", func(*workerFlags) {}, map[string]string{"GSO_WORKER_SLOTS": "two"}, "GSO_WORKER_SLOTS"},
		{"bad accept-public", func(f *workerFlags) { f.acceptPublic = "maybe" }, nil, "--accept-public"},
		{"bad credential store", func(f *workerFlags) { f.credentialStore = "vault" }, nil, "--credential-store"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := baseWorkerFlags()
			f.image = "img"
			tt.mutate(&f)
			changed := changedFlags("image")
			if tt.name == "zero slots" {
				changed = changedFlags("image", "slots")
			}
			_, err := resolveWorkerSettings(f, changed, envOf(tt.env), fixedHostname("box"), fixedStateDir("/cfg/gso"))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want one mentioning %q", err, tt.wantErr)
			}
		})
	}
}

func TestWorkerCommandRefusesTheNativeExecutor(t *testing.T) {
	rootCmd.SetArgs([]string{"worker", "--executor", "native", "--image", "img", "--state-dir", t.TempDir()})
	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	t.Cleanup(func() {
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	})
	err := rootCmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "P5") {
		t.Fatalf("gso worker --executor native: error = %v, want a P5 refusal", err)
	}
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestEnsureEnrolledReusesASavedEnrollment(t *testing.T) {
	dir := t.TempDir()
	store := worker.FileCredentialStore(filepath.Join(dir, "credential"))
	if err := store.Save("0123456789abcdef.secret"); err != nil {
		t.Fatal(err)
	}
	saved := worker.State{Name: "box", Addr: "router.lan:9200", Pin: strings.Repeat("a", 64)}
	if err := worker.SaveState(filepath.Join(dir, "worker.json"), saved); err != nil {
		t.Fatal(err)
	}
	st, err := ensureEnrolled(testCtx(t), workerSettings{StateDir: dir, Name: "box"}, store, "m", strings.NewReader(""), io.Discard, false)
	if err != nil {
		t.Fatalf("ensureEnrolled() error: %v", err)
	}
	if st.Addr != saved.Addr || st.Pin != saved.Pin {
		t.Fatalf("state = %+v, want the saved one", st)
	}
}

func TestEnsureEnrolledNeedsAJoinString(t *testing.T) {
	dir := t.TempDir()
	store := worker.FileCredentialStore(filepath.Join(dir, "credential"))
	_, err := ensureEnrolled(testCtx(t), workerSettings{StateDir: dir, Name: "box"}, store, "m", strings.NewReader(""), io.Discard, false)
	if err == nil || !strings.Contains(err.Error(), "--join") {
		t.Fatalf("error = %v, want one naming --join", err)
	}
}

func TestEnsureEnrolledEnrollsAndPersists(t *testing.T) {
	r := workertest.NewRouter(t)
	dir := t.TempDir()
	store := worker.FileCredentialStore(filepath.Join(dir, "credential"))
	var out bytes.Buffer
	st, err := ensureEnrolled(testCtx(t), workerSettings{StateDir: dir, Name: "box", Join: r.Join("box")}, store, "m", strings.NewReader(""), &out, false)
	if err != nil {
		t.Fatalf("ensureEnrolled() error: %v", err)
	}
	if st.Addr != r.Addr() || st.Pin != r.Pin() {
		t.Fatalf("state = %+v", st)
	}
	if _, err := store.Load(); err != nil {
		t.Fatalf("credential not saved: %v", err)
	}
	if _, err := worker.LoadState(filepath.Join(dir, "worker.json")); err != nil {
		t.Fatalf("worker.json not saved: %v", err)
	}
	if !strings.Contains(out.String(), r.Addr()) || !strings.Contains(out.String(), worker.ShortPin(r.Pin())) {
		t.Fatalf("output %q does not show the router address and short pin", out.String())
	}
}

func TestEnsureEnrolledAsksOnATerminal(t *testing.T) {
	r := workertest.NewRouter(t)
	dir := t.TempDir()
	store := worker.FileCredentialStore(filepath.Join(dir, "credential"))
	s := workerSettings{StateDir: dir, Name: "box", Join: r.Join("box")}

	_, err := ensureEnrolled(testCtx(t), s, store, "m", strings.NewReader("n\n"), io.Discard, true)
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("declined enrollment: error = %v, want cancelled", err)
	}
	if _, err := store.Load(); !errors.Is(err, worker.ErrNoCredential) {
		t.Fatalf("a declined enrollment saved a credential (Load error %v)", err)
	}
	// Declining never sent the code, so the same join string still works.
	if _, err := ensureEnrolled(testCtx(t), s, store, "m", strings.NewReader("y\n"), io.Discard, true); err != nil {
		t.Fatalf("confirmed enrollment error: %v", err)
	}
}

func TestLoadOrCreateMachineIDIsStable(t *testing.T) {
	dir := t.TempDir()
	first, err := loadOrCreateMachineID(dir)
	if err != nil || first == "" {
		t.Fatalf("first call = %q, %v", first, err)
	}
	second, err := loadOrCreateMachineID(dir)
	if err != nil || second != first {
		t.Fatalf("second call = %q, %v; want %q", second, err, first)
	}
	info, err := os.Stat(filepath.Join(dir, "machine-id"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 && os.PathSeparator == '/' {
		t.Fatalf("machine-id mode = %o, want 600", mode)
	}
}

func TestResolveWorkerSettingsPassesTrustedHosts(t *testing.T) {
	f := baseWorkerFlags()
	f.image = "img"
	f.trustedHosts = []string{" 127.0.0.1 ", "", "sim.lan"}
	s, err := resolveWorkerSettings(f, changedFlags("image", "trusted-host"), envOf(nil), fixedHostname("box"), fixedStateDir("/cfg/gso"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.TrustedHosts, []string{"127.0.0.1", "sim.lan"}) {
		t.Fatalf("TrustedHosts = %v", s.TrustedHosts)
	}
}

func TestEnsureEnrolledNeverPrintsTheCredentialOrCode(t *testing.T) {
	r := workertest.NewRouter(t)
	dir := t.TempDir()
	store := worker.FileCredentialStore(filepath.Join(dir, "credential"))
	join := r.Join("box")
	var out bytes.Buffer
	if _, err := ensureEnrolled(testCtx(t), workerSettings{StateDir: dir, Name: "box", Join: join}, store, "m", strings.NewReader(""), &out, false); err != nil {
		t.Fatal(err)
	}
	cred, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	j, err := worker.ParseJoin(join)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), cred) || strings.Contains(out.String(), j.Code) {
		t.Fatalf("output leaks the credential or the join code: %q", out.String())
	}
}

func TestEnsureEnrolledErrorsDoNotContainTheCode(t *testing.T) {
	r := workertest.NewRouter(t)
	dir := t.TempDir()
	store := worker.FileCredentialStore(filepath.Join(dir, "credential"))
	join := r.Join("box")
	j, _ := worker.ParseJoin(join)
	s := workerSettings{StateDir: dir, Name: "box", Join: join}
	if _, err := ensureEnrolled(testCtx(t), s, store, "m", strings.NewReader(""), io.Discard, false); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(dir, "worker.json"))
	_ = store.Delete()
	// The code is single-use: a second enrollment fails, and must not echo it.
	_, err := ensureEnrolled(testCtx(t), s, store, "m", strings.NewReader(""), io.Discard, false)
	if err == nil {
		t.Fatal("reused join code enrolled twice")
	}
	if strings.Contains(err.Error(), j.Code) {
		t.Fatalf("error contains the join code: %v", err)
	}
	_, err = ensureEnrolled(testCtx(t), workerSettings{StateDir: dir, Name: "box", Join: "gso-join:v1:bad:" + j.Code}, store, "m", strings.NewReader(""), io.Discard, false)
	if err == nil || strings.Contains(err.Error(), j.Code) {
		t.Fatalf("malformed join error = %v, must exist and not contain the code", err)
	}
}

func TestResolveWorkerSettingsRejectsAnInvalidAllowEntry(t *testing.T) {
	f := baseWorkerFlags()
	f.image = "img"
	f.allow = []string{"acme/*", "acme/re po"}
	_, err := resolveWorkerSettings(f, changedFlags("image"), envOf(nil), fixedHostname("box"), fixedStateDir("/cfg/gso"))
	if err == nil || !strings.Contains(err.Error(), "--allow") || !strings.Contains(err.Error(), `"acme/re po"`) {
		t.Fatalf("error = %v, want one naming --allow and the bad entry", err)
	}
}
