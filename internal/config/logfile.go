package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Log configures the TUI diagnostics file: where it lives, how loud
// it is and how it rotates.
type Log struct {
	// Level is the slog threshold: debug|info|warn|error (empty →
	// info). Unknown values degrade to info.
	Level string `toml:"level"`
	// File is the log file path; empty resolves to
	// <config dir>/anicli.log — the log lives with settings.toml,
	// never in /tmp.
	File string `toml:"file"`
	// Rotation is the age at which the file rotates on startup:
	// anicli.log → anicli-<timestamp>.log. Default 24h.
	Rotation time.Duration `toml:"rotation"`
	// MaxFiles bounds the rotated-file graveyard; the oldest are
	// pruned past it. Default 7.
	MaxFiles int `toml:"max_files"`
}

// SlogLevel maps the configured level string onto a slog level.
// Empty and unknown values degrade to info.
func SlogLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// ResolveFile resolves the log file path: the explicit File when set,
// otherwise anicli.log next to settings.toml.
func (l Log) ResolveFile(configDir string) string {
	if l.File != "" {
		return l.File
	}
	return filepath.Join(configDir, "anicli.log")
}

// RotateLogFile ages out the log file: when its mtime is older than
// rotation, it is renamed to anicli-<timestamp>.log and the rotated
// graveyard is pruned to the newest maxFiles entries. A missing or
// fresh file is a no-op. rotated reports whether a rename happened.
func RotateLogFile(path string, rotation time.Duration, maxFiles int) (rotated bool, err error) {
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat log %s: %w", path, err)
	}
	// The graveyard is pruned on every startup — lowering max_files
	// adapts the existing graveyard even before the next rotation.
	if err := pruneRotated(path, maxFiles); err != nil {
		return false, fmt.Errorf("prune rotated logs for %s: %w", path, err)
	}

	if rotation > 0 && time.Since(st.ModTime()) <= rotation {
		return false, nil
	}

	stamp := st.ModTime().Format("2006-01-02T150405")
	rotatedPath := strings.TrimSuffix(path, filepath.Ext(path)) +
		"-" + stamp + filepath.Ext(path)
	if err := os.Rename(path, rotatedPath); err != nil {
		return false, fmt.Errorf("rotate log %s: %w", path, err)
	}
	return true, nil
}

// pruneRotated removes the oldest anicli-*.log siblings beyond
// maxFiles (0 or negative disables pruning).
func pruneRotated(path string, maxFiles int) error {
	if maxFiles <= 0 {
		return nil
	}
	base := filepath.Base(path)
	prefix := strings.TrimSuffix(base, filepath.Ext(base)) + "-"
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), prefix+"*"+filepath.Ext(path)))
	if err != nil {
		return fmt.Errorf("glob rotated logs: %w", err)
	}
	if len(matches) <= maxFiles {
		return nil
	}

	type aged struct {
		path string
		mod  time.Time
	}
	files := make([]aged, 0, len(matches))
	for _, m := range matches {
		st, err := os.Stat(m)
		if err != nil {
			continue // vanished mid-prune: skip
		}
		files = append(files, aged{path: m, mod: st.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })

	for _, f := range files[:len(files)-maxFiles] {
		if err := os.Remove(f.path); err != nil {
			return fmt.Errorf("remove %s: %w", f.path, err)
		}
	}
	return nil
}
