package tui

import (
	"log/slog"
	"os"
	"strings"
)

// testLogger builds a quiet slog logger for tests.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.NewFile(0, os.DevNull), nil))
}

// contains is the shared substring assertion helper.
func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
