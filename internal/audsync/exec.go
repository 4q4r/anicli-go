package audsync

// The exec seam (the analog of the download package's runner): stdout
// buffers whole (PCM windows and ffprobe json are bounded), stderr is
// a string for error wrapping, and a dead context surfaces AS the
// context error so the budget kill is recognizable.

import (
	"bytes"
	"context"
	"os/exec"
)

// execFunc runs one external command and returns its stdout. stderr
// rides along for error messages; err is non-nil on failure — a
// context kill reports ctx.Err().
type execFunc func(ctx context.Context, bin string, args []string) (stdout []byte, stderr string, err error)

// realExec is the production execFunc: LookPath fail-loud (parity with
// the downloader's ffmpeg handling) and a direct child process with
// captured stdout/stderr.
func realExec(ctx context.Context, bin string, args []string) ([]byte, string, error) {
	if _, err := exec.LookPath(bin); err != nil {
		return nil, "", err
	}
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // bin/args are compiled-in program shapes, never request input
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return stdout.Bytes(), stderr.String(), ctx.Err()
		}
		return stdout.Bytes(), stderr.String(), err
	}
	return stdout.Bytes(), stderr.String(), nil
}
