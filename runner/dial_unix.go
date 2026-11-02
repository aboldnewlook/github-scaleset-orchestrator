//go:build !windows

package runner

import (
	"context"
	"net"
)

// DockerDialer returns a dialer for a Docker Engine endpoint. Outside
// Windows only unix:///absolute/path endpoints are supported; "" means
// DefaultDockerHost.
func DockerDialer(host string) (func(ctx context.Context) (net.Conn, error), error) {
	socket, err := DockerSocketPath(host)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}, nil
}
