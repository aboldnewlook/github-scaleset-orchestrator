package control

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTransportDefaultsToNotUnixSocket(t *testing.T) {
	if IsUnixSocket(context.Background()) {
		t.Fatal("a context without a transport must not count as the unix socket")
	}
	if !IsUnixSocket(WithTransport(context.Background(), true)) {
		t.Fatal("WithTransport(true) not reported as the unix socket")
	}
	if IsUnixSocket(WithTransport(context.Background(), false)) {
		t.Fatal("WithTransport(false) reported as the unix socket")
	}
}

type transportCapture struct{ unix chan bool }

func (h transportCapture) HandleRequest(ctx context.Context, _ Request) Response {
	h.unix <- IsUnixSocket(ctx)
	return Response{}
}

func TestUnixSocketRequestsAreMarkedLocal(t *testing.T) {
	dir, err := os.MkdirTemp("", "gsoc") // short path: macOS caps unix socket paths at 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "c.sock")
	h := transportCapture{unix: make(chan bool, 1)}
	srv := NewServer(sock, h, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Start(ctx) }()

	var client *Client
	deadline := time.Now().Add(2 * time.Second)
	for {
		client, err = NewClientWithPath(sock)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("control socket never came up: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer func() { _ = client.Close() }()
	if _, err := client.Call(ctx, "ping", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case unix := <-h.unix:
		if !unix {
			t.Fatal("a unix-socket request was not marked as local")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler was not called")
	}
}
