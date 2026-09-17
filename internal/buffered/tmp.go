// Package buffered implements the PR43 buffered watch mode: download
// one video source to a temporary file (progressive GET, torrent
// loopback stream or a minimal HLS downloader), hand the local path to
// the player, and delete the file once the player exits.
//
// HTTP semantics: playlist fetches ride the shared netclient (browser
// parity, proxy, CF solver — playlists are small); media bytes stream
// through a plain net/http client because netclient.Do BUFFERS every
// response body (bounded by its body limit) and therefore cannot
// stream multi-gigabyte media. The streaming client carries the same
// source headers and the configured proxy; redirects follow RFC 7231
// automatically. There are no external dependencies.
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

// releaseTempDir removes one download's temp dir and forgets it;
// safe to call twice.
func releaseTempDir(dir string) {
	activeMu.Lock()
	_, known := activeDirs[dir]
	delete(activeDirs, dir)
	activeMu.Unlock()
	if !known {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("buffered: remove temp dir failed", "dir", dir, "error", err)
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
	for _, dir := range dirs {
		if err := os.RemoveAll(dir); err != nil {
			slog.Warn("buffered: cleanup temp dir failed", "dir", dir, "error", err)
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
