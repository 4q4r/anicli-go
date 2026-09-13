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
//
// Orphan protection is the synchronous group-kill in Close (leader +
// renderer/gpu/utility children, plus the solver cancelling in-flight
// navigations on its own Close) — NOT PR_SET_PDEATHSIG. Pdeathsig is
// deliberately rejected and must not come back: prctl(2) ties the
// death signal to the forking THREAD, not the parent process, and the
// Go runtime retires OS threads freely (nothing holds the spawning
// thread alive for the child's lifetime), so the kernel SIGKILLs a
// Pdeathsig'd browser at an arbitrary moment seconds after launch —
// live-verified as every solve since PR14 navigating a dead process
// ("context canceled" on the first Navigate). This is a deliberate
// divergence from upstream chromedp v0.16, whose allocate_linux.go
// arms Pdeathsig and thereby carries the same race.
func setNewProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
