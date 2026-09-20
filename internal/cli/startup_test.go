package cli

import (
	"bytes"
	"context"
	"errors"
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
			onProgress(pct, "146.0.7680.177.5")
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
	installed := e.run(context.Background(), &out)
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
	if installed := e.run(context.Background(), &out); installed {
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
	if installed := e.run(context.Background(), &out); installed {
		t.Fatal("installed = true, want false (install failed)")
	}
	got := out.String()
	if !strings.Contains(got, "\x1b[31m") {
		t.Errorf("warning must be red, got %q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("warning must name the reason, got %q", got)
	}
	if !strings.Contains(got, "anicli cf install") {
		t.Errorf("warning must name the manual fallback, got %q", got)
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
	countdown(&out, ticks)
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
