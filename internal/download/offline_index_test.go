package download

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// seedMedia creates a fake media file inside dir and returns its path.
func seedMedia(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("media"), 0o600); err != nil {
		t.Fatalf("seed media: %v", err)
	}
	return path
}

// TestOfflineIndexRoundTrip pins the full write/read cycle: JSON
// shape, sorted entries and snapshot queries.
func TestOfflineIndexRoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	media := seedMedia(t, dir, "Anime - 2.mp4")

	if err := UpsertEntry(dir, EntryInput{
		EpisodeNum: "2", AnimeID: int64Ptr(7),
		VideoKey: "[k]v", AudioKey: "[k]a", Quality: 1080,
		FilePath: media, ChapterTypes: []string{"op", "ed"},
	}); err != nil {
		t.Fatalf("UpsertEntry: %v", err)
	}

	// numeric ordering: ep 10 after ep 2
	media10 := seedMedia(t, dir, "Anime - 10.mp4")
	if err := UpsertEntry(dir, EntryInput{
		EpisodeNum: "10", VideoKey: "[k]v", AudioKey: "[k]a", Quality: 720,
		FilePath: media10,
	}); err != nil {
		t.Fatalf("UpsertEntry: %v", err)
	}

	snap, err := LoadSnapshot(dir)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if snap.TotalDownloaded() != 2 {
		t.Fatalf("entries = %d, want 2", snap.TotalDownloaded())
	}
	if got := snap.Entries[0].EpisodeNum; got != "2" {
		t.Errorf("first entry episode = %q, want 2 (numeric sort)", got)
	}
	if got := snap.Entries[0].ChapterTypes; !reflect.DeepEqual(got, []string{"ed", "op"}) {
		t.Errorf("chapter types = %v, want [ed op] (sorted unique)", got)
	}

	// raw JSON shape
	raw, err := os.ReadFile(filepath.Join(dir, IndexFilename)) //nolint:gosec // test reads its own temp index
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	for _, want := range []string{
		`"version": 1`, `"episode_num": "2"`, `"video_key": "[k]v"`,
		`"quality": 1080`, `"container": "mp4"`, `"relative_path": "Anime - 2.mp4"`,
	} {
		if !contains(string(raw), want) {
			t.Errorf("index JSON missing %q:\n%s", want, raw)
		}
	}

	// queries
	if _, ok := snap.VideoCounts()["[k]v"]; !ok || snap.VideoCounts()["[k]v"] != 2 {
		t.Errorf("VideoCounts = %v", snap.VideoCounts())
	}
	keys := snap.EpisodeVideoKeys("2")
	if !reflect.DeepEqual(keys, []string{"[k]v"}) {
		t.Errorf("EpisodeVideoKeys = %v", keys)
	}
	found := snap.FindExact("2", "[k]v", "[k]a", 1080)
	if found == nil || *found != media {
		t.Errorf("FindExact = %v, want %q", found, media)
	}
	if got := snap.FindExact("3", "[k]v", "[k]a", 1080); got != nil {
		t.Errorf("FindExact(3) = %v, want nil", got)
	}
}

// TestOfflineIndexMissingFileDropped pins drift handling: entries whose
// media file vanished are dropped on load.
func TestOfflineIndexMissingFileDropped(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	media := seedMedia(t, dir, "a.mp4")
	if err := UpsertEntry(dir, EntryInput{EpisodeNum: "1", VideoKey: "v", AudioKey: "a", Quality: 1, FilePath: media}); err != nil {
		t.Fatalf("UpsertEntry: %v", err)
	}
	if err := os.Remove(media); err != nil {
		t.Fatalf("remove media: %v", err)
	}

	snap, err := LoadSnapshot(dir)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if snap.TotalDownloaded() != 0 {
		t.Errorf("entries = %d, want 0 (missing file dropped)", snap.TotalDownloaded())
	}
}

