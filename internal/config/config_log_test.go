package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLogDefaults: the zero-config log is INFO in the config dir,
// rotated daily, keeping 7 rotated files.
func TestLogDefaults(t *testing.T) {
	t.Parallel()

	def := Default()
	if def.Log.Level != "info" {
		t.Fatalf("default level = %q, want info", def.Log.Level)
	}
	if def.Log.File != "" {
		t.Fatalf("default file = %q, want empty (config-dir resolution)", def.Log.File)
	}
	if def.Log.Rotation != 24*time.Hour {
		t.Fatalf("default rotation = %v, want 24h", def.Log.Rotation)
	}
	if def.Log.MaxFiles != 7 {
		t.Fatalf("default max_files = %d, want 7", def.Log.MaxFiles)
	}
}

// TestLogSectionParsing: settings.toml's [log] section decodes onto
// the struct.
func TestLogSectionParsing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, SettingsFileName)
	src := `
[log]
level = "debug"
file = "/var/log/anicli/custom.log"
rotation = "48h"
max_files = 3
`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Log.Level != "debug" || s.Log.File != "/var/log/anicli/custom.log" {
		t.Fatalf("log section = %+v", s.Log)
	}
	if s.Log.Rotation != 48*time.Hour {
		t.Fatalf("rotation = %v, want 48h", s.Log.Rotation)
	}
	if s.Log.MaxFiles != 3 {
		t.Fatalf("max_files = %d, want 3", s.Log.MaxFiles)
	}
}

// TestLogSectionPartial: a partial [log] section keeps the defaults
// for the keys it omits.
func TestLogSectionPartial(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, SettingsFileName)
	src := `
[log]
level = "warn"
`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Log.Level != "warn" {
		t.Fatalf("level = %q, want warn", s.Log.Level)
	}
	if s.Log.File != "" {
		t.Fatalf("file = %q, want the empty (config-dir) default", s.Log.File)
	}
	if s.Log.Rotation != 24*time.Hour {
		t.Fatalf("rotation = %v, want the 24h default", s.Log.Rotation)
	}
	if s.Log.MaxFiles != 7 {
		t.Fatalf("max_files = %d, want the 7 default", s.Log.MaxFiles)
	}
}

// TestSlogLevelMapping: the level string maps onto slog levels;
// unknown and empty mean INFO.
func TestSlogLevelMapping(t *testing.T) {
	t.Parallel()

	for tc, want := range map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
		"":      slog.LevelInfo,
		"DEBUG": slog.LevelDebug,
		"junk":  slog.LevelInfo,
	} {
		if got := SlogLevel(tc); got != want {
			t.Fatalf("SlogLevel(%q) = %v, want %v", tc, got, want)
		}
	}
}

// TestResolveLogFile: empty file resolves into the config dir (the
// owner ruling: the log lives with settings.toml, not /tmp).
func TestResolveLogFile(t *testing.T) {
	t.Parallel()

	cfgDir := string(filepath.Separator) + filepath.Join("home", "u", ".config", "anicli")

	if got := (Log{}).ResolveFile(cfgDir); got != filepath.Join(cfgDir, "anicli.log") {
		t.Fatalf("resolved = %q, want the config-dir default", got)
	}
	custom := Log{File: "/var/log/anicli/custom.log"}
	if got := custom.ResolveFile(cfgDir); got != "/var/log/anicli/custom.log" {
		t.Fatalf("custom file = %q, want it kept", got)
	}
}

