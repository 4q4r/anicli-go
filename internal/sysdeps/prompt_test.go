package sysdeps

// PR148 prompt tests: the Y/n reader semantics and the i18n inventory
// helper. The reader never blocks on a non-TTY — the TTY gate lives in
// EnsureStartup, so the reader is only reached when a TTY is proven.

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestReadYnAcceptsYesFormsAndEmptyDefault(t *testing.T) {
	cases := map[string]bool{
		"y\n": true, "Y\n": true, "yes\n": true, "YES\n": true,
		"д\n": true, "Д\n": true, "да\n": true, "Да\n": true,
		"\n": true, // Enter — the displayed default
	}
	for in, want := range cases {
		if got := readYn(bufio.NewReader(strings.NewReader(in))); got != want {
			t.Errorf("readYn(%q) = %v, want %v", in, got, want)
		}
	}
	// Padded yes: surrounding whitespace is trimmed before matching.
	if !readYn(bufio.NewReader(strings.NewReader(" y  \n"))) {
		t.Error(`readYn(" y  \n") = false, want true (whitespace is trimmed)`)
	}
}

func TestReadYnRejectsEverythingElse(t *testing.T) {
	cases := []string{"n\n", "N\n", "no\n", "нет\n", "0\n", "yep\n", "junk\n"}
	for _, in := range cases {
		if readYn(bufio.NewReader(strings.NewReader(in))) {
			t.Errorf("readYn(%q) = true, want false (fail-safe)", in)
		}
	}
	if readYn(bufio.NewReader(strings.NewReader(""))) {
		t.Error("readYn(EOF) = true, want false (fail-safe)")
	}
	if readYn(bufio.NewReader(closedReader{})) {
		t.Error("readYn(closed reader) = true, want false")
	}
}

// closedReader always fails, modeling a closed/collapsed stdin.
type closedReader struct{}

func (closedReader) Read([]byte) (int, error) { return 0, os.ErrClosed }

// TestStdinIsTTYOnRejectsNonTerminals pins the gate against the
// classic misclassifications: a pipe (docker/systemd) and /dev/null
// (a character device that is NOT a terminal) must both resolve to
// false; only a real terminal answers the ioctl.
func TestStdinIsTTYOnRejectsNonTerminals(t *testing.T) {
	pipeR, _, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() { _ = pipeR.Close() })
	if stdinIsTTYOn(pipeR) {
		t.Error("stdinIsTTYOn(pipe) = true, want false")
	}
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	t.Cleanup(func() { _ = null.Close() })
	if stdinIsTTYOn(null) {
		t.Error("stdinIsTTYOn(/dev/null) = true, want false (a char device is not a terminal)")
	}
}

// TestReadYnTwoQuestionsSharedReader pins the shared-buffer contract:
// two answers in one stream must both be seen (a fresh buffer per
// question would swallow the second line).
func TestReadYnTwoQuestionsSharedReader(t *testing.T) {
	in := bufio.NewReader(strings.NewReader("y\nn\n"))
	if !readYn(in) {
		t.Error("first answer = false, want true")
	}
	if readYn(in) {
		t.Error("second answer = true, want false")
	}
}

// referencedKeys extracts every i18n key literal referenced by this
// package's sources, so a renamed key without a table update fails the
// build (mirrors the i18n package's tui-scanning guard).
func referencedKeys(t *testing.T) []string {
	t.Helper()
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob sources: %v", err)
	}
	re := regexp.MustCompile(`i18n\.T\("([a-z_.]+)"`)
	seen := map[string]bool{}
	var out []string
	for _, f := range matches {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f) //nolint:gosec // the test reads its own package sources
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range re.FindAllStringSubmatch(string(data), -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				out = append(out, m[1])
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("no i18n keys found — wrong working directory?")
	}
	return out
}
