//go:build !windows

package player

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

// setNewProcessGroup detaches the child into its own process group
// (python start_new_session=True) so group signals do not reach the
// CLI's process group.
func setNewProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateProcessGroup runs the Unix shutdown ladder against the
// child's whole process group: SIGTERM, grace window, SIGKILL. ESRCH
// means the group already exited — not an error.
func terminateProcessGroup(proc *process, grace time.Duration, wait <-chan error) error {
	pgid := -proc.cmd.Process.Pid
	if err := syscall.Kill(pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("player: SIGTERM process group: %w", err)
	}
	select {
	case err := <-wait:
		return err
	case <-time.After(grace):
		if err := syscall.Kill(pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("player: SIGKILL process group: %w", err)
		}
		return <-wait
	}
}
