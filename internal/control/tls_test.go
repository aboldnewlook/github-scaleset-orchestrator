package control

import (
	"crypto/tls"
	"crypto/x509"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateSelfSignedCert(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	tlsConfig, fp, err := LoadOrGenerateTLSConfig(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("LoadOrGenerateTLSConfig: %v", err)
	}

	if tlsConfig == nil {
		t.Fatal("expected non-nil TLS config")
	}
	if len(tlsConfig.Certificates) == 0 {
		t.Fatal("expected at least one certificate in TLS config")
	}

	// Check fingerprint format.
	if !strings.HasPrefix(fp, "sha256:") {
		t.Errorf("fingerprint should start with sha256:, got %q", fp)
	}

	// Check file permissions.
	for _, path := range []string{certPath, keyPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		perm := info.Mode().Perm()
		if perm != fs.FileMode(0600) {
			t.Errorf("%s: expected perm 0600, got %04o", path, perm)
		}
	}

	// Parse the generated cert and verify SANs.
	cert, err := x509.ParseCertificate(tlsConfig.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatalf("parsing certificate: %v", err)
	}

	hasDNS := map[string]bool{}
	for _, name := range cert.DNSNames {
		hasDNS[name] = true
	}
	if !hasDNS["localhost"] {
		t.Error("expected localhost in DNS SANs")
	}

	hasIP := false
	for _, ip := range cert.IPAddresses {
		if ip.Equal(net.ParseIP("127.0.0.1")) {
			hasIP = true
		}
	}
	if !hasIP {
		t.Error("expected 127.0.0.1 in IP SANs")
	}
}

func TestLoadExistingCert(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	// Generate.
	_, fp1, err := LoadOrGenerateTLSConfig(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Load existing.
	_, fp2, err := LoadOrGenerateTLSConfig(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}

	if fp1 != fp2 {
		t.Errorf("fingerprints differ: %s vs %s", fp1, fp2)
	}
}

func TestCertFingerprint(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	tlsConfig, fp, err := LoadOrGenerateTLSConfig(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("LoadOrGenerateTLSConfig: %v", err)
	}

	// Verify format: "sha256:" followed by 32 hex pairs separated by colons.
	if !strings.HasPrefix(fp, "sha256:") {
		t.Fatalf("expected sha256: prefix, got %q", fp)
	}
	hexPart := strings.TrimPrefix(fp, "sha256:")
	parts := strings.Split(hexPart, ":")
	if len(parts) != 32 {
		t.Errorf("expected 32 hex pairs, got %d", len(parts))
	}
	for _, part := range parts {
		if len(part) != 2 {
			t.Errorf("expected 2-char hex pair, got %q", part)
		}
	}

	// Verify CertFingerprint gives same result when called directly.
	cert, _ := x509.ParseCertificate(tlsConfig.Certificates[0].Certificate[0])
	fp2 := CertFingerprint(cert)
	if fp != fp2 {
		t.Errorf("fingerprints differ: %s vs %s", fp, fp2)
	}
}

func TestKnownHostsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")

	entries := map[string]string{
		"host1:9090": "sha256:aa:bb:cc",
		"host2:8080": "sha256:dd:ee:ff",
	}

	for host, fp := range entries {
		if err := SaveKnownHost(path, host, fp); err != nil {
			t.Fatalf("SaveKnownHost(%s): %v", host, err)
		}
	}

	loaded, err := LoadKnownHosts(path)
	if err != nil {
		t.Fatalf("LoadKnownHosts: %v", err)
	}

	for host, expectedFP := range entries {
		if got := loaded[host]; got != expectedFP {
			t.Errorf("host %s: expected %s, got %s", host, expectedFP, got)
		}
	}
}

func TestKnownHostsUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")

	if err := SaveKnownHost(path, "myhost", "sha256:old"); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if err := SaveKnownHost(path, "myhost", "sha256:new"); err != nil {
		t.Fatalf("second save: %v", err)
	}

	hosts, err := LoadKnownHosts(path)
	if err != nil {
		t.Fatalf("LoadKnownHosts: %v", err)
	}

	if got := hosts["myhost"]; got != "sha256:new" {
		t.Errorf("expected sha256:new, got %s", got)
	}
}

func TestKnownHostsMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent", "known_hosts")

	hosts, err := LoadKnownHosts(path)
	if err != nil {
		t.Fatalf("expected no error for missing file, got: %v", err)
	}
	if len(hosts) != 0 {
		t.Errorf("expected empty map, got %v", hosts)
	}
}

