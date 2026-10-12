//go:build live

package tui

// LIVE probe for #166 (owner-reported, live): «Сменить озвучку»
// re-resolves and RESTARTS THE EPISODE FROM ZERO — the requirement is
// that changing the dub continues from the current playback position
// («требовалось просто озвучку поменять»).
//
// Run manually:
//
//	go test -tags live -run TestLive166RedubResumePosition -count=1 -v ./internal/tui/
//
// What the probe proves against REAL mpv (windowed — this host has a
// display) and REAL providers:
//
//  1. The production player launch of a real stream binds its
//     per-launch IPC socket and the position tracker observes real
//     time-pos while mpv plays (the #166 tracking subsystem live).
//  2. After the bounded playback the FINAL position survives the
//     process exit — exactly what the redub pick reads.
//  3. The redub relaunch argv (built by the production BuildArgs with
//     the captured StartPosition) carries --start=<captured>, and the
//     REAL relaunched mpv — queried over its own IPC socket mid-flight
//     — is actually PLAYING at ≈the captured position. Playback
//     continues; nothing restarts from zero.
//
// The redub MENU wiring itself (pickRedub capture → doPlay consume) is
// pinned hermetically by redub_resume_test.go; this probe exercises
// the pieces that only exist against the real player and real
// streams. Bounded lifetimes + PID-tracked termination (the PR84
// pidsafe rule): the harness only ever signals children it spawned.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/player"
)

// TestLive166RedubResumePosition: the owner's scenario on a real
// title — watch a few seconds of dub A, redub to dub B, the new mpv
// continues at the captured position.
func TestLive166RedubResumePosition(t *testing.T) {
	_, deps := liveDeps(t)

	bySource := liveSearch(t, deps, "черная лагуна")
	if len(bySource) == 0 {
		t.Fatalf("no live results")
	}
	group := make([]contracts.SearchResult, 0, len(bySource))
	for _, r := range bySource {
		group = append(group, r)
	}
	group = stableGroup(group)
	primary := group[0]
	t.Logf("primary source: %s/%s (%q)", primary.SourceID, primary.URL, primary.Title)

	s := NewSessionScreen(deps, primary, group)
	s.loadEpisodesSync()
	if len(s.order) < 1 {
		t.Fatalf("no merged episodes")
	}

	// Real merged resolve of episode 1 across providers (the PR61
	// model): need TWO distinct dubs to redub between.
	ep := s.episodes[s.order[0]]
	entries, skipped, err := resolveAllStreams(context.Background(), deps.Episode, ep, "", nil)
	if err != nil {
		t.Fatalf("merged resolve: %v", err)
	}
	if line := skippedSummary(skipped); line != "" {
		t.Logf("skipped: %s", line)
	}
	dubs := map[string]string{} // dub key -> stream URL (highest quality)
	for _, e := range entries {
		if _, seen := dubs[e.DubKey]; !seen {
			dubs[e.DubKey] = e.Source.URL
		}
	}
	if len(dubs) < 2 {
		t.Fatalf("need 2+ dubs to redub between, got %v", dubs)
	}
	keys := make([]string, 0, len(dubs))
	for k := range dubs {
		keys = append(keys, k)
	}
	dubA, dubB := keys[0], keys[1]
	t.Logf("dub A %q → %s", dubA, dubs[dubA])
	t.Logf("dub B %q → %s", dubB, dubs[dubB])

	// --- 1. Launch dub A through the PRODUCTION player: real mpv,
	// real stream, real IPC position tracking.
	p := player.New(player.Options{Bin: "mpv"})
	p.SetLog(func(line string) { t.Logf("mpv| %s", line) })

	playCtx, stopPlay := context.WithTimeout(context.Background(), 12*time.Second)
	defer stopPlay()
	done := make(chan error, 1)
	go func() { done <- p.Play(playCtx, player.Request{URL: dubs[dubA]}) }()

	// Condition-based wait: the tracker must observe real progress.
	deadline := time.Now().Add(30 * time.Second)
	var captured float64
	for {
		_, pos, ok := p.LastPosition()
		if ok && pos >= 3 {
			captured = pos
			break
		}
		select {
		case err := <-done:
			t.Fatalf("mpv exited before any position was tracked: %v", err)
		case <-time.After(200 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("position never reached 3s, last=%v", pos)
		}
	}
	t.Logf("TRACKED POSITION while watching dub A: %.1fs", captured)

	// The bounded lifetime ends launch 1 (SIGTERM ladder) — the
	// redub pick reads the position AFTER the player exited, exactly
	// like the TUI (playedMsg → menu → pick).
	stopPlay()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("launch 1: %v", err)
	}
	url, pos, ok := p.LastPosition()
	if !ok || pos < 3 || url != dubs[dubA] {
		t.Fatalf("post-exit LastPosition = (%q, %.1f, %v), want (%q, ≥3, true)",
			url, pos, ok, dubs[dubA])
	}
	captured = pos
	t.Logf("FINAL CAPTURED POSITION (post-exit, the redub pick's read): %.1fs", captured)

	// --- 2. The redub relaunch: the production argv builder with the
	// captured StartPosition; the probe supplies the launch's IPC
	// socket so it can interrogate the real mpv mid-flight.
	sockDir, err := os.MkdirTemp("", "anicli-live166-ipc-")
	if err != nil {
		t.Fatalf("probe socket dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(sockDir) }()
	probeSock := filepath.Join(sockDir, "mpv.sock")

	argv := player.BuildArgs(player.Request{
		URL:           dubs[dubB],
		Title:         "[#166 live probe] redub resume",
		StartPosition: captured,
		IPCSocket:     probeSock,
	}, player.Options{Bin: "mpv"})

	startSeen, ipcSeen := false, false
	for _, a := range argv {
		switch {
		case strings.HasPrefix(a, "--start="):
			startSeen = true
			t.Logf("ARGV PROOF: %s (captured %.1f)", a, captured)
		case strings.HasPrefix(a, "--input-ipc-server="):
			ipcSeen = true
		}
	}
	if !startSeen || !ipcSeen {
		t.Fatalf("relaunch argv must carry --start and --input-ipc-server: %v", argv)
	}

	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // the fixed player binary, probe-built argv
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("relaunch stdout pipe: %v", err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatalf("relaunch mpv: %v", err)
	}
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			t.Logf("mpv2| %s", sc.Text())
		}
	}()
	handle := cmd.Process // the ONLY control channel (pidsafe rule)
	t.Logf("relaunched mpv PID %d on dub B", handle.Pid)
	defer func() {
		_ = handle.Signal(syscall.SIGTERM)
		_, _ = cmd.Process.Wait()
	}()

	// --- 3. THE LIVE PROOF: query the REAL relaunched mpv over its
	// own IPC — it must be PLAYING at ≈the captured position.
	resumed := queryTimePos(t, probeSock, 15*time.Second, captured)
	t.Logf("LIVE RESUME PROOF: relaunched mpv time-pos = %.1fs (start was %.1fs) — playback continued, no from-zero restart",
		resumed, captured)
	if resumed < captured-5 {
		t.Fatalf("relaunched mpv plays at %.1fs, want ≈%.1fs — the resume regressed", resumed, captured)
	}

	// Playback must be ADVANCING (not frozen at the start position).
	time.Sleep(2 * time.Second)
	advanced := queryTimePos(t, probeSock, 5*time.Second, captured)
	t.Logf("playback advancing: %.1fs → %.1fs", resumed, advanced)
	if advanced <= resumed {
		t.Fatalf("playback is not advancing (%.1fs → %.1fs)", resumed, advanced)
	}

	_ = handle.Signal(syscall.SIGTERM)
	_ = cmd.Wait()
	t.Logf("REDUB RESUME PROOF: dub A watched to %.1fs → redub to dub B → mpv continued at ≈%.1fs",
		captured, captured)
}

