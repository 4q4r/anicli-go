package download

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// RangesFilename is the per-folder ranges sidecar: it splits one
// local library folder into several Shikimori entries (seasons, OVAs),
// each rendered as its own virtual offline title with its own
// progress row. The on-disk version is always 1.
const RangesFilename = ".anicli_ranges.json"

// ErrRangesUnsupportedVersion reports a ranges file written by a
// different schema version: the caller must treat the folder as if
// the file were absent (today's plain title) and leave the file
// untouched for manual recovery.
var ErrRangesUnsupportedVersion = errors.New("offline ranges: unsupported version")

// rangeIDPattern constrains a range id: it feeds the synthetic
// progress source key and must stay filesystem/JSON safe.
var rangeIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// RangeMatch selects the files of one range.
type RangeMatch struct {
	// Kind is "glob" (patterns auto-join new files) or "exact"
	// (explicit filename list, owner order = episode order).
	Kind string `json:"kind"`
	// Patterns lists the glob patterns (glob mode, ORDERED: pattern
	// order is episode order).
	Patterns []string `json:"patterns,omitempty"`
	// Files lists exact file base names (exact mode, owner order =
	// episode order).
	Files []string `json:"files,omitempty"`
	// Quality is an optional glob matched against the file base name
	// (e.g. "1080p"): when set, only files matching it join the
	// range, so a multi-quality folder (one episode in 1080p + 720p)
	// produces no phantom episode positions.
	Quality string `json:"quality,omitempty"`
}

// RangeBinding is one range: a named slice of the folder's files
// bound (optionally) to one Shikimori entry.
type RangeBinding struct {
	// ID is the stable range key inside the file (^[A-Za-z0-9._-]+$,
	// unique); it feeds the synthetic progress source key.
	ID string `json:"id"`
	// Title is the picker name of the virtual title; non-empty and
	// unique per file.
	Title string `json:"title"`
	// ShikimoriID is the bound Shikimori anime; 0 = no sync.
	ShikimoriID int64 `json:"shikimori_id,omitempty"`
	// ShikimoriTitle caches the entry name for history rendering
	// (same column in anime_progress).
	ShikimoriTitle string `json:"shikimori_title,omitempty"`
	// Match selects the files.
	Match RangeMatch `json:"match"`
	// EpisodeOffset shifts the sync counter: shikimori_episode =
	// range position + offset (files named 1..12 under a Shikimori
	// entry counting 13..24 → offset 12).
	EpisodeOffset int `json:"episode_offset,omitempty"`
}

// RangesFile is the on-disk document.
type RangesFile struct {
	Version   int            `json:"version"`
	UpdatedAt string         `json:"updated_at"` // RFC3339 UTC
	Ranges    []RangeBinding `json:"ranges"`
}

// LoadRanges reads the folder's ranges file.
//
// Absent file → (nil, nil). A malformed document is quarantined as
// .corrupt-<ts> with the exact offline-index semantics (169-E: the
// broken bytes stay available for manual recovery; the rename must
// succeed or the error surfaces — a failed rename leaves the document
// untouched) and reads as (nil, nil). A foreign version returns
// ErrRangesUnsupportedVersion without touching the file. Overlapping
// and zero-file ranges LOAD fine — validation is a save-time concern.
func LoadRanges(titleDir string) (*RangesFile, error) {
	path := filepath.Join(titleDir, RangesFilename)
	data, err := os.ReadFile(path) //nolint:gosec // path built from caller-provided dir
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("offline ranges: read %s: %w", path, err)
	}

	var rf RangesFile
	if err := json.Unmarshal(data, &rf); err != nil {
		// 169-E quarantine, verbatim from the offline index: an
		// unparseable document (a crash mid-write) is kept as a
		// timestamped .corrupt backup instead of being overwritten by
		// the next save; the rename must succeed first.
		stamped := fmt.Sprintf("%s.corrupt-%s", path, time.Now().UTC().Format("20060102T150405.000000000"))
		if rerr := os.Rename(path, stamped); rerr != nil {
			return nil, fmt.Errorf("offline ranges: back up corrupt %s: %w", path, rerr)
		}
		return nil, nil
	}
	if rf.Version != 1 {
		return nil, fmt.Errorf("%w: got %d", ErrRangesUnsupportedVersion, rf.Version)
	}
	return &rf, nil
}

