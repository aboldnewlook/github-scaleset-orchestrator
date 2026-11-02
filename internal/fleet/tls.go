package fleet

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/worker"
)

const (
	workerCertValidity    = 365 * 24 * time.Hour
	workerCertRenewBefore = 30 * 24 * time.Hour
)

// LoadOrCreateTLS returns the worker listener's TLS config and SPKI pin
// (spec §4.2). dir/key.pem is generated once and never regenerated while it
// exists: workers pin its public key, so losing it means re-enrolling every
// worker. dir/cert.pem is reissued from that key when it is missing,
// unparsable, for another key, or within 30 days of expiry.
func LoadOrCreateTLS(dir string, logger *slog.Logger) (*tls.Config, string, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", fmt.Errorf("creating %s: %w", dir, err)
	}
	keyPath, certPath := filepath.Join(dir, "key.pem"), filepath.Join(dir, "cert.pem")
	key, err := loadWorkerKey(keyPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if key, err = createWorkerKey(keyPath); err != nil {
			return nil, "", err
		}
		logger.Info("generated the worker listener key; back it up, losing it means re-enrolling every worker", "path", keyPath)
	case err != nil:
		return nil, "", fmt.Errorf("loading worker listener key %s (it is never regenerated: restore it, or delete it and re-enroll every worker): %w", keyPath, err)
	}
	now := time.Now()
	cert, err := loadWorkerCert(certPath, key)
	if err != nil || now.Add(workerCertRenewBefore).After(cert.NotAfter) {
		if cert, err = issueWorkerCert(certPath, key, now); err != nil {
			return nil, "", err
		}
		logger.Info("issued the worker listener certificate", "not_after", cert.NotAfter.Format(time.RFC3339))
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{"h2", "http/1.1"},
		Certificates: []tls.Certificate{{Certificate: [][]byte{cert.Raw}, PrivateKey: key, Leaf: cert}},
	}, worker.SPKIPin(cert), nil
}

func loadWorkerKey(path string) (*ecdsa.PrivateKey, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s has mode %04o; it must not be accessible by group or others", path, info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("no PKCS#8 PRIVATE KEY block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("the key is not ECDSA")
	}
	return key, nil
}

func createWorkerKey(path string) (*ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating worker listener key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // never overwrite an existing key
	if err != nil {
		return nil, fmt.Errorf("creating %s: %w", path, err)
	}
	if err := pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der}); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("writing %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("writing %s: %w", path, err)
	}
	return key, nil
}

func loadWorkerCert(path string, key *ecdsa.PrivateKey) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("no CERTIFICATE block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return nil, errors.New("the certificate does not match the key")
	}
	return cert, nil
}

func issueWorkerCert(path string, key *ecdsa.PrivateKey, now time.Time) (*x509.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "gso-router"},
		DNSNames:              []string{"gso-router"}, // informational: workers verify the SPKI pin only
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(workerCertValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("issuing worker listener certificate: %w", err)
	}
	if err := writeFileAtomic(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}
