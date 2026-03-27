package control_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"path/filepath"
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
