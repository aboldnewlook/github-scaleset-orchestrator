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
