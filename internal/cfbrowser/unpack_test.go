package cfbrowser

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// buildTarGz packs files (name -> mode:content) into an in-memory
// tar.gz, mimicking the CloakBrowser linux asset layout.
func buildTarGz(t *testing.T, files map[string]struct {
	mode os.FileMode
	data string
}) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, f := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: int64(f.mode),
			Size: int64(len(f.data)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %s: %v", name, err)
		}
		if _, err := tw.Write([]byte(f.data)); err != nil {
			t.Fatalf("tar write %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// buildZip packs files into an in-memory zip (windows asset layout).
func buildZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(data)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestUnpackTarGzRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix exec-bit assertions")
	}
	archive := buildTarGz(t, map[string]struct {
		mode os.FileMode
		data string
	}{
		"chromium-1.2.3/chrome":            {0o755, "ELF..."},
		"chromium-1.2.3/chromedriver":      {0o755, "ELF..2"},
		"chromium-1.2.3/resources.pak":     {0o644, "pak-bytes"},
		"chromium-1.2.3/locales/en-US.pak": {0o644, "locale"},
	})

	dest := t.TempDir()
	if err := unpackArchive(archiveTarGz, bytes.NewReader(archive), dest); err != nil {
		t.Fatalf("unpack: %v", err)
	}
	for name, want := range map[string]string{
		"chromium-1.2.3/chrome":            "ELF...",
		"chromium-1.2.3/chromedriver":      "ELF..2",
		"chromium-1.2.3/resources.pak":     "pak-bytes",
		"chromium-1.2.3/locales/en-US.pak": "locale",
	} {
		data, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(data) != want {
			t.Errorf("%s content mismatch", name)
		}
	}
	fi, err := os.Stat(filepath.Join(dest, "chromium-1.2.3", "chrome"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&0o111 == 0 {
		t.Errorf("chrome must keep the exec bit, got %v", fi.Mode())
	}
}

func TestUnpackZipRoundTrip(t *testing.T) {
	archive := buildZip(t, map[string]string{
		"chromium-1.2.3/chrome.exe":        "MZ...",
		"chromium-1.2.3/resources.pak":     "pak-bytes",
		"chromium-1.2.3/locales/en-US.pak": "locale",
	})
	dest := t.TempDir()
	if err := unpackArchive(archiveZip, bytes.NewReader(archive), dest); err != nil {
		t.Fatalf("unpack: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dest, "chromium-1.2.3", "chrome.exe"))
	if err != nil {
		t.Fatalf("read chrome.exe: %v", err)
	}
	if string(data) != "MZ..." {
		t.Errorf("chrome.exe content mismatch")
	}
	// Zip carries no unix modes: the exec heuristic must have marked
	// the known browser executable runnable.
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(dest, "chromium-1.2.3", "chrome.exe"))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode()&0o111 == 0 {
			t.Errorf("chrome.exe must be marked executable by the zip heuristic, got %v", fi.Mode())
		}
	}
}

func TestUnpackRejectsPathTraversal(t *testing.T) {
	evil := buildTarGz(t, map[string]struct {
		mode os.FileMode
		data string
	}{
		"../../../etc/cfoops": {0o644, "nope"},
	})
	dest := t.TempDir()
	if err := unpackArchive(archiveTarGz, bytes.NewReader(evil), dest); err == nil {
		t.Fatal("expected traversal rejection")
	}
	if _, err := os.Stat(filepath.Join(dest, "..", "..", "..", "etc", "cfoops")); !os.IsNotExist(err) {
		t.Errorf("traversal file must not exist outside dest")
	}
	// Absolute paths must be rejected too.
	abs := buildTarGz(t, map[string]struct {
		mode os.FileMode
		data string
	}{
		"/etc/cfoops": {0o644, "nope"},
	})
	if err := unpackArchive(archiveTarGz, bytes.NewReader(abs), t.TempDir()); err == nil {
		t.Fatal("expected absolute-path rejection")
	}
}

func TestLocateExecutable(t *testing.T) {
	root := t.TempDir()
	// Flat linux layout: chromium-9.9.9/chrome.
	flat := filepath.Join(root, "chromium-9.9.9")
	if err := os.MkdirAll(flat, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(flat, "chrome"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := locateExecutable(root, "chrome")
	if err != nil {
		t.Fatalf("flat: %v", err)
	}
	if got != filepath.Join(flat, "chrome") {
		t.Errorf("flat: got %q", got)
	}

	// Darwin nested .app layout.
	appRoot := t.TempDir()
	nested := filepath.Join(appRoot, "chromium-9.9.9", "Chromium.app", "Contents", "MacOS")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "Chromium"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err = locateExecutable(appRoot, "Chromium.app/Contents/MacOS/Chromium")
	if err != nil {
		t.Fatalf("nested: %v", err)
	}
	if got != filepath.Join(nested, "Chromium") {
		t.Errorf("nested: got %q", got)
	}

	// Nothing found -> typed error.
	if _, err := locateExecutable(t.TempDir(), "chrome"); err == nil {
		t.Fatal("expected error when executable absent")
	}
}