// queryTimePos dials the launched mpv's IPC socket (retrying until it
// appears) and returns one get_property time-pos verdict. time-pos is
// unavailable while mpv is still loading the file, so a nil-data
// success reply is retried, never fatal.
func queryTimePos(t *testing.T, sock string, wait time.Duration, expect float64) float64 {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		conn, err := net.DialTimeout("unix", sock, time.Second)
		if err == nil {
			var data *float64
			if _, werr := conn.Write([]byte(`{"command":["get_property","time-pos"]}` + "\n")); werr != nil {
				t.Fatalf("ipc write: %v", werr)
			}
			line, rerr := bufio.NewReader(conn).ReadString('\n')
			if rerr != nil {
				t.Fatalf("ipc read: %v", rerr)
			}
			var reply struct {
				Data  *float64 `json:"data"`
				Error string   `json:"error"`
			}
			if jerr := json.Unmarshal([]byte(line), &reply); jerr != nil {
				t.Fatalf("ipc decode %q: %v", line, jerr)
			}
			if reply.Error != "success" {
				// "property unavailable": mpv is between file-open
				// and first playback — retry within the budget.
				_ = conn.Close()
				if time.Now().After(deadline) {
					t.Fatalf("ipc verdict %q after %v (expected start ≈%.1f)", reply.Error, wait, expect)
				}
				time.Sleep(500 * time.Millisecond)
				continue
			}
			data = reply.Data
			_ = conn.Close()
			if data != nil {
				return *data
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the relaunched mpv never reported a time-pos at %s (expected start ≈%.1f)", sock, expect)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