// SaveRanges validates and atomically writes the ranges file: version
// forced to 1, UpdatedAt stamped, one document per folder serialized
// under the shared index mutex, staged through .tmp + rename in the
// same directory (the offline-index write discipline).
func SaveRanges(titleDir string, rf *RangesFile) error {
	if rf == nil {
		return fmt.Errorf("offline ranges: nil document")
	}
	for i := range rf.Ranges {
		if err := validateBinding(rf.Ranges, i); err != nil {
			return err
		}
	}

	indexMu.Lock()
	defer indexMu.Unlock()

	rf.Version = 1
	rf.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rf); err != nil {
		return fmt.Errorf("offline ranges: encode: %w", err)
	}

	path := filepath.Join(titleDir, RangesFilename)
	// Stage in the same directory (rename stays on one filesystem)
	// and swap atomically — a crash mid-write can only lose the
	// staged temp, never leave a truncated document at the canonical
	// path.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("offline ranges: stage %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("offline ranges: swap %s: %w", path, err)
	}
	return nil
}

// validateBinding checks the binding at index self against the hard
// rejects and the whole-document uniqueness sets (ids, titles,
// non-zero shikimori ids — two ranges on one entry would break the
// GetByShikimoriID sync dispatch).
func validateBinding(all []RangeBinding, self int) error {
	b := all[self]
	if b.ID == "" {
		return fmt.Errorf("offline ranges: range %q: empty id", b.Title)
	}
	if !rangeIDPattern.MatchString(b.ID) {
		return fmt.Errorf("offline ranges: range id %q must match %s", b.ID, rangeIDPattern.String())
	}
	if b.Title == "" {
		return fmt.Errorf("offline ranges: range %s: empty title", b.ID)
	}
	switch b.Match.Kind {
	case "glob":
		if len(b.Match.Patterns) == 0 {
			return fmt.Errorf("offline ranges: range %s: glob match needs at least one pattern", b.ID)
		}
	case "exact":
		if len(b.Match.Files) == 0 {
			return fmt.Errorf("offline ranges: range %s: exact match needs at least one file", b.ID)
		}
	default:
		return fmt.Errorf("offline ranges: range %s: unknown match kind %q (want glob or exact)", b.ID, b.Match.Kind)
	}
	for i, other := range all {
		if i == self {
			continue
		}
		if other.ID == b.ID {
			return fmt.Errorf("offline ranges: duplicate range id %q", b.ID)
		}
		if other.Title == b.Title {
			return fmt.Errorf("offline ranges: duplicate range title %q", b.Title)
		}
		if b.ShikimoriID != 0 && other.ShikimoriID == b.ShikimoriID {
			return fmt.Errorf("offline ranges: shikimori id %d is already bound to range %q", b.ShikimoriID, other.ID)
		}
	}
	return nil
}

// RangeOverlaps maps every file base name provably matched by more
// than one range onto the sorted range ids. Only exact names are
// provable without the directory scan: a listed file matched by
// another range's glob, or listed in two exact ranges. Glob-vs-glob
// overlap needs real files and stays a load-time-legal, editor-
// confirmable warning (research §7).
func RangeOverlaps(rf *RangesFile) map[string][]string {
	if rf == nil {
		return nil
	}
	// globRanges pairs every glob range with its patterns.
	type globRange struct {
		id       string
		patterns []string
	}
	var globRanges []globRange
	for _, r := range rf.Ranges {
		if r.Match.Kind == "glob" {
			globRanges = append(globRanges, globRange{id: r.ID, patterns: r.Match.Patterns})
		}
	}

	out := map[string][]string{}
	add := func(file, id string) {
		for _, existing := range out[file] {
			if existing == id {
				return
			}
		}
		out[file] = append(out[file], id)
	}
	// An exact file is matched by its own range by definition; a
	// glob pattern matching it adds the second range.
	for _, r := range rf.Ranges {
		if r.Match.Kind != "exact" {
			continue
		}
		for _, file := range r.Match.Files {
			add(file, r.ID)
			for _, g := range globRanges {
				for _, pattern := range g.patterns {
					if ok, err := filepath.Match(pattern, file); err == nil && ok {
						add(file, g.id)
						break
					}
				}
			}
		}
	}
	// The same file listed by two different exact ranges.
	for a := range len(rf.Ranges) {
		if rf.Ranges[a].Match.Kind != "exact" {
			continue
		}
		for b := a + 1; b < len(rf.Ranges); b++ {
			if rf.Ranges[b].Match.Kind != "exact" {
				continue
			}
			for _, fa := range rf.Ranges[a].Match.Files {
				for _, fb := range rf.Ranges[b].Match.Files {
					if fa == fb {
						add(fa, rf.Ranges[a].ID)
						add(fa, rf.Ranges[b].ID)
					}
				}
			}
		}
	}
	for file := range out {
		if len(out[file]) < 2 {
			delete(out, file)
		}
		sort.Strings(out[file])
	}
	return out
}

