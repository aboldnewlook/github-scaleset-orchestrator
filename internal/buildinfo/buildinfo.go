package buildinfo

import (
	"fmt"
	"runtime"
)

// These variables are set at build time via -ldflags.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// String returns a human-readable build info string.
func String() string {
	return fmt.Sprintf("gso %s (commit %s, built %s, %s/%s, go %s)",
		Version, short(Commit), Date, runtime.GOOS, runtime.GOARCH, runtime.Version())
}

func short(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}
