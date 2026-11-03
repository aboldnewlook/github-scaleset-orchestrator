package worker

import (
	"strings"
	"testing"
)

func TestValidPattern(t *testing.T) {
	tests := []struct {
		in      string
		wantErr bool
	}{
		{"acme/*", false},
		{"acme/repo", false},
		{"Acme-1/my.repo_x-y", false},
		{"", true},
		{"acme", true},
		{"acme/", true},
		{"/repo", true},
		{"*/repo", true},
		{"*/*", true},
		{"acme/repo/extra", true},
		{"acme/re*", true},
		{"ac_me/repo", true}, // owners are [A-Za-z0-9-] only
		{"ac.me/repo", true},
		{" acme/repo", true},
		{"acme/repo ", true},
		{"acme/re po", true},
		{"acme/repo\n", true},
		{"acme/рepo", true}, // Cyrillic look-alike
		{"acmeK/repo", true},
		{"acme/rep@o", true},
		{"acme/repo?x", true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			err := ValidPattern(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidPattern(%q) = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
		})
	}
}

func TestNewRefusesAnInvalidAllowlistEntryByName(t *testing.T) {
	_, err := New(Config{Allowlist: []string{"acme/*", "bad entry"}})
	if err == nil || !strings.Contains(err.Error(), `"bad entry"`) {
		t.Fatalf("New() error = %v, want it to name the bad entry", err)
	}
}
