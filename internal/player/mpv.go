package player

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Player launch defaults (python play_detached: 5 attempts, 2s warmup,
// 1s restart delay).
const (
	defaultMaxRetries = 5
	defaultWarmup     = 2 * time.Second
	defaultRetryDelay = 1 * time.Second
	defaultTermGrace  = 3 * time.Second
)

// mpvExitInit is mpv's documented init-failure exit code ("error
// initializing mpv", including unknown options) — a deterministic
// failure no retry can fix (PR163 classification; exit 2 = "file
// couldn't be played" stays retryable as the transient class).
const mpvExitInit = 1

// The stderr tail embedded into launch errors (PR163 actionable
// errors): the last lines of mpv's own output — "Failed to open …",
// "HTTP Error 403" — are the only clue a user gets when playback
// fails. Bounded so a chatty mpv cannot balloon the message.
const (
	mpvTailLines   = 6
	mpvTailLineMax = 200
)

// ErrBinaryNotFound reports a missing player binary (fail-loud typed
// error; wraps exec.ErrNotFound).
type ErrBinaryNotFound struct {
	// Bin is the binary that was requested.
	Bin string
}

// Error implements error.
func (e *ErrBinaryNotFound) Error() string {
	return fmt.Sprintf("player: binary not found: %s", e.Bin)
}

// ErrLaunchExhausted reports that every launch attempt died within the
// warmup window.
var ErrLaunchExhausted = errors.New("player: all launch attempts failed")

// ErrLaunchInit reports that mpv exited with its init-failure code
// (exit 1: unknown option, config failure) — a deterministic failure
// no retry can fix (PR163).
var ErrLaunchInit = errors.New("player: mpv failed to initialize")

// LogFunc receives mpv stdout lines (python lifecycle manager printed
// them; the UI wires its own renderer).
type LogFunc func(line string)

// Player runs mpv with retries and deterministic shutdown.
type Player struct {
	bin        string
	opts       Options
	warmup     time.Duration
	retryDelay time.Duration
	termGrace  time.Duration
	maxRetries int
	onLog      LogFunc

	// tailMu guards tail, the bounded recent-output ring the launch
	// errors embed (PR163). Filled from mpv's own output only — the
	// player's own lifecycle lines stay out.
	tailMu sync.Mutex
	tail   []string
}

// New builds the player; zero Options fall back to the ported
// defaults.
func New(opts Options) *Player {
	if opts.Bin == "" {
		opts.Bin = "mpv"
	}
	p := &Player{
		bin:        opts.Bin,
		opts:       opts,
		warmup:     opts.Warmup,
		retryDelay: opts.RetryDelay,
		termGrace:  opts.TermGrace,
		maxRetries: opts.MaxRetries,
	}
	if p.warmup <= 0 {
		p.warmup = defaultWarmup
	}
	if p.retryDelay <= 0 {
		p.retryDelay = defaultRetryDelay
	}
	if p.termGrace <= 0 {
		p.termGrace = defaultTermGrace
	}
	if p.maxRetries <= 0 {
		p.maxRetries = defaultMaxRetries
	}
	return p
}

// SetLog installs the stdout line sink (optional).
func (p *Player) SetLog(fn LogFunc) { p.onLog = fn }

// Play launches mpv and blocks until it exits, the context is canceled
// or the retry budget is exhausted.
//
// Lifecycle (python play_detached port):
//   - up to MaxRetries attempts; a process surviving Warmup counts as
//     launched and Play waits for its natural exit (nil return on code
//     0, mapped error otherwise);
//   - a process dying within Warmup is retried after RetryDelay;
//   - context cancellation SIGTERMs the process group (python
//     start_new_session), waits TermGrace and escalates to SIGKILL;
//     Play then returns the context error;
//   - the chapters file is removed on every exit path.
func (p *Player) Play(ctx context.Context, req Request) error {
	args := BuildArgs(req, p.launchOpts())
	defer p.cleanupChapters(req.ChaptersFile)
	p.resetTail()

	var lastErr error
	for attempt := 1; attempt <= p.maxRetries; attempt++ {
		proc, err := p.start(ctx, args)
		if err != nil {
			var notFound *ErrBinaryNotFound
			if errors.As(err, &notFound) {
				return err
			}
			lastErr = err
			if !sleepCtx(ctx, p.retryDelay) {
				return ctx.Err()
			}
			continue
		}

		// Warmup window: did it stay alive?
		select {
		case err := <-proc.wait:
			// Died immediately: retry after the restart delay.
			p.logLine(fmt.Sprintf("mpv exited during warmup (attempt %d/%d): %v",
				attempt, p.maxRetries, err))
			lastErr = err
			// PR163 classification: mpv exit 1 = "error initializing
			// mpv" (unknown option, broken config) — deterministic,
			// so hammering the same launch cannot help. Exit 2 ("file
			// couldn't be played") covers the transient class and
			// keeps the python retry budget.
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == mpvExitInit {
				return fmt.Errorf("%w: %w; mpv output: %s",
					ErrLaunchInit, err, p.stderrTail())
			}
			if !sleepCtx(ctx, p.retryDelay) {
				return ctx.Err()
			}
			continue
		case <-ctx.Done():
			_ = proc.terminate(p.termGrace)
			return ctx.Err()
		case <-time.After(p.warmup):
			// Survived warmup: now the real wait.
			select {
			case err := <-proc.wait:
				return p.mapExit(err)
			case <-ctx.Done():
				_ = proc.terminate(p.termGrace)
				return ctx.Err()
			}
		}
	}
	var exhausted error
	if lastErr != nil {
		exhausted = fmt.Errorf("%w after %d attempts: %w", ErrLaunchExhausted, p.maxRetries, lastErr)
	} else {
		// Every attempt exited 0 within the warmup window: no
		// underlying error exists, and a %w with nil renders
		// %!w(<nil>) (PR163).
		exhausted = fmt.Errorf("%w after %d attempts", ErrLaunchExhausted, p.maxRetries)
	}
	// PR163: the error carries mpv's own last output lines — the bare
	// "exit status 2" told the owner nothing actionable.
	if tail := p.stderrTail(); tail != "" {
		return fmt.Errorf("%w; mpv output: %s", exhausted, tail)
	}
	return exhausted
}

// launchOpts renders the subset of Options BuildArgs consumes.
func (p *Player) launchOpts() Options {
	return Options{Bin: p.bin, Timeout: p.opts.Timeout, Profile: p.opts.Profile}
}

// mapExit is the exit-verdict seam for a natural (survived-warmup)
// exit. Clean exits pass through as nil; a failed exit carries mpv's
// own stderr tail (PR163) and the exit-1 init classification, so the
// user sees the failure reason instead of a bare "exit status 2".
func (p *Player) mapExit(err error) error {
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == mpvExitInit {
		return fmt.Errorf("%w: %w; mpv output: %s",
			ErrLaunchInit, err, p.stderrTail())
	}
	if tail := p.stderrTail(); tail != "" {
		return fmt.Errorf("%w; mpv output: %s", err, tail)
	}
	return err
}

// logLine forwards one mpv output line.
func (p *Player) logLine(line string) {
	if p.onLog != nil {
		p.onLog(line)
	}
}

// rememberTail appends one mpv output line to the bounded error tail.
func (p *Player) rememberTail(line string) {
	if line == "" {
		return
	}
	if len(line) > mpvTailLineMax {
		line = line[:mpvTailLineMax]
	}
	p.tailMu.Lock()
	defer p.tailMu.Unlock()
	p.tail = append(p.tail, line)
	if len(p.tail) > mpvTailLines {
		p.tail = p.tail[len(p.tail)-mpvTailLines:]
	}
}

// resetTail clears the ring for a fresh launch round.
func (p *Player) resetTail() {
	p.tailMu.Lock()
	defer p.tailMu.Unlock()
	p.tail = nil
}

// stderrTail renders the recent mpv output as one joined line; empty
// when the player said nothing.
func (p *Player) stderrTail() string {
	p.tailMu.Lock()
	defer p.tailMu.Unlock()
	if len(p.tail) == 0 {
		return ""
	}
	return strings.Join(p.tail, " | ")
}

// cleanupChapters removes the chapters file if it still exists
// (python lifecycle manager finally-block).
func (p *Player) cleanupChapters(path string) {
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		p.logLine(fmt.Sprintf("player: chapters cleanup %s: %v", path, err))
	}
}

