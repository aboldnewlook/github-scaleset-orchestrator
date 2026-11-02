package scaler

import (
	"container/list"
	"sync"
	"time"
)

const (
	dedupeMaxEntries = 2000
	dedupeTTL        = time.Hour
)

// dedupe remembers recently handled job messages. scaleset v0.5 acks a
// message only after Scale returns, so a crash or a failed ack redelivers it
// (design spec §5.2). The handlers are idempotent anyway; dedupe keeps events
// and logs from repeating.
type dedupe struct {
	mu      sync.Mutex
	max     int
	ttl     time.Duration
	now     func() time.Time
	order   *list.List // front = newest; values are *dedupeEntry
	entries map[string]*list.Element
}

type dedupeEntry struct {
	key  string
	seen time.Time
}

func newDedupe(maxEntries int, ttl time.Duration, now func() time.Time) *dedupe {
	return &dedupe{max: maxEntries, ttl: ttl, now: now, order: list.New(), entries: make(map[string]*list.Element)}
}

// seen records key and reports whether it was already recorded within the TTL.
func (d *dedupe) seen(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	for el := d.order.Back(); el != nil; el = d.order.Back() {
		e := el.Value.(*dedupeEntry)
		if now.Sub(e.seen) < d.ttl {
			break
		}
		d.order.Remove(el)
		delete(d.entries, e.key)
	}
	if _, ok := d.entries[key]; ok {
		return true
	}
	d.entries[key] = d.order.PushFront(&dedupeEntry{key: key, seen: now})
	for d.order.Len() > d.max {
		oldest := d.order.Back()
		d.order.Remove(oldest)
		delete(d.entries, oldest.Value.(*dedupeEntry).key)
	}
	return false
}

func (d *dedupe) len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.order.Len()
}
