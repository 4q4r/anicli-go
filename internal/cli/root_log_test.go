package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
)

// TestNewTUILoggerLivesInConfigDir: the log file resolves to
// <config dir>/anicli.log — never /tmp (owner ruling) — and the
// configured level gates what lands in it.
func TestNewTUILoggerLivesInConfigDir(t *testing.T) {
	dir := t.TempDir()
	tl := newTUILogger(config.Log{Level: "info"}, dir)
	defer tl.Close()

	if _, err := os.Stat(filepath.Join(dir, "anicli.log")); err != nil {
		t.Fatalf("the log file must exist in the config dir: %v", err)
	}

	tl.Info("visible line")
	tl.Debug("hidden debug line")

	data, err := os.ReadFile(filepath.Join(dir, "anicli.log")) //nolint:gosec // the test reads its own t.TempDir() log
	if err != nil {
		t.Fatal(err)
	}
	logged := string(data)
	if !strings.Contains(logged, "visible line") {
		t.Fatalf("the info line must land in the config-dir log: %q", logged)
	}
	if strings.Contains(logged, "hidden debug line") {
		t.Fatalf("debug must be filtered at level info: %q", logged)
	}
}

// TestNewTUILoggerDebugLevel: level=debug lets debug lines through.
func TestNewTUILoggerDebugLevel(t *testing.T) {
	dir := t.TempDir()
	tl := newTUILogger(config.Log{Level: "debug"}, dir)
	defer tl.Close()

	tl.Debug("dbg detail")

	data, _ := os.ReadFile(filepath.Join(dir, "anicli.log")) //nolint:gosec // the test reads its own t.TempDir() log
	if !strings.Contains(string(data), "dbg detail") {
		t.Fatalf("debug lines must pass at level debug: %q", string(data))
	}
}

// TestNewTUILoggerCustomFile: an explicit [log] file wins over the
// config-dir default.
func TestNewTUILoggerCustomFile(t *testing.T) {
	dir := t.TempDir()
	custom := filepath.Join(dir, "custom", "my.log")
	tl := newTUILogger(config.Log{File: custom}, dir)
	defer tl.Close()

	tl.Info("into custom")

	data, err := os.ReadFile(custom) //nolint:gosec // the test reads its own t.TempDir() log
	if err != nil {
		t.Fatalf("the custom log file: %v", err)
	}
	if !strings.Contains(string(data), "into custom") {
		t.Fatalf("custom log content = %q", string(data))
	}
}

// TestNewTUILoggerRotatesAgedFile: startup rotation renames an aged
// anicli.log and starts a fresh one.
func TestNewTUILoggerRotatesAgedFile(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "anicli.log")
	if err := os.WriteFile(logPath, []byte("yesterday"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(logPath, old, old); err != nil {
		t.Fatal(err)
	}

	tl := newTUILogger(config.Log{Level: "info"}, dir)
	defer tl.Close()
	tl.Info("today")

	matches, _ := filepath.Glob(filepath.Join(dir, "anicli-*.log"))
	if len(matches) != 1 {
		t.Fatalf("rotated file missing: %v", matches)
	}
	rotated, _ := os.ReadFile(matches[0]) //nolint:gosec // the test reads its own t.TempDir() log
	if !bytes.Contains(rotated, []byte("yesterday")) {
		t.Fatalf("the rotated file lost the old content: %q", string(rotated))
	}
	fresh, _ := os.ReadFile(logPath) //nolint:gosec // the test reads its own t.TempDir() log
	if !strings.Contains(string(fresh), "today") {
		t.Fatalf("the fresh log must carry new lines: %q", string(fresh))
	}
}

// TestNewTUILoggerUnwritableDegradesToDiscard: an unusable path
// degrades to a discard logger — never stderr inside the TUI.
func TestNewTUILoggerUnwritableDegradesToDiscard(t *testing.T) {
	// A regular FILE where a directory must be created: both the
	// mkdir and the open genuinely fail.
	base := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(base, []byte("a file, not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	tl := newTUILogger(config.Log{File: filepath.Join(base, "sub", "x.log")}, t.TempDir())
	defer tl.Close()
	if tl.f != nil {
		t.Fatal("an unwritable path must degrade to a discard logger")
	}
	tl.Info("swallowed") // must not panic
}
