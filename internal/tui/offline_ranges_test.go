package tui

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/download"
	"github.com/an0nx/anicli-go/internal/i18n"
)

// rangesOfflineFixture builds a real library folder under t.TempDir():
// an offline index over some files (the downloader's write path) plus
// optionally hand-placed stray media, via download.UpsertEntry.
func rangesOfflineFixture(t *testing.T, dir string, indexedFiles map[string]string) {
	t.Helper()
	for file, label := range indexedFiles {
		path := filepath.Join(dir, file)
		if err := writeFixtureFile(path); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		if err := download.UpsertEntry(dir, download.EntryInput{
			EpisodeNum: label,
			VideoKey:   "Kodik",
			AudioKey:   "Dub",
			Quality:    1080,
			FilePath:   path,
		}); err != nil {
			t.Fatalf("upsert %s: %v", file, err)
		}
	}
}

func writeFixtureFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("x"), 0o600)
}

// realOfflineFor returns the real service pointed at the temp root.
func realOfflineFor(root string) *realOffline {
	return &realOffline{settings: config.Settings{Download: config.Download{Dir: root}}}
}

// TestRealOfflineTitlesPlainFolderUnchanged is the backward-compat
// anchor (Review Focus #1): a folder with an index and NO ranges file
// renders exactly today's one plain title.
func TestRealOfflineTitlesPlainFolderUnchanged(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Тайтл")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	rangesOfflineFixture(t, dir, map[string]string{"EP_1_Kodik_1080p.mkv": "1", "EP_2_Kodik_1080p.mkv": "2"})

	titles, err := realOfflineFor(root).Titles()
	if err != nil {
		t.Fatalf("Titles: %v", err)
	}
	if len(titles) != 1 {
		t.Fatalf("titles = %d, want exactly the plain one", len(titles))
	}
	got := titles[0]
	if got.Name != "Тайтл" || got.Range != nil {
		t.Fatalf("plain title = %+v, want Name=Тайтл Range=nil", got)
	}
	if got.Dir != dir {
		t.Errorf("Dir = %q, want %q", got.Dir, dir)
	}
	if got.RelDir != "Тайтл" {
		t.Errorf("RelDir = %q, want Тайтл", got.RelDir)
	}
	if got.Snapshot.TotalDownloaded() != 2 {
		t.Errorf("snapshot entries = %d, want 2", got.Snapshot.TotalDownloaded())
	}
}

