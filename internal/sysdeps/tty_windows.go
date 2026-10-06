//go:build windows

package sysdeps

// Windows TTY gate: a console handle answers GetConsoleMode; pipes and
// redirected files fail it.

import (
	"os"

	"golang.org/x/sys/windows"
)

// stdinIsTTY reports whether the process's stdin is a console.
func stdinIsTTY() bool { return stdinIsTTYOn(os.Stdin) }

// stdinIsTTYOn is the injectable form.
func stdinIsTTYOn(f *os.File) bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(f.Fd()), &mode) == nil
}
