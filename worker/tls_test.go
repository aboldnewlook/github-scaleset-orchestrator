package worker

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testCert(t *testing.T, notBefore, notAfter time.Time) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "gso-router"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, leaf
}

func TestPinnedTLSConfig(t *testing.T) {
	now := time.Now()
	valid, validLeaf := testCert(t, now.Add(-time.Hour), now.Add(time.Hour))
	expired, expiredLeaf := testCert(t, now.Add(-48*time.Hour), now.Add(-24*time.Hour))
	_, otherLeaf := testCert(t, now.Add(-time.Hour), now.Add(time.Hour))
	tests := []struct {
		name       string
		cert       tls.Certificate
		pin        string
		maxVersion uint16
		wantErr    string
	}{
		{"matching pin", valid, SPKIPin(validLeaf), 0, ""},
		{"matching pin in uppercase", valid, strings.ToUpper(SPKIPin(validLeaf)), 0, ""},
		{"wrong pin", valid, SPKIPin(otherLeaf), 0, "does not match"},
		{"expired certificate, matching pin", expired, SPKIPin(expiredLeaf), 0, ""},
		{"server limited to TLS 1.2", valid, SPKIPin(validLeaf), tls.VersionTLS12, "protocol version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("ok"))
			}))
			srv.TLS = &tls.Config{Certificates: []tls.Certificate{tt.cert}, MaxVersion: tt.maxVersion}
			srv.StartTLS()
			defer srv.Close()
			client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: PinnedTLSConfig(tt.pin)}}
			resp, err := client.Get(srv.URL)
			if err == nil {
				_ = resp.Body.Close()
			}
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("GET error = %v, want success", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("GET error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestPinnedTLSConfigRequiresTLS13(t *testing.T) {
	if got := PinnedTLSConfig(testPin).MinVersion; got != tls.VersionTLS13 {
		t.Fatalf("MinVersion = %x, want TLS 1.3", got)
	}
}
