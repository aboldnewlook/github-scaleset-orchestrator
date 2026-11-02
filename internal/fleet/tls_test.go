package fleet

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/worker"
)

func readLeaf(t *testing.T, dir string) *x509.Certificate {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "cert.pem"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func TestLoadOrCreateTLSCreatesTheKeyOnceAndPinsIt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "workers", "tls")
	cfg, pin, err := LoadOrCreateTLS(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Fatalf("MinVersion = %x, want TLS 1.3", cfg.MinVersion)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(pin) {
		t.Fatalf("pin = %q", pin)
	}
	leaf := cfg.Certificates[0].Leaf
	if worker.SPKIPin(leaf) != pin {
		t.Fatal("pin does not match the served certificate")
	}
	keyPath := filepath.Join(dir, "key.pem")
	if runtime.GOOS != "windows" {
		info, err := os.Stat(keyPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("key.pem mode = %04o, want 0600", info.Mode().Perm())
		}
	}
	keyBefore, _ := os.ReadFile(keyPath)
	cfg2, pin2, err := LoadOrCreateTLS(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	keyAfter, _ := os.ReadFile(keyPath)
	if pin2 != pin || string(keyAfter) != string(keyBefore) || cfg2.Certificates[0].Leaf.SerialNumber.Cmp(leaf.SerialNumber) != 0 {
		t.Fatal("a reload changed the key or reissued a valid certificate")
	}
}

func TestLoadOrCreateTLSReissuesFromTheSameKey(t *testing.T) {
	dir := t.TempDir()
	_, pin, err := LoadOrCreateTLS(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(dir, "cert.pem")
	key, err := loadWorkerKey(filepath.Join(dir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := issueWorkerCert(certPath, key, time.Now().Add(-400*24*time.Hour)); err != nil { // expired
		t.Fatal(err)
	}
	expired := readLeaf(t, dir)
	cfg, pin2, err := LoadOrCreateTLS(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pin2 != pin {
		t.Fatal("the pin changed when the certificate was reissued")
	}
	if cfg.Certificates[0].Leaf.SerialNumber.Cmp(expired.SerialNumber) == 0 || time.Until(cfg.Certificates[0].Leaf.NotAfter) < 300*24*time.Hour {
		t.Fatal("the expired certificate was not reissued")
	}

	if err := os.Remove(certPath); err != nil {
		t.Fatal(err)
	}
	if _, pin3, err := LoadOrCreateTLS(dir, nil); err != nil || pin3 != pin {
		t.Fatalf("missing cert: pin %q, %v", pin3, err)
	}

	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := issueWorkerCert(certPath, other, time.Now()); err != nil { // a cert for a different key
		t.Fatal(err)
	}
	if _, pin4, err := LoadOrCreateTLS(dir, nil); err != nil || pin4 != pin {
		t.Fatalf("mismatched cert: pin %q, %v", pin4, err)
	}
}

func TestLoadOrCreateTLSNeverRegeneratesAKey(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := LoadOrCreateTLS(dir, nil); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(keyPath, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreateTLS(dir, nil); err == nil {
		t.Fatal("a corrupt key was replaced instead of reported")
	}
	if data, _ := os.ReadFile(keyPath); string(data) != "garbage" {
		t.Fatal("the key file was overwritten")
	}
}

func TestLoadOrCreateTLSRefusesAGroupReadableKey(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	dir := t.TempDir()
	if _, _, err := LoadOrCreateTLS(dir, nil); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "key.pem")
	if err := os.Chmod(keyPath, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreateTLS(dir, nil); err == nil {
		t.Fatal("a group-readable key was accepted")
	}
}
