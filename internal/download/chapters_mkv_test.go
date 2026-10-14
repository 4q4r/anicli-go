package download

// PR165b T7 tests: the FFMETADATA → mkvmerge simple-chapters
// converter. mkvmerge chapters are POINTS, not intervals — the END
// boundaries are ignored; milliseconds render HH:MM:SS.nnn.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const chapterFixture = `[CHAPTER]
TIMEBASE=1/1000
START=35000
END=92000
title=intro
[CHAPTER]
TIMEBASE=1/1000
START=1284000
END=1310000
title=outro
`

func TestConvertChaptersToMkv(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "chapters.txt")
	if err := os.WriteFile(src, []byte(chapterFixture), 0o600); err != nil { //nolint:gosec // G306: the test writes its own temp fixture
		t.Fatal(err)
	}
	got, err := ConvertChaptersToMkv(src, dir)
	if err != nil {
		t.Fatalf("ConvertChaptersToMkv: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(got) })
	raw, err := os.ReadFile(got) //nolint:gosec // G304: the test reads its own temp file
	if err != nil {
		t.Fatal(err)
	}
	want := "CHAPTER1=00:00:35.000\nCHAPTER1NAME=intro\nCHAPTER2=00:21:24.000\nCHAPTER2NAME=outro\n"
	if string(raw) != want {
		t.Fatalf("got %q, want %q", string(raw), want)
	}
	// The temp file sits next to the output directory it was asked for.
	if filepath.Dir(got) != dir {
		t.Fatalf("temp file %q outside the requested dir %q", got, dir)
	}
}

func TestConvertChaptersToMkvEmptyAndMissing(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ConvertChaptersToMkv(empty, dir)
	if err != nil || got != "" {
		t.Fatalf("empty file = %q, %v; want \"\", nil", got, err)
	}
	got, err = ConvertChaptersToMkv(filepath.Join(dir, "absent.txt"), dir)
	if err != nil || got != "" {
		t.Fatalf("missing file = %q, %v; want \"\", nil", got, err)
	}
}

func TestConvertChaptersToMkvRejectsBroken(t *testing.T) {
	dir := t.TempDir()
	broken := []string{
		"[CHAPTER]\nTIMEBASE=1/1000\nSTART=abc\nEND=100\ntitle=x\n", // non-numeric START
		"[CHAPTER]\nTIMEBASE=1/0\nSTART=1\nEND=2\ntitle=x\n",        // zero timebase denominator
		"[CHAPTER]\nSTART=1\nEND=2\n",                               // no timebase
		"[CHAPTER]\nTIMEBASE=1/1000\nSTART=-5\nEND=2\ntitle=x\n",    // negative start
		"title=orphan\n", // a field before any section
	}
	for i, body := range broken {
		src := filepath.Join(dir, "broken.txt")
		if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := ConvertChaptersToMkv(src, dir); err == nil {
			t.Fatalf("broken[%d] = %q accepted", i, strings.TrimSpace(got))
		}
	}
}

// TestConvertChaptersToMkvUnnamedChapter: a chapter without a title
// stays valid — mkvmerge accepts unnamed chapters; the NAME renders
// empty.
func TestConvertChaptersToMkvUnnamedChapter(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "unnamed.txt")
	if err := os.WriteFile(src, []byte("[CHAPTER]\nTIMEBASE=1/1000\nSTART=1000\nEND=2000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ConvertChaptersToMkv(src, dir)
	if err != nil {
		t.Fatalf("ConvertChaptersToMkv: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(got) })
	raw, err := os.ReadFile(got) //nolint:gosec // G304: the test reads its own temp file
	if err != nil {
		t.Fatal(err)
	}
	if want := "CHAPTER1=00:00:01.000\nCHAPTER1NAME=\n"; string(raw) != want {
		t.Fatalf("got %q, want %q", string(raw), want)
	}
}
