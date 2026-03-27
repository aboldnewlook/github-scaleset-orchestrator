package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	mgr := &Manager{cacheDir: dir, logger: testLogger()}

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
				os.Remove(checksumFile)
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
	mgr := &Manager{cacheDir: dir, logger: testLogger()}

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
	mgr := &Manager{cacheDir: dir, logger: testLogger()}

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

func TestLatestRelease(t *testing.T) {
	osName := runtime.GOOS
	if osName == "darwin" {
		osName = "osx"
	}
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "x64"
	}
	suffix := fmt.Sprintf("%s-%s", osName, arch)

	tarballName := fmt.Sprintf("actions-runner-%s-2.320.0.tar.gz", suffix)
	checksumName := fmt.Sprintf("actions-runner-%s-2.320.0.tar.gz.sha256", suffix)

	release := ghRelease{
		TagName: "v2.320.0",
		Assets: []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		}{
			{Name: tarballName, BrowserDownloadURL: "TARBALL_URL"},
			{Name: checksumName, BrowserDownloadURL: "CHECKSUM_URL"},
			{Name: "actions-runner-win-x64-2.320.0.zip", BrowserDownloadURL: "OTHER_URL"},
		},
	}

	// Mock the release endpoint
	releaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(release)
	}))
	defer releaseServer.Close()

	// Mock the checksum endpoint
	checksumServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "abcdef123456  %s\n", tarballName)
	}))
	defer checksumServer.Close()

	// Update URLs to point to our test servers
	release.Assets[0].BrowserDownloadURL = releaseServer.URL + "/tarball"
	release.Assets[1].BrowserDownloadURL = checksumServer.URL + "/checksum"

	// Re-create the release server with updated URLs
	releaseServer.Close()
	releaseServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(release)
	}))
	defer releaseServer.Close()

	// We need to override the URL used by latestRelease. Since it's hardcoded,
	// we test the parsing logic indirectly by mocking an HTTP server and
	// calling the function with a modified transport.
	origTransport := http.DefaultTransport
	http.DefaultTransport = &urlRewriter{
		inner: origTransport,
		rewrites: map[string]string{
			"https://api.github.com/repos/actions/runner/releases/latest": releaseServer.URL,
			"CHECKSUM_URL": checksumServer.URL + "/checksum",
		},
		prefix: checksumServer.URL,
	}
	defer func() { http.DefaultTransport = origTransport }()

	dir := t.TempDir()
	mgr := &Manager{cacheDir: dir, logger: testLogger()}

	rel, err := mgr.latestRelease(context.Background())
	if err != nil {
		t.Fatalf("latestRelease() error: %v", err)
	}

	if rel.version != "2.320.0" {
		t.Fatalf("version = %q, want %q", rel.version, "2.320.0")
	}
	if rel.url == "" {
		t.Fatal("expected non-empty tarball URL")
	}
}

// urlRewriter is a test RoundTripper that redirects specific URLs to test servers.
type urlRewriter struct {
	inner    http.RoundTripper
	rewrites map[string]string
	prefix   string // prefix for checksum URL rewriting
}

func (u *urlRewriter) RoundTrip(req *http.Request) (*http.Response, error) {
	url := req.URL.String()
	if newURL, ok := u.rewrites[url]; ok {
		newReq := req.Clone(req.Context())
		parsed, _ := req.URL.Parse(newURL)
		newReq.URL = parsed
		return u.inner.RoundTrip(newReq)
	}
	return u.inner.RoundTrip(req)
}

