package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Manager handles downloading and caching the GitHub Actions runner binary.
type Manager struct {
	cacheDir string
	logger   *slog.Logger
}

func NewManager(cacheDir string, logger *slog.Logger) (*Manager, error) {
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating cache dir: %w", err)
	}
	return &Manager{cacheDir: cacheDir, logger: logger}, nil
}

// RunnerDir returns the path to the cached runner binaries, downloading if needed.
func (m *Manager) RunnerDir(ctx context.Context) (string, error) {
	release, err := m.latestRelease(ctx)
	if err != nil {
		return "", fmt.Errorf("fetching latest release: %w", err)
	}

	// Check if we already have this version cached
	versionDir := filepath.Join(m.cacheDir, "runner-"+release.version)
	checksumFile := filepath.Join(versionDir, ".sha256")

	if m.verifyCached(versionDir, checksumFile, release.checksum) {
		m.logger.Info("runner binary cached", "version", release.version)
		return versionDir, nil
	}

	m.logger.Info("downloading runner binary", "version", release.version)
	if err := m.download(ctx, release, versionDir, checksumFile); err != nil {
		return "", fmt.Errorf("downloading runner: %w", err)
	}
	return versionDir, nil
}

// verifyCached checks if the runner is cached and the checksum matches.
func (m *Manager) verifyCached(dir, checksumFile, expectedChecksum string) bool {
	runExe := filepath.Join(dir, "run.sh")
	if runtime.GOOS == "windows" {
		runExe = filepath.Join(dir, "run.cmd")
	}

	if _, err := os.Stat(runExe); err != nil {
		return false
	}

	if expectedChecksum == "" {
		return true
	}

	stored, err := os.ReadFile(checksumFile)
	if err != nil {
		return false
	}

	return strings.TrimSpace(string(stored)) == expectedChecksum
}

func (m *Manager) download(ctx context.Context, release *runnerRelease, dest, checksumFile string) error {
	m.logger.Info("fetching runner", "url", release.url)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, release.url, nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("downloading runner tarball: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status downloading runner: %d", resp.StatusCode)
	}

	tmpFile, err := os.CreateTemp(m.cacheDir, "runner-*.tar.gz")
	if err != nil {
		return err
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Download with progress logging and checksum
	hasher := sha256.New()
	writer := io.MultiWriter(tmpFile, hasher)

	totalSize := resp.ContentLength
	pr := &progressReader{
		reader:    resp.Body,
		total:     totalSize,
		logger:    m.logger,
		interval:  30 * time.Second,
		lastPrint: time.Now(),
	}

	if _, err := io.Copy(writer, pr); err != nil {
		return fmt.Errorf("writing runner tarball: %w", err)
	}
	pr.logFinal()
	tmpFile.Close()

	// Verify checksum if available
	actualChecksum := hex.EncodeToString(hasher.Sum(nil))
	if release.checksum != "" && actualChecksum != release.checksum {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", release.checksum, actualChecksum)
	}

	// Clean up old version if exists, then extract
	os.RemoveAll(dest)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}

	if err := extractTarGz(tmpFile.Name(), dest); err != nil {
		return err
	}

	// Store checksum for future verification
	return os.WriteFile(checksumFile, []byte(actualChecksum), 0o644)
}

// progressReader wraps a reader and logs download progress periodically.
type progressReader struct {
	reader    io.Reader
	total     int64
	read      int64
	logger    *slog.Logger
	interval  time.Duration
	lastPrint time.Time
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.reader.Read(p)
	pr.read += int64(n)

	if time.Since(pr.lastPrint) >= pr.interval {
		pr.logProgress()
		pr.lastPrint = time.Now()
	}

	return n, err
}

func (pr *progressReader) logProgress() {
	if pr.total > 0 {
		pct := float64(pr.read) / float64(pr.total) * 100
		pr.logger.Info("downloading runner",
			"progress", fmt.Sprintf("%.0f%%", pct),
			"downloaded", formatBytes(pr.read),
			"total", formatBytes(pr.total),
		)
	} else {
		pr.logger.Info("downloading runner",
			"downloaded", formatBytes(pr.read),
		)
	}
}

func (pr *progressReader) logFinal() {
	pr.logger.Info("download complete", "size", formatBytes(pr.read))
}

func formatBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

type runnerRelease struct {
	version  string
	url      string
	checksum string
}

type ghRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func (m *Manager) latestRelease(ctx context.Context) (*runnerRelease, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/repos/actions/runner/releases/latest", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching latest runner release: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status fetching runner release: %d", resp.StatusCode)
	}

	var release ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("decoding release: %w", err)
	}

	osName := runtime.GOOS
	if osName == "darwin" {
		osName = "osx"
	}

	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "x64"
	case "arm64":
		// arm64 stays as-is
	}

	suffix := fmt.Sprintf("%s-%s", osName, arch)

	var tarballURL, checksumURL string
	for _, asset := range release.Assets {
		if strings.Contains(asset.Name, suffix) {
			if strings.HasSuffix(asset.Name, ".tar.gz") {
				tarballURL = asset.BrowserDownloadURL
			}
			if strings.HasSuffix(asset.Name, ".tar.gz.sha256") {
				checksumURL = asset.BrowserDownloadURL
			}
		}
	}

	if tarballURL == "" {
		return nil, fmt.Errorf("no runner binary found for %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	// Fetch checksum if available
	var checksum string
	if checksumURL != "" {
		checksum, _ = m.fetchChecksum(ctx, checksumURL)
	}

	version := strings.TrimPrefix(release.TagName, "v")

	return &runnerRelease{
		version:  version,
		url:      tarballURL,
		checksum: checksum,
	}, nil
}

func (m *Manager) fetchChecksum(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	// Checksum files are typically "hash  filename" or just "hash"
	parts := strings.Fields(strings.TrimSpace(string(data)))
	if len(parts) > 0 {
		return parts[0], nil
	}

	return "", fmt.Errorf("empty checksum file")
}
