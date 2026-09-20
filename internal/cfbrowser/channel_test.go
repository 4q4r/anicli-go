package cfbrowser

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// markProBinary stamps an installed fake dir as a pro-line download
// (the .channel marker written by real installs).
func markProBinary(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, channelMarkerFile), []byte(channelPro), 0o644); err != nil { //nolint:gosec // test-owned temp path
		t.Fatal(err)
	}
}

func TestDirChannelMarkerSemantics(t *testing.T) {
	dir := t.TempDir()
	if got := dirChannel(dir); got != channelFree {
		t.Errorf("unmarked dir = %q, want free (legacy compatibility)", got)
	}
	markProBinary(t, dir)
	if got := dirChannel(dir); got != channelPro {
		t.Errorf("pro-marked dir = %q, want pro", got)
	}
	if err := os.WriteFile(filepath.Join(dir, channelMarkerFile), []byte("  pro\n"), 0o644); err != nil { //nolint:gosec // test-owned temp path
		t.Fatal(err)
	}
	if got := dirChannel(dir); got != channelPro {
		t.Errorf("whitespace-padded pro marker = %q, want pro", got)
	}
	if err := os.WriteFile(filepath.Join(dir, channelMarkerFile), []byte("banana"), 0o644); err != nil { //nolint:gosec // test-owned temp path
		t.Fatal(err)
	}
	if got := dirChannel(dir); got != channelFree {
		t.Errorf("unknown marker value = %q, want free", got)
	}
}

func TestScanCacheVersionProRequiresProMarker(t *testing.T) {
	cache := t.TempDir()
	spec := linuxSpec(t)
	freeDir := filepath.Dir(fakeInstalledBinary(t, cache, "151.0.7922.108.6"))

	if bin, ok := scanCacheVersionPro(cache, spec, "151.0.7922.108.6"); ok {
		t.Fatalf("unmarked dir must not satisfy the pro-resolved activation rule: %+v", bin)
	}
	markProBinary(t, freeDir)
	bin, ok := scanCacheVersionPro(cache, spec, "151.0.7922.108.6")
	if !ok {
		t.Fatal("pro-marked exact version must hit")
	}
	if bin.Channel != channelPro || bin.Version != "151.0.7922.108.6" {
		t.Errorf("bin = %+v, want pro 151.0.7922.108.6", bin)
	}
	if _, ok := scanCacheVersionPro(cache, spec, "146.0.7680.177.5"); ok {
		t.Error("a version that is not installed must miss regardless of markers")
	}
}

func TestScanProCachePicksNewestProDir(t *testing.T) {
	cache := t.TempDir()
	spec := linuxSpec(t)
	fakeInstalledBinary(t, cache, "146.0.7680.177.5")                    // free, older
	proDir := filepath.Dir(fakeInstalledBinary(t, cache, "150.0.0.0.1")) // pro
	markProBinary(t, proDir)
	fakeInstalledBinary(t, cache, "152.0.0.0.1") // free, newest

	bin, ok := scanProCache(cache, spec)
	if !ok {
		t.Fatal("a pro-marked dir exists: scanProCache must resolve it")
	}
	if bin.Version != "150.0.0.0.1" || bin.Channel != channelPro {
		t.Errorf("bin = %+v, want pro 150.0.0.0.1 (newest PRO dir, not the newest dir)", bin)
	}
	if filepath.Dir(bin.Path) != proDir {
		t.Errorf("path = %q, want %q", bin.Path, proDir)
	}
}

// TestInstallWritesChannelMarker pins the free-flow half of the
// marker round-trip (the pro half is pinned by the pro install
// tests): every install records its line inside the versioned dir.
func TestInstallWritesChannelMarker(t *testing.T) {
	probeAlways(t)
	archive := buildTarGz(t, map[string]struct {
		mode os.FileMode
		data string
	}{
		"chromium-146.0.7680.177.5/chrome": {0o755, "ELF"},
	})
	fx := newInstallFixture(t, archive, "")

	cache := t.TempDir()
	info, err := Install(context.Background(), InstallOptions{
		CacheDir: cache,
		APIBase:  fx.apiURL,
		Platform: linuxSpec(t),
		Logger:   testLogger(t),
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if got := dirChannel(info.Dir); got != channelFree {
		t.Errorf("free install marker = %q, want free", got)
	}
}