// TestOfflineIndexReconcileRewrites pins: reconcile drops dead entries
// and rewrites the file in canonical form; stray media files are NOT
// adopted (index is the source of truth).
func TestOfflineIndexReconcileRewrites(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	keep := seedMedia(t, dir, "keep.mp4")
	gone := filepath.Join(dir, "gone.mp4")
	if err := UpsertEntry(dir, EntryInput{EpisodeNum: "1", VideoKey: "v", AudioKey: "a", Quality: 1, FilePath: keep}); err != nil {
		t.Fatalf("UpsertEntry: %v", err)
	}
	if err := UpsertEntry(dir, EntryInput{EpisodeNum: "2", VideoKey: "v", AudioKey: "a", Quality: 1, FilePath: gone}); err != nil {
		t.Fatalf("UpsertEntry: %v", err)
	}
	stray := seedMedia(t, dir, "stray.mp4")

	snap, err := ReconcileSnapshot(dir)
	if err != nil {
		t.Fatalf("ReconcileSnapshot: %v", err)
	}
	if snap.TotalDownloaded() != 1 {
		t.Fatalf("entries = %d, want 1 (gone dropped, stray ignored)", snap.TotalDownloaded())
	}
	if snap.Entries[0].RelativePath != "keep.mp4" {
		t.Errorf("kept entry = %+v", snap.Entries[0])
	}

	// Rewritten file only carries the live entry.
	snap2, err := LoadSnapshot(dir)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if snap2.TotalDownloaded() != 1 {
		t.Errorf("post-reconcile entries = %d, want 1", snap2.TotalDownloaded())
	}
	_ = stray
}

// TestOfflineIndexRemoveEntry pins explicit removal.
func TestOfflineIndexRemoveEntry(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	media := seedMedia(t, dir, "a.mp4")
	if err := UpsertEntry(dir, EntryInput{EpisodeNum: "1", VideoKey: "v", AudioKey: "a", Quality: 1, FilePath: media}); err != nil {
		t.Fatalf("UpsertEntry: %v", err)
	}

	removed, err := RemoveEntry(dir, "1", "v", "a", 1)
	if err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	if !removed {
		t.Error("RemoveEntry = false, want true")
	}
	snap, err := LoadSnapshot(dir)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if snap.TotalDownloaded() != 0 {
		t.Errorf("entries = %d, want 0", snap.TotalDownloaded())
	}

	if removed, err := RemoveEntry(dir, "1", "v", "a", 1); err != nil || removed {
		t.Errorf("RemoveEntry of absent = %v/%v, want false/nil", removed, err)
	}
}

// TestOfflineIndexSetEntryChapterTypes pins marker updates.
func TestOfflineIndexSetEntryChapterTypes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	media := seedMedia(t, dir, "a.mp4")
	if err := UpsertEntry(dir, EntryInput{EpisodeNum: "1", VideoKey: "v", AudioKey: "a", Quality: 1, FilePath: media}); err != nil {
		t.Fatalf("UpsertEntry: %v", err)
	}

	if err := SetEntryChapterTypes(dir, "1", "v", "a", 1, []string{"OP", "ed", "op"}); err != nil {
		t.Fatalf("SetEntryChapterTypes: %v", err)
	}
	snap, err := LoadSnapshot(dir)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if !reflect.DeepEqual(snap.Entries[0].ChapterTypes, []string{"ed", "op"}) {
		t.Errorf("chapter types = %v, want [ed op] (lowercased, sorted unique)", snap.Entries[0].ChapterTypes)
	}
}

// TestOfflineIndexMissingAndCorrupt pins: a missing index reads as an
// empty snapshot; a corrupt index is treated as empty (python
// silently-ignore semantics for this cache file).
func TestOfflineIndexMissingAndCorrupt(t *testing.T) {
	t.Parallel()

	empty := t.TempDir()
	snap, err := LoadSnapshot(empty)
	if err != nil {
		t.Fatalf("LoadSnapshot(missing): %v", err)
	}
	if snap.TotalDownloaded() != 0 {
		t.Errorf("entries = %d, want 0", snap.TotalDownloaded())
	}

	corrupt := t.TempDir()
	if err := os.WriteFile(filepath.Join(corrupt, IndexFilename), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed corrupt: %v", err)
	}
	snap, err = LoadSnapshot(corrupt)
	if err != nil {
		t.Fatalf("LoadSnapshot(corrupt): %v", err)
	}
	if snap.TotalDownloaded() != 0 {
		t.Errorf("entries = %d, want 0", snap.TotalDownloaded())
	}
}

// TestCleanTitleForFS strips unsafe path characters.
func TestCleanTitleForFS(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		`Anime: Second / Season?`: "Anime Second  Season",
		`A <B> "C" |D| *E* \F/`:   "A B C D E F",
		`Plain Title`:             "Plain Title",
	}
	for in, want := range cases {
		if got := CleanTitleForFS(in); got != want {
			t.Errorf("CleanTitleForFS(%q) = %q, want %q", in, got, want)
		}
	}
}

func int64Ptr(v int64) *int64 { return &v }

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
