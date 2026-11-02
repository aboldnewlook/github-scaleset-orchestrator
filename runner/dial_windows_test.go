//go:build windows

package runner

import "testing"

func TestDockerDialerAcceptsNamedPipes(t *testing.T) {
	for _, host := range []string{"", "npipe:////./pipe/docker_engine", "npipe:////./pipe/dockerDesktopLinuxEngine"} {
		if _, err := DockerDialer(host); err != nil {
			t.Errorf("DockerDialer(%q): %v", host, err)
		}
	}
}
