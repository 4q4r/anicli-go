package cfbrowser

// PR85: every cfbrowser log line lands on the WIRED logger — the TUI
// file logger — never on slog.Default (stderr corrupts alt-screen).
// Unwired seams degrade to this discard logger, and the CLI faces
// pass slog.Default() explicitly (pre-alt-screen terminal allowed).

import (
	"io"
	"log/slog"
)

// discardLogger returns the never-stderr sink for unwired seams.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
