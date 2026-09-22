package cfbrowser

// PR85: EVERY cfbrowser log line must land on the WIRED logger — the
// TUI file logger — never on slog.Default (stderr corrupts the TUI
// alt-screen; PR62 fixed the same defect class for providers, but
// cfbrowser was never plumbed).

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureDefault swaps slog.Default for a recording handler and
// restores it on cleanup. Returns the buffer the default handler
// writes to (i.e. what would land on stderr in production).
func captureDefault(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buf
}

// TestVerdictStorePersistWarnNeverOnStderr (PR85): the verdict
// persist diagnostics land on the WIRED logger and NEVER on
// slog.Default (stderr corrupts the TUI alt-screen — the owner's
// capture showed the updater-cycle warn glued to the status line).
func TestVerdictStorePersistWarnNeverOnStderr(t *testing.T) {
	defaultBuf := captureDefault(t)

	probeBuf := &bytes.Buffer{}
	probe := slog.New(slog.NewTextHandler(probeBuf, nil))

	// An unwritable store path: the parent is a FILE, so MkdirAll
	// fails and the persist warn fires.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := loadVerdictStore(filepath.Join(blocker, "nested", "verdicts.json"))
	s.logger = probe // the wired file logger (the TUI wiring shape)
	s.recordBad(151, "renderer crash")

	if defaultBuf.Len() != 0 {
		t.Fatalf("slog.Default captured a cfbrowser line — stderr leak:\n%s",
			defaultBuf.String())
	}
	if !strings.Contains(probeBuf.String(), "persist compat verdicts") {
		t.Fatalf("the wired logger must carry the persist warn, got:\n%s",
			probeBuf.String())
	}
}

// The unwired store degrades to DISCARD: nothing on slog.Default
// either (the never-stderr invariant holds without wiring).
func TestVerdictStoreUnwiredPersistIsSilent(t *testing.T) {
	defaultBuf := captureDefault(t)

	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := loadVerdictStore(filepath.Join(blocker, "nested", "verdicts.json"))
	s.recordBad(151, "renderer crash")

	if defaultBuf.Len() != 0 {
		t.Fatalf("unwired store wrote to slog.Default: %s", defaultBuf.String())
	}
}