func TestLatestRelease_NoMatchingAsset(t *testing.T) {
	release := ghRelease{
		TagName: "v2.320.0",
		Assets: []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		}{
			{Name: "actions-runner-win-x64-2.320.0.zip", BrowserDownloadURL: "http://example.com"},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(release)
	}))
	defer server.Close()

	origTransport := http.DefaultTransport
	http.DefaultTransport = &urlRewriter{
		inner: origTransport,
		rewrites: map[string]string{
			"https://api.github.com/repos/actions/runner/releases/latest": server.URL,
		},
	}
	defer func() { http.DefaultTransport = origTransport }()

	dir := t.TempDir()
	mgr := &Manager{cacheDir: dir, logger: testLogger()}

	// On non-Windows with non-x64, there might not be a matching asset.
	// This test verifies the error path when no matching tarball is found.
	_, err := mgr.latestRelease(context.Background())

	// We may or may not get an error depending on the current OS/arch matching "win-x64"
	osName := runtime.GOOS
	if osName == "darwin" {
		osName = "osx"
	}
	if osName == "windows" && runtime.GOARCH == "amd64" {
		// The zip file won't match because it's .zip not .tar.gz
		if err == nil {
			t.Fatal("expected error when only .zip available")
		}
	} else {
		if err == nil {
			t.Fatal("expected error when no matching asset")
		}
	}
	if err != nil && !strings.Contains(err.Error(), "no runner binary found") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLatestRelease_BadJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{bad json"))
	}))
	defer server.Close()

	origTransport := http.DefaultTransport
	http.DefaultTransport = &urlRewriter{
		inner: origTransport,
		rewrites: map[string]string{
			"https://api.github.com/repos/actions/runner/releases/latest": server.URL,
		},
	}
	defer func() { http.DefaultTransport = origTransport }()

	dir := t.TempDir()
	mgr := &Manager{cacheDir: dir, logger: testLogger()}

	_, err := mgr.latestRelease(context.Background())
	if err == nil {
		t.Fatal("expected error for bad JSON")
	}
}

func TestLatestRelease_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	origTransport := http.DefaultTransport
	http.DefaultTransport = &urlRewriter{
		inner: origTransport,
		rewrites: map[string]string{
			"https://api.github.com/repos/actions/runner/releases/latest": server.URL,
		},
	}
	defer func() { http.DefaultTransport = origTransport }()

	dir := t.TempDir()
	mgr := &Manager{cacheDir: dir, logger: testLogger()}

	_, err := mgr.latestRelease(context.Background())
	if err == nil {
		t.Fatal("expected error for server error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected status code in error, got: %v", err)
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

func TestFetchChecksum(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		status   int
		wantHash string
		wantErr  bool
	}{
		{
			name:     "hash only",
			body:     "abcdef1234567890\n",
			status:   http.StatusOK,
			wantHash: "abcdef1234567890",
		},
		{
			name:     "hash with filename",
			body:     "abcdef1234567890  actions-runner-osx-arm64-2.320.0.tar.gz\n",
			status:   http.StatusOK,
			wantHash: "abcdef1234567890",
		},
		{
			name:    "server error",
			body:    "",
			status:  http.StatusNotFound,
			wantErr: true,
		},
		{
			name:    "empty body",
			body:    "",
			status:  http.StatusOK,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.body))
			}))
			defer server.Close()

			dir := t.TempDir()
			mgr := &Manager{cacheDir: dir, logger: testLogger()}

			hash, err := mgr.fetchChecksum(context.Background(), server.URL)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if hash != tt.wantHash {
				t.Fatalf("hash = %q, want %q", hash, tt.wantHash)
			}
		})
	}
}

func TestDownload_ChecksumMismatch(t *testing.T) {
	// Serve a tarball (just some bytes — we expect checksum mismatch before extraction)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "11")
		w.Write([]byte("hello world"))
	}))
	defer server.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "runner")
	checksumFile := filepath.Join(dest, ".sha256")

	mgr := &Manager{cacheDir: dir, logger: testLogger()}

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

	mgr := &Manager{cacheDir: dir, logger: testLogger()}

	rel := &runnerRelease{
		version: "1.0.0",
		url:     server.URL,
	}

	err := mgr.download(context.Background(), rel, dest, checksumFile)
	if err == nil {
		t.Fatal("expected error for server error")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("expected status code in error, got: %v", err)
	}
}
