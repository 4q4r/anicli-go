//go:build linux

package sysdeps

// The real TTY gate: a terminal answers the TCGETS ioctl; character
// devices like /dev/null (instant EOF) and /dev/zero (endless NULs —
// a readYn hang vector) do not. The golang.org/x/sys module was
// already pinned in go.mod (indirect) — promoting it to direct adds
// no module to the graph.

import (
	"os"

	"golang.org/x/sys/unix"
)

// stdinIsTTY reports whether the process's stdin is a terminal.
func stdinIsTTY() bool { return stdinIsTTYOn(os.Stdin) }

// stdinIsTTYOn is the injectable form (os.Pipe in tests resolves to
// false; a pty would resolve to true).
func stdinIsTTYOn(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	return err == nil
}
