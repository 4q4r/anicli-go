package audsync

// PR165b T6: the fMP4-discontinuity verification (the owner's
// directive). The hermetic half pins the playlist builder; the live
// half builds a REAL discontinuous HLS (two mpegts segments of
// different audio geometry joined by #EXT-X-DISCONTINUITY) and proves
// the three §1.3 claims: the pipeline decode is through (no holes),
// a window lands on its expected span, and the boundary-crossing
// window degrades honestly — the consensus verdict stands.

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// discontinuousPlaylist writes an HLS playlist joining the given
// segment files with an #EXT-X-DISCONTINUITY between every pair.
func discontinuousPlaylist(dir string, segments []string, durations []float64) (string, error) {
	if len(segments) != len(durations) || len(segments) < 2 {
		return "", errors.New("audsync: discontinuity fixture needs ≥2 segments with durations")
	}
	target := durations[0]
	for _, d := range durations {
		if d > target {
			target = d
		}
	}
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n")
	b.WriteString("#EXT-X-TARGETDURATION:" + strconv.Itoa(int(math.Ceil(target))) + "\n")
	for i, seg := range segments {
		if i > 0 {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		b.WriteString("#EXTINF:" + strconv.FormatFloat(durations[i], 'f', 3, 64) + ",\n" + seg + "\n")
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	path := filepath.Join(dir, "discont.m3u8")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil { //nolint:gosec // G306: the test writes its own temp playlist
		return "", err
	}
	return path, nil
}

func TestDiscontinuousPlaylistMarksBoundary(t *testing.T) {
	dir := t.TempDir()
	path, err := discontinuousPlaylist(dir, []string{"a.ts", "b.ts"}, []float64{160, 160})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the test reads its own temp playlist
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "#EXTM3U") || !strings.Contains(text, "#EXT-X-ENDLIST") {
		t.Fatalf("playlist is not HLS:\n%s", text)
	}
	a, b := strings.Index(text, "a.ts"), strings.Index(text, "b.ts")
	mark := strings.Index(text, "#EXT-X-DISCONTINUITY")
	if a < 0 || a >= mark || mark >= b {
		t.Fatalf("discontinuity tag must sit BETWEEN the segments:\n%s", text)
	}
}

func TestDiscontinuousPlaylistRejectsSingles(t *testing.T) {
	if _, err := discontinuousPlaylist(t.TempDir(), []string{"a.ts"}, []float64{160}); err == nil {
		t.Fatal("a single segment has no boundary — must error")
	}
}