// process is one running mpv instance.
type process struct {
	cmd  *exec.Cmd
	wait chan error // buffered; receives the Wait verdict exactly once
}

// start spawns the process group and begins pumping stdout.
func (p *Player) start(ctx context.Context, args []string) (*process, error) {
	if _, err := exec.LookPath(p.bin); err != nil {
		return nil, &ErrBinaryNotFound{Bin: p.bin}
	}

	// BuildArgs returns the full display argv WITH the binary as its
	// first element (the goldens pin that shape). exec.Command already
	// installs its name argument as argv[0], so the slice is consumed
	// WITHOUT the leading bin — passing it verbatim duplicated the
	// path as mpv's first playlist file ("Playing: /usr/bin/mpv"),
	// which poisoned every playback session's error flag (PR163).
	cmd := exec.Command(p.bin, args[1:]...) //nolint:gosec // bin/args are config-derived, not request input
	// python start_new_session=True: detach into its own process group
	// so group signals do not hit the CLI (platform-specific; see
	// mpv_unix.go / mpv_windows.go).
	setNewProcessGroup(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("player: stdout pipe: %w", err)
	}
	cmd.Stderr = cmd.Stdout // python redirected stderr into stdout

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("player: start %s: %w", p.bin, err)
	}

	proc := &process{cmd: cmd, wait: make(chan error, 1)}
	go func() {
		// Drain the pipe to EOF before Wait: Wait closes it on
		// process exit, which would drop log lines still buffered
		// inside (os/exec pipe contract).
		p.pump(stdout)
		proc.wait <- cmd.Wait()
	}()
	return proc, nil
}

// pump streams mpv output lines to the log sink.
func (p *Player) pump(stdout io.ReadCloser) {
	buf := make([]byte, 4096)
	var line []byte
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			line = append(line, buf[:n]...)
			for {
				idx := indexByte(line, '\n')
				if idx < 0 {
					break
				}
				text := string(trimCR(line[:idx]))
				p.rememberTail(text)
				p.logLine(text)
				line = line[idx+1:]
			}
		}
		if err != nil {
			if len(line) > 0 {
				text := string(trimCR(line))
				p.rememberTail(text)
				p.logLine(text)
			}
			_ = stdout.Close()
			return
		}
	}
}

// terminate runs the shutdown ladder: ask the platform to stop the
// process (group) — SIGTERM the group, wait the grace window, then
// SIGKILL on Unix; an immediate kill on Windows, where no deliverable
// SIGTERM exists (see terminateProcessGroup in the platform files) —
// and reap the exit status.
func (proc *process) terminate(grace time.Duration) error {
	if proc.cmd.Process == nil {
		return nil
	}
	return terminateProcessGroup(proc, grace, proc.wait)
}

// sleepCtx sleeps d unless ctx finishes first; false means interrupted.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// indexByte is bytes.IndexByte without the import churn.
func indexByte(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
}

// trimCR strips a trailing carriage return.
func trimCR(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\r' {
		return b[:len(b)-1]
	}
	return b
}
