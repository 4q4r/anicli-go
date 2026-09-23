package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/cfbrowser"
)

// Fakes for the ensure-browser seams.

func ensureFakeResolve(err error) func() error {
	return func() error { return err }
}

func ensureFakeInstall(steps ...int) func(context.Context, func(int, string)) error {
	return func(_ context.Context, onProgress func(int, string)) error {
		for _, pct := range steps {
			if onProgress != nil {
				onProgress(pct, "146.0.7680.177.5")
			}
		}
		return nil
	}
}

// A missing binary triggers the install: the output carries the cyan
// label, the throttled percents and the final state, and the caller
// owes the countdown (installed=true).
func TestEnsureBrowserMissingInstallsWithColoredProgress(t *testing.T) {
	var out bytes.Buffer
	e := browserEnsure{
		resolve: ensureFakeResolve(&cfbrowser.BinaryMissingError{}),
		install: ensureFakeInstall(40, 85, 100),
	}
	installed := e.run(context.Background(), &out, true)
	if !installed {
		t.Fatal("installed = false, want true (an install completed this run)")
	}
	got := out.String()
	for _, want := range []string{
		"\x1b[36mУстановка stealth-браузера…\x1b[0m 40%",
		"\x1b[36mУстановка stealth-браузера…\x1b[0m 100%",
		"\x1b[36mУстановка stealth-браузера… готово\x1b[0m",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%q", want, got)
		}
	}
}

// A present binary is silent: no output, no countdown owed.
func TestEnsureBrowserFreshIsSilent(t *testing.T) {
	var out bytes.Buffer
	e := browserEnsure{resolve: ensureFakeResolve(nil)}
	if installed := e.run(context.Background(), &out, true); installed {
		t.Fatal("installed = true, want false (binary already present)")
	}
	if out.Len() != 0 {
		t.Fatalf("fresh run must print nothing, got %q", out.String())
	}
}

// A failed install is a loud colored warning naming the reason — and
// degradation, not abort: installed=false, the TUI still starts.
func TestEnsureBrowserFailureWarnsAndDegrades(t *testing.T) {
	var out bytes.Buffer
	e := browserEnsure{
		resolve: ensureFakeResolve(&cfbrowser.BinaryMissingError{}),
		install: func(context.Context, func(int, string)) error {
			return errors.New(" dial tcp: connection refused")
		},
	}
	if installed := e.run(context.Background(), &out, true); installed {
		t.Fatal("installed = true, want false (install failed)")
	}
	got := out.String()
	if !strings.Contains(got, "\x1b[31m") {
		t.Errorf("warning must be red, got %q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("warning must name the reason, got %q", got)
	}
	// PR86: the manual cf install fallback is gone — the startup
	// auto-download retries at the next launch.
	if strings.Contains(got, "anicli cf install") {
		t.Errorf("warning must not reference the removed command, got %q", got)
	}
	if !strings.Contains(got, "следующем запуске") {
		t.Errorf("warning must promise the next-launch retry, got %q", got)
	}
}

// The countdown renders the owner-mandated lines literally —
// «Запуск через... 3» / «...2» / «...1», integer, colored, one per
// second — and consumes exactly three ticks before returning.
func TestCountdownLinesExact(t *testing.T) {
	var out bytes.Buffer
	ticks := make(chan time.Time, 3)
	for range 3 {
		ticks <- time.Now()
	}
	countdown(&out, ticks, true)
	got := out.String()
	for _, want := range []string{
		"\x1b[36mЗапуск через... 3\x1b[0m\n",
		"\x1b[36mЗапуск через... 2\x1b[0m\n",
		"\x1b[36mЗапуск через... 1\x1b[0m\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%q", want, got)
		}
	}
	if strings.Contains(got, "🚀") || strings.Contains(got, "CloakBrowser") {
		t.Errorf("countdown must carry no emoji and no product name:\n%q", got)
	}
}

