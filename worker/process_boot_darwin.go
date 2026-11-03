//go:build darwin

package worker

import (
	"encoding/binary"
	"syscall"
)

// bootTime returns the machine's boot time as Unix seconds (sysctl
// kern.boottime, a struct timeval), or 0 when it cannot be read.
func bootTime() int64 {
	raw, err := syscall.Sysctl("kern.boottime")
	if err != nil {
		return 0
	}
	// syscall.Sysctl drops one trailing NUL, so pad back to a full timeval.
	b := make([]byte, 16)
	copy(b, raw)
	return int64(binary.LittleEndian.Uint64(b[:8])) // darwin/amd64 and arm64 are little-endian
}