// TestRealOfflineTitlesRangesSplit: one folder with a ranges file
// renders one virtual title per range plus the unsorted limbo title,
// in file order, with rewritten position labels and the REAL dir.
func TestRealOfflineTitlesRangesSplit(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("ru"); err != nil {
		t.Fatalf("Init(ru): %v", err)
	}
	root := t.TempDir()
	dir := filepath.Join(root, "Реинкарнация")
	files := []string{
		"EP_1_Kodik_1080p.mkv", "EP_2_Kodik_1080p.mkv", // s1 (indexed)
		"OVA_1.mkv", // s2... actually the OVA range
		"stray.mkv", // unmatched
	}
	for _, f := range files {
		if err := writeFixtureFile(filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}
	rangesOfflineFixture(t, dir, map[string]string{"EP_1_Kodik_1080p.mkv": "1", "EP_2_Kodik_1080p.mkv": "2"})
	rf := download.RangesFile{Version: 1, Ranges: []download.RangeBinding{
		{ID: "s1", Title: "Реинкарнация — S1", Match: download.RangeMatch{Kind: "glob", Patterns: []string{"EP_*"}}},
		{ID: "ova", Title: "Реинкарнация — OVA", Match: download.RangeMatch{Kind: "glob", Patterns: []string{"OVA_*"}}},
	}}
	if err := download.SaveRanges(dir, &rf); err != nil {
		t.Fatalf("SaveRanges: %v", err)
	}

	titles, err := realOfflineFor(root).Titles()
	if err != nil {
		t.Fatalf("Titles: %v", err)
	}
	if len(titles) != 3 {
		t.Fatalf("titles = %d (%+v), want 3 (s1, ova, unsorted)", len(titles), titles)
	}
	wantNames := []string{"Реинкарнация — S1", "Реинкарнация — OVA", "Реинкарнация · несортированное"}
	for i, want := range wantNames {
		if titles[i].Name != want {
			t.Errorf("titles[%d].Name = %q, want %q", i, titles[i].Name, want)
		}
	}
	s1 := titles[0]
	if s1.Range == nil || s1.Range.ID != "s1" {
		t.Fatalf("s1.Range = %+v, want the s1 binding", s1.Range)
	}
	if s1.Snapshot.TitleDir != dir {
		t.Errorf("Snapshot.TitleDir = %q, want the REAL dir (playback path)", s1.Snapshot.TitleDir)
	}
	labels := []string{}
	for _, e := range s1.Snapshot.Entries {
		labels = append(labels, e.EpisodeNum)
	}
	if strings.Join(labels, ",") != "1,2" {
		t.Errorf("s1 position labels = %v, want [1 2]", labels)
	}
	if titles[2].Range != nil {
		t.Errorf("unsorted title carries a Range — the limbo must stay progress-less")
	}
	for _, e := range titles[2].Snapshot.Entries {
		if e.EpisodeNum != "stray.mkv" {
			t.Errorf("unsorted label = %q, want the base name", e.EpisodeNum)
		}
	}
}

// TestRealOfflineTitlesZeroFileRangeHidden: a range matching nothing
// is not rendered.
func TestRealOfflineTitlesZeroFileRangeHidden(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Тайтл")
	for _, f := range []string{"EP_1_a.mkv", "EP_2_a.mkv"} {
		if err := writeFixtureFile(filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}
	rf := download.RangesFile{Version: 1, Ranges: []download.RangeBinding{
		{ID: "s1", Title: "S1", Match: download.RangeMatch{Kind: "glob", Patterns: []string{"EP_*"}}},
		{ID: "ghost", Title: "Ghost", Match: download.RangeMatch{Kind: "glob", Patterns: []string{"OVA_*"}}},
	}}
	if err := download.SaveRanges(dir, &rf); err != nil {
		t.Fatal(err)
	}
	titles, err := realOfflineFor(root).Titles()
	if err != nil {
		t.Fatal(err)
	}
	if len(titles) != 1 || titles[0].Range == nil || titles[0].Range.ID != "s1" {
		t.Fatalf("titles = %+v, want only the s1 virtual title", titles)
	}
}

// TestRealOfflineTitlesCorruptRangesDegrades: a corrupt ranges file
// quarantines and the folder degrades to today's plain title.
func TestRealOfflineTitlesCorruptRangesDegrades(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Тайтл")
	rangesOfflineFixture(t, dir, map[string]string{"EP_1_Kodik_1080p.mkv": "1"})
	if err := download.SaveRanges(dir, &download.RangesFile{Version: 1, Ranges: []download.RangeBinding{
		{ID: "s1", Title: "S1", Match: download.RangeMatch{Kind: "glob", Patterns: []string{"EP_*"}}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, download.RangesFilename), []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}

	titles, err := realOfflineFor(root).Titles()
	if err != nil {
		t.Fatal(err)
	}
	if len(titles) != 1 || titles[0].Name != "Тайтл" || titles[0].Range != nil {
		t.Fatalf("titles = %+v, want the single plain title", titles)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	quarantined := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), download.RangesFilename+".corrupt-") {
			quarantined = true
		}
	}
	if !quarantined {
		t.Error("no .corrupt-* quarantine file after the degraded load")
	}
}

// TestRealOfflineTitlesEmptyIndexWithRanges: a hand-placed folder
// (no index at all) with a ranges file still renders its virtual
// titles as file pseudo-entries.
func TestRealOfflineTitlesEmptyIndexWithRanges(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Ручная")
	for _, f := range []string{"EP_1_a.mkv", "EP_2_a.mkv"} {
		if err := writeFixtureFile(filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}
	rf := download.RangesFile{Version: 1, Ranges: []download.RangeBinding{
		{ID: "s1", Title: "Ручная — S1", Match: download.RangeMatch{Kind: "glob", Patterns: []string{"EP_*"}}},
	}}
	if err := download.SaveRanges(dir, &rf); err != nil {
		t.Fatal(err)
	}
	titles, err := realOfflineFor(root).Titles()
	if err != nil {
		t.Fatal(err)
	}
	if len(titles) != 1 || titles[0].Name != "Ручная — S1" {
		t.Fatalf("titles = %+v, want the virtual title over pseudo-entries", titles)
	}
	if titles[0].Snapshot.TotalDownloaded() != 2 {
		t.Fatalf("entries = %d, want 2", titles[0].Snapshot.TotalDownloaded())
	}
	for i, e := range titles[0].Snapshot.Entries {
		if e.EpisodeNum != strconv.Itoa(i+1) || e.VideoKey != "file" {
			t.Errorf("entries[%d] = %+v, want the file pseudo-entry with label %d", i, e, i+1)
		}
	}
}

// TestLocalEpisodeCountsAggregatesByDir (R4 fix): the online
// session's «★» marks must see the WHOLE folder — counts sum across
// every title sharing the Dir, matched by directory not by name.
func TestLocalEpisodeCountsAggregatesByDir(t *testing.T) {
	dir := "/dl/Тайтл"
	s1 := OfflineTitle{
		Dir: dir, Name: "Тайтл — S1", RelDir: "Тайтл",
		Range: &download.RangeBinding{ID: "s1", Title: "Тайтл — S1"},
		Snapshot: download.Snapshot{TitleDir: dir, Entries: []download.Entry{
			{EpisodeNum: "1", VideoKey: "Kodik", AudioKey: "Dub", Quality: 1080, RelativePath: "EP_1.mkv"},
		}},
	}
	s2 := OfflineTitle{
		Dir: dir, Name: "Тайтл — S2", RelDir: "Тайтл",
		Range: &download.RangeBinding{ID: "s2", Title: "Тайтл — S2"},
		Snapshot: download.Snapshot{TitleDir: dir, Entries: []download.Entry{
			{EpisodeNum: "1", VideoKey: "Kodik", AudioKey: "Dub", Quality: 1080, RelativePath: "EP_1b.mkv"},
			{EpisodeNum: "2", VideoKey: "Kodik", AudioKey: "Dub", Quality: 1080, RelativePath: "EP_2b.mkv"},
		}},
	}
	counts := LocalEpisodeCounts([]OfflineTitle{s1, s2}, "Тайтл")
	if counts["1"] != 2 {
		t.Errorf("counts[1] = %d, want 2 (both ranges share the folder)", counts["1"])
	}
	if counts["2"] != 1 {
		t.Errorf("counts[2] = %d, want 1", counts["2"])
	}
}