// mediaExtensions are the file extensions ScanMediaFiles picks up
// (lowercase compare; the set covers the downloader's containers and
// the common hand-placed rips).
var mediaExtensions = map[string]struct{}{
	".mkv":  {},
	".mp4":  {},
	".avi":  {},
	".m4v":  {},
	".mov":  {},
	".ts":   {},
	".webm": {},
}

// ScanMediaFiles lists the directory's media files: regular files
// only, base names, byte-sorted.
func ScanMediaFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if _, ok := mediaExtensions[ext]; !ok {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// isRegularFile stats the dir entry and reports a readable regular
// file (missing files silently drop — exact lists pre-date the disk).
func isRegularFile(dir, name string) bool {
	info, err := os.Stat(filepath.Join(dir, name))
	return err == nil && info.Mode().IsRegular()
}

// resolveRangeFiles resolves one range's Match onto the directory:
// glob — patterns against the base name, pattern order is episode
// order, matches inside one pattern sort byte-wise, a file matched by
// an earlier pattern is never re-matched (first-wins); exact — the
// listed order is the episode order, missing files drop silently.
// A non-empty Match.Quality keeps only the files whose base name
// matches the quality glob (owner amendment A1: a multi-quality
// folder must not produce phantom positions).
func resolveRangeFiles(dir string, match RangeMatch) []string {
	var matched []string
	claimed := map[string]struct{}{}
	switch match.Kind {
	case "glob":
		files := ScanMediaFiles(dir)
		for _, pattern := range match.Patterns {
			for _, file := range files {
				if _, taken := claimed[file]; taken {
					continue
				}
				ok, err := filepath.Match(pattern, file)
				if err != nil || !ok {
					continue
				}
				claimed[file] = struct{}{}
				matched = append(matched, file)
			}
		}
	case "exact":
		for _, file := range match.Files {
			if !isRegularFile(dir, file) {
				continue
			}
			if _, taken := claimed[file]; taken {
				continue
			}
			claimed[file] = struct{}{}
			matched = append(matched, file)
		}
	default:
		return nil
	}
	if match.Quality == "" {
		return matched
	}
	kept := make([]string, 0, len(matched))
	for _, file := range matched {
		if qualityMatches(match.Quality, file) {
			kept = append(kept, file)
		}
	}
	return kept
}

// qualityMatches reports whether the quality glob selects one file:
// the pattern matches the whole base name (so "*1080p*" works) or any
// delimiter-separated token of it (so the plain "1080p" of the owner
// example matches "EP_1_1080p.mkv").
func qualityMatches(quality, file string) bool {
	if ok, err := filepath.Match(quality, file); err == nil && ok {
		return true
	}
	for _, token := range strings.FieldsFunc(file, func(r rune) bool {
		return r == '_' || r == '-' || r == '.' || r == ' '
	}) {
		if ok, err := filepath.Match(quality, token); err == nil && ok {
			return true
		}
	}
	return false
}

// RangeView is one range expanded against the folder: the binding and
// its entries with EpisodeNum REWRITTEN to position labels "1".."N"
// (the index's own episode labels never surface inside a range —
// label-collision immunity).
type RangeView struct {
	Binding RangeBinding
	Entries []Entry
}

// ExpandRanges expands the ranges file against the folder contents.
//
// Per position: an index entry whose RelativePath base name equals
// the file donates its stream metadata (VideoKey/AudioKey/Quality/
// Container/ChapterTypes, RelativePath kept); otherwise the position
// degrades to the honest pseudo-entry (VideoKey "file", AudioKey "—",
// Quality 0 — one position is one playable file). Files matched by no
// range land in the limbo bucket under their base-name label. Zero-
// file ranges are omitted from the views (they stay in the file — a
// glob can come alive later).
func ExpandRanges(dir string, index Snapshot, rf *RangesFile) (views []RangeView, unsorted []Entry) {
	if rf == nil {
		return nil, nil
	}
	byBase := make(map[string]Entry, len(index.Entries))
	for _, e := range index.Entries {
		byBase[filepath.Base(e.RelativePath)] = e
	}

	matched := map[string]struct{}{}
	for _, binding := range rf.Ranges {
		files := resolveRangeFiles(dir, binding.Match)
		if len(files) == 0 {
			continue
		}
		view := RangeView{Binding: binding, Entries: make([]Entry, 0, len(files))}
		for pos, file := range files {
			label := strconv.Itoa(pos + 1)
			matched[file] = struct{}{}
			view.Entries = append(view.Entries, entryForFile(dir, file, label, byBase))
		}
		views = append(views, view)
	}
	for _, file := range ScanMediaFiles(dir) {
		if _, taken := matched[file]; taken {
			continue
		}
		unsorted = append(unsorted, entryForFile(dir, file, file, byBase))
	}
	return views, unsorted
}

// entryForFile synthesizes the playable entry of one file: index
// metadata when the folder's index knows the file, the pseudo-entry
// otherwise.
func entryForFile(dir, file, label string, byBase map[string]Entry) Entry {
	if e, ok := byBase[file]; ok {
		e.EpisodeNum = label
		return e
	}
	return Entry{
		EpisodeNum:   label,
		AnimeID:      nil,
		VideoKey:     "file",
		AudioKey:     "—",
		Quality:      0,
		Container:    strings.TrimPrefix(filepath.Ext(file), "."),
		RelativePath: file,
	}
}

// SyntheticSourceURL composes the range's progress source key:
// source_id "offline" + this URL give the anime_progress row its
// unique identity (the relDir is slash-separated and relative to the
// watched root, so a moved/remounted folder keeps its progress).
func SyntheticSourceURL(relDir, rangeID string) string {
	return "anicli-offline:" + relDir + "#" + rangeID
}

// SuggestRanges proposes the folder's groupings deterministically:
// media files grouped by their leading non-digit prefix ("EP_13_x.mkv"
// → "EP_", "OVA_2.mkv" → "OVA_", "S2E01.mkv" → "S"), one unbound
// glob binding per group in sorted-prefix order. Digit-leading files
// carry no prefix and are NOT suggested (they land in the editor's
// unsorted pool). The result is a disposable suggestion — never
// auto-saved, the owner shapes it in the binding editor.
func SuggestRanges(dir string) []RangeBinding {
	files := ScanMediaFiles(dir)
	if len(files) == 0 {
		return nil
	}
	prefixes := map[string]struct{}{}
	for _, file := range files {
		if p := leadingNonDigitPrefix(file); p != "" {
			prefixes[p] = struct{}{}
		}
	}
	sorted := make([]string, 0, len(prefixes))
	for p := range prefixes {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	out := make([]RangeBinding, 0, len(sorted))
	for i, prefix := range sorted {
		title := strings.TrimRight(prefix, "_-. ")
		if title == "" {
			title = fmt.Sprintf("range-%d", i+1)
		}
		out = append(out, RangeBinding{
			ID:    fmt.Sprintf("r%d", i+1),
			Title: title,
			Match: RangeMatch{Kind: "glob", Patterns: []string{prefix + "*"}},
		})
	}
	return out
}

// leadingNonDigitPrefix returns the file name's run of leading
// non-digit characters ("" when the name starts with a digit).
func leadingNonDigitPrefix(file string) string {
	end := 0
	for end < len(file) {
		r := rune(file[end])
		if r < 0x80 && r >= '0' && r <= '9' {
			break
		}
		end++
	}
	return file[:end]
}
