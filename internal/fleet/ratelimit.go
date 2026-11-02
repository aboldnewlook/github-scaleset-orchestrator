package fleet

import (
	"sync"
	"time"
)

// enrollLimiter caps Enroll attempts per source address with fixed
// one-minute and one-hour windows (spec §4.4: e.g. 5/min, 30/h).
type enrollLimiter struct {
	mu      sync.Mutex
	perMin  int
	perHour int
	now     func() time.Time
	windows map[string]*enrollWindow
}

type enrollWindow struct {
	minuteStart, hourStart time.Time
	minute, hour           int
}

func newEnrollLimiter(perMin, perHour int, now func() time.Time) *enrollLimiter {
	return &enrollLimiter{perMin: perMin, perHour: perHour, now: now, windows: map[string]*enrollWindow{}}
}

func (l *enrollLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.windows) > 4096 {
		for k, w := range l.windows {
			if now.Sub(w.hourStart) >= time.Hour {
				delete(l.windows, k)
			}
		}
	}
	w := l.windows[key]
	if w == nil {
		w = &enrollWindow{minuteStart: now, hourStart: now}
		l.windows[key] = w
	}
	if now.Sub(w.minuteStart) >= time.Minute {
		w.minuteStart, w.minute = now, 0
	}
	if now.Sub(w.hourStart) >= time.Hour {
		w.hourStart, w.hour = now, 0
	}
	if w.minute >= l.perMin || w.hour >= l.perHour {
		return false
	}
	w.minute++
	w.hour++
	return true
}
