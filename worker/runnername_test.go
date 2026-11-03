package worker

import (
	"strings"
	"testing"
)

func TestValidRunnerName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"gso-local-app-abc123", true},
		{"gso-mac-mini-my-repo-x1y2z3", true},
		{"a", true},
		{strings.Repeat("a", 64), true},
		{strings.Repeat("a", 65), false},
		{"", false},
		{"-x", false},
		{"A", false},
		{"a_b", false},
		{"a.b", false},
		{"../x", false},
		{"a/b", false},
		{"a b", false},
		{"a\x00b", false},
	}
	for _, tt := range tests {
		if got := validRunnerName(tt.name); got != tt.want {
			t.Errorf("validRunnerName(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}
