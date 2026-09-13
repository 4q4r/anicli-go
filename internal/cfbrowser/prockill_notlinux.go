//go:build !linux && !windows

package cfbrowser

import "os/exec"

// enableParentDeathSignal is a no-op outside linux: Pdeathsig is a
// linux-only SysProcAttr field; other platforms rely on the
// ephemeral session lifecycle (idle close) for reaping.
func enableParentDeathSignal(*exec.Cmd) {}
