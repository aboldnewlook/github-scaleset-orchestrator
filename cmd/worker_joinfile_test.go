package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/worker"
	"github.com/aboldnewlook/github-scaleset-orchestrator/worker/workertest"
)

// lockedBuffer is an io.Writer ensureEnrolled can log to while the test reads.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestResolveWorkerSettingsJoinFile(t *testing.T) {
	env := envOf(map[string]string{"GSO_WORKER_JOIN_FILE": "/bootstrap/join", "GSO_WORKER_IMAGE": "img:1"})
	s, err := resolveWorkerSettings(baseWorkerFlags(), changedFlags(), env, fixedHostname("box"), fixedStateDir("/cfg/gso"))
	if err != nil {
		t.Fatal(err)
	}
	if s.JoinFile != "/bootstrap/join" {
		t.Errorf("JoinFile from GSO_WORKER_JOIN_FILE = %q, want /bootstrap/join", s.JoinFile)
	}
	f := baseWorkerFlags()
	f.joinFile = "/flag/join"
	s, err = resolveWorkerSettings(f, changedFlags("join-file"), env, fixedHostname("box"), fixedStateDir("/cfg/gso"))
	if err != nil {
		t.Fatal(err)
	}
	if s.JoinFile != "/flag/join" {
		t.Errorf("JoinFile with --join-file = %q, want the flag to beat the environment", s.JoinFile)
	}
	if s.JoinPoll <= 0 || s.JoinLogEvery <= 0 {
		t.Errorf("JoinPoll/JoinLogEvery = %v/%v, want positive defaults", s.JoinPoll, s.JoinLogEvery)
	}
}

func TestEnsureEnrolledWaitsForTheJoinFile(t *testing.T) {
	r := workertest.NewRouter(t)
	dir := t.TempDir()
	joinFile := filepath.Join(dir, "bootstrap", "join")
	store := worker.FileCredentialStore(filepath.Join(dir, "credential"))
	s := workerSettings{StateDir: dir, Name: "box", JoinFile: joinFile, JoinPoll: 5 * time.Millisecond, JoinLogEvery: 20 * time.Millisecond}

	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = os.MkdirAll(filepath.Dir(joinFile), 0o700)
		_ = os.WriteFile(joinFile, []byte(r.Join("box")+"\n"), 0o600)
	}()
	var out lockedBuffer
	st, err := ensureEnrolled(testCtx(t), s, store, "m", strings.NewReader(""), &out, false)
	if err != nil {
		t.Fatalf("ensureEnrolled() error: %v", err)
	}
	if st.Addr != r.Addr() || st.Pin != r.Pin() {
		t.Fatalf("state = %+v, want the router from the join file", st)
	}
	if _, err := store.Load(); err != nil {
		t.Fatalf("credential not saved: %v", err)
	}
	if n := strings.Count(out.String(), "Waiting for"); n < 2 {
		t.Errorf("logged the wait %d times in output %q, want an initial line and periodic ones", n, out.String())
	}
}

func TestEnsureEnrolledJoinFileWaitStopsWithTheContext(t *testing.T) {
	dir := t.TempDir()
	store := worker.FileCredentialStore(filepath.Join(dir, "credential"))
	s := workerSettings{StateDir: dir, Name: "box", JoinFile: filepath.Join(dir, "never"), JoinPoll: 5 * time.Millisecond, JoinLogEvery: time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := ensureEnrolled(ctx, s, store, "m", strings.NewReader(""), io.Discard, false)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want the context's", err)
	}
}

