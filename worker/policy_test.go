package worker

import "testing"

func TestMatchRepo(t *testing.T) {
	tests := []struct {
		patterns []string
		repo     string
		want     bool
	}{
		{[]string{"jrschumacher/*"}, "jrschumacher/prune", true},
		{[]string{"JRSchumacher/*"}, "jrschumacher/prune", true},
		{[]string{"aboldnewlook/gso"}, "aboldnewlook/gso", true},
		{[]string{"aboldnewlook/gso"}, "aboldnewlook/other", false},
		{[]string{"aboldnewlook/*"}, "jrschumacher/prune", false},
		{[]string{"*/*"}, "jrschumacher/prune", false},
		{[]string{"jrschumacher"}, "jrschumacher/prune", false},
		{nil, "jrschumacher/prune", false},
		{[]string{"jrschumacher/*"}, "not-a-repo", false},
		{[]string{" jrschumacher/* "}, "jrschumacher/prune", true},
	}
	for _, tt := range tests {
		if got := MatchRepo(tt.patterns, tt.repo); got != tt.want {
			t.Errorf("MatchRepo(%v, %q) = %v, want %v", tt.patterns, tt.repo, got, tt.want)
		}
	}
}

func TestParsePublicMode(t *testing.T) {
	tests := []struct {
		in      string
		want    PublicMode
		wantErr bool
	}{
		{"", PublicFollow, false},
		{"follow", PublicFollow, false},
		{"Accept", PublicAccept, false},
		{" refuse ", PublicRefuse, false},
		{"yes", "", true},
	}
	for _, tt := range tests {
		got, err := ParsePublicMode(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("ParsePublicMode(%q) = %q, %v; want %q, err=%v", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestEffectiveAcceptPublic(t *testing.T) {
	tests := []struct {
		mode          PublicMode
		routerDefault bool
		want          bool
	}{
		{PublicFollow, false, false},
		{PublicFollow, true, true},
		{"", true, true},
		{PublicAccept, false, true},
		{PublicRefuse, true, false},
		{"garbage", true, false},
	}
	for _, tt := range tests {
		if got := EffectiveAcceptPublic(tt.mode, tt.routerDefault); got != tt.want {
			t.Errorf("EffectiveAcceptPublic(%q, %v) = %v, want %v", tt.mode, tt.routerDefault, got, tt.want)
		}
	}
}
