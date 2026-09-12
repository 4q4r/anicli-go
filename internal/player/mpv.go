package player

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
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
	return fmt.Errorf("%w after %d attempts: %w", ErrLaunchExhausted, p.maxRetries, lastErr)
}

// launchOpts renders the subset of Options BuildArgs consumes.
func (p *Player) launchOpts() Options {
	return Options{Bin: p.bin, Timeout: p.opts.Timeout, Profile: p.opts.Profile}
}

// mapExit is the exit-verdict seam; it is currently the identity.
// Wait already reports clean exits as nil, and the cancellation paths
// return ctx.Err() directly, so no translation is needed here.
func (p *Player) mapExit(err error) error {
	return err
}

// logLine forwards one mpv output line.
func (p *Player) logLine(line string) {
	if p.onLog != nil {
		p.onLog(line)
	}
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

	cmd := exec.Command(p.bin, args...) //nolint:gosec // bin/args are config-derived, not request input
	// python start_new_session=True: detach into its own process group
	// so group signals do not hit the CLI.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

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
				p.logLine(string(trimCR(line[:idx])))
				line = line[idx+1:]
			}
		}
		if err != nil {
			if len(line) > 0 {
				p.logLine(string(trimCR(line)))
			}
			_ = stdout.Close()
			return
		}
	}
}

// terminate runs the shutdown ladder: SIGTERM the process group, wait
// the grace window, then SIGKILL.
func (proc *process) terminate(grace time.Duration) error {
	if proc.cmd.Process == nil {
		return nil
	}
	pgid := -proc.cmd.Process.Pid
	if err := syscall.Kill(pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("player: SIGTERM process group: %w", err)
	}
	select {
	case err := <-proc.wait:
		return err
	case <-time.After(grace):
		if err := syscall.Kill(pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("player: SIGKILL process group: %w", err)
		}
		return <-proc.wait
	}
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
