//go:build windows

package worker

import "errors"

// NewProcessLauncher is not available on Windows (it needs process groups).
func NewProcessLauncher(ProcessLauncherConfig) (Launcher, error) {
	return nil, errors.New("process launcher: not supported on Windows")
}
