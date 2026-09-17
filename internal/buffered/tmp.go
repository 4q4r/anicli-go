package buffered

import (
	"log/slog"
	"os"
	"sync"
)

// activeDirs tracks the temp directories of in-flight downloads so an
// application exit while buffering cannot leave garbage in os.TempDir:
// the TUI teardown calls CleanupAll.
var (
	activeMu   sync.Mutex
	activeDirs = map[string]struct{}{}
)

// newTempDir creates the anicli-buffer-* working dir of one download
// and registers it for CleanupAll.
func newTempDir() (string, error) {
	dir, err := os.MkdirTemp("", "anicli-buffer-")
	if err != nil {
		return "", err
	}
	activeMu.Lock()
	activeDirs[dir] = struct{}{}
	activeMu.Unlock()
	return dir, nil
}

// releaseDir removes one download's temp dir and forgets it; safe to
// call twice. Failures are reported to log (nil logger = silent — the
// warnings NEVER go to slog's default, i.e. stderr in the TUI).
func releaseDir(dir string, log *slog.Logger) {
	activeMu.Lock()
	_, known := activeDirs[dir]
	delete(activeDirs, dir)
	activeMu.Unlock()
	if !known {
		return
	}
	if err := os.RemoveAll(dir); err != nil && log != nil {
		log.Warn("buffered: remove temp dir failed", "dir", dir, "error", err)
	}
}

// CleanupAll removes every registered temp dir (application teardown).
func CleanupAll() {
	activeMu.Lock()
	dirs := make([]string, 0, len(activeDirs))
	for dir := range activeDirs {
		dirs = append(dirs, dir)
	}
	activeDirs = map[string]struct{}{}
	activeMu.Unlock()
	cleanupDirs(dirs, nil)
}

// CleanupAll implements the tui.BufferedService teardown contract;
// removal failures reach the downloader's wired (file) logger.
func (d *Downloader) CleanupAll() {
	activeMu.Lock()
	dirs := make([]string, 0, len(activeDirs))
	for dir := range activeDirs {
		dirs = append(dirs, dir)
	}
	activeDirs = map[string]struct{}{}
	activeMu.Unlock()
	cleanupDirs(dirs, d.log)
}

// cleanupDirs removes dirs, warning per failure through the given
// logger (nil = silent).
func cleanupDirs(dirs []string, log *slog.Logger) {
	for _, dir := range dirs {
		if err := os.RemoveAll(dir); err != nil && log != nil {
			log.Warn("buffered: cleanup temp dir failed", "dir", dir, "error", err)
		}
	}
}

// activeDirsSnapshot lists the registered dirs (test seam).
func activeDirsSnapshot() []string {
	activeMu.Lock()
	defer activeMu.Unlock()
	out := make([]string, 0, len(activeDirs))
	for dir := range activeDirs {
		out = append(out, dir)
	}
	return out
}