// TestBrowserInstallOptionsCarriesConfig (PR80 review): the channel
// and the download proxy from [cf] must reach the startup install —
// auto/free/pro honored at boot.
func TestBrowserInstallOptionsCarriesConfig(t *testing.T) {
	progress := func(int, string) {}
	opts := browserInstallOptions("socks5://p:1080", "pro", progress)
	if opts.Channel != "pro" {
		t.Errorf("Channel = %q, want pro", opts.Channel)
	}
	if opts.ProxyURL != "socks5://p:1080" {
		t.Errorf("ProxyURL = %q, want the [cf] proxy", opts.ProxyURL)
	}
	if opts.OnProgress == nil {
		t.Error("OnProgress = nil, want the progress renderer wired")
	}
}

// Plain mode (pipes/CI): no ANSI at all — the start line, the final
// state and the countdown render as plain text, and intermediate
// percent updates (cursor feedback) are skipped entirely.
func TestEnsureBrowserPlainModeNoANSI(t *testing.T) {
	var out bytes.Buffer
	e := browserEnsure{
		resolve: ensureFakeResolve(&cfbrowser.BinaryMissingError{}),
		install: ensureFakeInstall(40, 100),
	}
	installed := e.run(context.Background(), &out, false)
	if !installed {
		t.Fatal("installed = false, want true")
	}
	got := out.String()
	if strings.Contains(got, "\x1b[") {
		t.Fatalf("plain mode must not emit ANSI, got %q", got)
	}
	for _, want := range []string{"Установка stealth-браузера…", "готово"} {
		if !strings.Contains(got, want) {
			t.Errorf("plain output missing %q:\n%q", want, got)
		}
	}
	if strings.Contains(got, "40%") {
		t.Errorf("plain mode must skip cursor-feedback percents:\n%q", got)
	}
}

func TestCountdownPlainModeNoANSI(t *testing.T) {
	var out bytes.Buffer
	ticks := make(chan time.Time, 3)
	for range 3 {
		ticks <- time.Now()
	}
	countdown(&out, ticks, false)
	got := out.String()
	if strings.Contains(got, "\x1b[") {
		t.Fatalf("plain countdown must not emit ANSI, got %q", got)
	}
	for _, want := range []string{"Запуск через... 3\n", "Запуск через... 2\n", "Запуск через... 1\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("plain countdown missing %q:\n%q", want, got)
		}
	}
}

// colorsEnabled: NO_COLOR (the no-color.org standard) and non-terminal
// writers disable styling.
func TestColorsEnabled(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if colorsEnabled(io.Discard) {
		t.Error("NO_COLOR must disable styling")
	}
	t.Setenv("NO_COLOR", "")
	if colorsEnabled(&bytes.Buffer{}) {
		t.Error("a non-file writer (pipe under test) must disable styling")
	}
}

// Styled mode renders ONE updating line: no separate start line, and
// a new install attempt (version change across the auto ladder)
// starts a fresh line — a backward percent jump never rewrites the
// previous attempt's line.
func TestEnsureBrowserStyledSingleLineAndAttemptBreaks(t *testing.T) {
	var out bytes.Buffer
	steps := []struct {
		pct     int
		version string
	}{
		{40, "146.0.7680.177.9"}, // pro attempt
		{96, "146.0.7680.177.9"},
		{10, "146.0.7680.177.5"}, // fallback attempt
		{100, "146.0.7680.177.5"},
	}
	e := browserEnsure{
		resolve: ensureFakeResolve(&cfbrowser.BinaryMissingError{}),
		install: func(_ context.Context, onProgress func(int, string)) error {
			for _, s := range steps {
				onProgress(s.pct, s.version)
			}
			return nil
		},
	}
	if installed := e.run(context.Background(), &out, true); !installed {
		t.Fatal("installed = false, want true")
	}
	got := out.String()
	if strings.Count(got, "\n") < 2 {
		t.Errorf("attempt break must start a fresh line, got %q", got)
	}
	// The fallback's 10% must not rewrite the pro attempt's 96% line.
	ten := strings.Index(got, " 10%")
	ninetySix := strings.Index(got, " 96%")
	if ten == -1 || ninetySix == -1 {
		t.Fatalf("missing percents in %q", got)
	}
	if !strings.Contains(got[:ten], "\n") || ten < ninetySix {
		t.Errorf("the fallback attempt must render after a line break:\n%q", got)
	}
	// No standalone start line before the first percent update.
	if strings.HasPrefix(got, eraseLine+ansiCyan+installLabel+ansiReset+"\n") {
		t.Errorf("the start label must not occupy its own line:\n%q", got)
	}
}
