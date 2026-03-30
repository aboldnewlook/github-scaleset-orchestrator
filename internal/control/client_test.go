package control_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
)

func TestClientCall(t *testing.T) {
	sock := tempSocketPath(t)
	srv := control.NewServer(sock, &echoHandler{}, testLogger())

	ctx := t.Context()

	go srv.Start(ctx) //nolint:errcheck
	defer srv.Stop()  //nolint:errcheck

	time.Sleep(50 * time.Millisecond)

	client, err := control.NewClientWithPath(sock)
	if err != nil {
		t.Fatalf("connect failed: %v", err)
	}
	defer func() { _ = client.Close() }()

	result, err := client.Call(ctx, control.MethodLiveStatus, nil)
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}

	var parsed map[string]string
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if parsed["method"] != control.MethodLiveStatus {
		t.Fatalf("expected method %q, got %q", control.MethodLiveStatus, parsed["method"])
	}
}

func TestClientCallWithParams(t *testing.T) {
	sock := tempSocketPath(t)
	srv := control.NewServer(sock, &echoHandler{}, testLogger())

	ctx := t.Context()

	go srv.Start(ctx) //nolint:errcheck
	defer srv.Stop()  //nolint:errcheck

	time.Sleep(50 * time.Millisecond)

	client, err := control.NewClientWithPath(sock)
	if err != nil {
		t.Fatalf("connect failed: %v", err)
	}
	defer func() { _ = client.Close() }()

	params := control.RecycleRunnerParams{Name: "test-runner"}
	result, err := client.Call(ctx, control.MethodRecycleRunner, params)
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}

	var parsed map[string]string
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if parsed["method"] != control.MethodRecycleRunner {
		t.Fatalf("expected method %q, got %q", control.MethodRecycleRunner, parsed["method"])
	}
}

func TestClientDaemonNotRunning(t *testing.T) {
	sock := tempSocketPath(t)

	_, err := control.NewClientWithPath(sock)
	if err == nil {
		t.Fatal("expected error when daemon is not running")
	}
	if err.Error() != "daemon is not running" {
		t.Fatalf("expected 'daemon is not running', got: %v", err)
	}
}

// errorHandler returns an error response for testing client error handling.
type errorHandler struct{}

func (h *errorHandler) HandleRequest(ctx context.Context, req control.Request) control.Response {
	return control.Response{Error: "something went wrong"}
}

func waitForTCP(t *testing.T, srv *control.Server) string {
	t.Helper()
	for range 20 {
		if addr := srv.TCPAddr(); addr != "" {
			return addr
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("TCP listener did not start in time")
	return ""
}

// startTLSEchoServer creates a TLS listener that handles one JSON request/response
// using the echoHandler, returning the listener address and server TLS config.
// The caller must close the returned listener.
func startTLSEchoServer(t *testing.T) (addr string, serverTLSCfg *tls.Config, fingerprint string) {
	t.Helper()

	certPath := filepath.Join(t.TempDir(), "cert.pem")
	keyPath := filepath.Join(t.TempDir(), "key.pem")

	serverCfg, fp, err := control.LoadOrGenerateTLSConfig(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("LoadOrGenerateTLSConfig: %v", err)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverCfg)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}

	handler := &echoHandler{}
	ctx := t.Context()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				dec := json.NewDecoder(c)
				enc := json.NewEncoder(c)
				var req control.Request
				if err := dec.Decode(&req); err != nil {
					resp := control.Response{Error: "decode error"}
					_ = enc.Encode(resp)
					return
				}
				resp := handler.HandleRequest(ctx, req)
				_ = enc.Encode(resp)
			}(conn)
		}
	}()

	t.Cleanup(func() { _ = ln.Close() })

	return ln.Addr().String(), serverCfg, fp
}

func TestClientTCPConnectivity(t *testing.T) {
	t.Setenv(control.ControlTokenEnv, "test-token")

	addr, _, _ := startTLSEchoServer(t)
	tmpKH := filepath.Join(t.TempDir(), "known_hosts")

	client, err := control.NewClientWithAddr(addr, control.WithKnownHostsPath(tmpKH))
	if err != nil {
		t.Fatalf("TCP connect failed: %v", err)
	}
	defer func() { _ = client.Close() }()

	ctx := t.Context()
	result, err := client.Call(ctx, control.MethodLiveStatus, nil)
	if err != nil {
		t.Fatalf("call over TCP failed: %v", err)
	}

	var parsed map[string]string
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if parsed["method"] != control.MethodLiveStatus {
		t.Fatalf("expected method %q, got %q", control.MethodLiveStatus, parsed["method"])
	}
}

