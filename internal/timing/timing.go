// Package timing scales gso's default timers for tests and the GitHub
// simulator. Production leaves GSO_TIMING_SCALE unset, and every timer keeps
// its documented value.
package timing

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const envScale = "GSO_TIMING_SCALE"

var factor = sync.OnceValue(func() float64 { return parseFactor(os.Getenv(envScale)) })

// Scale multiplies d by GSO_TIMING_SCALE, a float in (0,1] read once per
// process. An unset or invalid value means 1. A positive d never scales
// below 1 ms (or below d itself when d is shorter), so tickers stay valid.
// Test and simulator use only.
func Scale(d time.Duration) time.Duration {
	return scaleBy(d, factor())
}

func parseFactor(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 1
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || !(f > 0 && f <= 1) {
		return 1
	}
	return f
}

func scaleBy(d time.Duration, f float64) time.Duration {
	if f == 1 || d <= 0 {
		return d
	}
	return max(time.Duration(float64(d)*f), min(d, time.Millisecond))
}
