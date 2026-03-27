package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// createTarGz builds a tar.gz archive in memory from the given entries and
// writes it to disk at the returned path.
func createTarGz(t *testing.T, dir string, entries []tarEntry) string {
	t.Helper()

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     e.mode,
			Typeflag: e.typeflag,
		}
		if e.typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if e.typeflag == tar.TypeSymlink {
			hdr.Linkname = e.linkname
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %q: %v", e.name, err)
		}
		if e.typeflag == tar.TypeReg && len(e.body) > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("write body %q: %v", e.name, err)
			}
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}

	archivePath := filepath.Join(dir, "test.tar.gz")
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	return archivePath
}

type tarEntry struct {
	name     string
	mode     int64
	typeflag byte
	body     string
	linkname string
}

func TestExtractTarGz_BasicFiles(t *testing.T) {
	tmpDir := t.TempDir()
	destDir := filepath.Join(tmpDir, "out")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatal(err)
	}

	archive := createTarGz(t, tmpDir, []tarEntry{
		{name: "run.sh", mode: 0o755, typeflag: tar.TypeReg, body: "#!/bin/bash\necho hello"},
		{name: "bin/", mode: 0o755, typeflag: tar.TypeDir},
		{name: "bin/runner", mode: 0o755, typeflag: tar.TypeReg, body: "binary-data"},
		{name: "config.txt", mode: 0o644, typeflag: tar.TypeReg, body: "key=value"},
	})

	if err := extractTarGz(archive, destDir); err != nil {
		t.Fatalf("extractTarGz() error: %v", err)
	}

	// Verify files exist with correct contents
	tests := []struct {
		path string
		body string
	}{
		{filepath.Join(destDir, "run.sh"), "#!/bin/bash\necho hello"},
		{filepath.Join(destDir, "bin", "runner"), "binary-data"},
		{filepath.Join(destDir, "config.txt"), "key=value"},
	}
	for _, tt := range tests {
		data, err := os.ReadFile(tt.path)
		if err != nil {
			t.Fatalf("reading %s: %v", tt.path, err)
		}
		if string(data) != tt.body {
			t.Fatalf("%s: got %q, want %q", tt.path, data, tt.body)
		}
	}
}

func TestExtractTarGz_Symlink(t *testing.T) {
	tmpDir := t.TempDir()
	destDir := filepath.Join(tmpDir, "out")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatal(err)
	}

	archive := createTarGz(t, tmpDir, []tarEntry{
		{name: "target.txt", mode: 0o644, typeflag: tar.TypeReg, body: "hello"},
		{name: "link.txt", mode: 0o644, typeflag: tar.TypeSymlink, linkname: "target.txt"},
	})

	if err := extractTarGz(archive, destDir); err != nil {
		t.Fatalf("extractTarGz() error: %v", err)
	}

	linkTarget, err := os.Readlink(filepath.Join(destDir, "link.txt"))
	if err != nil {
		t.Fatalf("reading symlink: %v", err)
	}
	if linkTarget != "target.txt" {
		t.Fatalf("symlink target = %q, want %q", linkTarget, "target.txt")
	}
}

func TestExtractTarGz_ZipSlip(t *testing.T) {
	tmpDir := t.TempDir()
	destDir := filepath.Join(tmpDir, "out")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatal(err)
	}

	archive := createTarGz(t, tmpDir, []tarEntry{
		{name: "../../../etc/passwd", mode: 0o644, typeflag: tar.TypeReg, body: "evil"},
	})

	err := extractTarGz(archive, destDir)
	if err == nil {
		t.Fatal("expected error for zip-slip path traversal")
	}
	if !containsStr(err.Error(), "illegal file path") {
		t.Fatalf("expected 'illegal file path' error, got: %v", err)
	}

	// Verify the file was NOT written outside the dest
	if _, statErr := os.Stat(filepath.Join(tmpDir, "..", "..", "etc", "passwd")); statErr == nil {
		t.Fatal("zip-slip file should not have been created")
	}
}

func TestExtractTarGz_RootDirEntry(t *testing.T) {
	tmpDir := t.TempDir()
	destDir := filepath.Join(tmpDir, "out")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// An archive with a root dir entry "./" which should be skipped
	archive := createTarGz(t, tmpDir, []tarEntry{
		{name: "./", mode: 0o755, typeflag: tar.TypeDir},
		{name: "file.txt", mode: 0o644, typeflag: tar.TypeReg, body: "content"},
	})

	if err := extractTarGz(archive, destDir); err != nil {
		t.Fatalf("extractTarGz() error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(destDir, "file.txt"))
	if err != nil {
		t.Fatalf("reading file: %v", err)
	}
	if string(data) != "content" {
		t.Fatalf("got %q, want %q", data, "content")
	}
}

func TestExtractTarGz_EmptyArchive(t *testing.T) {
	tmpDir := t.TempDir()
	destDir := filepath.Join(tmpDir, "out")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatal(err)
	}

	archive := createTarGz(t, tmpDir, nil)

	if err := extractTarGz(archive, destDir); err != nil {
		t.Fatalf("extractTarGz() error: %v", err)
	}
}

func TestExtractTarGz_InvalidPath(t *testing.T) {
	tmpDir := t.TempDir()
	destDir := filepath.Join(tmpDir, "out")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatal(err)
	}

	err := extractTarGz("/nonexistent/archive.tar.gz", destDir)
	if err == nil {
		t.Fatal("expected error for nonexistent archive")
	}
}

func TestExtractTarGz_InvalidGzip(t *testing.T) {
	tmpDir := t.TempDir()
	destDir := filepath.Join(tmpDir, "out")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatal(err)
	}

	badFile := filepath.Join(tmpDir, "bad.tar.gz")
	if err := os.WriteFile(badFile, []byte("not gzip data"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := extractTarGz(badFile, destDir)
	if err == nil {
		t.Fatal("expected error for invalid gzip")
	}
}

func TestExtractTarGz_NestedDirectories(t *testing.T) {
	tmpDir := t.TempDir()
	destDir := filepath.Join(tmpDir, "out")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatal(err)
	}

	archive := createTarGz(t, tmpDir, []tarEntry{
		// File in a deeply nested dir without explicit dir entries
		{name: "a/b/c/deep.txt", mode: 0o644, typeflag: tar.TypeReg, body: "deep"},
	})

	if err := extractTarGz(archive, destDir); err != nil {
		t.Fatalf("extractTarGz() error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(destDir, "a", "b", "c", "deep.txt"))
	if err != nil {
		t.Fatalf("reading deep file: %v", err)
	}
	if string(data) != "deep" {
		t.Fatalf("got %q, want %q", data, "deep")
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
