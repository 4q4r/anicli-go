package cfbrowser

import (
	"path/filepath"
	"testing"
)

func TestVersionMajorFailClosed(t *testing.T) {
	// The verdict store keys on the major; unparsable input must fail
	// closed (no verdict can ever be consulted for garbage).
	cases := []struct {
		version string
		want    int
		ok      bool
	}{
		{"146.0.7680.177.5", 146, true},
		{"146", 146, true},
		{"151.0.7922.108.6", 151, true},
		{"152.0.0.0.1", 152, true},
		{"", 0, false},         // unparsable fails closed
		{"banana.1", 0, false}, // unparsable fails closed
	}
	for _, tc := range cases {
		major, ok := versionMajor(tc.version)
		if ok != tc.ok || (ok && major != tc.want) {
			t.Errorf("versionMajor(%q) = (%d, %v), want (%d, %v)", tc.version, major, ok, tc.want, tc.ok)
		}
	}
}

func TestNormalizeChannel(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", channelAuto, true}, // zero value = auto (back-compat)
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

func TestFreeLineCandidatesSkipProMarkedDirs(t *testing.T) {
	cache := t.TempDir()
	spec := linuxSpec(t)
	// pro-marked 151: the free candidate list never includes it
	// (channel semantics by marker — verdicts are a separate axis).
	proDir := filepath.Dir(fakeInstalledBinary(t, cache, "151.0.7922.108.6"))
	markProBinary(t, proDir)
	want := fakeInstalledBinary(t, cache, "146.0.7680.177.5")

	cands := freeLineCandidates(cache, spec)
	if len(cands) != 1 {
		t.Fatalf("freeLineCandidates = %+v, want exactly the free dir", cands)
	}
	if cands[0].Path != want {
		t.Errorf("candidate = %q, want free %q (pro-marked dirs are excluded)", cands[0].Path, want)
	}
	if cands[0].Channel != channelFree || cands[0].Version != "146.0.7680.177.5" {
		t.Errorf("candidate = %+v, want free 146.0.7680.177.5", cands[0])
	}

	// The pro list and the ordered scan see the same dirs their own
	// way: pro-marked only / everything, both newest first.
	if pro := proLineCandidates(cache, spec); len(pro) != 1 || pro[0].Version != "151.0.7922.108.6" {
		t.Errorf("proLineCandidates = %+v, want the pro-marked 151", pro)
	}
	if all := scanCacheOrdered(cache, spec); len(all) != 2 || all[0].Version != "151.0.7922.108.6" {
		t.Errorf("scanCacheOrdered = %+v, want both dirs newest first", all)
	}
}
