package fleet

import (
	"testing"
	"time"
)

func TestEnrollLimiterPerMinuteAndPerHour(t *testing.T) {
	clock := newTestClock()
	l := newEnrollLimiter(5, 30, clock.Now)
	for i := 1; i <= 5; i++ {
		if !l.allow("10.0.0.1") {
			t.Fatalf("attempt %d refused", i)
		}
	}
	if l.allow("10.0.0.1") {
		t.Fatal("6th attempt within a minute allowed")
	}
	if !l.allow("10.0.0.2") {
		t.Fatal("another source address was limited")
	}
	allowed := 5
	for minute := 1; minute < 10; minute++ {
		clock.Advance(time.Minute)
		for i := 0; i < 5; i++ {
			if l.allow("10.0.0.1") {
				allowed++
			}
		}
	}
	if allowed != 30 {
		t.Fatalf("allowed %d attempts in an hour, want 30", allowed)
	}
	clock.Advance(time.Hour)
	if !l.allow("10.0.0.1") {
		t.Fatal("still limited after the hour window")
	}
}
