package runner

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed pin.json
var pinJSON []byte

// Pin is the GitHub Actions runner release this gso build uses, with the
// SHA-256 of each platform's tarball. Bump it with `task runner:bump`.
type Pin struct {
	Version string            `json:"version"`
	SHA256  map[string]string `json:"sha256"` // key: asset platform, e.g. "linux-x64"
}

// PinnedRunner returns the runner pin embedded at build time.
func PinnedRunner() (Pin, error) {
	var p Pin
	if err := json.Unmarshal(pinJSON, &p); err != nil {
		return Pin{}, fmt.Errorf("decoding runner pin: %w", err)
	}
	if p.Version == "" || len(p.SHA256) == 0 {
		return Pin{}, fmt.Errorf("runner pin is incomplete")
	}
	return p, nil
}

// Platform maps GOOS/GOARCH to the runner's asset platform, e.g.
// darwin/arm64 -> "osx-arm64". Windows has no tar.gz asset (P5).
func Platform(goos, goarch string) (string, error) {
	osName := map[string]string{"linux": "linux", "darwin": "osx"}[goos]
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[goarch]
	if osName == "" || arch == "" {
		return "", fmt.Errorf("no pinned runner for %s/%s", goos, goarch)
	}
	return osName + "-" + arch, nil
}

// Asset returns the download URL and expected SHA-256 for a platform.
func (p Pin) Asset(platform string) (url, sha256 string, err error) {
	sum, ok := p.SHA256[platform]
	if !ok {
		return "", "", fmt.Errorf("runner pin %s has no digest for %s", p.Version, platform)
	}
	url = fmt.Sprintf("https://github.com/actions/runner/releases/download/v%[1]s/actions-runner-%[2]s-%[1]s.tar.gz", p.Version, platform)
	return url, sum, nil
}
