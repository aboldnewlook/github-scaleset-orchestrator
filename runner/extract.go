package runner

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func extractTarGz(tarGzPath, dest string) error {
	f, err := os.Open(tarGzPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar: %w", err)
		}

		target := filepath.Join(dest, hdr.Name)
		cleanTarget := filepath.Clean(target)
		cleanDest := filepath.Clean(dest)

		// Guard against zip slip — allow the dest dir itself (e.g. "./")
		if cleanTarget != cleanDest && !strings.HasPrefix(cleanTarget, cleanDest+string(os.PathSeparator)) {
			return fmt.Errorf("illegal file path in archive: %s", hdr.Name)
		}

		// Skip the root directory entry
		if cleanTarget == cleanDest {
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			outFile, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(outFile, tr); err != nil {
				_ = outFile.Close()
				return err
			}
			if err := outFile.Close(); err != nil {
				return err
			}
		case tar.TypeLink:
			// Hardlink targets are relative to the archive root, so join with dest.
			linkTarget := filepath.Clean(filepath.Join(dest, hdr.Linkname))
			if linkTarget != cleanDest && !strings.HasPrefix(linkTarget, cleanDest+string(os.PathSeparator)) {
				return fmt.Errorf("illegal hardlink target in archive: %s -> %s (escapes destination)", hdr.Name, hdr.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Link(linkTarget, target); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// Guard against symlink escape — reject absolute targets
			if filepath.IsAbs(hdr.Linkname) {
				return fmt.Errorf("illegal symlink target in archive: %s -> %s (absolute path)", hdr.Name, hdr.Linkname)
			}
			// Resolve relative symlink target against the symlink's parent directory
			resolvedLink := filepath.Clean(filepath.Join(filepath.Dir(target), hdr.Linkname))
			if resolvedLink != cleanDest && !strings.HasPrefix(resolvedLink, cleanDest+string(os.PathSeparator)) {
				return fmt.Errorf("illegal symlink target in archive: %s -> %s (escapes destination)", hdr.Name, hdr.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		}
	}

	return nil
}