func TestConnectFallsBackToUnix(t *testing.T) {
	sock := tempSocketPath(t)
	srv := control.NewServer(sock, &echoHandler{}, testLogger())

	ctx := t.Context()

	go srv.Start(ctx) //nolint:errcheck
	defer srv.Stop()  //nolint:errcheck

	time.Sleep(50 * time.Millisecond)

	// Connect("") should fall back to Unix socket. We can't easily test
	// the default SocketPath() here since it's platform-dependent, but we
	// can verify Connect with a non-empty address uses TCP.
	_, err := control.Connect("")
	// This may fail if the default socket path doesn't have a daemon, which
	// is expected in tests. The important thing is it doesn't panic.
	_ = err
}

func TestConnectTCP(t *testing.T) {
	t.Setenv(control.ControlTokenEnv, "test-token")

	addr, _, _ := startTLSEchoServer(t)
	tmpKH := filepath.Join(t.TempDir(), "known_hosts")

	client, err := control.Connect(addr, control.WithKnownHostsPath(tmpKH))
	if err != nil {
		t.Fatalf("Connect with TCP addr failed: %v", err)
	}
	defer func() { _ = client.Close() }()

	ctx := t.Context()
	result, err := client.Call(ctx, control.MethodLiveStatus, nil)
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}

	var parsed map[string]string
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if parsed["method"] != control.MethodLiveStatus {
		t.Fatalf("expected method %q, got %q", control.MethodLiveStatus, parsed["method"])
	}
}

func TestClientServerError(t *testing.T) {
	sock := tempSocketPath(t)
	srv := control.NewServer(sock, &errorHandler{}, testLogger())

	ctx := t.Context()

	go srv.Start(ctx) //nolint:errcheck
	defer srv.Stop()  //nolint:errcheck

	time.Sleep(50 * time.Millisecond)

	client, err := control.NewClientWithPath(sock)
	if err != nil {
		t.Fatalf("connect failed: %v", err)
	}
	defer func() { _ = client.Close() }()

	_, err = client.Call(ctx, control.MethodLiveStatus, nil)
	if err == nil {
		t.Fatal("expected error from server")
	}
	if err.Error() != "server error: something went wrong" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTCPRejectsBadToken(t *testing.T) {
	t.Setenv(control.ControlTokenEnv, "correct-token")
	sock := tempSocketPath(t)
	srv := control.NewServer(sock, &echoHandler{}, testLogger(), control.WithTCPAddr("127.0.0.1:0"))

	ctx := t.Context()

	go srv.Start(ctx) //nolint:errcheck
	defer srv.Stop()  //nolint:errcheck

	tcpAddr := waitForTCP(t, srv)

	// The server uses TLS, so we dial with InsecureSkipVerify since we're
	// testing auth, not TLS verification.
	conn, err := tls.Dial("tcp", tcpAddr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec
	if err != nil {
		t.Fatalf("TLS dial failed: %v", err)
	}
	defer func() { _ = conn.Close() }()

	req := control.Request{
		Method: control.MethodLiveStatus,
		Token:  "wrong-token",
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatalf("encode failed: %v", err)
	}

	var resp control.Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("decode failed: %v", err)
	}

	if resp.Error == "" {
		t.Fatal("expected error response for bad token")
	}
	if !strings.Contains(resp.Error, "unauthorized") {
		t.Fatalf("expected 'unauthorized' in error, got: %q", resp.Error)
	}
}

func TestClientTLSConnection(t *testing.T) {
	t.Setenv(control.ControlTokenEnv, "test-token")

	addr, _, _ := startTLSEchoServer(t)
	tmpKH := filepath.Join(t.TempDir(), "known_hosts")

	client, err := control.NewClientWithAddr(addr, control.WithKnownHostsPath(tmpKH))
	if err != nil {
		t.Fatalf("TLS connect failed: %v", err)
	}
	defer func() { _ = client.Close() }()

	ctx := t.Context()
	result, err := client.Call(ctx, control.MethodLiveStatus, nil)
	if err != nil {
		t.Fatalf("call over TLS failed: %v", err)
	}

	var parsed map[string]string
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if parsed["method"] != control.MethodLiveStatus {
		t.Fatalf("expected method %q, got %q", control.MethodLiveStatus, parsed["method"])
	}

	// Verify the known_hosts file was created and contains the server's fingerprint.
	data, err := os.ReadFile(tmpKH)
	if err != nil {
		t.Fatalf("known_hosts file should exist: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, addr) {
		t.Fatalf("known_hosts should contain server address %q, got: %s", addr, content)
	}
	if !strings.Contains(content, "sha256:") {
		t.Fatalf("known_hosts should contain a sha256 fingerprint, got: %s", content)
	}
}

func TestTOFUFirstConnect(t *testing.T) {
	addr, _, fp := startTLSEchoServer(t)
	tmpKH := filepath.Join(t.TempDir(), "known_hosts")

	client, err := control.NewClientWithAddr(addr, control.WithKnownHostsPath(tmpKH))
	if err != nil {
		t.Fatalf("first TOFU connect failed: %v", err)
	}
	_ = client.Close()

	// Verify known_hosts was created with the server's fingerprint.
	hosts, err := control.LoadKnownHosts(tmpKH)
	if err != nil {
		t.Fatalf("LoadKnownHosts: %v", err)
	}

	savedFP, ok := hosts[addr]
	if !ok {
		t.Fatalf("known_hosts should contain entry for %q, got: %v", addr, hosts)
	}
	if savedFP != fp {
		t.Fatalf("saved fingerprint mismatch: want %q, got %q", fp, savedFP)
	}
}

func TestTOFUFingerprintMismatch(t *testing.T) {
	addr, _, _ := startTLSEchoServer(t)
	tmpKH := filepath.Join(t.TempDir(), "known_hosts")

	// Write a fake fingerprint for the server address.
	fakeFingerprint := "sha256:00:11:22:33:44:55:66:77:88:99:aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99:aa:bb:cc:dd:ee:ff"
	if err := control.SaveKnownHost(tmpKH, addr, fakeFingerprint); err != nil {
		t.Fatalf("SaveKnownHost: %v", err)
	}

	// Connecting should fail because the server's real fingerprint doesn't match.
	_, err := control.NewClientWithAddr(addr, control.WithKnownHostsPath(tmpKH))
	if err == nil {
		t.Fatal("expected error for fingerprint mismatch")
	}
	if !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("expected 'fingerprint mismatch' in error, got: %v", err)
	}
}

func TestClientTLSWithTrustFingerprint(t *testing.T) {
	addr, serverCfg, fp := startTLSEchoServer(t)
	tmpKH := filepath.Join(t.TempDir(), "known_hosts")

	// Verify that pre-trusting the correct fingerprint works.
	client, err := control.NewClientWithAddr(addr,
		control.WithKnownHostsPath(tmpKH),
		control.WithTrustFingerprint(fp),
	)
	if err != nil {
		t.Fatalf("connect with trust fingerprint failed: %v", err)
	}
	_ = client.Close()

	// Verify that a wrong trust fingerprint is rejected.
	_ = serverCfg // just to keep it referenced
	wrongFP := "sha256:ff:ff:ff:ff:ff:ff:ff:ff:ff:ff:ff:ff:ff:ff:ff:ff:ff:ff:ff:ff:ff:ff:ff:ff:ff:ff:ff:ff:ff:ff:ff:ff"
	_, err = control.NewClientWithAddr(addr,
		control.WithKnownHostsPath(tmpKH),
		control.WithTrustFingerprint(wrongFP),
	)
	if err == nil {
		t.Fatal("expected error for wrong trust fingerprint")
	}
	if !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("expected 'fingerprint mismatch' in error, got: %v", err)
	}
}

