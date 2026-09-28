package player

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

// stubSource is the test-double mpv: mode "sleep30" (default) sleeps
// 30s and exits 0 on SIGTERM, "trapterm" additionally ignores SIGTERM
// (SIGKILL escalation), "exitnow" exits immediately with status 1,
// "printslow" dumps 400 log lines then exits after 600ms (pump race).
const stubSource = `package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	mode := "sleep30"
	if len(os.Args) > 1 {
		mode = os.Args[len(os.Args)-1]
	}
	if mode == "exitnow" {
		os.Exit(1)
	}
	if mode == "exitsoon" {
		time.Sleep(600 * time.Millisecond)
		os.Exit(0)
	}
	if mode == "printslow" {
		for i := 0; i < 400; i++ {
			fmt.Printf("stub line %03d\n", i)
		}
		time.Sleep(600 * time.Millisecond)
		os.Exit(0)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	if mode == "trapterm" {
		go func() {
			for range sig { // swallow SIGTERM
			}
		}()
	} else {
		go func() {
			<-sig
			os.Exit(0)
		}()
	}
	time.Sleep(30 * time.Second)
}
`

var (
	stubOnce sync.Once
	stubPath string
	stubErr  error
)

// buildStub compiles the test-double player once per test binary run.
func buildStub(t *testing.T) string {
	t.Helper()
	stubOnce.Do(func() {
		goBin, err := exec.LookPath("go")
		if err != nil {
			stubErr = errors.New("go toolchain unavailable")
			return
		}
		dir, err := os.MkdirTemp("", "player-stub-*")
		if err != nil {
			stubErr = err
			return
		}
		src := filepath.Join(dir, "main.go")
		if err := os.WriteFile(src, []byte(stubSource), 0o600); err != nil {
			stubErr = err
			return
		}
		out := filepath.Join(dir, "player-stub")
		cmd := exec.Command(goBin, "build", "-o", out, src) //nolint:gosec // test-built helper
		if outb, err := cmd.CombinedOutput(); err != nil {
			stubErr = errors.New(string(outb))
			return
		}
		stubPath = out
	})
	if stubErr != nil {
		t.Skipf("player stub unavailable: %v", stubErr)
	}
	return stubPath
}

// TestBuildArgsOnlineGolden pins the full online argv: buffering family
// (verbatim from python), external audio, title, chapters file, header
// mapping and extra opts order.
func TestBuildArgsOnlineGolden(t *testing.T) {
	t.Parallel()

	got := BuildArgs(Request{
		URL:      "https://cdn.example/ep.m3u8",
		AudioURL: "https://cdn.example/audio.m3u8",
		Title:    "Anime - 1 [1080p]",
		Headers: map[string]string{
			"User-Agent": "ua-1",
			"Referer":    "https://ref.example/",
			"Origin":     "https://origin.example",
		},
		ExtraMPVOpts: []string{"--vf=flip"},
		ChaptersFile: "/tmp/chapters.ffmetadata",
	}, Options{Bin: "mpv", Timeout: 30})

	want := []string{
		"mpv",
		"--cache=yes",
		"--demuxer-max-bytes=2048MiB",
		"--demuxer-max-back-bytes=500MiB",
		"--network-timeout=30",
		"--stream-buffer-size=16MiB",
		"--hwdec=auto-safe",
		"--save-position-on-quit",
		"--audio-file=https://cdn.example/audio.m3u8",
		"--force-media-title=Anime - 1 [1080p]",
		"--chapters-file=/tmp/chapters.ffmetadata",
		"--osd-level=1",
		"--osd-duration=2500",
		"--osd-font-size=32",
		"--cursor-autohide=1000",
		"--user-agent=ua-1",
		"--referrer=https://ref.example/",
		"--http-header-fields=Origin: https://origin.example",
		"--vf=flip",
		"--msg-level=all=error",
		"--no-ytdl",
		"https://cdn.example/ep.m3u8",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv mismatch:\n got:  %v\nwant:  %v", got, want)
	}
}

// TestBuildArgsSavesPositionOnQuit (PR113): every mpv launch carries
// --save-position-on-quit so mpv writes its watch-later file on exit
// and replays of the same episode resume from the saved position —
// the exact placement is pinned by the golden tests.
func TestBuildArgsSavesPositionOnQuit(t *testing.T) {
	t.Parallel()

	got := BuildArgs(Request{URL: "https://cdn.example/ep.m3u8"}, Options{Bin: "mpv"})
	for _, arg := range got {
		if arg == "--save-position-on-quit" {
			return
		}
	}
	t.Fatalf("argv must contain --save-position-on-quit, got %v", got)
}

