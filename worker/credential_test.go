package worker

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

const testCredential = "0123456789abcdef.c2VjcmV0LXNlY3JldC1zZWNyZXQtc2VjcmV0LXNlY3JldA"

func TestFileCredentialStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker", "credential")
	store := FileCredentialStore(path)
	if _, err := store.Load(); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("Load() on a missing file = %v, want ErrNoCredential", err)
	}
	if err := store.Save(testCredential); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil || got != testCredential {
		t.Fatalf("Load() = %q, %v", got, err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("credential file mode = %04o, want 0600", perm)
		}
	}
	if err := store.Delete(); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(); err != nil {
		t.Fatalf("second Delete() = %v, want nil", err)
	}
	if _, err := store.Load(); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("Load() after Delete = %v, want ErrNoCredential", err)
	}
}

func TestFileCredentialStoreRefusesLoosePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte(testCredential+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := FileCredentialStore(path).Load()
	if err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("Load() error = %v, want a refusal that says chmod 600", err)
	}
	if strings.Contains(err.Error(), testCredential) {
		t.Fatal("refusal leaks the credential")
	}
}

func TestKeychainCredentialStore(t *testing.T) {
	keyring.MockInit()
	store := KeychainCredentialStore("io.github.jrschumacher.burst-runner", "gso-worker")
	if _, err := store.Load(); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("Load() on an empty keychain = %v, want ErrNoCredential", err)
	}
	if err := store.Save(testCredential); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Load(); err != nil || got != testCredential {
		t.Fatalf("Load() = %q, %v", got, err)
	}
	if err := store.Delete(); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(); err != nil {
		t.Fatalf("second Delete() = %v, want nil", err)
	}
}
