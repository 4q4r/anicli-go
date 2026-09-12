package download

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// IndexFilename is the per-title offline index hostfile (FEATURE E:
// series/video_key/audio_key/quality/file_path).
const IndexFilename = ".anicli_offline_index.json"

// indexVersion is the on-disk schema version.
const indexVersion = 1

// fsUnsafeChars matches characters python's clean_title_for_fs strips.
var fsUnsafeChars = regexp.MustCompile(`[\\/*?:"<>|]`)

// Entry is one locally downloaded stream combination.
type Entry struct {
	// EpisodeNum is the episode label.
	EpisodeNum string `json:"episode_num"`
	// AnimeID is the local anime record id, nil when unbound.
	AnimeID *int64 `json:"anime_id"`
	// VideoKey is the selected video stream key.
	VideoKey string `json:"video_key"`
	// AudioKey is the selected audio stream key.
	AudioKey string `json:"audio_key"`
	// Quality is the vertical resolution of the file.
	Quality int `json:"quality"`
	// RelativePath locates the file inside the title directory.
	RelativePath string `json:"relative_path"`
	// Container is the extension without the leading dot.
	Container string `json:"container"`
	// ChapterTypes lists the markers captured at download time.
	ChapterTypes []string `json:"chapter_types"`
	// CreatedAt is an RFC3339 timestamp.
	CreatedAt string `json:"created_at"`
}

// Snapshot is the in-memory view of one title directory.
type Snapshot struct {
	// TitleDir is the directory holding the media files and the index.
	TitleDir string
	// Entries lists validated entries (existing files only).
	Entries []Entry
}

// TotalDownloaded counts indexed combinations.
func (s Snapshot) TotalDownloaded() int { return len(s.Entries) }

// VideoCounts maps video key -> distinct episode count.
func (s Snapshot) VideoCounts() map[string]int {
	counts := make(map[string]map[string]struct{})
	for _, e := range s.Entries {
		if counts[e.VideoKey] == nil {
			counts[e.VideoKey] = make(map[string]struct{})
		}
		counts[e.VideoKey][e.EpisodeNum] = struct{}{}
	}
	out := make(map[string]int, len(counts))
	for key, eps := range counts {
		out[key] = len(eps)
	}
	return out
}

// AudioCounts maps audio key -> distinct episode count.
func (s Snapshot) AudioCounts() map[string]int {
	counts := make(map[string]map[string]struct{})
	for _, e := range s.Entries {
		if counts[e.AudioKey] == nil {
			counts[e.AudioKey] = make(map[string]struct{})
		}
		counts[e.AudioKey][e.EpisodeNum] = struct{}{}
	}
	out := make(map[string]int, len(counts))
	for key, eps := range counts {
		out[key] = len(eps)
	}
	return out
}

// EpisodeVideoKeys returns the local video keys of one episode.
func (s Snapshot) EpisodeVideoKeys(episodeNum string) []string {
	seen := make(map[string]struct{})
	for _, e := range s.Entries {
		if e.EpisodeNum == episodeNum {
			seen[e.VideoKey] = struct{}{}
		}
	}
	return sortedKeys(seen)
}

// EpisodeAudioKeys returns the local audio keys of one episode.
func (s Snapshot) EpisodeAudioKeys(episodeNum string) []string {
	seen := make(map[string]struct{})
	for _, e := range s.Entries {
		if e.EpisodeNum == episodeNum {
			seen[e.AudioKey] = struct{}{}
		}
	}
	return sortedKeys(seen)
}

// FindExact resolves an episode/stream/quality combination to its
// local file path, or nil when absent (files that vanished on disk do
// not resolve).
func (s Snapshot) FindExact(episodeNum, videoKey, audioKey string, quality int) *string {
	for _, e := range s.Entries {
		if e.EpisodeNum != episodeNum || e.VideoKey != videoKey ||
			e.AudioKey != audioKey || e.Quality != quality {
			continue
		}
		path := filepath.Join(s.TitleDir, e.RelativePath)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return &path
		}
	}
	return nil
}

