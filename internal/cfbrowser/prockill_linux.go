//go:build linux

package cfbrowser

import (
	"os"
	"os/exec"
	"syscall"
)

// enableParentDeathSignal replicates chromedp's default Linux command
// setup (allocate_linux.go), which our ModifyCmdFunc override
// displaces: the browser dies with the Go parent, so a crashed anicli
// cannot orphan it. Skipped on AWS Lambda exactly like the upstream
// default.
func enableParentDeathSignal(cmd *exec.Cmd) {
	if _, ok := os.LookupEnv("LAMBDA_TASK_ROOT"); ok {
		return
	}
	if cmd.SysProcAttr != nil {
		cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
	}
}
