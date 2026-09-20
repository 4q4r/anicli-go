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

// installLabel is the colored progress line's text (the owner's
// example shape: `\x1b[36mУстановка stealth-браузера…\x1b[0m 42%`).
const installLabel = "Установка stealth-браузера…"

// browserEnsure is the startup flow with injected seams: resolve
// answers whether a binary is usable, install performs the download
// reporting integer percents. Tests script both; production wires the
// cfbrowser resolver and installer with the [cf] download proxy.
type browserEnsure struct {
	resolve func() error
	install func(ctx context.Context, onProgress func(int, string)) error
}

// newBrowserEnsure wires the production seams. Any resolve failure
// (missing binary, incomplete cache dir) routes into Install, which
// re-resolves overrides and the cache itself. proxyURL is the [cf]
// download/update proxy (empty = direct).
func newBrowserEnsure(proxyURL string) browserEnsure {
	return browserEnsure{
		resolve: func() error {
			_, err := cfbrowser.ResolveCurrentBinary(cfbrowser.ResolveOptions{})
			return err
		},
		install: func(ctx context.Context, onProgress func(int, string)) error {
			_, err := cfbrowser.Install(ctx, cfbrowser.InstallOptions{
				OnProgress: onProgress,
				ProxyURL:   proxyURL,
			})
			return err
		},
	}
}

// run executes the flow against out (the pre-TUI stdout). It returns
// true when an install completed in THIS run — the caller owes the
// countdown before the TUI takes over.
func (e browserEnsure) run(ctx context.Context, out io.Writer) bool {
	if e.resolve() == nil {
		return false // present and usable: silent fast path
	}

	_, _ = fmt.Fprintf(out, "%s%s%s\n", ansiCyan, installLabel, ansiReset)
	err := e.install(ctx, func(pct int, _ string) {
		_, _ = fmt.Fprintf(out, "%s%s%s%s %d%%", eraseLine, ansiCyan, installLabel, ansiReset, pct)
	})
	if err != nil {
		_, _ = fmt.Fprintf(out, "%s%sНе удалось скачать stealth-браузер: %v — CF-источники могут не работать; позже выполните: anicli cf install%s\n",
			eraseLine, ansiRed, err, ansiReset)
		return false
	}
	_, _ = fmt.Fprintf(out, "%s%s%s готово%s\n", eraseLine, ansiCyan, installLabel, ansiReset)
	return true
}

// countdown renders the owner-mandated integer countdown — the lines
// are literally «Запуск через... 3» … «Запуск через... 1», colored,
// one line per second (three ticks consumed in total) — and returns
// right before the TUI starts. tick is injectable for tests;
// production passes a 1-second ticker.
func countdown(out io.Writer, tick <-chan time.Time) {
	for n := 3; n >= 1; n-- {
		_, _ = fmt.Fprintf(out, "%sЗапуск через... %d%s\n", ansiCyan, n, ansiReset)
		<-tick
	}
}

// runCountdown is the production wrapper: a fresh 1-second ticker,
// stopped when the countdown completes.
func runCountdown(out io.Writer) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	countdown(out, ticker.C)
}

// ensureBrowserSilent is the non-interactive variant (the serve face):
// same resolve/install flow, but progress stays on slog and failures
// log instead of printing — no colors, no countdown.
func ensureBrowserSilent(ctx context.Context, proxyURL string) {
	ensure := newBrowserEnsure(proxyURL)
	if ensure.resolve() == nil {
		return
	}
	if err := ensure.install(ctx, nil); err != nil {
		slog.Warn("cfbrowser: startup auto-download failed; CF consumers may type-error until it heals", "error", err)
	}
}