func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// CleanTitleForFS strips filesystem-unsafe characters (python
// clean_title_for_fs).
func CleanTitleForFS(value string) string {
	return fsUnsafeChars.ReplaceAllString(value, "")
}

// EntryInput carries an upsert.
type EntryInput struct {
	// EpisodeNum is the episode label.
	EpisodeNum string
	// AnimeID is the local anime record id, nil when unbound.
	AnimeID *int64
	// VideoKey is the selected video stream key.
	VideoKey string
	// AudioKey is the selected audio stream key.
	AudioKey string
	// Quality is the selected resolution.
	Quality int
	// FilePath is the absolute output file inside the title dir.
	FilePath string
	// ChapterTypes are the markers captured at download time.
	ChapterTypes []string
}

// indexPayload is the on-disk document.
type indexPayload struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
}

// LoadSnapshot reads and validates the index: malformed documents read
// as empty (python silently-ignore cache semantics) and entries whose
// media file vanished are dropped.
func LoadSnapshot(titleDir string) (Snapshot, error) {
	index := filepath.Join(titleDir, IndexFilename)
	data, err := os.ReadFile(index) //nolint:gosec // path built from caller-provided dir
	if err != nil {
		if os.IsNotExist(err) {
			return Snapshot{TitleDir: titleDir, Entries: []Entry{}}, nil
		}
		return Snapshot{}, fmt.Errorf("offline index: read %s: %w", index, err)
	}

	var payload indexPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return Snapshot{TitleDir: titleDir, Entries: []Entry{}}, nil
	}

	entries := make([]Entry, 0, len(payload.Entries))
	for _, e := range payload.Entries {
		if !entryValid(e) {
			continue
		}
		if info, err := os.Stat(filepath.Join(titleDir, e.RelativePath)); err != nil || info.IsDir() {
			continue
		}
		entries = append(entries, e)
	}
	return Snapshot{TitleDir: titleDir, Entries: entries}, nil
}

// entryValid guards structurally broken rows.
func entryValid(e Entry) bool {
	return e.EpisodeNum != "" && e.VideoKey != "" && e.AudioKey != "" && e.RelativePath != "" && e.Quality > 0
}

// UpsertEntry adds or updates one entry and rewrites the index in
// canonical sorted form (python upsert_entry).
func UpsertEntry(titleDir string, in EntryInput) error {
	if err := os.MkdirAll(titleDir, 0o750); err != nil {
		return fmt.Errorf("offline index: create %s: %w", titleDir, err)
	}
	snapshot, err := LoadSnapshot(titleDir)
	if err != nil {
		return err
	}

	rel, err := filepath.Rel(titleDir, in.FilePath)
	if err != nil {
		return fmt.Errorf("offline index: relativize %s: %w", in.FilePath, err)
	}
	container := strings.TrimPrefix(filepath.Ext(in.FilePath), ".")
	if container == "" {
		container = "mp4"
	}

	entry := Entry{
		EpisodeNum:   in.EpisodeNum,
		AnimeID:      in.AnimeID,
		VideoKey:     in.VideoKey,
		AudioKey:     in.AudioKey,
		Quality:      in.Quality,
		RelativePath: filepath.ToSlash(rel),
		Container:    container,
		ChapterTypes: sortedUniqueLower(in.ChapterTypes),
		CreatedAt:    time.Now().UTC().Format(time.RFC3339Nano),
	}

	merged := make([]Entry, 0, len(snapshot.Entries)+1)
	replaced := false
	for _, existing := range snapshot.Entries {
		if sameKey(existing, entry) {
			if !replaced {
				merged = append(merged, entry)
				replaced = true
			}
			continue
		}
		merged = append(merged, existing)
	}
	if !replaced {
		merged = append(merged, entry)
	}

	return writeIndex(titleDir, merged)
}

