//go:build windows

package cfbrowser

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// setNewProcessGroup is a no-op on Windows: there is no POSIX process
// group to detach into and no group signalling for console children
// (mirrors the player package split).
func setNewProcessGroup(*exec.Cmd) {}

// killProcessGroup kills the direct browser process only: Windows has
// no deliverable group signal; the browser's children follow its
// lifetime. os.ErrProcessDone means it is already gone — not an
// error.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("cfbrowser: kill chromium process: %w", err)
	}
	return nil
}
