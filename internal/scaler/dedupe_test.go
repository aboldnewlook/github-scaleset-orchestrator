package scaler

import (
	"testing"
	"time"
)

func TestDedupeSeen(t *testing.T) {
	now := time.Unix(0, 0)
	d := newDedupe(2, time.Hour, func() time.Time { return now })
	if d.seen("a") {
		t.Fatal("first a reported as seen")
	}
	if !d.seen("a") {
		t.Fatal("repeated a not reported as seen")
	}
	if d.seen("b") {
		t.Fatal("first b reported as seen")
	}
}

func TestDedupeForgetsAfterTTL(t *testing.T) {
	now := time.Unix(0, 0)
	d := newDedupe(10, time.Hour, func() time.Time { return now })
	d.seen("a")
	now = now.Add(time.Hour)
	if d.seen("a") {
		t.Fatal("a still remembered after the TTL")
	}
}

func TestDedupeEvictsOldestBeyondCapacity(t *testing.T) {
	d := newDedupe(2, time.Hour, time.Now)
	d.seen("a")
	d.seen("b")
	d.seen("c")
	if got := d.len(); got != 2 {
		t.Fatalf("len = %d, want 2", got)
	}
	if d.seen("a") {
		t.Fatal("oldest entry a was not evicted")
	}
	if !d.seen("c") {
		t.Fatal("newest entry c was evicted")
	}
}
