package cfbrowser

import "testing"

func TestParseVersionFromTag(t *testing.T) {
	cases := []struct {
		tag     string
		version string
		wantErr bool
	}{
		{"chromium-v146.0.7680.177.5", "146.0.7680.177.5", false},
		{"chromium-v146.0.7680.177.4", "146.0.7680.177.4", false},
		{"chromium-v142.0.7444.175", "142.0.7444.175", false},
		{"chromium-v151.0.7922.108.6-pro", "151.0.7922.108.6", false},
		{"chromium-v", "", true},
		{"v146.0.1", "", true},
		{"", "", true},
	}
	for _, tc := range cases {
		got, err := ParseVersionFromTag(tc.tag)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("tag %q: expected error, got %q", tc.tag, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("tag %q: unexpected error: %v", tc.tag, err)
		}
		if got != tc.version {
			t.Errorf("tag %q: version = %q, want %q", tc.tag, got, tc.version)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"146.0.7680.177.5", "146.0.7680.177.4", 1},
		{"146.0.7680.177.4", "146.0.7680.177.5", -1},
		{"146.0.7680.177.5", "146.0.7680.177.5", 0},
		{"147.0.0.0", "146.9.9.9.9", 1},
		{"142.0.7444.175", "146.0.7680.177.1", -1},
		// Shorter equal-prefix versions sort earlier.
		{"146.0", "146.0.1", -1},
		{"146.0.1", "146.0", 1},
		{"146", "146.0", -1},
		// Numeric, not lexicographic: 10 > 9.
		{"10.0", "9.0", 1},
	}
	for _, tc := range cases {
		got := CompareVersions(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestVersionDirName(t *testing.T) {
	const v = "146.0.7680.177.5"
	if got := VersionDirName(v); got != "chromium-146.0.7680.177.5" {
		t.Errorf("VersionDirName = %q, want chromium-146.0.7680.177.5", got)
	}
}

func TestVersionFromDirName(t *testing.T) {
	if got, ok := VersionFromDirName("chromium-146.0.7680.177.5"); !ok || got != "146.0.7680.177.5" {
		t.Errorf("VersionFromDirName = %q,%v", got, ok)
	}
	if _, ok := VersionFromDirName("chromium-"); ok {
		t.Error("empty version must not parse")
	}
	if _, ok := VersionFromDirName("artifacts"); ok {
		t.Error("non-chromium dir must not parse")
	}
	if _, ok := VersionFromDirName("chromium-abc"); ok {
		t.Error("non-numeric version must not parse")
	}
}
