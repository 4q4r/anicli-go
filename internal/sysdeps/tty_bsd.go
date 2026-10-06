//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package sysdeps

// BSD-line TTY gate: same ioctl handshake as the Linux variant, with
// the BSD request constant.

import (
	"os"

	"golang.org/x/sys/unix"
)

// stdinIsTTY reports whether the process's stdin is a terminal.
func stdinIsTTY() bool { return stdinIsTTYOn(os.Stdin) }

// stdinIsTTYOn is the injectable form (os.Pipe in tests resolves to
// false; a pty would resolve to true).
func stdinIsTTYOn(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TIOCGETA)
	return err == nil
}
