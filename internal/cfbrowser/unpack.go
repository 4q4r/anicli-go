package cfbrowser

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxLocateDepth bounds locateExecutable's walk: the deepest shipped
// layout is darwin's Chromium.app/Contents/MacOS/Chromium (3 levels
// below the chromium-<version> root).
const maxLocateDepth = 4

// zipExecNames are asset files that must land executable when a zip
// (windows asset) carries no unix modes.
var zipExecNames = map[string]bool{
	"chrome": true, "chrome.exe": true,
	"chromedriver": true, "chromedriver.exe": true,
	"Chromium": true, "chrome_crashpad_handler": true,
	"chrome_crashpad_handler.exe": true, "headless_shell": true,
}

// unpackArchive extracts an archive of kind into dest, rejecting path
// traversal (entries escaping dest fail the whole unpack loudly).
// Tar entries keep their unix permission bits; zip entries get the
// exec heuristic (known browser executables or extension-less files).
func unpackArchive(kind archiveKind, r io.Reader, dest string) error {
	switch kind {
	case archiveTarGz:
		return untar(r, dest)
	case archiveZip:
		return unzip(r, dest)
	default:
		return fmt.Errorf("cfbrowser: unknown archive kind %q", kind)
	}
}

// safeJoin resolves name inside dest, failing on absolute paths and
// traversal outside dest.
func safeJoin(dest, name string) (string, error) {
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("cfbrowser: archive entry %q: absolute path", name)
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("cfbrowser: archive entry %q: escapes destination", name)
	}
	return filepath.Join(dest, clean), nil
}

// untar extracts a gzipped tar stream into dest.
func untar(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("cfbrowser: open gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("cfbrowser: read tar entry: %w", err)
		}
		target, err := safeJoin(dest, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)&0o777); err != nil {
				return fmt.Errorf("cfbrowser: mkdir %s: %w", hdr.Name, err)
			}
		case tar.TypeReg:
			mode := os.FileMode(hdr.Mode) & 0o777
			if mode == 0 {
				mode = 0o644
			}
			if err := writeFile(tr, target, mode, hdr.Size); err != nil {
				return fmt.Errorf("cfbrowser: extract %s: %w", hdr.Name, err)
			}
		case tar.TypeSymlink:
			// Chromium archives ship no symlinks; refuse rather than
			// guess link targets inside the cache.
			return fmt.Errorf("cfbrowser: tar entry %q: symlink not supported", hdr.Name)
		default:
			return fmt.Errorf("cfbrowser: tar entry %q: unsupported type %q", hdr.Name, string(hdr.Typeflag))
		}
	}
}

// unzip extracts a zip stream into dest.
func unzip(r io.Reader, dest string) error {
	// archive/zip needs ReaderAt + size: buffer the stream (install
	// already buffered the verified archive on disk; callers pass a
	// bytes reader in tests).
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("cfbrowser: read zip: %w", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("cfbrowser: open zip: %w", err)
	}
	for _, f := range zr.File {
		target, err := safeJoin(dest, f.Name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o750); err != nil {
				return fmt.Errorf("cfbrowser: mkdir %s: %w", f.Name, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return fmt.Errorf("cfbrowser: mkdir for %s: %w", f.Name, err)
		}
		src, err := f.Open()
		if err != nil {
			return fmt.Errorf("cfbrowser: open zip entry %s: %w", f.Name, err)
		}
		mode := zipMode(f.Name)
		err = writeFile(src, target, mode, int64(f.UncompressedSize64))
		_ = src.Close()
		if err != nil {
			return fmt.Errorf("cfbrowser: extract %s: %w", f.Name, err)
		}
	}
	return nil
}

// zipMode picks unix permission bits for a zip entry: known browser
// executables and extension-less files run (0o755), everything else
// stays 0o644.
func zipMode(name string) os.FileMode {
	base := filepath.Base(name)
	if zipExecNames[base] || !strings.Contains(base, ".") {
		return 0o755
	}
	return 0o644
}

// writeFile streams src into path with mode, truncating any prior
// content; size is the announced length used only for the io.Copy
// short-read guard.
func writeFile(src io.Reader, path string, mode os.FileMode, size int64) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	n, err := io.Copy(f, src)
	if err != nil {
		return err
	}
	if size > 0 && n != size {
		return fmt.Errorf("short write: %d of %d bytes", n, size)
	}
	return nil
}

// locateExecutable finds execName (slash-separated relative path)
// under root within maxLocateDepth levels and returns its absolute
// path. Fails with a typed error when absent.
func locateExecutable(root, execName string) (string, error) {
	want := filepath.FromSlash(execName)
	var found string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if depth(root, path) >= maxLocateDepth {
			return filepath.SkipDir
		}
		candidate := filepath.Join(path, want)
		if fi, statErr := os.Stat(candidate); statErr == nil && fi.Mode().IsRegular() {
			found = candidate
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil && found == "" {
		return "", fmt.Errorf("cfbrowser: locate %s under %s: %w", execName, root, err)
	}
	if found == "" {
		return "", fmt.Errorf("cfbrowser: %s not found under %s (incomplete or foreign cache layout)", execName, root)
	}
	return found, nil
}

// depth counts directory levels of path below root (root itself = 0).
func depth(root, path string) int {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return maxLocateDepth + 1
	}
	if rel == "." {
		return 0
	}
	return strings.Count(rel, string(filepath.Separator)) + 1
}
