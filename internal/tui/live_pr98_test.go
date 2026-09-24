//go:build live

package tui

// LIVE probe for PR98 (bordered fan-out table). Excluded from the
// hermetic default suite by the `live` build tag. Run manually:
//
//	go test -tags live -run TestLivePR98FanoutTable -count=1 -v ./internal/tui/
//
// Prints, for «Ателье колдовских колпаков»:
//  1. the live bordered fan-out after EVERY settle (the box must stay
//     rectangular and keep its width while rows land — the python
//     «ровные поля» ruling);
//  2. the same settled table compressed to a 44-column terminal;
//  3. the per-settle log lines carry the FULL provider errors (the
//     stderr sink doubles as the file log; cells show short labels).

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
)

func TestLivePR98FanoutTable(t *testing.T) {
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
	if cmd := sp.Init(); cmd != nil {
		runLiveCmd(t, sp, cmd)
	}
	if len(sp.pending) > 0 {
		t.Fatalf("fan-out must have settled every row, %d still pending", len(sp.pending))
	}

	t.Logf("=== FINAL TABLE (tracked width 100) ===\n%s",
		indentBlock(strings.TrimRight(sp.View().Content, "\n")))

	if _, _ = sp.Update(tea.WindowSizeMsg{Width: 44, Height: 30}); false {
		t.Fatal("unreachable")
	}
	t.Logf("=== NARROW TABLE (tracked width 44) ===\n%s",
		indentBlock(strings.TrimRight(sp.View().Content, "\n")))
}

// runLiveCmd executes a command chain the way the bubbletea runtime
// would: batches fan out, each provider command runs to completion,
// and every settle re-renders the live table into the test log.
func runLiveCmd(t *testing.T, sp *searchProgress, cmd tea.Cmd) {
	t.Helper()
	for i := 0; cmd != nil && i < 256; i++ {
		msg := cmd()
		switch m := msg.(type) {
		case spinner.TickMsg:
			return
		case tea.BatchMsg:
			for _, sub := range m {
				runLiveCmd(t, sp, sub)
			}
			return
		default:
			if pr, ok := msg.(providerResultMsg); ok {
				_, next := sp.Update(msg)
				t.Logf("--- settled %-14s (pending %d) ---\n%s",
					pr.provider.ID, len(sp.pending),
					indentBlock(strings.TrimRight(sp.View().Content, "\n")))
				cmd = next
				continue
			}
			var next tea.Cmd
			_, next = sp.Update(msg)
			cmd = next
		}
	}
}
