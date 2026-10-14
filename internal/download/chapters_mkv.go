package download

// PR165b T7: the FFMETADATA → mkvmerge simple-chapters converter.
// mkvmerge chapters are POINTS on the timeline, not intervals — the
// END boundaries are ignored; each [CHAPTER] record renders one
// "CHAPTERn=HH:MM:SS.nnn / CHAPTERnNAME=<title>" pair. A missing or
// empty source renders "" (the flag is omitted upstream); a broken
// record is a typed error, never a silently truncated file.

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// ConvertChaptersToMkv converts the FFMETADATA chapter file into the
// mkvmerge simple format, written to a temp file in outDir (the
// caller renames/removes it with the assembly). A missing or empty
// source is ("", nil).
func ConvertChaptersToMkv(ffmetaPath, outDir string) (string, error) {
	raw, err := os.ReadFile(ffmetaPath) //nolint:gosec // G304: the path is the caller-provided temp metadata file
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("download: chapters: %w", err)
	}
	records, err := parseFFMetadataChapters(string(raw))
	if err != nil {
		return "", fmt.Errorf("download: chapters: %w", err)
	}
	if len(records) == 0 {
		return "", nil
	}

	var b strings.Builder
	for i, rec := range records {
		ms := chapterMillis(rec)
		fmt.Fprintf(&b, "CHAPTER%d=%s\nCHAPTER%dNAME=%s\n", i+1, formatChapterTime(ms), i+1, rec.title)
	}

	out, err := os.CreateTemp(outDir, "chapters-*.simple")
	if err != nil {
		return "", fmt.Errorf("download: chapters: %w", err)
	}
	defer func() { _ = out.Close() }()
	if _, err := out.WriteString(b.String()); err != nil {
		return "", fmt.Errorf("download: chapters: %w", err)
	}
	return out.Name(), nil
}

// ffChapter is one parsed [CHAPTER] record. startTicks is the raw
// START value in TIMEBASE ticks.
type ffChapter struct {
	timebaseNum, timebaseDen int64
	startTicks               int64
	title                    string
}

// chapterMillis renders the chapter point in whole milliseconds
// (start × num × 1000 / den, half-up).
func chapterMillis(rec ffChapter) int64 {
	return (rec.startTicks*rec.timebaseNum*1000 + rec.timebaseDen/2) / rec.timebaseDen
}

// parseFFMetadataChapters walks the FFMETADATA [CHAPTER] blocks.
// Structural breakage — a field outside a block, a missing TIMEBASE,
// a zero timebase denominator, a non-numeric or negative START — is
// a typed error. A missing title stays valid (an unnamed chapter).
func parseFFMetadataChapters(text string) ([]ffChapter, error) {
	var (
		records []ffChapter
		cur     *ffChapter
		inMeta  bool // inside the global ;FFMETADATA header — skip it
	)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, ";FFMETADATA"):
			inMeta = true
			continue
		case line == "[CHAPTER]":
			records = append(records, ffChapter{timebaseDen: 1})
			cur = &records[len(records)-1]
			continue
		case line == "[STREAM]" || line == "[FORMAT]":
			inMeta = true
			cur = nil
			continue
		case line == "":
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			// An unknown section header: the global-metadata state
			// applies until the next recognized header.
			inMeta = true
			cur = nil
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			return nil, fmt.Errorf("malformed line %q", line)
		}
		if cur == nil {
			if !inMeta {
				return nil, fmt.Errorf("field %q outside any section", key)
			}
			continue // a global-metadata field — not chapter data
		}
		switch key {
		case "TIMEBASE":
			num, den, ok := strings.Cut(value, "/")
			if !ok {
				return nil, fmt.Errorf("TIMEBASE %q has no denominator", value)
			}
			n, err := parseInt64(num)
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("TIMEBASE numerator %q", num)
			}
			d, err := parseInt64(den)
			if err != nil || d <= 0 {
				return nil, fmt.Errorf("TIMEBASE denominator %q", den)
			}
			cur.timebaseNum, cur.timebaseDen = n, d
		case "START":
			ticks, err := parseInt64(value)
			if err != nil || ticks < 0 {
				return nil, fmt.Errorf("START %q", value)
			}
			cur.startTicks = ticks
		case "title":
			cur.title = value
		}
	}
	for i, rec := range records {
		if rec.timebaseNum == 0 {
			return nil, fmt.Errorf("chapter %d has no TIMEBASE", i+1)
		}
	}
	return records, nil
}

func parseInt64(s string) (int64, error) {
	var v int64
	if s == "" {
		return 0, errors.New("empty number")
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("not a number: %q", s)
		}
		if v > (1<<62)/10 {
			return 0, fmt.Errorf("number overflow: %q", s)
		}
		v = v*10 + int64(s[i]-'0')
	}
	return v, nil
}

// formatChapterTime renders milliseconds as HH:MM:SS.nnn (the
// mkvmerge simple-chapters timestamp shape).
func formatChapterTime(ms int64) string {
	h, rest := ms/3_600_000, ms%3_600_000
	m, rest2 := rest/60_000, rest%60_000
	s, milli := rest2/1000, rest2%1000
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, milli)
}