// RemoveEntry deletes one entry by its combination key; it reports
// whether anything was removed.
func RemoveEntry(titleDir, episodeNum, videoKey, audioKey string, quality int) (bool, error) {
	snapshot, err := LoadSnapshot(titleDir)
	if err != nil {
		return false, err
	}
	kept := make([]Entry, 0, len(snapshot.Entries))
	removed := false
	for _, e := range snapshot.Entries {
		if e.EpisodeNum == episodeNum && e.VideoKey == videoKey &&
			e.AudioKey == audioKey && e.Quality == quality {
			removed = true
			continue
		}
		kept = append(kept, e)
	}
	if !removed {
		return false, nil
	}
	return true, writeIndex(titleDir, kept)
}

// ReconcileSnapshot validates the index against the filesystem and
// rewrites it canonically: dead entries drop, stray media files are
// not adopted (the index is the source of truth) — python
// reconcile_snapshot.
func ReconcileSnapshot(titleDir string) (Snapshot, error) {
	snapshot, err := LoadSnapshot(titleDir)
	if err != nil {
		return Snapshot{}, err
	}
	if err := writeIndex(titleDir, snapshot.Entries); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

// SetEntryChapterTypes updates the markers of one existing entry
// (python set_entry_chapter_types; lowercased, sorted, unique).
func SetEntryChapterTypes(titleDir, episodeNum, videoKey, audioKey string, quality int, chapterTypes []string) error {
	normalized := sortedUniqueLower(chapterTypes)
	if len(normalized) == 0 {
		return nil
	}
	snapshot, err := LoadSnapshot(titleDir)
	if err != nil {
		return err
	}

	changed := false
	entries := make([]Entry, 0, len(snapshot.Entries))
	for _, e := range snapshot.Entries {
		if e.EpisodeNum == episodeNum && e.VideoKey == videoKey &&
			e.AudioKey == audioKey && e.Quality == quality {
			if !equalStrings(e.ChapterTypes, normalized) {
				e.ChapterTypes = normalized
				changed = true
			}
		}
		entries = append(entries, e)
	}
	if !changed {
		return nil
	}
	return writeIndex(titleDir, entries)
}

// writeIndex persists the canonical document: version 1, entries
// sorted by numeric episode then stream keys (python sort key).
func writeIndex(titleDir string, entries []Entry) error {
	sorted := make([]Entry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		ei, iOK := episodeNumber(sorted[i].EpisodeNum)
		ej, jOK := episodeNumber(sorted[j].EpisodeNum)
		if !iOK {
			ei = 0
		}
		if !jOK {
			ej = 0
		}
		if ei != ej {
			return ei < ej
		}
		if sorted[i].VideoKey != sorted[j].VideoKey {
			return sorted[i].VideoKey < sorted[j].VideoKey
		}
		if sorted[i].AudioKey != sorted[j].AudioKey {
			return sorted[i].AudioKey < sorted[j].AudioKey
		}
		return sorted[i].Quality < sorted[j].Quality
	})

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(indexPayload{Version: indexVersion, Entries: sorted}); err != nil {
		return fmt.Errorf("offline index: encode: %w", err)
	}

	path := filepath.Join(titleDir, IndexFilename)
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("offline index: write %s: %w", path, err)
	}
	return nil
}

// episodeNumber parses an episode label the python way: digits with at
// most one dot.
func episodeNumber(s string) (float64, bool) {
	trimmed := strings.Replace(s, ".", "", 1)
	if trimmed == "" || !isAllDigits(trimmed) {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// sortedUniqueLower normalizes marker lists.
func sortedUniqueLower(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	for _, v := range values {
		lowered := strings.ToLower(strings.TrimSpace(v))
		if lowered == "" {
			continue
		}
		seen[lowered] = struct{}{}
	}
	return sortedKeys(seen)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sameKey compares combination identity.
func sameKey(a, b Entry) bool {
	return a.EpisodeNum == b.EpisodeNum && a.VideoKey == b.VideoKey &&
		a.AudioKey == b.AudioKey && a.Quality == b.Quality
}