func TestVerifyOrTrustFingerprint_NewHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")

	err := VerifyOrTrustFingerprint(path, "newhost:443", "sha256:aa:bb")
	if err != nil {
		t.Fatalf("expected nil for new host, got: %v", err)
	}

	hosts, _ := LoadKnownHosts(path)
	if got := hosts["newhost:443"]; got != "sha256:aa:bb" {
		t.Errorf("expected saved fingerprint, got %q", got)
	}
}

func TestVerifyOrTrustFingerprint_Match(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")

	_ = SaveKnownHost(path, "knownhost:443", "sha256:aa:bb")

	err := VerifyOrTrustFingerprint(path, "knownhost:443", "sha256:aa:bb")
	if err != nil {
		t.Fatalf("expected nil for matching fingerprint, got: %v", err)
	}
}

func TestVerifyOrTrustFingerprint_Mismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")

	_ = SaveKnownHost(path, "knownhost:443", "sha256:aa:bb")

	err := VerifyOrTrustFingerprint(path, "knownhost:443", "sha256:xx:yy")
	if err == nil {
		t.Fatal("expected error for mismatched fingerprint")
	}
	if !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Errorf("expected fingerprint mismatch error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "sha256:aa:bb") {
		t.Errorf("error should contain known fingerprint, got: %v", err)
	}
	if !strings.Contains(err.Error(), "sha256:xx:yy") {
		t.Errorf("error should contain received fingerprint, got: %v", err)
	}
}

func TestParseCIDRs(t *testing.T) {
	tests := []struct {
		name    string
		cidrs   []string
		wantN   int
		wantErr bool
	}{
		{
			name:  "valid IPv4",
			cidrs: []string{"192.168.1.0/24", "10.0.0.0/8"},
			wantN: 2,
		},
		{
			name:  "valid IPv6",
			cidrs: []string{"fd00::/8", "::1/128"},
			wantN: 2,
		},
		{
			name:  "empty",
			cidrs: []string{},
			wantN: 0,
		},
		{
			name:    "invalid CIDR",
			cidrs:   []string{"not-a-cidr"},
			wantErr: true,
		},
		{
			name:    "mixed valid and invalid",
			cidrs:   []string{"192.168.1.0/24", "bad"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nets, err := ParseCIDRs(tt.cidrs)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(nets) != tt.wantN {
				t.Errorf("expected %d nets, got %d", tt.wantN, len(nets))
			}
		})
	}
}

