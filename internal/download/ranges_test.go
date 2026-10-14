package download

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// rangesRoundtripDoc is the canonical fixture: two ranges with both
// match kinds, a shikimori binding and an episode offset.
func rangesRoundtripDoc() RangesFile {
	return RangesFile{
		Version:   1,
		UpdatedAt: "2026-10-14T10:00:00Z",
		Ranges: []RangeBinding{
			{
				ID:             "s1",
				Title:          "Тайтл — S1",
				ShikimoriID:    39535,
				ShikimoriTitle: "Mushoku Tensei",
				Match:          RangeMatch{Kind: "glob", Patterns: []string{"EP_1_*", "EP_2_*"}},
				EpisodeOffset:  0,
			},
			{
				ID:            "s2p2",
				Title:         "Тайтл — S2 ч.2",
				ShikimoriID:   55888,
				Match:         RangeMatch{Kind: "exact", Files: []string{"EP_13_a.mkv", "EP_14_a.mkv"}},
				EpisodeOffset: 12,
			},
		},
	}
}

// TestLoadRangesAbsent: no ranges file reads as (nil, nil) — the
// backward-compat anchor (a folder without one behaves as today).
func TestLoadRangesAbsent(t *testing.T) {
	dir := t.TempDir()
	rf, err := LoadRanges(dir)
	if err != nil {
		t.Fatalf("LoadRanges(absent): %v", err)
	}
	if rf != nil {
		t.Fatalf("LoadRanges(absent) = %+v, want nil", rf)
	}
}

// TestLoadRangesRoundtrip: SaveRanges → LoadRanges returns an
// identical document (JSON round-trip pins the schema tags).
func TestLoadRangesRoundtrip(t *testing.T) {
	dir := t.TempDir()
	want := rangesRoundtripDoc()
	if err := SaveRanges(dir, &want); err != nil {
		t.Fatalf("SaveRanges: %v", err)
	}
	got, err := LoadRanges(dir)
	if err != nil {
		t.Fatalf("LoadRanges: %v", err)
	}
	if got == nil {
		t.Fatal("LoadRanges = nil, want the saved document")
	}
	if got.Version != 1 {
		t.Errorf("Version = %d, want 1", got.Version)
	}
	if _, err := time.Parse(time.RFC3339, got.UpdatedAt); err != nil {
		t.Errorf("UpdatedAt %q is not RFC3339: %v", got.UpdatedAt, err)
	}
	if !reflect.DeepEqual(got.Ranges, want.Ranges) {
		t.Errorf("ranges round-trip mismatch:\n got %+v\nwant %+v", got.Ranges, want.Ranges)
	}
}

// TestLoadRangesCorruptQuarantines: a malformed document is renamed
// to .corrupt-<ts> (169-E semantics) and reads as absent — never a
// faked success, never a deleted-without-backup state.
func TestLoadRangesCorruptQuarantines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, RangesFilename)
	broken := "{oops"
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatalf("write broken file: %v", err)
	}
	rf, err := LoadRanges(dir)
	if err != nil {
		t.Fatalf("LoadRanges(corrupt) err = %v, want nil (quarantine degrades)", err)
	}
	if rf != nil {
		t.Fatalf("LoadRanges(corrupt) = %+v, want nil", rf)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("canonical path still present after quarantine: %v", statErr)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("read dir: %v", readErr)
	}
	var quarantined bool
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), RangesFilename+".corrupt-") {
			quarantined = true
			//nolint:gosec // the test reads its own quarantine file
			data, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
			if rerr != nil {
				t.Fatalf("read quarantine: %v", rerr)
			}
			if string(data) != broken {
				t.Errorf("quarantine holds %q, want the broken bytes %q", data, broken)
			}
		}
	}
	if !quarantined {
		t.Fatal("no .corrupt-* quarantine file created")
	}
}

