//go:build live

package tui

// LIVE probe for PR111 (live settle counter on the provider search
// loading line). Excluded from the hermetic default suite by the
// `live` build tag. Run manually:
//
//	go test -tags live -run TestLivePR111SettleCounter -count=1 -v ./internal/tui/
//
// Prints, for the query below (or $ANICLI_LIVE_QUERY):
//  1. the loading frame right after Init;
//  2. mid-flight frames at the first two settles and at the halfway
//     mark: the line re-renders as «k/N providers, K results…»
//     while the spinner frame animates — the fan-out visibly moves;
//  3. the settled frame: the found/not-found summary above the
//     merged checklist (the PR110 surface, unchanged).

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
)

func TestLivePR111SettleCounter(t *testing.T) {
	_, deps := liveDeps(t)

	query := os.Getenv("ANICLI_LIVE_QUERY")
	if query == "" {
		query = "Ателье колдовских колпаков"
	}
	total := len(deps.Search.Providers())

	sp := NewSearchProgress(deps, query)
	// The real runtime always delivers the tracked terminal size
	// before the first render.
	if _, cmd := sp.Update(tea.WindowSizeMsg{Width: 100, Height: 30}); cmd != nil {
		t.Fatalf("WindowSizeMsg must not schedule commands")
	}

	t.Logf("=== FRAME 0 right after build (providers %d) ===\n%s", total, pr110Indent(sp.View().Content))

	if cmd := sp.Init(); cmd != nil {
		runLivePR111(t, sp, cmd, total)
	}
	if len(sp.pending) > 0 {
		t.Fatalf("fan-out must have settled every row, %d still pending", len(sp.pending))
	}

	// The settled frame: summary + merged checklist, no loading line.
	v := sp.View().Content
	t.Logf("=== FRAME final settled (%d/%d, %d results) ===\n%s",
		total, total, len(sp.results), pr110Indent(v))
	if strings.Contains(v, "провайдеров,") {
		t.Fatalf("the settled frame must drop the loading line:\n%s", v)
	}
	if want := "Found: " + strconv.Itoa(len(sp.results)); !strings.Contains(v, want) {
		t.Fatalf("settled frame must carry the summary %q:\n%s", want, v)
	}
}

// runLivePR111 executes a command chain the way the bubbletea runtime
// would: batches fan out through a queue (so settle numbering stays
// global across branches), every provider command runs to completion,
// and the first two settles plus the halfway settle re-render the
// mid-flight live-counter frame into the test log. Each logged frame
// must show the exact counter «settled/total провайдеров» — the
// feature under proof.
func runLivePR111(t *testing.T, sp *searchProgress, cmd tea.Cmd, total int) {
	t.Helper()
	queue := []tea.Cmd{cmd}
	settled, logged := 0, map[int]bool{}
	for i := 0; len(queue) > 0 && i < 8192; i++ {
		cmd := queue[0]
		queue = queue[1:]
		if cmd == nil {
			continue
		}
		msg := cmd()
		switch m := msg.(type) {
		case spinner.TickMsg:
			// Never follow the blink loop.
		case tea.BatchMsg:
			queue = append(queue, m...)
		case providerResultMsg:
			_, next := sp.Update(msg)
			if next != nil {
				queue = append(queue, next)
			}
			settled++
			show := settled <= 2 || settled == (total+1)/2
			if show && len(sp.pending) > 0 && !logged[settled] {
				logged[settled] = true
				frame := sp.View().Content
				want := strconv.Itoa(total-len(sp.pending)) + "/" + strconv.Itoa(total) + " провайдеров"
				if !strings.Contains(frame, want) {
					t.Fatalf("mid-flight frame must carry the live counter %q:\n%s", want, frame)
				}
				t.Logf("=== FRAME after settle #%d (%d results) ===\n%s",
					settled, len(sp.results), pr110Indent(frame))
			}
		default:
			_, next := sp.Update(msg)
			if next != nil {
				queue = append(queue, next)
			}
		}
	}
}
