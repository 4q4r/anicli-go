package cli

// PR80 startup auto-download: the stealth-Chromium binary is
// infrastructure, not a setup step — when it is missing the CLI
// downloads it BEFORE the TUI starts, with a colored pre-TUI progress
// line (plain terminal, ANSI is fine). A completed install this run
// owes the owner-mandated integer countdown («Запуск через... 3 … 2 …
// 1»); normal runs (binary present and fresh) boot straight away. A
// failed install is a degradation, never an abort: a colored warning
// names the reason, the TUI still starts, CF consumers surface typed
// errors at use and the background updater self-heals the install.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/an0nx/anicli-go/internal/cfbrowser"
)

// Pre-TUI ANSI palette (hand-rolled: no new deps).
const (
	ansiCyan  = "\x1b[36m"
	ansiRed   = "\x1b[31m"
	ansiReset = "\x1b[0m"
	// eraseLine rewrites the current console line in place.
	eraseLine = "\r\x1b[2K"
)

// installLabel is the progress line's text (the owner's example shape:
// `\x1b[36mУстановка stealth-браузера…\x1b[0m 42%` on a terminal).
const installLabel = "Установка stealth-браузера…"

// colorsEnabled reports whether ANSI styling may be used on out: a
// terminal character device, and NO_COLOR unset (the no-color.org
// standard) — pipes and CI get plain text.
func colorsEnabled(out io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := out.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// browserEnsure is the startup flow with injected seams: resolve
// answers whether a binary is usable, install performs the download
// reporting integer percents. Tests script both; production wires the
// cfbrowser resolver and installer.
type browserEnsure struct {
	resolve func() error
	install func(ctx context.Context, onProgress func(int, string)) error
}

// newBrowserEnsure wires the production seams. Any resolve failure
// (missing binary, incomplete cache dir) routes into Install, which
// re-resolves overrides and the cache itself. proxyURL is the [cf]
// download/update proxy (empty = direct); channel is the [cf] channel
// (auto/free/pro) honored at boot.
func newBrowserEnsure(proxyURL, channel string) browserEnsure {
	return browserEnsure{
		resolve: func() error {
			_, err := cfbrowser.ResolveCurrentBinary(cfbrowser.ResolveOptions{Channel: channel})
			return err
		},
		install: func(ctx context.Context, onProgress func(int, string)) error {
			_, err := cfbrowser.Install(ctx, browserInstallOptions(proxyURL, channel, onProgress))
			return err
		},
	}
}

// browserInstallOptions builds the startup install options: the [cf]
// channel picks the install ladder and proxyURL routes the download
// traffic; onProgress feeds the colored pre-TUI progress line.
func browserInstallOptions(proxyURL, channel string, onProgress func(int, string)) cfbrowser.InstallOptions {
	return cfbrowser.InstallOptions{
		Channel:    channel,
		ProxyURL:   proxyURL,
		OnProgress: onProgress,
	}
}

// warnText is the degradation warning: loud, colored red by the
// caller, names the reason and the manual fallback.
func warnText(err error) string {
	return fmt.Sprintf("Не удалось скачать stealth-браузер: %v — CF-источники могут не работать; установка повторится при следующем запуске с интернетом", err)
}

// run executes the flow against out (the pre-TUI stdout). It returns
// true when an install completed in THIS run — the caller owes the
// countdown before the TUI takes over. styled gates the ANSI palette
// (terminals only; pipes/CI render plain text without the cursor
// feedback percents).
func (e browserEnsure) run(ctx context.Context, out io.Writer, styled bool) bool {
	if e.resolve() == nil {
		return false // present and usable: silent fast path
	}

	if !styled {
		// Plain (pipes/CI): one start line, one final line, no ANSI,
		// no cursor feedback.
		_, _ = fmt.Fprintf(out, "%s\n", installLabel)
		if err := e.install(ctx, nil); err != nil {
			_, _ = fmt.Fprintf(out, "%s\n", warnText(err))
			return false
		}
		_, _ = fmt.Fprintf(out, "%s\n", installLabel+" готово")
		return true
	}

	// Styled (terminal): ONE updating line. The auto ladder may
	// attempt pro, fail and fall back to free — a version change
	// starts a fresh line (the previous attempt's line stays in
	// scrollback) and re-arms the monotonic percent clamp.
	_, _ = fmt.Fprintf(out, "%s%s%s", eraseLine, ansiCyan, installLabel)
	lastVersion, lastPct := "", -1
	err := e.install(ctx, func(pct int, version string) {
		if version != lastVersion {
			lastVersion, lastPct = version, -1
			_, _ = fmt.Fprint(out, "\n")
		}
		if pct < lastPct {
			return // display clamp: never backward within one attempt
		}
		lastPct = pct
		_, _ = fmt.Fprintf(out, "%s%s%s%s %d%%", eraseLine, ansiCyan, installLabel, ansiReset, pct)
	})
	if err != nil {
		_, _ = fmt.Fprintf(out, "%s%s%s%s", eraseLine, ansiRed, warnText(err), ansiReset)
		return false
	}
	_, _ = fmt.Fprintf(out, "%s%s%s готово%s", eraseLine, ansiCyan, installLabel, ansiReset)
	return true
}

// countdown renders the owner-mandated integer countdown — the lines
// are literally «Запуск через... 3» … «Запуск через... 1», one line
// per second (three ticks consumed in total) — and returns right
// before the TUI starts. tick is injectable for tests; production
// passes a 1-second ticker. styled gates the ANSI palette.
func countdown(out io.Writer, tick <-chan time.Time, styled bool) {
	for n := 3; n >= 1; n-- {
		if styled {
			_, _ = fmt.Fprintf(out, "%sЗапуск через... %d%s\n", ansiCyan, n, ansiReset)
		} else {
			_, _ = fmt.Fprintf(out, "Запуск через... %d\n", n)
		}
		<-tick
	}
}

// runCountdown is the production wrapper: a fresh 1-second ticker,
// stopped when the countdown completes.
func runCountdown(out io.Writer, styled bool) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	countdown(out, ticker.C, styled)
}

// ensureBrowserSilent is the non-interactive variant (the serve face):
// same resolve/install flow, but progress stays on slog and failures
// log instead of printing — no colors, no countdown.
func ensureBrowserSilent(ctx context.Context, proxyURL, channel string) {
	ensure := newBrowserEnsure(proxyURL, channel)
	if ensure.resolve() == nil {
		return
	}
	if err := ensure.install(ctx, nil); err != nil {
		slog.Warn("cfbrowser: startup auto-download failed; CF consumers may type-error until it heals", "error", err)
	}
}
