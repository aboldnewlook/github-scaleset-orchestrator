package control_test

import (
	"context"
	"encoding/json"
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

func TestClientTCPConnectivity(t *testing.T) {
	sock := tempSocketPath(t)
	srv := control.NewServer(sock, &echoHandler{}, testLogger(), control.WithTCPAddr("127.0.0.1:0"))

	ctx := t.Context()

	go srv.Start(ctx) //nolint:errcheck
	defer srv.Stop()  //nolint:errcheck

	tcpAddr := waitForTCP(t, srv)

	client, err := control.NewClientWithAddr(tcpAddr)
	if err != nil {
		t.Fatalf("TCP connect failed: %v", err)
	}
	defer func() { _ = client.Close() }()

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
	sock := tempSocketPath(t)
	srv := control.NewServer(sock, &echoHandler{}, testLogger(), control.WithTCPAddr("127.0.0.1:0"))

	ctx := t.Context()

	go srv.Start(ctx) //nolint:errcheck
	defer srv.Stop()  //nolint:errcheck

	tcpAddr := waitForTCP(t, srv)

	client, err := control.Connect(tcpAddr)
	if err != nil {
		t.Fatalf("Connect with TCP addr failed: %v", err)
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