// TestLoadRangesUnsupportedVersion: a foreign version is a typed
// error and the file stays untouched (manual recovery stays
// possible; the folder degrades to today's plain title).
func TestLoadRangesUnsupportedVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, RangesFilename)
	doc := `{"version":99,"updated_at":"2026-10-14T10:00:00Z","ranges":[]}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	rf, err := LoadRanges(dir)
	if rf != nil {
		t.Fatalf("LoadRanges = %+v, want nil", rf)
	}
	if !errors.Is(err, ErrRangesUnsupportedVersion) {
		t.Fatalf("err = %v, want ErrRangesUnsupportedVersion", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("file was touched on version mismatch: %v", statErr)
	}
}

// TestSaveRangesValidation: the hard rejects of the saver and the
// one accept case (ShikimoriID 0 twice is NOT a duplicate).
func TestSaveRangesValidation(t *testing.T) {
	valid := func() []RangeBinding {
		return []RangeBinding{
			{ID: "s1", Title: "S1", Match: RangeMatch{Kind: "glob", Patterns: []string{"EP_*"}}},
			{ID: "ova", Title: "OVA", Match: RangeMatch{Kind: "exact", Files: []string{"OVA_1.mkv"}}},
		}
	}
	cases := []struct {
		name  string
		docs  []RangeBinding
		match string // substring of the expected error, "" = accepted
	}{
		{"valid", valid(), ""},
		{"zero shikimori id twice is not a duplicate", []RangeBinding{
			{ID: "a", Title: "A", ShikimoriID: 0, Match: RangeMatch{Kind: "glob", Patterns: []string{"A_*"}}},
			{ID: "b", Title: "B", ShikimoriID: 0, Match: RangeMatch{Kind: "glob", Patterns: []string{"B_*"}}},
		}, ""},
		{"duplicate id", func() []RangeBinding {
			d := valid()
			d[1].ID = "s1"
			return d
		}(), "id"},
		{"empty id", func() []RangeBinding {
			d := valid()
			d[1].ID = ""
			return d
		}(), "id"},
		{"invalid id characters", func() []RangeBinding {
			d := valid()
			d[1].ID = "bad id!"
			return d
		}(), "id"},
		{"duplicate title", func() []RangeBinding {
			d := valid()
			d[1].Title = "S1"
			return d
		}(), "title"},
		{"empty title", func() []RangeBinding {
			d := valid()
			d[1].Title = ""
			return d
		}(), "title"},
		{"unknown match kind", func() []RangeBinding {
			d := valid()
			d[1].Match = RangeMatch{Kind: "regex", Patterns: []string{"EP_.*"}}
			return d
		}(), "kind"},
		{"glob with zero patterns", []RangeBinding{
			{ID: "s1", Title: "S1", Match: RangeMatch{Kind: "glob"}},
		}, "pattern"},
		{"exact with zero files", []RangeBinding{
			{ID: "s1", Title: "S1", Match: RangeMatch{Kind: "exact"}},
		}, "file"},
		{"duplicate shikimori id", []RangeBinding{
			{ID: "a", Title: "A", ShikimoriID: 55888, Match: RangeMatch{Kind: "glob", Patterns: []string{"A_*"}}},
			{ID: "b", Title: "B", ShikimoriID: 55888, Match: RangeMatch{Kind: "glob", Patterns: []string{"B_*"}}},
		}, "shikimori"},
	}
	dir := t.TempDir()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := t.TempDir()
			err := SaveRanges(target, &RangesFile{Version: 1, Ranges: tc.docs})
			if tc.match == "" {
				if err != nil {
					t.Fatalf("SaveRanges(valid): %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("SaveRanges accepted an invalid document")
			}
			if !strings.Contains(err.Error(), tc.match) {
				t.Fatalf("err = %q, want it to mention %q", err, tc.match)
			}
			if _, statErr := os.Stat(filepath.Join(target, RangesFilename)); !os.IsNotExist(statErr) {
				t.Error("rejected document was written anyway")
			}
		})
	}
	_ = dir
}

// TestRangeOverlaps: files provably matched by more than one range
// (exact names matched by another range's glob or listed twice) map
// onto the sorted range ids; disjoint ranges produce an empty map.
func TestRangeOverlaps(t *testing.T) {
	glob := RangeBinding{ID: "g", Title: "G", Match: RangeMatch{Kind: "glob", Patterns: []string{"a*"}}}
	exact := RangeBinding{ID: "e", Title: "E", Match: RangeMatch{Kind: "exact", Files: []string{"a1.mkv"}}}
	overlaps := RangeOverlaps(&RangesFile{Version: 1, Ranges: []RangeBinding{glob, exact}})
	want := map[string][]string{"a1.mkv": {"e", "g"}}
	if !reflect.DeepEqual(overlaps, want) {
		t.Fatalf("overlaps = %v, want %v (ids sorted)", overlaps, want)
	}

	disjoint := &RangesFile{Version: 1, Ranges: []RangeBinding{
		{ID: "g", Title: "G", Match: RangeMatch{Kind: "glob", Patterns: []string{"b*"}}},
		{ID: "e", Title: "E", Match: RangeMatch{Kind: "exact", Files: []string{"a1.mkv"}}},
	}}
	if got := RangeOverlaps(disjoint); len(got) != 0 {
		t.Fatalf("disjoint overlaps = %v, want empty", got)
	}
}
