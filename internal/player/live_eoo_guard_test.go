//go:build live

package player

// LIVE proof for 169-I: with the `--` guard, a hostile URL of
// `--version` makes mpv try to OPEN a file named --version (exit 2,
// the surfaced stderr tail names the file) instead of printing its
// version and exiting 0 (the pre-fix behavior). Run:
//
//	go test -tags live -run TestLiveEndOfOptionsGuard -count=1 -v ./internal/player/
//

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestLiveEndOfOptionsGuard(t *testing.T) {
	argv := BuildArgs(Request{URL: "--version"}, Options{Bin: "mpv"})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("mpv exited 0 on URL %q: it printed the version instead of opening a file; output:\n%s", "--version", out)
	}
	if !strings.Contains(string(out), "--version") {
		t.Fatalf("mpv must try to OPEN a file named --version (the tail should name it), got exit %v:\n%s", err, out)
	}
}