func TestCheckIPAllowed(t *testing.T) {
	allow24, _ := ParseCIDRs([]string{"192.168.1.0/24"})
	allowV6, _ := ParseCIDRs([]string{"fd00::/8"})
	allowBoth, _ := ParseCIDRs([]string{"192.168.1.0/24", "fd00::/8"})

	tests := []struct {
		name      string
		addr      net.Addr
		allowlist []*net.IPNet
		want      bool
	}{
		{
			name:      "empty allowlist allows all",
			addr:      &net.TCPAddr{IP: net.ParseIP("1.2.3.4"), Port: 1234},
			allowlist: nil,
			want:      true,
		},
		{
			name:      "IP in range",
			addr:      &net.TCPAddr{IP: net.ParseIP("192.168.1.50"), Port: 443},
			allowlist: allow24,
			want:      true,
		},
		{
			name:      "IP out of range",
			addr:      &net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: 443},
			allowlist: allow24,
			want:      false,
		},
		{
			name:      "IPv6 in range",
			addr:      &net.TCPAddr{IP: net.ParseIP("fd00::1"), Port: 443},
			allowlist: allowV6,
			want:      true,
		},
		{
			name:      "IPv6 out of range",
			addr:      &net.TCPAddr{IP: net.ParseIP("fe80::1"), Port: 443},
			allowlist: allowV6,
			want:      false,
		},
		{
			name:      "multiple CIDRs, match second",
			addr:      &net.TCPAddr{IP: net.ParseIP("fd00::5"), Port: 443},
			allowlist: allowBoth,
			want:      true,
		},
		{
			name:      "UDP addr in range",
			addr:      &net.UDPAddr{IP: net.ParseIP("192.168.1.10"), Port: 53},
			allowlist: allow24,
			want:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CheckIPAllowed(tt.addr, tt.allowlist)
			if got != tt.want {
				t.Errorf("CheckIPAllowed = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTOFUTLSConfig(t *testing.T) {
	cfg := TOFUTLSConfig("/tmp/test-known-hosts", "localhost:9090", "")
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	if !cfg.InsecureSkipVerify {
		t.Error("expected InsecureSkipVerify to be true")
	}
	if cfg.VerifyPeerCertificate == nil {
		t.Error("expected VerifyPeerCertificate callback to be set")
	}
}

func TestTOFUTLSConfigWithTrustFingerprint(t *testing.T) {
	// Generate a cert to get a real fingerprint and raw bytes.
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	tlsConfig, fp, err := LoadOrGenerateTLSConfig(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("generating cert: %v", err)
	}

	rawCerts := make([][]byte, len(tlsConfig.Certificates[0].Certificate))
	copy(rawCerts, tlsConfig.Certificates[0].Certificate)

	// Matching fingerprint should pass.
	cfg := TOFUTLSConfig("", "localhost:9090", fp)
	err = cfg.VerifyPeerCertificate(rawCerts, nil)
	if err != nil {
		t.Fatalf("expected nil error for matching fingerprint, got: %v", err)
	}

	// Wrong fingerprint should fail.
	cfg2 := TOFUTLSConfig("", "localhost:9090", "sha256:wrong")
	err = cfg2.VerifyPeerCertificate(rawCerts, nil)
	if err == nil {
		t.Fatal("expected error for wrong fingerprint")
	}
}

func TestMismatchedCertKeyFiles(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	// Create only the cert file.
	if err := os.WriteFile(certPath, []byte("dummy"), 0600); err != nil {
		t.Fatal(err)
	}

	_, _, err := LoadOrGenerateTLSConfig(certPath, keyPath, nil)
	if err == nil {
		t.Fatal("expected error for mismatched files")
	}
	if !strings.Contains(err.Error(), "mismatched") {
		t.Errorf("expected mismatched error, got: %v", err)
	}
}

func TestCertDirAndKnownHostsPath(t *testing.T) {
	// These depend on os.UserConfigDir which should work in test environments.
	dir, err := CertDir()
	if err != nil {
		t.Fatalf("CertDir: %v", err)
	}
	if !strings.Contains(dir, "gso") || !strings.Contains(dir, "tls") {
		t.Errorf("unexpected cert dir: %s", dir)
	}

	khPath, err := KnownHostsPath()
	if err != nil {
		t.Fatalf("KnownHostsPath: %v", err)
	}
	if !strings.Contains(khPath, "gso") || !strings.Contains(khPath, "known_hosts") {
		t.Errorf("unexpected known_hosts path: %s", khPath)
	}
}

func TestLoadOrGenerateCreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "nested", "deep", "cert.pem")
	keyPath := filepath.Join(dir, "nested", "deep", "key.pem")

	_, _, err := LoadOrGenerateTLSConfig(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("expected success with nested dirs, got: %v", err)
	}

	if _, err := os.Stat(certPath); err != nil {
		t.Errorf("cert file not created: %v", err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Errorf("key file not created: %v", err)
	}
}

// tlsListenerAddr is a helper that implements net.Addr for string-based addresses.
type tlsListenerAddr string

func (a tlsListenerAddr) Network() string { return "tcp" }
func (a tlsListenerAddr) String() string  { return string(a) }

func TestCheckIPAllowed_StringAddr(t *testing.T) {
	allow, _ := ParseCIDRs([]string{"10.0.0.0/8"})

	got := CheckIPAllowed(tlsListenerAddr("10.1.2.3:5555"), allow)
	if !got {
		t.Error("expected 10.1.2.3 to be allowed in 10.0.0.0/8")
	}

	got = CheckIPAllowed(tlsListenerAddr("192.168.1.1:5555"), allow)
	if got {
		t.Error("expected 192.168.1.1 to be denied from 10.0.0.0/8")
	}
}

// Verify that VerifyPeerCertificate with no certs returns an error.
func TestTOFUNoCerts(t *testing.T) {
	cfg := TOFUTLSConfig("", "localhost:9090", "sha256:aa")
	err := cfg.VerifyPeerCertificate(nil, nil)
	if err == nil {
		t.Fatal("expected error when no certs presented")
	}
}

// Ensure the generated cert has the correct tls.Config structure for use.
func TestGeneratedCertWorksWithTLSListener(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	serverCfg, _, err := LoadOrGenerateTLSConfig(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("generating cert: %v", err)
	}

	// Create a TLS listener to verify the config is usable.
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverCfg)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	ln.Close()
}
