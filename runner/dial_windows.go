//go:build windows

package runner

import (
	"context"
	"net"
	"strings"

	"github.com/Microsoft/go-winio"
)

// defaultWindowsDockerHost is the pipe Docker Desktop and Docker Engine for
// Windows listen on.
const defaultWindowsDockerHost = "npipe:////./pipe/docker_engine"

// DockerDialer returns a dialer for a Docker Engine endpoint. On Windows it
// accepts npipe:////./pipe/<name> ("" means the docker_engine pipe) and
// unix:///absolute/path.
func DockerDialer(host string) (func(ctx context.Context) (net.Conn, error), error) {
	if host == "" {
		host = defaultWindowsDockerHost
	}
	if strings.HasPrefix(host, "npipe:") {
		pipe, err := namedPipePath(host)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context) (net.Conn, error) {
			return winio.DialPipeContext(ctx, pipe)
		}, nil
	}
	socket, err := DockerSocketPath(host)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}, nil
}
