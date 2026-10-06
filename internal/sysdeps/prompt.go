package sysdeps

// The TTY gate and the Y/n reader (PR148 owner preference
// [2026-09-05]): interactive prompting ONLY when stdin is a terminal;
// under systemd/docker/pipe the flow prints the exact commands and
// never blocks. The terminal verdict comes from the per-GOOS
// tty_*.go ioctl handshake (a character-device file mode alone
// misclassifies /dev/null as a terminal and hangs on /dev/zero);
// golang.org/x/sys was already pinned in go.mod — no new module.

import (
	"bufio"
	"strings"
)

// readYn reads one answer line from the SHARED prompt reader (a fresh
// buffer per question would discard already-buffered input and turn
// the second answer into EOF). Enter (an empty line) takes the
// displayed default Y; recognized yes forms in both interface
// languages are accepted; EVERYTHING else — including EOF, a closed
// reader or unrecognized input — takes the safe answer N, so no
// system-mutating command ever runs on ambiguous input.
func readYn(in *bufio.Reader) bool {
	line, err := in.ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	if answer == "" && err != nil {
		return false // nothing readable (EOF/closed): the safe answer
	}
	switch answer {
	case "", "y", "yes", "д", "да":
		return true
	default:
		return false
	}
}