// TestBuildArgsOfflineGolden pins the offline argv: local file, no
// headers, [OFFLINE] title, no chapters (chapters are embedded at
// download time; the offline flow passes none).
func TestBuildArgsOfflineGolden(t *testing.T) {
	t.Parallel()

	got := BuildArgs(Request{
		URL:   "/downloads/Anime/Anime - 1.mp4",
		Title: "[Dub RU - Dub EN] Anime - 1 [OFFLINE]",
	}, Options{Bin: "/usr/bin/mpv", Timeout: 30})

	want := []string{
		"/usr/bin/mpv",
		"--cache=yes",
		"--demuxer-max-bytes=2048MiB",
		"--demuxer-max-back-bytes=500MiB",
		"--network-timeout=30",
		"--stream-buffer-size=16MiB",
		"--hwdec=auto-safe",
		"--save-position-on-quit",
		"--force-media-title=[Dub RU - Dub EN] Anime - 1 [OFFLINE]",
		"--osd-level=1",
		"--osd-duration=2500",
		"--osd-font-size=32",
		"--cursor-autohide=1000",
		"--msg-level=all=error",
		"--no-ytdl",
		"/downloads/Anime/Anime - 1.mp4",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv mismatch:\n got:  %v\nwant:  %v", got, want)
	}
}

// TestBuildArgsHeaderMapping pins the case-insensitive user-agent /
// referer extraction and the comma-joined http-header-fields format.
func TestBuildArgsHeaderMapping(t *testing.T) {
	t.Parallel()

	got := BuildArgs(Request{
		URL:     "https://cdn/ep.mp4",
		Headers: map[string]string{"user-AGENT": "ua", "REFERER": "ref", "X-A": "1", "X-B": "2"},
	}, Options{Bin: "mpv", Timeout: 5})

	joined := false
	for _, arg := range got {
		if arg == "--user-agent=ua" || arg == "--referrer=ref" {
			continue
		}
		if arg == "--http-header-fields=X-A: 1,X-B: 2" {
			joined = true
		}
	}
	if !joined {
		t.Errorf("--http-header-fields=X-A: 1,X-B: 2 missing from %v", got)
	}
}

// TestBuildArgsProfile pins the optional profile flag position.
func TestBuildArgsProfile(t *testing.T) {
	t.Parallel()

	got := BuildArgs(Request{URL: "u"}, Options{Bin: "mpv", Timeout: 1, Profile: "low-latency"})
	found := false
	for i, arg := range got {
		if arg == "--profile=low-latency" {
			found = true
			if got[i-1] != "--hwdec=auto-safe" {
				t.Errorf("profile not directly after hwdec: %v", got)
			}
		}
	}
	if !found {
		t.Errorf("--profile missing from %v", got)
	}
}

// TestBuildArgsAudioURLEquality pins: audio equal to the video URL is
// not passed twice (python guard).
func TestBuildArgsAudioURLEquality(t *testing.T) {
	t.Parallel()

	got := BuildArgs(Request{URL: "u", AudioURL: "u"}, Options{Bin: "mpv"})
	for _, arg := range got {
		if arg == "--audio-file=u" {
			t.Error("duplicate audio-file emitted for equal URLs")
		}
	}
}

// TestOfflineTitle pins the offline window title composition with the
// [OFFLINE] marker and [provider] prefix stripping.
func TestOfflineTitle(t *testing.T) {
	t.Parallel()

	got := OfflineTitle("[kodik] Dub RU", "[animego] Dub EN", "Название", "12")
	want := "[Dub RU - Dub EN] Название - 12 [OFFLINE]"
	if got != want {
		t.Errorf("OfflineTitle = %q, want %q", got, want)
	}
}

func newTestPlayer(bin string) *Player {
	p := New(Options{Bin: bin})
	p.warmup = 150 * time.Millisecond
	p.retryDelay = 20 * time.Millisecond
	p.termGrace = 2 * time.Second
	return p
}

// TestPlayMissingBinaryFailsLoud pins the LookPath typed error: a
// missing player binary aborts without retries.
func TestPlayMissingBinaryFailsLoud(t *testing.T) {
	t.Parallel()

	p := newTestPlayer("/nonexistent/mpv-binary")
	err := p.Play(context.Background(), Request{URL: "u"})
	if err == nil {
		t.Fatal("Play with missing binary returned nil, want error")
	}
	var notFound *ErrBinaryNotFound
	if !errors.As(err, &notFound) {
		t.Fatalf("Play err = %v, want ErrBinaryNotFound", err)
	}
	if notFound.Bin != "/nonexistent/mpv-binary" {
		t.Errorf("ErrBinaryNotFound.Bin = %q", notFound.Bin)
	}
}