func TestEnsureEnrolledIgnoresTheJoinFileOnceEnrolled(t *testing.T) {
	r := workertest.NewRouter(t)
	dir := t.TempDir()
	store := worker.FileCredentialStore(filepath.Join(dir, "credential"))
	if _, err := ensureEnrolled(testCtx(t), workerSettings{StateDir: dir, Name: "box", Join: r.Join("box")}, store, "m", strings.NewReader(""), io.Discard, false); err != nil {
		t.Fatal(err)
	}
	// A later start finds a join file that would otherwise block or fail.
	joinFile := filepath.Join(dir, "join")
	if err := os.WriteFile(joinFile, []byte("not a join string"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := workerSettings{StateDir: dir, Name: "box", JoinFile: joinFile, JoinPoll: time.Millisecond, JoinLogEvery: time.Hour}
	st, err := ensureEnrolled(testCtx(t), s, store, "m", strings.NewReader(""), io.Discard, false)
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	if st.Pin != r.Pin() {
		t.Fatalf("state = %+v, want the saved enrollment", st)
	}
	if b, _ := os.ReadFile(joinFile); string(b) != "not a join string" {
		t.Errorf("join file was modified: %q", b)
	}
}

func TestEnsureEnrolledJoinBeatsJoinFile(t *testing.T) {
	viaFlag := workertest.NewRouter(t)
	viaFile := workertest.NewRouter(t)
	dir := t.TempDir()
	joinFile := filepath.Join(dir, "join")
	if err := os.WriteFile(joinFile, []byte(viaFile.Join("box")), 0o600); err != nil {
		t.Fatal(err)
	}
	store := worker.FileCredentialStore(filepath.Join(dir, "credential"))
	s := workerSettings{StateDir: dir, Name: "box", Join: viaFlag.Join("box"), JoinFile: joinFile, JoinPoll: time.Millisecond, JoinLogEvery: time.Hour}
	st, err := ensureEnrolled(testCtx(t), s, store, "m", strings.NewReader(""), io.Discard, false)
	if err != nil {
		t.Fatal(err)
	}
	if st.Pin != viaFlag.Pin() || st.Addr != viaFlag.Addr() {
		t.Fatalf("enrolled at %s (pin %s), want the --join router %s", st.Addr, st.Pin, viaFlag.Addr())
	}
}

func TestEnsureEnrolledRejectsABadJoinFileWithoutLeakingIt(t *testing.T) {
	dir := t.TempDir()
	joinFile := filepath.Join(dir, "join")
	secret := strings.Repeat("c", 31) + "Z"
	if err := os.WriteFile(joinFile, []byte("gso-join:v1:unraid.lan:9200:"+strings.Repeat("a", 64)+":"+secret), 0o600); err != nil {
		t.Fatal(err)
	}
	store := worker.FileCredentialStore(filepath.Join(dir, "credential"))
	s := workerSettings{StateDir: dir, Name: "box", JoinFile: joinFile, JoinPoll: time.Millisecond, JoinLogEvery: time.Hour}
	_, err := ensureEnrolled(testCtx(t), s, store, "m", strings.NewReader(""), io.Discard, false)
	if err == nil || !strings.Contains(err.Error(), "--join-file") || strings.Contains(err.Error(), secret) {
		t.Fatalf("error = %v, want one naming --join-file and not the code", err)
	}
}

func TestWaitForJoinFileAddsALostStateHintAfterTheThreshold(t *testing.T) {
	r := workertest.NewRouter(t)
	dir := t.TempDir()
	joinFile := filepath.Join(dir, "join")
	store := worker.FileCredentialStore(filepath.Join(dir, "credential"))
	s := workerSettings{
		StateDir: dir, Name: "box", JoinFile: joinFile,
		JoinPoll: 5 * time.Millisecond, JoinLogEvery: 20 * time.Millisecond, JoinHintAfter: 120 * time.Millisecond,
	}
	go func() {
		time.Sleep(400 * time.Millisecond)
		_ = os.WriteFile(joinFile, []byte(r.Join("box")), 0o600)
	}()
	var out lockedBuffer
	if _, err := ensureEnrolled(testCtx(t), s, store, "m", strings.NewReader(""), &out, false); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) == 0 || strings.Contains(lines[0], "revoke") {
		t.Fatalf("the first wait line must not carry the hint: %q", lines)
	}
	hint := ""
	for _, l := range lines {
		if strings.Contains(l, "revoke") {
			hint = l
			break
		}
	}
	for _, want := range []string{`bootstrap: worker is enrolled`, "gso workers revoke", "Re-enrolling the Unraid worker", "docs/unraid-portainer.md"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint %q does not mention %q (all output: %q)", hint, want, lines)
		}
	}
}

func TestWaitForJoinFileHasNoHintBeforeTheThreshold(t *testing.T) {
	dir := t.TempDir()
	s := workerSettings{StateDir: dir, Name: "box", JoinFile: filepath.Join(dir, "never"), JoinPoll: 5 * time.Millisecond, JoinLogEvery: 10 * time.Millisecond, JoinHintAfter: time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var out lockedBuffer
	_, _ = ensureEnrolled(ctx, s, worker.FileCredentialStore(filepath.Join(dir, "credential")), "m", strings.NewReader(""), &out, false)
	if strings.Contains(out.String(), "revoke") {
		t.Fatalf("hint appeared before the threshold: %q", out.String())
	}
}

func TestEnsureEnrolledRefusesAnOversizedJoinFile(t *testing.T) {
	dir := t.TempDir()
	joinFile := filepath.Join(dir, "join")
	secret := strings.Repeat("d", 32)
	if err := os.WriteFile(joinFile, []byte(strings.Repeat("x", 5000)+secret), 0o600); err != nil {
		t.Fatal(err)
	}
	s := workerSettings{StateDir: dir, Name: "box", JoinFile: joinFile, JoinPoll: time.Millisecond, JoinLogEvery: time.Hour}
	_, err := ensureEnrolled(testCtx(t), s, worker.FileCredentialStore(filepath.Join(dir, "credential")), "m", strings.NewReader(""), io.Discard, false)
	if err == nil || !strings.Contains(err.Error(), "4 KiB") || strings.Contains(err.Error(), secret) {
		t.Fatalf("error = %v, want one naming the 4 KiB limit", err)
	}
}
