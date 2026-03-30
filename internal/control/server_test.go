package control_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aboldnewlook/github-scaleset-orchestrator/internal/control"
)

// echoHandler returns a Response with the method name as the result.
type echoHandler struct{}

func (h *echoHandler) HandleRequest(ctx context.Context, req control.Request) control.Response {
	result, _ := json.Marshal(map[string]string{"method": req.Method})
	return control.Response{Result: result}
}

func tempSocketPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "test.sock")
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestServerStartStop(t *testing.T) {
	sock := tempSocketPath(t)
	srv := control.NewServer(sock, &echoHandler{}, testLogger())

	ctx := t.Context()

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Start(ctx)
	}()

	// Wait briefly for the server to start listening.
	time.Sleep(50 * time.Millisecond)

	// Verify the socket file exists.
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("socket file should exist: %v", err)
	}

	// Stop the server.
	if err := srv.Stop(); err != nil {
		t.Fatalf("stop failed: %v", err)
	}

	// Verify the socket file is removed.
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket file should be removed after stop")
	}

	// Start should return without error.
	if err := <-errCh; err != nil {
		t.Fatalf("start returned error: %v", err)
	}
}

func TestServerRequestResponse(t *testing.T) {
	sock := tempSocketPath(t)
	srv := control.NewServer(sock, &echoHandler{}, testLogger())

	ctx := t.Context()

	go srv.Start(ctx) //nolint:errcheck
	defer srv.Stop()  //nolint:errcheck

	time.Sleep(50 * time.Millisecond)

	// Connect and send a request.
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer func() { _ = conn.Close() }()

	req := control.Request{Method: control.MethodLiveStatus}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatalf("encode failed: %v", err)
	}

	var resp control.Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("decode failed: %v", err)
	}

	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}

	var result map[string]string
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result["method"] != control.MethodLiveStatus {
		t.Fatalf("expected method %q, got %q", control.MethodLiveStatus, result["method"])
	}
}

func TestServerStaleSocketRemoval(t *testing.T) {
	sock := tempSocketPath(t)

	// Create a stale socket file (just a regular file, no listener).
	if err := os.WriteFile(sock, []byte{}, 0o600); err != nil {
		t.Fatalf("creating stale file: %v", err)
	}

	srv := control.NewServer(sock, &echoHandler{}, testLogger())

	ctx := t.Context()

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Start(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	// Should have started successfully after removing the stale file.
	// Verify by connecting.
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("should be able to connect after stale socket removal: %v", err)
	}
	_ = conn.Close()

	srv.Stop() //nolint:errcheck
}

