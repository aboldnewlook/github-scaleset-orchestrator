package runner

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestNamedPipePath(t *testing.T) {
	tests := []struct {
		host    string
		want    string
		wantErr bool
	}{
		{"npipe:////./pipe/docker_engine", `\\.\pipe\docker_engine`, false},
		{"npipe:////./pipe/dockerDesktopLinuxEngine", `\\.\pipe\dockerDesktopLinuxEngine`, false},
		{"npipe://./pipe/docker_engine", "", true},
		{"npipe:////./pipe/", "", true},
		{"npipe:////./pipe/a/b", "", true},
		{"npipe:////./pipe/a b", "", true},
		{"unix:///var/run/docker.sock", "", true},
	}
	for _, tt := range tests {
		got, err := namedPipePath(tt.host)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("namedPipePath(%q) = %q, %v; want %q, err=%v", tt.host, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestDockerDialerRejectsUnsupportedHosts(t *testing.T) {
	hosts := []string{"tcp://127.0.0.1:2375", "unix://docker.sock", "/var/run/docker.sock", "npipe://./pipe/docker_engine"}
	if runtime.GOOS != "windows" {
		hosts = append(hosts, "npipe:////./pipe/docker_engine")
	}
	for _, host := range hosts {
		if _, err := DockerDialer(host); err == nil {
			t.Errorf("DockerDialer(%q) succeeded, want an error", host)
		}
	}
}

func TestDockerDialerDialsUnixSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named pipes are covered by dial_windows_test.go")
	}
	dir, err := os.MkdirTemp("", "gsod") // short path: macOS caps unix socket paths at 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan struct{})
	go func() {
		if c, err := ln.Accept(); err == nil {
			_ = c.Close()
			close(accepted)
		}
	}()

	dial, err := DockerDialer("unix://" + sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("the socket never saw a connection")
	}
}
