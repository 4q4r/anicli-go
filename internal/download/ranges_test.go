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

// rangesMediaDir creates a fixture directory with the named media
// files (content is irrelevant — expansion stat-checks only).
func rangesMediaDir(t *testing.T, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// TestScanMediaFiles: media-extension regular files only, sorted.
func TestScanMediaFiles(t *testing.T) {
	dir := rangesMediaDir(t, "EP_2.mkv", "EP_10.mp4", "cover.jpg", "notes.txt")
	if err := os.Mkdir(filepath.Join(dir, "subdir.mkv"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	got := ScanMediaFiles(dir)
	want := []string{"EP_10.mp4", "EP_2.mkv"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ScanMediaFiles = %v, want %v", got, want)
	}
}

// TestResolveFilesGlobOrder: pattern ORDER is episode order and beats
// byte order; a single pattern yields byte order (R7 pin). Files are
// suffixed so the per-episode patterns actually match them
// (filepath.Match: "EP_1_*" needs the literal "EP_1_" prefix).
func TestResolveFilesGlobOrder(t *testing.T) {
	dir := rangesMediaDir(t, "EP_1_a.mkv", "EP_2_a.mkv", "EP_10_a.mkv")
	got := resolveRangeFiles(dir, RangeMatch{Kind: "glob", Patterns: []string{"EP_1_*", "EP_2_*", "EP_10_*"}})
	want := []string{"EP_1_a.mkv", "EP_2_a.mkv", "EP_10_a.mkv"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pattern order = %v, want %v", got, want)
	}
	got = resolveRangeFiles(dir, RangeMatch{Kind: "glob", Patterns: []string{"EP_*"}})
	want = []string{"EP_10_a.mkv", "EP_1_a.mkv", "EP_2_a.mkv"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("byte order = %v, want %v (R7 caveat pin)", got, want)
	}
}

// TestResolveFilesGlobDedup: a file matched by an earlier pattern of
// the same range is not re-matched (first-wins).
func TestResolveFilesGlobDedup(t *testing.T) {
	dir := rangesMediaDir(t, "EP_1.mkv", "EP_2.mkv")
	got := resolveRangeFiles(dir, RangeMatch{Kind: "glob", Patterns: []string{"EP_*", "EP_1_*"}})
	want := []string{"EP_1.mkv", "EP_2.mkv"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dedup = %v, want %v", got, want)
	}
}

// TestResolveFilesExactOrderAndMissing: the listed order is the
// episode order; files missing on disk drop silently.
func TestResolveFilesExactOrderAndMissing(t *testing.T) {
	dir := rangesMediaDir(t, "b.mkv", "a.mkv")
	got := resolveRangeFiles(dir, RangeMatch{Kind: "exact", Files: []string{"b.mkv", "ghost.mkv", "a.mkv"}})
	want := []string{"b.mkv", "a.mkv"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("exact order = %v, want %v", got, want)
	}
}

// TestResolveFilesQualityFilter (owner amendment A1): a range with
// match.quality takes only the files matching the quality glob, so a
// multi-quality folder yields ONE position per episode; without the
// field the plan behavior (every matched file = one position) stays.
func TestResolveFilesQualityFilter(t *testing.T) {
	dir := rangesMediaDir(t, "EP_1_1080p.mkv", "EP_1_720p.mkv", "EP_2_1080p.mkv", "EP_2_720p.mkv")
	filtered := resolveRangeFiles(dir, RangeMatch{Kind: "glob", Patterns: []string{"EP_*"}, Quality: "1080p"})
	want := []string{"EP_1_1080p.mkv", "EP_2_1080p.mkv"}
	if !reflect.DeepEqual(filtered, want) {
		t.Fatalf("quality-filtered = %v, want %v (one position per episode)", filtered, want)
	}
	all := resolveRangeFiles(dir, RangeMatch{Kind: "glob", Patterns: []string{"EP_*"}})
	if len(all) != 4 {
		t.Fatalf("unfiltered = %v, want all 4 files (plan behavior)", all)
	}
}

// TestExpandRangesPositionsAndMetadata: positions carry rewritten
// labels "1".."N"; an indexed file keeps its stream metadata, a
// hand-placed file degrades to the honest one-file pseudo-entry.
func TestExpandRangesPositionsAndMetadata(t *testing.T) {
	dir := rangesMediaDir(t, "EP_1_Kodik_1080p.mkv", "OVA_1.mkv")
	index := Snapshot{
		TitleDir: dir,
		Entries: []Entry{
			{EpisodeNum: "1", VideoKey: "Kodik", AudioKey: "Dub", Quality: 1080, RelativePath: "EP_1_Kodik_1080p.mkv", Container: "mkv"},
		},
	}
	rf := &RangesFile{Version: 1, Ranges: []RangeBinding{
		{ID: "s1", Title: "S1", Match: RangeMatch{Kind: "glob", Patterns: []string{"EP_*"}}},
		{ID: "ova", Title: "OVA", Match: RangeMatch{Kind: "glob", Patterns: []string{"OVA_*"}}},
	}}
	views, unsorted := ExpandRanges(dir, index, rf)
	if len(unsorted) != 0 {
		t.Fatalf("unsorted = %v, want empty (fully matched)", unsorted)
	}
	if len(views) != 2 {
		t.Fatalf("views = %d, want 2", len(views))
	}
	s1 := views[0]
	if s1.Binding.ID != "s1" || len(s1.Entries) != 1 {
		t.Fatalf("s1 view = %+v", s1)
	}
	e := s1.Entries[0]
	if e.EpisodeNum != "1" {
		t.Errorf("s1 position label = %q, want \"1\"", e.EpisodeNum)
	}
	if e.VideoKey != "Kodik" || e.AudioKey != "Dub" || e.Quality != 1080 || e.Container != "mkv" {
		t.Errorf("indexed metadata lost: %+v", e)
	}
	if e.RelativePath != "EP_1_Kodik_1080p.mkv" {
		t.Errorf("RelativePath = %q, want the real path", e.RelativePath)
	}
	ova := views[1]
	if len(ova.Entries) != 1 {
		t.Fatalf("ova view = %+v", ova)
	}
	p := ova.Entries[0]
	if p.EpisodeNum != "1" || p.VideoKey != "file" || p.AudioKey != "—" || p.Quality != 0 || p.Container != "mkv" {
		t.Errorf("pseudo-entry = %+v, want {1, file, —, 0, mkv}", p)
	}
	if p.AnimeID != nil {
		t.Errorf("pseudo-entry AnimeID = %v, want nil", p.AnimeID)
	}
	if p.RelativePath != "OVA_1.mkv" {
		t.Errorf("pseudo RelativePath = %q, want the base name", p.RelativePath)
	}
}

// TestExpandRangesZeroFileRangeOmitted: a range matching nothing is
// not rendered (it stays in the file — a glob can come alive later).
func TestExpandRangesZeroFileRangeOmitted(t *testing.T) {
	dir := rangesMediaDir(t, "EP_1.mkv")
	rf := &RangesFile{Version: 1, Ranges: []RangeBinding{
		{ID: "s1", Title: "S1", Match: RangeMatch{Kind: "glob", Patterns: []string{"EP_*"}}},
		{ID: "ghost-glob", Title: "Ghost", Match: RangeMatch{Kind: "glob", Patterns: []string{"OVA_*"}}},
		{ID: "ghost-exact", Title: "Ghost2", Match: RangeMatch{Kind: "exact", Files: []string{"missing.mkv"}}},
	}}
	views, unsorted := ExpandRanges(dir, Snapshot{TitleDir: dir}, rf)
	if len(views) != 1 || views[0].Binding.ID != "s1" {
		t.Fatalf("views = %+v, want only s1", views)
	}
	if len(unsorted) != 0 {
		t.Fatalf("unsorted = %v, want empty", unsorted)
	}
}

// TestExpandRangesUnsorted: a file matched by no range lands in the
// limbo bucket under its base-name label; matched files never
// duplicate into it.
func TestExpandRangesUnsorted(t *testing.T) {
	dir := rangesMediaDir(t, "EP_1.mkv", "stray.mkv")
	rf := &RangesFile{Version: 1, Ranges: []RangeBinding{
		{ID: "s1", Title: "S1", Match: RangeMatch{Kind: "glob", Patterns: []string{"EP_*"}}},
	}}
	views, unsorted := ExpandRanges(dir, Snapshot{TitleDir: dir}, rf)
	if len(views) != 1 {
		t.Fatalf("views = %d, want 1", len(views))
	}
	if len(unsorted) != 1 {
		t.Fatalf("unsorted = %+v, want 1 entry", unsorted)
	}
	if unsorted[0].EpisodeNum != "stray.mkv" || unsorted[0].VideoKey != "file" {
		t.Fatalf("unsorted entry = %+v, want the stray pseudo-entry", unsorted[0])
	}
}

// TestExpandRangesFirstRangeWins: a file matched by two ranges plays
// in both (overlap is legal), but the unsorted bucket takes it once —
// via the first matching range's claim, not twice.
func TestExpandRangesFirstRangeWins(t *testing.T) {
	dir := rangesMediaDir(t, "a1.mkv")
	rf := &RangesFile{Version: 1, Ranges: []RangeBinding{
		{ID: "g", Title: "G", Match: RangeMatch{Kind: "glob", Patterns: []string{"a*"}}},
		{ID: "e", Title: "E", Match: RangeMatch{Kind: "exact", Files: []string{"a1.mkv"}}},
	}}
	views, unsorted := ExpandRanges(dir, Snapshot{TitleDir: dir}, rf)
	if len(views) != 2 {
		t.Fatalf("views = %d, want 2 (overlapping ranges both render)", len(views))
	}
	if len(unsorted) != 0 {
		t.Fatalf("unsorted = %v, want empty (the file is claimed)", unsorted)
	}
}

// TestSyntheticSourceURL: the synthetic progress source key.
func TestSyntheticSourceURL(t *testing.T) {
	if got := SyntheticSourceURL("Реинкарнация/s1", "s2"); got != "anicli-offline:Реинкарнация/s1#s2" {
		t.Fatalf("SyntheticSourceURL = %q", got)
	}
	if got := SyntheticSourceURL("", "s1"); got != "anicli-offline:#s1" {
		t.Fatalf("empty relDir = %q, want anicli-offline:#s1 (legal, pinned)", got)
	}
}

// TestSuggestRanges: the deterministic, disposable group suggestion —
// files grouped by their leading non-digit prefix, digit-leading
// files stay out (they land in the editor's unsorted pool).
func TestSuggestRanges(t *testing.T) {
	dir := rangesMediaDir(t, "EP_1_a.mkv", "EP_2_a.mkv", "OVA_1.mkv", "001.mkv")
	got := SuggestRanges(dir)
	if len(got) != 2 {
		t.Fatalf("suggestions = %d (%+v), want 2", len(got), got)
	}
	if got[0].ID != "r1" || got[0].Title != "EP" {
		t.Errorf("first = %+v, want r1/EP", got[0])
	}
	if got[0].Match.Kind != "glob" || !reflect.DeepEqual(got[0].Match.Patterns, []string{"EP_*"}) {
		t.Errorf("first match = %+v, want glob EP_*", got[0].Match)
	}
	if got[1].ID != "r2" || got[1].Title != "OVA" || !reflect.DeepEqual(got[1].Match.Patterns, []string{"OVA_*"}) {
		t.Errorf("second = %+v, want r2/OVA glob OVA_*", got[1])
	}
	for _, s := range got {
		if s.ShikimoriID != 0 || s.EpisodeOffset != 0 {
			t.Errorf("suggestion must ship unbound: %+v", s)
		}
	}
	// The suggested document round-trips through the saver.
	if err := SaveRanges(t.TempDir(), &RangesFile{Version: 1, Ranges: got}); err != nil {
		t.Errorf("suggestions must be saveable: %v", err)
	}

	empty := t.TempDir()
	if got := SuggestRanges(empty); got != nil {
		t.Errorf("empty dir = %+v, want nil", got)
	}
}
