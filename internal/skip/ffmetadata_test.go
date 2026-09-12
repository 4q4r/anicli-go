package skip

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func jsonUnmarshalString(s string, v any) error {
	return json.Unmarshal([]byte(s), v)
}

func toFloat(v any) (float64, error) {
	n, ok := v.(float64)
	if !ok {
		return 0, errors.New("not a number")
	}
	return n, nil
}

func isErrUnsupportedSkipType(err error) bool {
	return errors.Is(err, ErrUnsupportedSkipType)
}

// TestGenerateFFMetadataGolden pins the full FFMETADATA1 rendering:
// header, prologue gap filler, main-content gap filler, typed labels and
// the epilogue tail rule for ed endings.
func TestGenerateFFMetadataGolden(t *testing.T) {
	t.Parallel()

	got := GenerateFFMetadata([]Interval{
		{SkipType: "op", StartTime: 0, EndTime: 90, EpisodeLength: 1440},
		{SkipType: "ed", StartTime: 1300, EndTime: 1400, EpisodeLength: 1440},
	})

	want := strings.Join([]string{
		";FFMETADATA1",
		"",
		"[CHAPTER]",
		"TIMEBASE=1/1000",
		"START=0",
		"END=90000",
		"title=Опенинг",
		"",
		"[CHAPTER]",
		"TIMEBASE=1/1000",
		"START=90000",
		"END=1300000",
		"title=Основной контент",
		"",
		"[CHAPTER]",
		"TIMEBASE=1/1000",
		"START=1300000",
		"END=1400000",
		"title=Эндинг",
		"",
		"[CHAPTER]",
		"TIMEBASE=1/1000",
		"START=1400000",
		"END=1440000",
		"title=Эпилог/Превью",
		"",
	}, "\n")

	if got != want {
		t.Errorf("FFMETADATA mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestGenerateFFMetadataPrologueAndRecap pins: prologue appears when the
// first skip starts after 5s, non-op/ed types keep their labels, and a
// missing episode_length falls back to 1440s.
//
// Ported quirk (replicated + documented, PR7 aniboom precedent): the
// python generator never advances current_time after inserting the
// prologue, so the first gap also emits a duplicate "Основной контент"
// chapter covering the prologue span. Kept verbatim for parity.
func TestGenerateFFMetadataPrologueAndRecap(t *testing.T) {
	t.Parallel()

	got := GenerateFFMetadata([]Interval{
		{SkipType: "recap", StartTime: 20, EndTime: 60, EpisodeLength: 0},
	})

	want := strings.Join([]string{
		";FFMETADATA1",
		"",
		"[CHAPTER]",
		"TIMEBASE=1/1000",
		"START=0",
		"END=20000",
		"title=Пролог",
		"",
		"[CHAPTER]",
		"TIMEBASE=1/1000",
		"START=0",
		"END=20000",
		"title=Основной контент",
		"",
		"[CHAPTER]",
		"TIMEBASE=1/1000",
		"START=20000",
		"END=60000",
		"title=Пересказ",
		"",
		"[CHAPTER]",
		"TIMEBASE=1/1000",
		"START=60000",
		"END=1440000",
		"title=Основной контент",
		"",
	}, "\n")

	if got != want {
		t.Errorf("FFMETADATA mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestGenerateFFMetadataLabels pins the type->label table including the
// mixed-* aliases and the fallback label.
func TestGenerateFFMetadataLabels(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"op":       "Опенинг",
		"mixed-op": "Опенинг",
		"ed":       "Эндинг",
		"mixed-ed": "Эндинг",
		"recap":    "Пересказ",
		"preview":  "Превью",
		"eyecatch": "Айкэтч",
		"sponsor":  "Спонсор",
		"whatever": "Скип",
	}

	for skipType, wantLabel := range cases {
		content := GenerateFFMetadata([]Interval{
			// Single interval inside the episode; episode_length equals
			// its end so no tail chapter is emitted.
			{SkipType: skipType, StartTime: 10, EndTime: 20, EpisodeLength: 20},
		})
		if !strings.Contains(content, "title="+wantLabel+"\n") {
			t.Errorf("type %q: label %q missing from\n%s", skipType, wantLabel, content)
		}
	}
}

// TestGenerateFFMetadataPreviewTail pins the "Титры" tail title when the
// last interval is a preview.
func TestGenerateFFMetadataPreviewTail(t *testing.T) {
	t.Parallel()

	content := GenerateFFMetadata([]Interval{
		{SkipType: "preview", StartTime: 1380, EndTime: 1420, EpisodeLength: 1440},
	})
	if !strings.Contains(content, "title=Титры\n") {
		t.Errorf("preview tail title missing from\n%s", content)
	}
}

// TestGenerateFFMetadataEmpty pins the no-input contract: empty content,
// never a bare header.
func TestGenerateFFMetadataEmpty(t *testing.T) {
	t.Parallel()

	if got := GenerateFFMetadata(nil); got != "" {
		t.Errorf("GenerateFFMetadata(nil) = %q, want empty", got)
	}
	if got := GenerateFFMetadata([]Interval{}); got != "" {
		t.Errorf("GenerateFFMetadata(empty) = %q, want empty", got)
	}
}

// TestGenerateFFMetadataFractionalTruncation pins millisecond
// truncation toward zero (python int() semantics).
func TestGenerateFFMetadataFractionalTruncation(t *testing.T) {
	t.Parallel()

	content := GenerateFFMetadata([]Interval{
		{SkipType: "op", StartTime: 0.5, EndTime: 90.9, EpisodeLength: 91},
	})
	if !strings.Contains(content, "START=500\n") {
		t.Errorf("START=500 missing from\n%s", content)
	}
	if !strings.Contains(content, "END=90900\n") {
		t.Errorf("END=90900 missing from\n%s", content)
	}
}

// TestWriteChaptersFile pins the temp-file materialization used by mpv
// --chapters-file and ffmpeg -i: .ffmetadata suffix, exact content,
// cleanup support.
func TestWriteChaptersFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := GenerateFFMetadata([]Interval{
		{SkipType: "op", StartTime: 0, EndTime: 90, EpisodeLength: 1440},
	})

	path, err := WriteChaptersFile(dir, "anicli_", content)
	if err != nil {
		t.Fatalf("WriteChaptersFile: %v", err)
	}
	if filepath.Base(path) == dir {
		t.Fatalf("path %q not inside temp dir", path)
	}
	if !strings.HasSuffix(path, ".ffmetadata") {
		t.Errorf("path %q lacks .ffmetadata suffix", path)
	}
	if !strings.Contains(filepath.Base(path), "anicli_") {
		t.Errorf("path %q lacks anicli_ prefix", path)
	}

	got, err := os.ReadFile(path) //nolint:gosec // test reads its own temp file
	if err != nil {
		t.Fatalf("read chapters file: %v", err)
	}
	if string(got) != content {
		t.Errorf("content mismatch: got %q want %q", got, content)
	}
}
