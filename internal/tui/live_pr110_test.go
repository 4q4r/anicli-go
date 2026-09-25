//go:build live

package tui

// LIVE probe for PR110 (minimal fan-out view: loading line, summary,
// checklist — the bordered table is gone). Excluded from the hermetic
// default suite by the `live` build tag. Run manually:
//
//	go test -tags live -run TestLivePR110MinimalFanout -count=1 -v ./internal/tui/
//
// Prints, for the query below (or $ANICLI_LIVE_QUERY):
//  1. the loading frame right after Init: ONE line — spinner +
//     «Ищу по N провайдерам…» — with no table chrome;
//  2. the first mid-flight settle: the loading line persists;
//  3. the settled frame: the found/not-found summary above the
//     merged checklist;
//  4. per-provider settle log lines carry the FULL errors on the log
//     sink (the stderr handler stands in for the file log) while the
//     TUI frames stay free of them.

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
)

func TestLivePR110MinimalFanout(t *testing.T) {
	_, deps := liveDeps(t)
	// Production wires Deps.Log onto the file log; the probe mirrors
	// that by pointing it at stderr so the per-settle lines (full
	// errors) land in the captured output.
	deps.Log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))

	query := os.Getenv("ANICLI_LIVE_QUERY")
	if query == "" {
		query = "Ателье колдовских колпаков"
	}

	sp := NewSearchProgress(deps, query)
	// The real runtime always delivers the tracked terminal size
	// before the first render.
	if _, cmd := sp.Update(tea.WindowSizeMsg{Width: 100, Height: 30}); cmd != nil {
		t.Fatalf("WindowSizeMsg must not schedule commands")
	}

	// 1. The loading frame BEFORE any settle: one minimal line under
	// the title (the Shikimori notice while enriching, the «Ищу по N
	// провайдерам…» line once the fan-out runs). Either way: no
	// table, no summary.
	loading := sp.View().Content
	t.Logf("=== LOADING FRAME (pending %d) ===\n%s", len(sp.pending), pr110Indent(loading))
	for _, banned := range []string{"┌", "│", "└", "Ответившие"} {
		if strings.Contains(loading, banned) {
			t.Fatalf("loading frame must not carry table chrome %q:\n%s", banned, loading)
		}
	}
	if !strings.Contains(loading, "Shikimori") && !strings.Contains(loading, "Ищу по") {
		t.Fatalf("loading frame must carry a loading line:\n%s", loading)
	}

	if cmd := sp.Init(); cmd != nil {
		runLivePR110(t, sp, cmd)
	}
	if len(sp.pending) > 0 {
		t.Fatalf("fan-out must have settled every row, %d still pending", len(sp.pending))
	}

	// 3. The settled frame: summary + merged checklist.
	v := sp.View().Content
	t.Logf("=== SETTLED (results %d) ===\n%s", len(sp.results), pr110Indent(v))

	want := "Найдено: " + strconv.Itoa(len(sp.results))
	if !strings.Contains(v, want) {
		t.Fatalf("settled frame must carry the summary %q:\n%s", want, v)
	}
	for _, banned := range []string{"┌", "│", "└", "Ответившие", "Завершено"} {
		if strings.Contains(v, banned) {
			t.Fatalf("settled frame must not carry table chrome %q:\n%s", banned, v)
		}
	}
}

// runLivePR110 executes a command chain the way the bubbletea runtime
// would: batches fan out, each provider command runs to completion,
// and the FIRST settle re-renders the mid-flight loading frame into
// the test log.
func runLivePR110(t *testing.T, sp *searchProgress, cmd tea.Cmd) {
	t.Helper()
	firstSettleLogged := false
	for i := 0; cmd != nil && i < 256; i++ {
		msg := cmd()
		switch m := msg.(type) {
		case spinner.TickMsg:
			return
		case tea.BatchMsg:
			for _, sub := range m {
				runLivePR110(t, sp, sub)
			}
			return
		default:
			if _, ok := msg.(providerResultMsg); ok {
				_, next := sp.Update(msg)
				if !firstSettleLogged && len(sp.pending) > 0 {
					firstSettleLogged = true
					t.Logf("=== MID-FLIGHT FRAME (pending %d) ===\n%s",
						len(sp.pending), pr110Indent(sp.View().Content))
				}
				cmd = next
				continue
			}
			var next tea.Cmd
			_, next = sp.Update(msg)
			cmd = next
		}
	}
}

// pr110Indent prefixes every line with the two-space screen gutter so
// the captured frames read like the rendered screen.
func pr110Indent(s string) string {
	return "  " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n  ")
}
