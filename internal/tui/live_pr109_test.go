//go:build live

package tui

// LIVE capture for PR109 (fan-out table viewport). Excluded from the
// hermetic default suite by the `live` build tag. Run manually:
//
//	go test -tags live -run TestLivePR109Viewport -count=1 -v ./internal/tui/
//
// Drives the REAL provider fan-out on «Ателье колдовских колпаков»
// against a 120x24 terminal: captures the live render (tail-follow,
// bounded), the settled render (review window at the roster head) and
// the Shift+Down scrolled state.

import (
	"sync"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
)

// flattenBatch turns a tea.Cmd tree (BatchMsg) into a flat list of
// leaf commands, dropping spinner frames.
func flattenBatch(cmd tea.Cmd) []tea.Cmd {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	switch msg.(type) {
	case spinner.TickMsg:
		return nil
	case tea.BatchMsg:
		var out []tea.Cmd
		for _, sub := range msg.(tea.BatchMsg) {
			out = append(out, flattenBatch(sub)...)
		}
		return out
	default:
		return []tea.Cmd{func() tea.Msg { return msg }}
	}
}

func TestLivePR109Viewport(t *testing.T) {
	_, deps := liveDeps(t)

	sp := NewSearchProgress(deps, "Ателье колдовских колпаков")
	if next, _ := sp.Update(tea.WindowSizeMsg{Width: 120, Height: 24}); next != nil {
		sp = next.(*searchProgress)
	}

	// runCmds executes a flat command list concurrently (the bubbletea
	// runtime runs batch members in parallel) and streams the
	// resolved messages back.
	runCmds := func(cmds []tea.Cmd) <-chan tea.Msg {
		out := make(chan tea.Msg, 256)
		var wg sync.WaitGroup
		for _, c := range cmds {
			if c == nil {
				continue
			}
			wg.Add(1)
			go func(c tea.Cmd) {
				defer wg.Done()
				if m := c(); m != nil {
					out <- m
				}
			}(c)
		}
		go func() {
			wg.Wait()
			close(out)
		}()
		return out
	}

	// Init: the Shikimori enrichment resolves the variant set first.
	initCmd := sp.Init()
	variants := searchVariantsMsg{}
	for _, c := range flattenBatch(initCmd) {
		if sv, ok := c().(searchVariantsMsg); ok {
			variants = sv
		}
	}
	t.Logf("=== variants resolved (%d) ===", len(variants.variants))

	next, fanoutCmd := sp.Update(variants)
	sp = next.(*searchProgress)
	results := runCmds(flattenBatch(fanoutCmd))
	t.Logf("=== fan-out dispatched over %d providers ===", len(sp.rows))

	// Feed settlements as they land; snapshot mid-flight (the live
	// tail-follow) and at settle (the review window).
	liveSnapshotted := false
	settled := 0
	deadline := time.After(5 * time.Minute)
	for {
		var m tea.Msg
		select {
		case m = <-results:
		case <-deadline:
			t.Fatalf("live fan-out exceeded the 5-minute capture deadline (settled %d/%d)",
				settled, len(sp.rows))
		}
		if _, ok := m.(spinner.TickMsg); ok {
			continue
		}
		next, _ := sp.Update(m)
		sp = next.(*searchProgress)
		if _, isResult := m.(providerResultMsg); isResult {
			settled++
			if settled%10 == 0 {
				t.Logf("... progress: %d/%d settled", settled, len(sp.rows))
			}
		}
		if !liveSnapshotted && settled >= 6 && len(sp.pending) > 0 {
			t.Logf("=== LIVE RENDER (settled %d/%d in flight, terminal 120x24) ===\n%s",
				settled, len(sp.rows), sp.View().Content)
			liveSnapshotted = true
		}
		if len(sp.pending) == 0 {
			break
		}
	}

	t.Logf("=== SETTLED RENDER (%d providers, review window at the head) ===\n%s",
		len(sp.rows), sp.View().Content)

	// Review scroll: Shift+Down past the clamp, then back up.
	for range 40 {
		next, _ := sp.Update(tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModShift})
		sp = next.(*searchProgress)
	}
	t.Logf("=== SCROLLED TO TAIL (Shift+Down ×40) ===\n%s", sp.View().Content)
	for range 10 {
		next, _ := sp.Update(tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModShift})
		sp = next.(*searchProgress)
	}
	t.Logf("=== SCROLLED BACK (Shift+Up ×10) ===\n%s", sp.View().Content)
}
