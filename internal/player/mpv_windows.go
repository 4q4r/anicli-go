//go:build windows

package player

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// setNewProcessGroup is a no-op on Windows: there is no POSIX process
// group to detach into and no group signalling for console children
// (python start_new_session has no equivalent here); shutdown talks to
// the direct child only.
func setNewProcessGroup(*exec.Cmd) {}

// terminateProcessGroup runs the Windows reading of the shutdown
// ladder. Windows has no deliverable SIGTERM, so the TERM→grace→KILL
// ladder collapses to an immediate TerminateProcess (cmd.Process.Kill,
// taskkill-free); the grace window is kept only to reap the exit
// status. os.ErrProcessDone means the child already exited.
func terminateProcessGroup(proc *process, grace time.Duration, wait <-chan error) error {
	if err := proc.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("player: kill process: %w", err)
	}
	select {
	case err := <-wait:
		return err
	case <-time.After(grace):
		return fmt.Errorf("player: process did not exit %v after kill", grace)
	}
}
