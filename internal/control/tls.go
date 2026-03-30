package control

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LoadOrGenerateTLSConfig loads a TLS certificate from certPath/keyPath, or
// generates a self-signed ECDSA P-256 certificate if the files don't exist.
// Returns the TLS config and SHA-256 fingerprint of the certificate.
func LoadOrGenerateTLSConfig(certPath, keyPath string, logger *slog.Logger) (*tls.Config, string, error) {
	certExists := fileExists(certPath)
	keyExists := fileExists(keyPath)

	if certExists && keyExists {
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return nil, "", fmt.Errorf("loading TLS keypair: %w", err)
		}

		parsed, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return nil, "", fmt.Errorf("parsing certificate: %w", err)
		}

		fp := CertFingerprint(parsed)
		return &tls.Config{Certificates: []tls.Certificate{cert}}, fp, nil
	}

	if certExists != keyExists {
		return nil, "", fmt.Errorf("mismatched TLS files: cert exists=%v, key exists=%v", certExists, keyExists)
	}

	// Generate a self-signed certificate.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("generating ECDSA key: %w", err)
	}

	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return nil, "", fmt.Errorf("generating serial number: %w", err)
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "gso"},
		NotBefore:    now,
		NotAfter:     now.Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},

		DNSNames:    []string{"localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}

	if hostname, err := os.Hostname(); err == nil && hostname != "" {
		template.DNSNames = append(template.DNSNames, hostname)
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, "", fmt.Errorf("creating certificate: %w", err)
	}

	// Write cert file.
	if err := os.MkdirAll(filepath.Dir(certPath), 0700); err != nil {
		return nil, "", fmt.Errorf("creating cert directory: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	if err := os.WriteFile(certPath, certPEM, 0600); err != nil {
		return nil, "", fmt.Errorf("writing cert file: %w", err)
	}

	// Write key file.
	if err := os.MkdirAll(filepath.Dir(keyPath), 0700); err != nil {
		return nil, "", fmt.Errorf("creating key directory: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, "", fmt.Errorf("marshaling private key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return nil, "", fmt.Errorf("writing key file: %w", err)
	}

	parsed, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, "", fmt.Errorf("parsing generated certificate: %w", err)
	}

	fp := CertFingerprint(parsed)
	if logger != nil {
		logger.Info("generated self-signed TLS certificate", "fingerprint", fp)
	}

	tlsCert := tls.Certificate{
		Certificate: [][]byte{certDER},
		PrivateKey:  key,
		Leaf:        parsed,
	}

	return &tls.Config{Certificates: []tls.Certificate{tlsCert}}, fp, nil
}

// CertFingerprint returns the SHA-256 fingerprint of a certificate in
// "sha256:aa:bb:cc:..." format (matching OpenSSH convention).
func CertFingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	hexParts := make([]string, len(sum))
	for i, b := range sum {
		hexParts[i] = fmt.Sprintf("%02x", b)
	}
	return "sha256:" + strings.Join(hexParts, ":")
}

// CertDir returns the default directory for auto-generated TLS certificates.
// Creates the directory if it doesn't exist.
func CertDir() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("getting user config dir: %w", err)
	}
	dir := filepath.Join(configDir, "gso", "tls")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("creating cert dir: %w", err)
	}
	return dir, nil
}

// KnownHostsPath returns the default path for the known_hosts file.
func KnownHostsPath() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("getting user config dir: %w", err)
	}
	return filepath.Join(configDir, "gso", "known_hosts"), nil
}

// LoadKnownHosts reads the known_hosts file and returns a map of host -> fingerprint.
// Returns an empty map if the file doesn't exist.
func LoadKnownHosts(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return make(map[string]string), nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading known_hosts: %w", err)
	}

	hosts := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) == 2 {
			hosts[parts[0]] = parts[1]
		}
	}
	return hosts, nil
}

// SaveKnownHost appends or updates a host entry in the known_hosts file.
func SaveKnownHost(path, host, fingerprint string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating known_hosts directory: %w", err)
	}

	hosts, err := LoadKnownHosts(path)
	if err != nil {
		return err
	}

	hosts[host] = fingerprint

	var lines []string
	for h, fp := range hosts {
		lines = append(lines, h+" "+fp)
	}

	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600)
}

// VerifyOrTrustFingerprint implements TOFU (Trust On First Use).
//   - New host: saves fingerprint, returns nil
//   - Known host with matching fingerprint: returns nil
//   - Known host with different fingerprint: returns error with both fingerprints
func VerifyOrTrustFingerprint(knownHostsPath, host, fingerprint string) error {
	hosts, err := LoadKnownHosts(knownHostsPath)
	if err != nil {
		return err
	}

	known, exists := hosts[host]
	if !exists {
		return SaveKnownHost(knownHostsPath, host, fingerprint)
	}

	if known == fingerprint {
		return nil
	}

	return fmt.Errorf(
		"fingerprint mismatch for host %q: known=%s, received=%s (possible MITM attack)",
		host, known, fingerprint,
	)
}

// TOFUTLSConfig creates a TLS config that implements Trust On First Use.
// If trustFingerprint is non-empty, it's used instead of the known_hosts file
// (useful for automation / --trust-fingerprint flag).
func TOFUTLSConfig(knownHostsPath, serverAddr, trustFingerprint string) *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // TOFU: we verify the fingerprint ourselves
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("server presented no certificates")
			}

			cert, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return fmt.Errorf("parsing server certificate: %w", err)
			}

			fp := CertFingerprint(cert)

			if trustFingerprint != "" {
				if fp != trustFingerprint {
					return fmt.Errorf(
						"fingerprint mismatch: expected=%s, received=%s",
						trustFingerprint, fp,
					)
				}
				return nil
			}

			return VerifyOrTrustFingerprint(knownHostsPath, serverAddr, fp)
		},
	}
}

// ParseCIDRs parses a slice of CIDR strings into net.IPNet values.
// Returns an error if any CIDR is invalid.
func ParseCIDRs(cidrs []string) ([]*net.IPNet, error) {
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", cidr, err)
		}
		nets = append(nets, ipNet)
	}
	return nets, nil
}

// CheckIPAllowed checks if the remote address is within any of the allowed CIDRs.
// Returns true if the allowlist is empty (no restriction) or the IP matches.
func CheckIPAllowed(remoteAddr net.Addr, allowlist []*net.IPNet) bool {
	if len(allowlist) == 0 {
		return true
	}

	var ip net.IP
	switch addr := remoteAddr.(type) {
	case *net.TCPAddr:
		ip = addr.IP
	case *net.UDPAddr:
		ip = addr.IP
	default:
		// Try parsing as "host:port" string.
		host, _, err := net.SplitHostPort(remoteAddr.String())
		if err != nil {
			// Try as bare IP.
			ip = net.ParseIP(remoteAddr.String())
		} else {
			ip = net.ParseIP(host)
		}
	}

	if ip == nil {
		return false
	}

	for _, cidr := range allowlist {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
