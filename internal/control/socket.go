package control

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// SocketPath returns the platform-appropriate path for the control socket.
// macOS: $TMPDIR/gso-$UID.sock
// Linux: $XDG_RUNTIME_DIR/gso.sock (fallback: /tmp/gso-$UID.sock)
func SocketPath() string {
	uid := os.Getuid()

	if runtime.GOOS == "darwin" {
		tmpDir := os.TempDir()
		return filepath.Join(tmpDir, fmt.Sprintf("gso-%d.sock", uid))
	}

	// Linux: prefer XDG_RUNTIME_DIR
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return filepath.Join(xdg, "gso.sock")
	}

	return fmt.Sprintf("/tmp/gso-%d.sock", uid)
}
