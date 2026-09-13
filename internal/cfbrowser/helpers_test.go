package cfbrowser

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"testing"
)

// testLogger returns a discard slog logger (progress lines must not
// pollute test output).
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// sha256Hex renders the sha256:<hex> digest of b.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
