package player

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ipcLogSink collects the stub's stdout lines (the "ipc: <socket>"
// line carries the socket path the test needs for the cleanup pin).
func ipcLogSink(mu *sync.Mutex, sink *[]string) LogFunc {
	return func(line string) {
		mu.Lock()
		*sink = append(*sink, line)
		mu.Unlock()
	}
}

// TestPlayTracksPositionOverIPC (#166): the launched player carries a
// per-launch --input-ipc-server unix socket, observes time-pos over
// the mpv JSON IPC while the process lives, and retains the FINAL
// observed position after the process is gone — the redub pick reads
// exactly this value (the TUI can only reach the redub menu after the
// player exited, so a live query at pick time would find nothing).
func TestPlayTracksPositionOverIPC(t *testing.T) {
	t.Parallel()

	p := newTestPlayer(buildStub(t))
	var mu sync.Mutex
	var lines []string
	p.SetLog(ipcLogSink(&mu, &lines))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.Play(ctx, Request{URL: "ipcserve"}) }()

	// Condition-based wait: the stub pushes 10 → 20 → 30; the tracker
	// must settle on the final value.
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, pos, ok := p.LastPosition()
		if ok && pos == 30 {
			break
		}
		if time.Now().After(deadline) {
			// Snapshot under the lock, Fatalf outside it — Goexit on
			// Fatalf would skip the deferred Unlock (review N1).
			mu.Lock()
			last := fmt.Sprintf("last=(%v, %v); log: %v", pos, ok, lines)
			mu.Unlock()
			t.Fatalf("position never reached 30, %s", last)
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Play did not return after cancellation")
	}

	// The final value survives the process exit (the redub reads it
	// after playedMsg).
	url, pos, ok := p.LastPosition()
	if !ok || pos != 30 {
		t.Fatalf("LastPosition after exit = (%q, %v, %v), want (_, 30, true)", url, pos, ok)
	}
	if url != "ipcserve" {
		t.Errorf("LastPosition url = %q, want the tracked launch's URL", url)
	}

	// Socket lifecycle: the per-launch temp dir (printed by the stub)
	// is removed once Play returns.
	mu.Lock()
	defer mu.Unlock()
	sockPath := ""
	for _, line := range lines {
		if strings.HasPrefix(line, "ipc: ") {
			sockPath = strings.TrimPrefix(line, "ipc: ")
		}
	}
	if sockPath == "" {
		t.Fatalf("the stub never printed its socket path; log: %v", lines)
	}
	if _, err := os.Stat(filepath.Dir(sockPath)); !os.IsNotExist(err) {
		t.Errorf("the IPC socket dir survived Play: %s (stat err %v)", filepath.Dir(sockPath), err)
	}
}

// TestPlayCarriesIPCServerArg (#166): EVERY streaming launch wires the
// IPC socket into the argv — the position must be tracked during any
// playback because the user may redub right after it. The socket path
// is unique per launch (a fresh temp dir), never a shared well-known
// name.
func TestPlayCarriesIPCServerArg(t *testing.T) {
	t.Parallel()

	p := newTestPlayer(buildStub(t))
	var mu sync.Mutex
	var lines []string
	p.SetLog(ipcLogSink(&mu, &lines))

	if err := p.Play(context.Background(), Request{URL: "dumpargv"}); err != nil {
		t.Fatalf("Play: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	sockArgs := 0
	var sockPath string
	for _, line := range lines {
		if strings.HasPrefix(line, "argv: --input-ipc-server=") {
			sockArgs++
			sockPath = strings.TrimPrefix(line, "argv: --input-ipc-server=")
		}
	}
	if sockArgs != 1 {
		t.Fatalf("argv carries --input-ipc-server %d times (log: %v), want exactly 1", sockArgs, lines)
	}
	// MkdirTemp("", "anicli-mpv-ipc-") places the socket at
	// <temp>/anicli-mpv-ipc-<random>/mpv.sock — a unique per-launch
	// directory, never a shared well-known name.
	if filepath.Dir(filepath.Dir(sockPath)) != os.TempDir() {
		t.Errorf("the socket must live under the temp dir, got %q (temp %q)", sockPath, os.TempDir())
	}
	if base := filepath.Base(filepath.Dir(sockPath)); !strings.HasPrefix(base, "anicli-mpv-ipc-") {
		t.Errorf("the socket dir must carry the per-launch prefix, got %q", base)
	}
}

// TestPlayIPCFailureDegradesSilently (#166): a player that never
// creates the socket (a broken mpv build, an odd sandbox) must not
// fail the launch — the watcher gives up bounded, the position reads
// invalid, and the redub relaunches without --start. Playback itself
// is never blocked by the tracker.
func TestPlayIPCFailureDegradesSilently(t *testing.T) {
	t.Parallel()

	p := newTestPlayer(buildStub(t))
	p.warmup = 100 * time.Millisecond

	start := time.Now()
	if err := p.Play(context.Background(), Request{URL: "exitsoon"}); err != nil {
		t.Fatalf("Play: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("a missing IPC socket delayed the exit by %v — the tracker must be bounded", elapsed)
	}
	if _, _, ok := p.LastPosition(); ok {
		t.Fatal("LastPosition must read invalid when no socket ever existed")
	}
}