// TestRotateLogFile: a file older than the rotation age is renamed to
// anicli-<timestamp>.log; a fresh file is untouched.
func TestRotateLogFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	logPath := filepath.Join(dir, "anicli.log")

	if err := os.WriteFile(logPath, []byte("old logs"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(logPath, old, old); err != nil {
		t.Fatal(err)
	}

	rotated, err := RotateLogFile(logPath, 24*time.Hour, 7)
	if err != nil {
		t.Fatal(err)
	}
	if !rotated {
		t.Fatal("an aged file must rotate")
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("the original must be gone after rotation, stat err = %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "anicli-*.log"))
	if len(matches) != 1 {
		t.Fatalf("rotated files = %v, want one", matches)
	}

	// A fresh file is left alone.
	if err := os.WriteFile(logPath, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	rotated, err = RotateLogFile(logPath, 24*time.Hour, 7)
	if err != nil {
		t.Fatal(err)
	}
	if rotated {
		t.Fatal("a fresh file must not rotate")
	}
}

// TestRotateLogFilePrune: beyond max_files the OLDEST rotated files
// are pruned.
func TestRotateLogFilePrune(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	logPath := filepath.Join(dir, "anicli.log")

	// Seed 4 rotated files with distinct ages (the prune targets the
	// oldest by mtime).
	now := time.Now()
	for i, name := range []string{"anicli-2026-01-01T000000.log", "anicli-2026-01-02T000000.log", "anicli-2026-01-03T000000.log", "anicli-2026-01-04T000000.log"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		stamp := now.Add(-time.Duration(10-i) * time.Hour)
		if err := os.Chtimes(p, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(logPath, []byte("current"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(logPath, now, now); err != nil {
		t.Fatal(err)
	}

	if _, err := RotateLogFile(logPath, 1*time.Hour, 2); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "anicli-*.log"))
	if len(matches) != 2 {
		t.Fatalf("rotated files after prune = %v, want 2", matches)
	}
	// The newest two (2026-01-04, 2026-01-03) survive; the January 1
	// file must be gone.
	for _, p := range matches {
		if strings.Contains(p, "2026-01-01") || strings.Contains(p, "2026-01-02") {
			t.Fatalf("the oldest rotated file survived the prune: %v", matches)
		}
	}
}

// TestRotateMissingFile: no log file yet — nothing to do, no error.
func TestRotateMissingFile(t *testing.T) {
	t.Parallel()

	rotated, err := RotateLogFile(filepath.Join(t.TempDir(), "anicli.log"), 24*time.Hour, 7)
	if err != nil || rotated {
		t.Fatalf("missing file: rotated=%v err=%v, want false nil", rotated, err)
	}
}

// TestLogSectionMalformed: the fail-loud contract covers [log] too —
// a bad rotation duration fails at parse, an unknown [log] key fails
// with the dotted key path.
func TestLogSectionMalformed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	badDuration := filepath.Join(dir, "bad_duration.toml")
	src := `
[log]
rotation = "junk"
`
	if err := os.WriteFile(badDuration, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(badDuration); err == nil {
		t.Fatal("a junk rotation duration must fail loud at parse")
	}

	unknownKey := filepath.Join(dir, "unknown_key.toml")
	src = `
[log]
max_files_typo = 3
`
	if err := os.WriteFile(unknownKey, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(unknownKey)
	if err == nil {
		t.Fatal("an unknown [log] key must fail loud")
	}
	if !strings.Contains(err.Error(), "log.max_files_typo") {
		t.Fatalf("the unknown-key error must name the key path: %v", err)
	}
}

// seedRotated seeds n rotated files with distinct mtimes (oldest
// first) and returns the log path inside dir.
func seedRotated(t *testing.T, dir string, n int) string {
	t.Helper()
	logPath := filepath.Join(dir, "anicli.log")
	now := time.Now()
	for i := 1; i <= n; i++ {
		p := filepath.Join(dir, fmt.Sprintf("anicli-2026-01-%02dT000000.log", i))
		if err := os.WriteFile(p, []byte(p), 0o600); err != nil {
			t.Fatal(err)
		}
		stamp := now.Add(-time.Duration(n+1-i) * time.Hour)
		if err := os.Chtimes(p, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(logPath, []byte("current"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(logPath, now, now); err != nil {
		t.Fatal(err)
	}
	return logPath
}

// TestRotatePruneSkipsDanglingEntries (169-H): rotated entries that
// cannot be Stat'ed (a dangling symlink, a file vanished mid-prune)
// were skipped from the survivor list — with fewer real survivors than
// maxFiles the slice bound files[:len-maxFiles] went NEGATIVE and
// panicked AT STARTUP (root.go calls RotateLogFile before any
// recover). No panic below the cap, and correct pruning above it.
func TestRotatePruneSkipsDanglingEntries(t *testing.T) {
	t.Parallel()

	t.Run("no panic below the cap", func(t *testing.T) {
		dir := t.TempDir()
		logPath := seedRotated(t, dir, 1)
		for _, name := range []string{"anicli-2026-01-05T000000.log", "anicli-2026-01-06T000000.log"} {
			if err := os.Symlink(filepath.Join(dir, "vanished.log"), filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
		}
		// RED today: files[:1-2] panics at startup.
		if _, err := RotateLogFile(logPath, 1*time.Hour, 2); err != nil {
			t.Fatalf("RotateLogFile: %v", err)
		}
		if _, err := os.Stat(logPath); err != nil {
			t.Fatalf("the current log must survive: %v", err)
		}
	})

	t.Run("prunes correctly above the cap", func(t *testing.T) {
		dir := t.TempDir()
		logPath := seedRotated(t, dir, 4)
		for _, name := range []string{"anicli-2026-01-05T000000.log", "anicli-2026-01-06T000000.log"} {
			if err := os.Symlink(filepath.Join(dir, "vanished.log"), filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
		}

		if _, err := RotateLogFile(logPath, 1*time.Hour, 2); err != nil {
			t.Fatalf("RotateLogFile: %v", err)
		}
		// Count REAL files only — dangling entries may remain on disk.
		remaining, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var real []string
		for _, e := range remaining {
			if strings.HasPrefix(e.Name(), "anicli-") && strings.HasSuffix(e.Name(), ".log") {
				if info, err := e.Info(); err == nil && info.Mode()&os.ModeSymlink == 0 {
					real = append(real, e.Name())
				}
			}
		}
		if len(real) != 2 {
			t.Fatalf("real rotated files after prune = %v, want the 2 newest", real)
		}
		for _, name := range real {
			if strings.Contains(name, "2026-01-01") || strings.Contains(name, "2026-01-02") {
				t.Fatalf("the oldest real rotated file survived: %v", real)
			}
		}
	})
}

// TestRotatePruneBracketDir (169-H): filepath.Glob reads [ and ] as a
// character class, so a bracketed directory path never matched and the
// graveyard grew forever. Enumeration + plain string matching prunes
// there too.
func TestRotatePruneBracketDir(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "season [2026]")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	logPath := seedRotated(t, dir, 4)

	if _, err := RotateLogFile(logPath, 1*time.Hour, 2); err != nil {
		t.Fatalf("RotateLogFile: %v", err)
	}
	remaining, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range remaining {
		if strings.HasPrefix(e.Name(), "anicli-") && strings.HasSuffix(e.Name(), ".log") {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("rotated files after prune = %d, want 2 (the glob shape never matched here)", count)
	}
}
