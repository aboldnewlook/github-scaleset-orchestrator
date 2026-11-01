package runner

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNewManager(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")

	mgr, err := NewManager(cacheDir, testLogger())
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}
	if mgr.cacheDir != cacheDir {
		t.Fatalf("cacheDir = %q, want %q", mgr.cacheDir, cacheDir)
	}

	// The cache dir should have been created
	info, err := os.Stat(cacheDir)
	if err != nil {
		t.Fatalf("cache dir not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("cache dir is not a directory")
	}
}

func TestNewManager_NestedDir(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "a", "b", "c")

	_, err := NewManager(cacheDir, testLogger())
	if err != nil {
		t.Fatalf("NewManager() error: %v", err)
	}

	if _, err := os.Stat(cacheDir); err != nil {
		t.Fatalf("nested cache dir not created: %v", err)
	}
}

func TestVerifyCached(t *testing.T) {
	dir := t.TempDir()
	mgr := &Manager{cacheDir: dir, logger: testLogger(), httpClient: &http.Client{}}

	versionDir := filepath.Join(dir, "runner-2.320.0")
	checksumFile := filepath.Join(versionDir, ".sha256")

	// Create the version dir with a run.sh
	if err := os.MkdirAll(versionDir, 0o755); err != nil {
		t.Fatal(err)
	}

	runScript := "run.sh"
	if runtime.GOOS == "windows" {
		runScript = "run.cmd"
	}
	if err := os.WriteFile(filepath.Join(versionDir, runScript), []byte("#!/bin/bash"), 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name             string
		checksumContent  string
		expectedChecksum string
		want             bool
	}{
		{
			name:             "matching checksum",
			checksumContent:  "abc123",
			expectedChecksum: "abc123",
			want:             true,
		},
		{
			name:             "mismatched checksum",
			checksumContent:  "abc123",
			expectedChecksum: "def456",
			want:             false,
		},
		{
			name:             "empty expected checksum",
			checksumContent:  "",
			expectedChecksum: "",
			want:             true,
		},
		{
			name:             "checksum with whitespace",
			checksumContent:  "abc123\n",
			expectedChecksum: "abc123",
			want:             true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.checksumContent != "" {
				if err := os.WriteFile(checksumFile, []byte(tt.checksumContent), 0o644); err != nil {
					t.Fatal(err)
				}
			} else {
				_ = os.Remove(checksumFile)
			}

			got := mgr.verifyCached(versionDir, checksumFile, tt.expectedChecksum)
			if got != tt.want {
				t.Fatalf("verifyCached() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestVerifyCached_MissingRunScript(t *testing.T) {
	dir := t.TempDir()
	mgr := &Manager{cacheDir: dir, logger: testLogger(), httpClient: &http.Client{}}

	versionDir := filepath.Join(dir, "runner-missing")
	if err := os.MkdirAll(versionDir, 0o755); err != nil {
		t.Fatal(err)
	}

	got := mgr.verifyCached(versionDir, filepath.Join(versionDir, ".sha256"), "abc")
	if got {
		t.Fatal("verifyCached() should return false when run script is missing")
	}
}

func TestVerifyCached_MissingChecksumFile(t *testing.T) {
	dir := t.TempDir()
	mgr := &Manager{cacheDir: dir, logger: testLogger(), httpClient: &http.Client{}}

	versionDir := filepath.Join(dir, "runner-nosum")
	if err := os.MkdirAll(versionDir, 0o755); err != nil {
		t.Fatal(err)
	}

	runScript := "run.sh"
	if runtime.GOOS == "windows" {
		runScript = "run.cmd"
	}
	if err := os.WriteFile(filepath.Join(versionDir, runScript), []byte("#!/bin/bash"), 0o755); err != nil {
		t.Fatal(err)
	}

	// When expected checksum is non-empty but file doesn't exist, should return false
	got := mgr.verifyCached(versionDir, filepath.Join(versionDir, ".sha256"), "abc")
	if got {
		t.Fatal("verifyCached() should return false when checksum file is missing")
	}
}

func TestPinnedReleaseMatchesHostPlatform(t *testing.T) {
	mgr := &Manager{cacheDir: t.TempDir(), logger: testLogger(), httpClient: &http.Client{}}
	rel, err := mgr.pinnedRelease()
	if runtime.GOOS == "windows" {
		if err == nil {
			t.Fatal("pinnedRelease() succeeded on windows, which has no tar.gz runner")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	pin, _ := PinnedRunner()
	platform, _ := Platform(runtime.GOOS, runtime.GOARCH)
	url, sum, _ := pin.Asset(platform)
	if rel.version != pin.Version || rel.url != url || rel.checksum != sum {
		t.Fatalf("pinnedRelease() = %+v, want version %s url %s checksum %s", rel, pin.Version, url, sum)
	}
}

func TestDownloadRefusesReleaseWithoutPinnedChecksum(t *testing.T) {
	requested := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requested = true }))
	defer server.Close()
	dir := t.TempDir()
	mgr := &Manager{cacheDir: dir, logger: testLogger(), httpClient: &http.Client{}}
	err := mgr.download(context.Background(), &runnerRelease{version: "1.0.0", url: server.URL}, filepath.Join(dir, "runner"), filepath.Join(dir, "runner", ".sha256"))
	if err == nil || !strings.Contains(err.Error(), "pinned checksum") {
		t.Fatalf("download() error = %v, want pinned checksum error", err)
	}
	if requested {
		t.Fatal("download() fetched a runner it could not verify")
	}
}

func TestProgressReader(t *testing.T) {
	data := []byte("hello world, this is test data for the progress reader")
	reader := bytes.NewReader(data)

	pr := &progressReader{
		reader:    reader,
		total:     int64(len(data)),
		logger:    testLogger(),
		interval:  time.Hour, // Don't trigger periodic logging during test
		lastPrint: time.Now(),
	}

	buf := make([]byte, 10)
	totalRead := 0
	for {
		n, err := pr.Read(buf)
		totalRead += n
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("unexpected read error: %v", err)
		}
	}

	if totalRead != len(data) {
		t.Fatalf("total read = %d, want %d", totalRead, len(data))
	}
	if pr.read != int64(len(data)) {
		t.Fatalf("pr.read = %d, want %d", pr.read, len(data))
	}
}

func TestProgressReader_TriggersLog(t *testing.T) {
	data := []byte("test data for logging")
	reader := bytes.NewReader(data)

	pr := &progressReader{
		reader:    reader,
		total:     int64(len(data)),
		logger:    testLogger(),
		interval:  0, // Always trigger logging
		lastPrint: time.Time{},
	}

	buf := make([]byte, len(data))
	_, err := pr.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("unexpected error: %v", err)
	}

	// Just verify it doesn't panic. The log goes to Discard.
	pr.logFinal()
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{1572864, "1.5 MB"},
		{1073741824, "1.0 GB"},
		{1610612736, "1.5 GB"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := formatBytes(tt.bytes)
			if got != tt.want {
				t.Fatalf("formatBytes(%d) = %q, want %q", tt.bytes, got, tt.want)
			}
		})
	}
}

func TestDownload_ChecksumMismatch(t *testing.T) {
	// Serve a tarball (just some bytes — we expect checksum mismatch before extraction)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "11")
		_, _ = w.Write([]byte("hello world"))
	}))
	defer server.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "runner")
	checksumFile := filepath.Join(dest, ".sha256")

	mgr := &Manager{cacheDir: dir, logger: testLogger(), httpClient: &http.Client{}}

	rel := &runnerRelease{
		version:  "1.0.0",
		url:      server.URL,
		checksum: "definitely-wrong-checksum",
	}

	err := mgr.download(context.Background(), rel, dest, checksumFile)
	if err == nil {
		t.Fatal("expected checksum mismatch error")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch error, got: %v", err)
	}
}

func TestDownload_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "runner")
	checksumFile := filepath.Join(dest, ".sha256")

	mgr := &Manager{cacheDir: dir, logger: testLogger(), httpClient: &http.Client{}}

	rel := &runnerRelease{
		version:  "1.0.0",
		url:      server.URL,
		checksum: strings.Repeat("0", 64),
	}

	err := mgr.download(context.Background(), rel, dest, checksumFile)
	if err == nil {
		t.Fatal("expected error for server error")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("expected status code in error, got: %v", err)
	}
}
