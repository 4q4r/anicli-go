//go:build !windows

package cfbrowser

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

// setNewProcessGroup detaches the browser into its own process group
// (Setpgid), mirroring the player package's platform split: group
// signals cannot cross between the CLI and the browser in either
// direction, and close-time kills address the whole group via the
// negative pid.
func setNewProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	enableParentDeathSignal(cmd)
}

// killProcessGroup SIGKILLs the browser's whole process group — the
// leader plus its renderer/gpu/utility children. ESRCH means the
// group is already fully gone: not an error.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("cfbrowser: SIGKILL process group %d: %w", cmd.Process.Pid, err)
	}
	return nil
}
