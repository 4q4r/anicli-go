package cfbrowser

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaxKnownGoodChromiumMajorPinned(t *testing.T) {
	// PR73 verified live: chromium 146 drives the pinned chromedp
	// driver; 151 kills sessions on the first Navigate. The default
	// bound pins that fact.
	if maxKnownGoodChromiumMajor != 146 {
		t.Errorf("maxKnownGoodChromiumMajor = %d, want 146 (the PR73-verified bound)", maxKnownGoodChromiumMajor)
	}
}

func TestChromiumMajorKnownGood(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{"146.0.7680.177.5", true},
		{"146", true},
		{"145.9", true},
		{"147.0.0.0.1", false},
		{"151.0.7922.108.6", false},
		{"", false},        // unparsable fails closed
		{"banana.1", false}, // unparsable fails closed
	}
	for _, tc := range cases {
		if got := chromiumMajorKnownGood(tc.version); got != tc.want {
			t.Errorf("chromiumMajorKnownGood(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}
}

func TestCompatBoundIsTestOverridable(t *testing.T) {
	old := maxKnownGoodChromiumMajor
	t.Cleanup(func() { maxKnownGoodChromiumMajor = old })
	maxKnownGoodChromiumMajor = 151

	if !chromiumMajorKnownGood("151.0.7922.108.6") {
		t.Error("raised bound must admit 151")
	}
	if chromiumMajorKnownGood("152.0.0.0.1") {
		t.Error("152 must stay above the raised bound")
	}
}

func TestNormalizeChannel(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", channelAuto, true},   // zero value = auto (back-compat)
		{"auto", channelAuto, true},
		{"free", channelFree, true},
		{"pro", channelPro, true},
		{"banana", "", false},
		{"FREE", "", false}, // exact values only: a typo must fail loud
	}
	for _, tc := range cases {
		got, err := normalizeChannel(tc.in)
		if tc.ok {
			if err != nil {
				t.Errorf("normalizeChannel(%q): %v", tc.in, err)
				continue
			}
			if got != tc.want {
				t.Errorf("normalizeChannel(%q) = %q, want %q", tc.in, got, tc.want)
			}
			continue
		}
		if err == nil {
			t.Errorf("normalizeChannel(%q) = %q, want a loud error", tc.in, got)
		}
	}
}

func TestScanCacheFreeSkipsProMarkedAndIncompatibleDirs(t *testing.T) {
	cache := t.TempDir()
	spec := linuxSpec(t)
	// pro-marked 151: newest overall, but the free line never
	// satisfies a free scan (mirror of the pro-tier marker rule).
	proDir := filepath.Dir(fakeInstalledBinary(t, cache, "151.0.7922.108.6"))
	markProBinary(t, proDir)
	// free 147: incompatible with the pinned chromedp driver.
	fakeInstalledBinary(t, cache, "147.0.0.0.1")
	// free 146: compatible — the expected pick.
	want := fakeInstalledBinary(t, cache, "146.0.7680.177.5")

	bin, ok, err := scanCacheFree(cache, spec)
	if err != nil || !ok {
		t.Fatalf("scanCacheFree = (%v, %v)", ok, err)
	}
	if bin.Path != want {
		t.Errorf("path = %q, want compatible free %q (pro dirs and majors above the bound are skipped)", bin.Path, want)
	}
	if bin.Channel != channelFree || bin.Version != "146.0.7680.177.5" {
		t.Errorf("bin = %+v, want free 146.0.7680.177.5", bin)
	}
}

func TestScanCacheFreeOnlyIncompatibleIsLoudCompatError(t *testing.T) {
	cache := t.TempDir()
	spec := linuxSpec(t)
	fakeInstalledBinary(t, cache, "151.0.7922.108.6")
	fakeInstalledBinary(t, cache, "150.0.0.0.1")

	bin, ok, err := scanCacheFree(cache, spec)
	if ok || bin != nil {
		t.Fatalf("only incompatible dirs: must not resolve, got %+v", bin)
	}
	var compat *CompatError
	if !errors.As(err, &compat) {
		t.Fatalf("want *CompatError, got %T: %v", err, err)
	}
	if compat.Newest != "151.0.7922.108.6" || compat.Bound != maxKnownGoodChromiumMajor {
		t.Errorf("compat = %+v", compat)
	}
	// The error names both the rejected version and the fix.
	for _, want := range []string{"151.0.7922.108.6", "chromedp"} {
		if !strings.Contains(compat.Error(), want) {
			t.Errorf("compat error %q must mention %q", compat.Error(), want)
		}
	}
}

func TestScanCacheFreeEmptyCacheAndProOnlyCacheAreQuietMisses(t *testing.T) {
	spec := linuxSpec(t)

	// Empty cache: a quiet miss (the caller proceeds to download).
	if bin, ok, err := scanCacheFree(t.TempDir(), spec); ok || bin != nil || err != nil {
		t.Errorf("empty cache = (%+v, %v, %v), want a quiet miss", bin, ok, err)
	}

	// Pro-only cache: pro dirs are excluded by channel semantics, not
	// by the compat bound — a quiet miss, never a CompatError.
	cache := t.TempDir()
	proDir := filepath.Dir(fakeInstalledBinary(t, cache, "151.0.7922.108.6"))
	markProBinary(t, proDir)
	if bin, ok, err := scanCacheFree(cache, spec); ok || bin != nil || err != nil {
		t.Errorf("pro-only cache = (%+v, %v, %v), want a quiet miss", bin, ok, err)
	}
}
