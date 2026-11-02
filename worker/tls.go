package worker

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// SPKIPin is the lowercase hex SHA-256 of a certificate's SubjectPublicKeyInfo.
// The router reuses its key when it reissues its certificate, so the pin
// outlives any one certificate (design spec §4.2).
func SPKIPin(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

// PinnedTLSConfig trusts exactly the router key whose SPKI pin it is given. It
// ignores validity dates and host names: the pinned key is the only trust
// anchor (design spec §4.2). TLS 1.3 is required.
func PinnedTLSConfig(pin string) *tls.Config {
	want := strings.ToLower(pin)
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		// The SPKI pin checked in VerifyPeerCertificate replaces chain and
		// hostname verification.
		InsecureSkipVerify: true, //nolint:gosec // #nosec G402 -- pinned SPKI is the trust anchor (design spec §4.2)
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if !pinPattern.MatchString(want) {
				return errors.New("no valid router pin configured")
			}
			if len(rawCerts) == 0 {
				return errors.New("router presented no certificate")
			}
			leaf, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return fmt.Errorf("parsing router certificate: %w", err)
			}
			got := SPKIPin(leaf)
			if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
				return fmt.Errorf("router key %s does not match the pinned key %s", ShortPin(got), ShortPin(want))
			}
			return nil
		},
	}
}
