package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeTokenFile(t *testing.T, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("secret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // WriteFile's mode is masked by umask; Chmod is exact
		t.Fatal(err)
	}
	return path
}

func TestResolveRefusesTokenFileReadableByOthers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits only")
	}
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o444, 0o660} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			tok, err := (&TokenSource{File: writeTokenFile(t, mode)}).Resolve()
			if err == nil {
				t.Fatalf("Resolve() = %q, want a permission error", tok)
			}
			if tok != "" || strings.Contains(err.Error(), "secret-token") {
				t.Fatalf("token leaked: tok=%q err=%v", tok, err)
			}
			if !strings.Contains(err.Error(), "chmod 600") {
				t.Errorf("error %q does not tell the user how to fix it", err)
			}
		})
	}
}

func TestResolveAcceptsPrivateTokenFile(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o400} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			tok, err := (&TokenSource{File: writeTokenFile(t, mode)}).Resolve()
			if err != nil || tok != "secret-token" {
				t.Fatalf("Resolve() = %q, %v; want secret-token", tok, err)
			}
		})
	}
}
