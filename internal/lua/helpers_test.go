package lua

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// testLogger returns a slog.Logger whose output lands in a buffer
// (assertable, never stderr) plus the buffer itself.
func testLogger(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()

	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

// logContains reports whether the buffer contains substr.
func logContains(buf *bytes.Buffer, substr string) bool {
	return strings.Contains(buf.String(), substr)
}

// mustLogger returns just the buffered test logger (for calls that do
// not need the buffer).
func mustLogger(t *testing.T) *slog.Logger {
	t.Helper()
	l, _ := testLogger(t)
	return l
}
