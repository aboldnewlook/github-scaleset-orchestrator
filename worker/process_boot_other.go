//go:build !linux && !darwin

package worker

// bootTime is unknown on this platform: pid records carry no boot time and are
// never treated as stale.
func bootTime() int64 { return 0 }
