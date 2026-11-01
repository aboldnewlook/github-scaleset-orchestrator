package timing

import (
	"os"
	"testing"
	"time"
)

func TestParseFactor(t *testing.T) {
	tests := map[string]float64{
		"": 1, "1": 1, "0.02": 0.02, " 0.5 ": 0.5,
		"0": 1, "-1": 1, "1.5": 1, "abc": 1, "NaN": 1, "Inf": 1,
	}
	for in, want := range tests {
		if got := parseFactor(in); got != want {
			t.Errorf("parseFactor(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestScaleBy(t *testing.T) {
	tests := []struct {
		d      time.Duration
		factor float64
		want   time.Duration
	}{
		{time.Minute, 0.5, 30 * time.Second},
		{time.Second, 1, time.Second},
		{time.Second, 0.0001, time.Millisecond}, // floored so timers stay positive
		{0, 0.5, 0},
		{500 * time.Microsecond, 0.1, 500 * time.Microsecond},
	}
	for _, tt := range tests {
		if got := scaleBy(tt.d, tt.factor); got != tt.want {
			t.Errorf("scaleBy(%v, %v) = %v, want %v", tt.d, tt.factor, got, tt.want)
		}
	}
}

func TestScaleIsIdentityWithoutTheVariable(t *testing.T) {
	if _, set := os.LookupEnv(envScale); set {
		t.Skip(envScale + " is set for this run")
	}
	if got := Scale(10 * time.Second); got != 10*time.Second {
		t.Fatalf("Scale(10s) = %v without %s", got, envScale)
	}
}
