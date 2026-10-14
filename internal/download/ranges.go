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
