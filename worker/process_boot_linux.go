//go:build linux

package worker

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// bootTime returns the machine's boot time as Unix seconds (/proc/stat btime),
// or 0 when it cannot be read.
func bootTime() int64 {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "btime "); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return 0
			}
			return n
		}
	}
	return 0
}