func TestServerAlreadyRunning(t *testing.T) {
	sock := tempSocketPath(t)

	// Start a first server.
	srv1 := control.NewServer(sock, &echoHandler{}, testLogger())
	ctx := t.Context()

	go srv1.Start(ctx) //nolint:errcheck
	defer srv1.Stop()  //nolint:errcheck

	time.Sleep(50 * time.Millisecond)

	// A second server should fail.
	srv2 := control.NewServer(sock, &echoHandler{}, testLogger())
	err := srv2.Start(ctx)
	if err == nil {
		t.Fatal("expected error when another instance is running")
	}
	if err.Error() != "another instance is already running (socket "+sock+" is active)" {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestServerContextCancellation(t *testing.T) {
	sock := tempSocketPath(t)
	srv := control.NewServer(sock, &echoHandler{}, testLogger())

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Start(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	// Cancel the context — server should stop.
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("expected nil error on context cancel, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop after context cancellation")
	}
}

func TestTCPRefusedWithoutToken(t *testing.T) {
	// Ensure no token is set.
	t.Setenv(control.ControlTokenEnv, "")

	sock := tempSocketPath(t)
	srv := control.NewServer(sock, &echoHandler{}, testLogger(), control.WithTCPAddr("127.0.0.1:0"))

	ctx := t.Context()

	go srv.Start(ctx) //nolint:errcheck
	defer srv.Stop()  //nolint:errcheck

	time.Sleep(50 * time.Millisecond)

	// TCP should not be listening.
	if addr := srv.TCPAddr(); addr != "" {
		t.Fatalf("TCP listener should not start without token, got addr %q", addr)
	}
}

func TestSocketPermissions(t *testing.T) {
	sock := tempSocketPath(t)
	srv := control.NewServer(sock, &echoHandler{}, testLogger())

	ctx := t.Context()

	go srv.Start(ctx) //nolint:errcheck
	defer srv.Stop()  //nolint:errcheck

	time.Sleep(50 * time.Millisecond)

	info, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	perm := info.Mode().Perm()
	// On macOS, socket permissions may differ slightly, but should not be world-readable.
	if perm&0077 != 0 {
		t.Fatalf("socket permissions %o allow group/other access, want owner-only", perm)
	}
}

func TestServerInvalidJSON(t *testing.T) {
	sock := tempSocketPath(t)
	srv := control.NewServer(sock, &echoHandler{}, testLogger())

	ctx := t.Context()

	go srv.Start(ctx) //nolint:errcheck
	defer srv.Stop()  //nolint:errcheck

	time.Sleep(50 * time.Millisecond)

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// Send invalid JSON.
	conn.Write([]byte("not json\n")) //nolint:errcheck

	var resp control.Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("decode failed: %v", err)
	}

	if resp.Error == "" {
		t.Fatal("expected error response for invalid JSON")
	}
}

func TestTCPListenerUsesTLS(t *testing.T) {
	token := "test-token-tls"
	t.Setenv(control.ControlTokenEnv, token)

	sock := tempSocketPath(t)
	srv := control.NewServer(sock, &echoHandler{}, testLogger(), control.WithTCPAddr("127.0.0.1:0"))

	ctx := t.Context()
	go srv.Start(ctx) //nolint:errcheck
	defer srv.Stop()  //nolint:errcheck

	addr := waitForTCP(t, srv)

	// Plain TCP connection should fail to get a valid JSON response (TLS handshake noise).
	plainConn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("plain dial failed: %v", err)
	}
	// Send raw JSON — the TLS layer should reject or garble this.
	_, _ = plainConn.Write([]byte(`{"method":"live_status","token":"` + token + `"}` + "\n"))
	_ = plainConn.SetReadDeadline(time.Now().Add(1 * time.Second))
	var resp control.Response
	err = json.NewDecoder(plainConn).Decode(&resp)
	_ = plainConn.Close()
	if err == nil {
		t.Fatal("expected error when sending plain text to TLS listener, but got valid JSON response")
	}

	// TLS connection should succeed.
	tlsConn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec
	if err != nil {
		t.Fatalf("TLS dial failed: %v", err)
	}
	defer func() { _ = tlsConn.Close() }()

	req := control.Request{Method: control.MethodLiveStatus, Token: token}
	if err := json.NewEncoder(tlsConn).Encode(req); err != nil {
		t.Fatalf("encode over TLS failed: %v", err)
	}

	var tlsResp control.Response
	if err := json.NewDecoder(tlsConn).Decode(&tlsResp); err != nil {
		t.Fatalf("decode over TLS failed: %v", err)
	}
	if tlsResp.Error != "" {
		t.Fatalf("unexpected error over TLS: %s", tlsResp.Error)
	}

	var result map[string]string
	if err := json.Unmarshal(tlsResp.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result["method"] != control.MethodLiveStatus {
		t.Fatalf("expected method %q, got %q", control.MethodLiveStatus, result["method"])
	}
}

func TestIPAllowlistAccepts(t *testing.T) {
	token := "test-token-allow"
	t.Setenv(control.ControlTokenEnv, token)

	sock := tempSocketPath(t)
	srv := control.NewServer(sock, &echoHandler{}, testLogger(),
		control.WithTCPAddr("127.0.0.1:0"),
		control.WithAllowCIDRs([]string{"127.0.0.0/8"}),
	)

	ctx := t.Context()
	go srv.Start(ctx) //nolint:errcheck
	defer srv.Stop()  //nolint:errcheck

	addr := waitForTCP(t, srv)

	// Localhost should be allowed by 127.0.0.0/8.
	tlsConn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec
	if err != nil {
		t.Fatalf("TLS dial failed: %v", err)
	}
	defer func() { _ = tlsConn.Close() }()

	req := control.Request{Method: control.MethodLiveStatus, Token: token}
	if err := json.NewEncoder(tlsConn).Encode(req); err != nil {
		t.Fatalf("encode failed: %v", err)
	}

	var resp control.Response
	if err := json.NewDecoder(tlsConn).Decode(&resp); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
}

func TestIPAllowlistRejects(t *testing.T) {
	token := "test-token-reject"
	t.Setenv(control.ControlTokenEnv, token)

	sock := tempSocketPath(t)
	srv := control.NewServer(sock, &echoHandler{}, testLogger(),
		control.WithTCPAddr("127.0.0.1:0"),
		control.WithAllowCIDRs([]string{"10.0.0.0/8"}), // excludes 127.0.0.1
	)

	ctx := t.Context()
	go srv.Start(ctx) //nolint:errcheck
	defer srv.Stop()  //nolint:errcheck

	addr := waitForTCP(t, srv)

	// Localhost (127.0.0.1) is not in 10.0.0.0/8, so connection should be rejected.
	tlsConn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec
	if err != nil {
		// Connection refused or reset is acceptable — the server closed the conn.
		return
	}
	defer func() { _ = tlsConn.Close() }()

	// If we got a TLS connection, try to send a request — it should fail
	// because the server closed the underlying connection after the IP check.
	req := control.Request{Method: control.MethodLiveStatus, Token: token}
	_ = tlsConn.SetDeadline(time.Now().Add(2 * time.Second))
	if err := json.NewEncoder(tlsConn).Encode(req); err != nil {
		// Write error — connection was closed by server. This is expected.
		return
	}

	var resp control.Response
	err = json.NewDecoder(tlsConn).Decode(&resp)
	if err != nil {
		// Read error — connection was closed by server. This is expected.
		return
	}

	// If we somehow got a response, the allowlist did not work.
	t.Fatal("expected connection to be rejected by IP allowlist, but got a response")
}

func TestIPAllowlistEmpty(t *testing.T) {
	token := "test-token-empty-allowlist"
	t.Setenv(control.ControlTokenEnv, token)

	sock := tempSocketPath(t)
	// No WithAllowCIDRs — empty allowlist means allow all.
	srv := control.NewServer(sock, &echoHandler{}, testLogger(),
		control.WithTCPAddr("127.0.0.1:0"),
	)

	ctx := t.Context()
	go srv.Start(ctx) //nolint:errcheck
	defer srv.Stop()  //nolint:errcheck

	addr := waitForTCP(t, srv)

	tlsConn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec
	if err != nil {
		t.Fatalf("TLS dial failed: %v", err)
	}
	defer func() { _ = tlsConn.Close() }()

	req := control.Request{Method: control.MethodLiveStatus, Token: token}
	if err := json.NewEncoder(tlsConn).Encode(req); err != nil {
		t.Fatalf("encode failed: %v", err)
	}

	var resp control.Response
	if err := json.NewDecoder(tlsConn).Decode(&resp); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
}

func TestAutoGeneratedCert(t *testing.T) {
	token := "test-token-autogen"
	t.Setenv(control.ControlTokenEnv, token)

	sock := tempSocketPath(t)
	srv := control.NewServer(sock, &echoHandler{}, testLogger(),
		control.WithTCPAddr("127.0.0.1:0"),
	)

	ctx := t.Context()
	go srv.Start(ctx) //nolint:errcheck
	defer srv.Stop()  //nolint:errcheck

	// Wait for TCP to be ready, which means TLS was set up.
	_ = waitForTCP(t, srv)

	fp := srv.Fingerprint()
	if fp == "" {
		t.Fatal("expected non-empty fingerprint after auto-generating TLS cert")
	}
	if !strings.HasPrefix(fp, "sha256:") {
		t.Fatalf("expected fingerprint to start with 'sha256:', got %q", fp)
	}
}