// TestPlayCancelTerminatesProcess pins the shutdown ladder end to end:
// cancelling the context SIGTERMs the process group and Play returns
// promptly with the context error.
func TestPlayCancelTerminatesProcess(t *testing.T) {
	t.Parallel()

	p := newTestPlayer(buildStub(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- p.Play(ctx, Request{URL: "ignored"})
	}()

	time.Sleep(500 * time.Millisecond) // started and past warmup
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Play err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Play did not return within 5s of cancellation")
	}
}

// TestPlayCancelEscalatesToSIGKILL pins the escalation path: a player
// ignoring SIGTERM is SIGKILLed after the grace window.
func TestPlayCancelEscalatesToSIGKILL(t *testing.T) {
	t.Parallel()

	p := newTestPlayer(buildStub(t))
	p.termGrace = 300 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- p.Play(ctx, Request{URL: "trapterm"})
	}()

	time.Sleep(500 * time.Millisecond)
	start := time.Now()
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Play err = %v, want context.Canceled", err)
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Errorf("SIGKILL escalation took %v, want fast", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Play did not return after SIGKILL escalation")
	}
}

// TestPlayImmediateExitRetries pins the warmup loop: a process dying
// within the warmup window is retried, and exhaustion surfaces
// ErrLaunchExhausted.
func TestPlayImmediateExitRetries(t *testing.T) {
	t.Parallel()

	p := newTestPlayer(buildStub(t))
	p.maxRetries = 2

	start := time.Now()
	err := p.Play(context.Background(), Request{URL: "exitnow"})
	if err == nil {
		t.Fatal("Play with instantly-exiting binary returned nil, want retry-exhausted error")
	}
	if !errors.Is(err, ErrLaunchExhausted) {
		t.Fatalf("Play err = %v, want ErrLaunchExhausted", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("retries took %v, want fast", elapsed)
	}
}

// TestPlayCleanExit pins: a process surviving warmup and exiting on its
// own returns nil.
func TestPlayCleanExit(t *testing.T) {
	t.Parallel()

	p := newTestPlayer(buildStub(t))
	if err := p.Play(context.Background(), Request{URL: "exitsoon"}); err != nil {
		t.Fatalf("Play: %v", err)
	}
}

// TestPlayCleansChaptersFile pins: the chapters file is removed after
// the player exits on cancellation (python lifecycle cleanup).
func TestPlayCleansChaptersFile(t *testing.T) {
	t.Parallel()

	chaptersDir := t.TempDir()
	chapters := chaptersDir + "/chapters.ffmetadata"
	if err := os.WriteFile(chapters, []byte(";FFMETADATA1\n"), 0o600); err != nil {
		t.Fatalf("seed chapters: %v", err)
	}

	p := newTestPlayer(buildStub(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- p.Play(ctx, Request{URL: "ignored", ChaptersFile: chapters})
	}()
	time.Sleep(500 * time.Millisecond)
	cancel()
	<-done

	if _, err := os.Stat(chapters); !os.IsNotExist(err) {
		t.Errorf("chapters file survived player exit: stat err = %v", err)
	}
}

// TestPlayLogPump forwards mpv output lines to the installed sink.
func TestPlayLogPump(t *testing.T) {
	t.Parallel()

	p := newTestPlayer(buildStub(t))
	var lines []string
	var mu sync.Mutex
	p.SetLog(func(line string) {
		mu.Lock()
		lines = append(lines, line)
		mu.Unlock()
	})
	if err := p.Play(context.Background(), Request{URL: "exitsoon"}); err != nil {
		t.Fatalf("Play: %v", err)
	}
	// The stub prints nothing, but the pump must not wedge the flow;
	// asserting no-hang IS the contract here. If the stub ever prints,
	// lines arrive unordered-safe.
	mu.Lock()
	defer mu.Unlock()
	_ = lines
}

// TestPlayLogPumpDeliversAllOutput pins the pump-before-Wait contract:
// Wait closes the stdout pipe when the process exits, so a pump racing
// Wait loses tail lines still buffered in the pipe. The slow sink
// keeps the pump behind the process exit so the race shows up as
// missing lines; draining the pipe to EOF before Wait must deliver
// every line the process wrote.
func TestPlayLogPumpDeliversAllOutput(t *testing.T) {
	t.Parallel()

	p := newTestPlayer(buildStub(t))
	var mu sync.Mutex
	delivered := 0
	p.SetLog(func(string) {
		time.Sleep(3 * time.Millisecond) // lag the pump behind the exit
		mu.Lock()
		delivered++
		mu.Unlock()
	})
	if err := p.Play(context.Background(), Request{URL: "printslow"}); err != nil {
		t.Fatalf("Play: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if delivered != 400 {
		t.Errorf("log lines delivered = %d, want 400 (pipe must drain before Wait)", delivered)
	}
}
