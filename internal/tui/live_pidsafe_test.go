//go:build live

package tui

// pidsafePlayer is the PR84 live-proof player (the PR63 delegating-
// wrapper precedent, evolved): it launches the REAL mpv for every
// Play and remembers the *os.Process of exactly the children IT
// spawned. Cleanup signals ONLY those handles — never name-based
// sweeps, never process-table scans. The owner's parallel playback
// is untouchable by construction: this helper literally cannot
// reference a process it did not spawn.
//
// Bounded lifetime: every Play self-terminates after playLifetime
// (the "player exited" event is synthesized by the harness, rule 4).
// A PID that is already gone is gone — signal errors are ignored.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// playLifetime bounds one live playback: after this the harness
// terminates ITS OWN child through the retained handle.
const playLifetime = 8 * time.Second

// pidsafePlayer implements the tui PlaybackService surface for live
// proofs with the real mpv binary, PID-tracked.
type pidsafePlayer struct {
	bin string

	mu     sync.Mutex
	procs  []*os.Process
	plays  int
	active int
}

// newPidsafePlayer resolves the player binary through PATH ("mpv").
func newPidsafePlayer() *pidsafePlayer { return &pidsafePlayer{bin: "mpv"} }

// Plays counts launched playbacks (the harness's launch proof).
func (p *pidsafePlayer) Plays() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.plays
}

// Active reports how many launched children are still alive.
func (p *pidsafePlayer) Active() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active
}

// ResolveSkips: the live probe needs no skip fetch — an empty verdict.
func (p *pidsafePlayer) ResolveSkips(context.Context, int64, float64) (string, func(), string, error) {
	return "", func() {}, "", nil
}

// Play launches the real mpv, registers the child's handle and blocks
// until mpv exits or the bounded lifetime expires — then terminates
// the child strictly through that handle.
func (p *pidsafePlayer) Play(_ context.Context, req PlayRequest) error {
	p.mu.Lock()
	p.plays++
	p.active++
	p.mu.Unlock()

	cmd := exec.Command(p.bin, "--force-media-title="+req.Title, "--no-ytdl", req.URL) //nolint:gosec // bin is the fixed player, the URL is the pipeline's own output
	if err := cmd.Start(); err != nil {
		p.mu.Lock()
		p.active--
		p.mu.Unlock()
		return fmt.Errorf("pidsafe: start %s: %w", p.bin, err)
	}
	handle := cmd.Process // the ONLY control channel to this child

	playCtx, cancel := context.WithTimeout(context.Background(), playLifetime)
	defer cancel()
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()

	var playErr error
	select {
	case playErr = <-wait:
	case <-playCtx.Done():
		// Bounded lifetime: terminate through the retained handle.
		_ = handle.Signal(syscall.SIGTERM)
		select {
		case playErr = <-wait:
		case <-time.After(3 * time.Second):
			_ = handle.Kill()
			playErr = <-wait
		}
		playErr = playCtx.Err()
	}

	p.mu.Lock()
	p.active--
	p.mu.Unlock()
	return playErr
}

// StopAll terminates every child this helper spawned, strictly through
// the retained handles. A handle whose PID is already gone errors —
// gone is gone, no sweeping. The reaping itself belongs to each
// Play's wait goroutine.
func (p *pidsafePlayer) StopAll() {
	p.mu.Lock()
	procs := p.procs
	p.procs = nil
	p.mu.Unlock()
	for _, proc := range procs {
		_ = proc.Signal(syscall.SIGTERM)
	}
}
