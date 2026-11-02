package cmd

import (
	"strings"
	"testing"
	"time"
)

func TestSimJobSpec(t *testing.T) {
	spec, err := simJobSpec("o/a", " self-hosted , Linux ,", "pull_request", "failure", 5*time.Second, 3, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Repo != "o/a" || len(spec.Labels) != 2 || spec.Labels[1] != "Linux" || !spec.Public || !spec.Fork || spec.Duration != 5*time.Second || spec.Event != "pull_request" || spec.Result != "failure" {
		t.Fatalf("spec = %+v", spec)
	}
	for _, tc := range []struct {
		name, repo string
		count      int
		duration   time.Duration
		want       string
	}{
		{"no repo", "", 1, time.Second, "--repo"},
		{"bad repo", "owner-only", 1, time.Second, "owner/name"},
		{"count", "o/a", 0, time.Second, "--count"},
		{"too many", "o/a", 101, time.Second, "--count"},
		{"negative", "o/a", 1, -time.Second, "--duration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := simJobSpec(tc.repo, "self-hosted", "push", "succeeded", tc.duration, tc.count, false, false)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one mentioning %s", err, tc.want)
			}
		})
	}
}

func TestRequireSimURL(t *testing.T) {
	if _, err := requireSimURL(""); err == nil || !strings.Contains(err.Error(), "GSO_SIM_URL") {
		t.Fatalf("empty URL error = %v", err)
	}
	if _, err := requireSimURL("unraid.lan:9300"); err == nil {
		t.Fatal("a URL without a scheme was accepted")
	}
	if got, err := requireSimURL("http://unraid.lan:9300/"); err != nil || got != "http://unraid.lan:9300" {
		t.Fatalf("got %q, %v", got, err)
	}
}
