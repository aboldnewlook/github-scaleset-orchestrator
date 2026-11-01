package runner

import (
	"regexp"
	"testing"
)

func TestPinnedRunnerHasDigestForEverySupportedPlatform(t *testing.T) {
	pin, err := PinnedRunner()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(pin.Version) {
		t.Fatalf("version = %q", pin.Version)
	}
	for _, p := range [][2]string{{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "amd64"}, {"darwin", "arm64"}} {
		platform, err := Platform(p[0], p[1])
		if err != nil {
			t.Fatal(err)
		}
		url, sum, err := pin.Asset(platform)
		if err != nil {
			t.Fatal(err)
		}
		if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(sum) {
			t.Errorf("%s digest = %q", platform, sum)
		}
		want := "https://github.com/actions/runner/releases/download/v" + pin.Version + "/actions-runner-" + platform + "-" + pin.Version + ".tar.gz"
		if url != want {
			t.Errorf("url = %q, want %q", url, want)
		}
	}
}

func TestPlatformMapsGoNamesToRunnerNames(t *testing.T) {
	got, err := Platform("darwin", "arm64")
	if err != nil || got != "osx-arm64" {
		t.Fatalf("Platform(darwin, arm64) = %q, %v", got, err)
	}
	for _, p := range [][2]string{{"windows", "amd64"}, {"linux", "386"}, {"freebsd", "amd64"}} {
		if _, err := Platform(p[0], p[1]); err == nil {
			t.Errorf("Platform(%s, %s) succeeded, want error", p[0], p[1])
		}
	}
}
