package fleet

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/runner"
)

type runnerClass = runner.Class

func openTestRegistry(t *testing.T) (*Registry, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "workers.json")
	r, err := OpenRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	return r, path
}

var testNow = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func TestRegistryEnrollAndAuthenticate(t *testing.T) {
	r, path := openTestRegistry(t)
	b := Bounds{Classes: []runnerClass{linuxArm64}, MaxSlots: 4, Repos: []string{"jrschumacher/*"}}
	code, expires, err := r.AddEnrollment("mac-m1", b, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(code) {
		t.Fatalf("code = %q, want 32 lowercase hex", code)
	}
	if !expires.Equal(testNow.Add(15 * time.Minute)) {
		t.Fatalf("expires = %v", expires)
	}
	cred, rec, err := r.Enroll(code, "something-else", "machine-1", testNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{16}\.[A-Za-z0-9_-]{43}$`).MatchString(cred) {
		t.Fatalf("credential = %q, want <16 hex>.<43 base64url>", cred)
	}
	if rec.Name != "mac-m1" || rec.Segment != "macm1" || rec.MachineID != "machine-1" {
		t.Fatalf("record = %+v (the admin-declared name must win)", rec)
	}
	if rec.SecretHash == "" || rec.SecretHash == cred {
		t.Fatalf("secret hash = %q", rec.SecretHash)
	}
	got, err := r.Authenticate(cred)
	if err != nil || got.ID != rec.ID {
		t.Fatalf("Authenticate = %+v, %v", got, err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("workers.json mode = %04o, want 0600", info.Mode().Perm())
		}
	}
	reopened, err := OpenRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := reopened.Authenticate(cred); err != nil || got.Bounds.MaxSlots != 4 || got.Bounds.Classes[0] != linuxArm64 || got.Bounds.Repos[0] != "jrschumacher/*" {
		t.Fatalf("after reopen: %+v, %v", got, err)
	}
}

func TestRegistryCodesAreSingleUseAndExpire(t *testing.T) {
	r, _ := openTestRegistry(t)
	b := Bounds{Classes: []runnerClass{linuxX64}, MaxSlots: 1}
	code, _, _ := r.AddEnrollment("box", b, testNow)
	if _, _, err := r.Enroll(code, "box", "m", testNow); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Enroll(code, "box", "m", testNow); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("reused code: err = %v, want ErrInvalidCode", err)
	}
	late, _, _ := r.AddEnrollment("late", b, testNow)
	if _, _, err := r.Enroll(late, "late", "m", testNow.Add(16*time.Minute)); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("expired code: err = %v, want ErrInvalidCode", err)
	}
	if _, _, err := r.Enroll("", "x", "m", testNow); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("empty code: err = %v", err)
	}
	first, _, _ := r.AddEnrollment("again", b, testNow)
	second, _, _ := r.AddEnrollment("again", b, testNow)
	if _, _, err := r.Enroll(first, "again", "m", testNow); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("replaced code still works: %v", err)
	}
	if _, _, err := r.Enroll(second, "again", "m", testNow); err != nil {
		t.Fatalf("replacement code: %v", err)
	}
}

func TestRegistryNameRules(t *testing.T) {
	r, _ := openTestRegistry(t)
	b := Bounds{Classes: []runnerClass{linuxX64}, MaxSlots: 1}
	for _, bad := range []string{"", "Mac", "-x", "a_b", "local", "lo-cal", "x23456789012345678901234567890123"} {
		if _, _, err := r.AddEnrollment(bad, b, testNow); !errors.Is(err, ErrBadName) {
			t.Errorf("AddEnrollment(%q) err = %v, want ErrBadName", bad, err)
		}
	}
	code, _, _ := r.AddEnrollment("mac-m1", b, testNow)
	if _, _, err := r.Enroll(code, "", "m", testNow); err != nil {
		t.Fatal(err)
	}
	for _, taken := range []string{"mac-m1", "macm1", "ma-cm1"} { // same name or same segment "macm1"
		if _, _, err := r.AddEnrollment(taken, b, testNow); !errors.Is(err, ErrNameTaken) {
			t.Errorf("AddEnrollment(%q) err = %v, want ErrNameTaken", taken, err)
		}
	}
	for _, bounds := range []Bounds{{MaxSlots: 1}, {Classes: []runnerClass{linuxX64}}, {Classes: []runnerClass{linuxX64}, MaxSlots: 257}, {Classes: []runnerClass{linuxX64}, MaxSlots: 1, Repos: []string{"noslash"}}} {
		if _, _, err := r.AddEnrollment("valid", bounds, testNow); err == nil {
			t.Errorf("AddEnrollment accepted bounds %+v", bounds)
		}
	}
}

func TestRegistryAuthenticateFailures(t *testing.T) {
	r, _ := openTestRegistry(t)
	code, _, _ := r.AddEnrollment("box", Bounds{Classes: []runnerClass{linuxX64}, MaxSlots: 1}, testNow)
	cred, rec, _ := r.Enroll(code, "box", "m", testNow)
	for _, bad := range []string{"", "nodot", ".", rec.ID + ".", "." + "abc", rec.ID + ".wrongsecret", "ffffffffffffffff.abc"} {
		if _, err := r.Authenticate(bad); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("Authenticate(%q) err = %v, want ErrUnauthenticated", bad, err)
		}
	}
	if _, err := r.Revoke("box"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Authenticate(cred); !errors.Is(err, ErrRevoked) {
		t.Fatalf("revoked: err = %v, want ErrRevoked", err)
	}
	if _, ok := r.ByName("box"); ok {
		t.Fatal("ByName returned a revoked worker")
	}
	// A revoked name may be enrolled again; the old credential stays refused.
	code2, _, err := r.AddEnrollment("box", Bounds{Classes: []runnerClass{linuxX64}, MaxSlots: 1}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Enroll(code2, "box", "m", testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Authenticate(cred); !errors.Is(err, ErrRevoked) {
		t.Fatalf("old credential after re-enroll: %v", err)
	}
	if _, err := r.Revoke("nobody"); !errors.Is(err, ErrUnknownWorker) {
		t.Fatalf("Revoke(nobody) = %v", err)
	}
}

func TestRegistryUpdatePersists(t *testing.T) {
	r, path := openTestRegistry(t)
	code, _, _ := r.AddEnrollment("box", Bounds{Classes: []runnerClass{linuxX64}, MaxSlots: 1}, testNow)
	cred, _, _ := r.Enroll(code, "box", "m", testNow)
	if _, err := r.Update("box", Bounds{Classes: []runnerClass{linuxX64, linuxArm64}, MaxSlots: 3}); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Authenticate(cred)
	if err != nil || got.Bounds.MaxSlots != 3 || len(got.Bounds.Classes) != 2 {
		t.Fatalf("after update: %+v, %v", got.Bounds, err)
	}
	if len(reopened.List()) != 1 {
		t.Fatalf("List = %+v", reopened.List())
	}
	if _, err := r.Update("nobody", Bounds{Classes: []runnerClass{linuxX64}, MaxSlots: 1}); !errors.Is(err, ErrUnknownWorker) {
		t.Fatalf("Update(nobody) = %v", err)
	}
}

func TestOpenRegistryRefusesLoosePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions only")
	}
	path := filepath.Join(t.TempDir(), "workers.json")
	if err := os.WriteFile(path, []byte(`{"workers":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRegistry(path); err == nil {
		t.Fatal("OpenRegistry accepted a 0644 workers.json")
	}
}