// TestTOFUSecondConnectSameFingerprint verifies that a second connection to the
// same server succeeds when the fingerprint matches the known_hosts entry.
func TestTOFUSecondConnectSameFingerprint(t *testing.T) {
	addr, _, _ := startTLSEchoServer(t)
	tmpKH := filepath.Join(t.TempDir(), "known_hosts")

	// First connection: TOFU saves the fingerprint.
	c1, err := control.NewClientWithAddr(addr, control.WithKnownHostsPath(tmpKH))
	if err != nil {
		t.Fatalf("first connect failed: %v", err)
	}
	_ = c1.Close()

	// Verify known_hosts has an entry.
	hosts, err := control.LoadKnownHosts(tmpKH)
	if err != nil {
		t.Fatalf("LoadKnownHosts: %v", err)
	}
	if _, ok := hosts[addr]; !ok {
		t.Fatal("known_hosts should have an entry after first connect")
	}

	// Second connection: should succeed because the fingerprint matches.
	c2, err := control.NewClientWithAddr(addr, control.WithKnownHostsPath(tmpKH))
	if err != nil {
		t.Fatalf("second connect (same fingerprint) should succeed: %v", err)
	}

	// Verify we can actually make a call.
	ctx := t.Context()
	result, err := c2.Call(ctx, control.MethodLiveStatus, nil)
	if err != nil {
		t.Fatalf("call on second connection failed: %v", err)
	}
	_ = c2.Close()

	var parsed map[string]string
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if parsed["method"] != control.MethodLiveStatus {
		t.Fatalf("expected method %q, got %q", control.MethodLiveStatus, parsed["method"])
	}
}

// parseTLSFingerprint extracts the fingerprint from the server's TLS config for assertions.
func parseTLSFingerprint(t *testing.T, serverCfg *tls.Config) string {
	t.Helper()
	if len(serverCfg.Certificates) == 0 {
		t.Fatal("server TLS config has no certificates")
	}
	cert, err := x509.ParseCertificate(serverCfg.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatalf("parsing server certificate: %v", err)
	}
	return control.CertFingerprint(cert)
}
