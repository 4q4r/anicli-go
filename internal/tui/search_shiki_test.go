package tui

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/storage"
)

// shikiFlowDeps builds the production-shaped deps for the Shikimori
// pick flow tests: two providers, enabled Shikimori autocomplete, and
// metadata aliases.
func shikiFlowDeps(log *slog.Logger) (*fakeSearch, *Deps) {
	fs := newFakeSearch()
	fs.providers = []ProviderMeta{
		{ID: "animego", Name: "AnimeGO"},
		{ID: "anilib", Name: "AniLib"},
	}
	fs.results["animego"] = []contracts.SearchResult{{Title: "Наруто", URL: "u1", SourceID: "animego"}}
	shiki := &fakeShiki{enabled: true, ids: map[string]int64{"Наруто": 21}}
	md := &fakeMetadata{aliases: map[string][]string{"Наруто": {"Naruto"}}}
	deps := hybridDeps(fs, shiki, md, nil)
	deps.Log = log
	return fs, deps
}

// TestShikiPickFlowReachesFanOut: the Shikimori-first search flow must
// not dead-end after the user picks a title — the shikiFanOutMsg
// settles onto the PICK screen, which replaces itself with the
// provider fan-out table (PR28: the message used to be dropped and
// the table never appeared).
func TestShikiPickFlowReachesFanOut(t *testing.T) {
	_, deps := shikiFlowDeps(testLogger())
	app := NewApp(NewRootScreen(deps), deps, testLogger())

	// root -> search input -> submit query -> pick screen.
	model := drive(app, pushMsg{screen: NewSearchInput(deps)})
	input := topOf(model).(*TextPrompt)
	input.typeText("наруто")
	_, submitCmd := input.Update(enter())
	rm, ok := submitCmd().(replaceMsg)
	if !ok {
		t.Fatalf("submit must replace with the pick screen, got %T", submitCmd())
	}
	model = drive(model, rm)

	// Autocomplete settles into the candidate list.
	pick := topOf(model).(*shikiPickScreen)
	model = drive(model, pick.Init())
	if pick := topOf(model).(*shikiPickScreen); pick.list == nil {
		t.Fatalf("autocomplete must build the candidate list, err=%v", pick.err)
	}

	// Enter on the first candidate resolves variants and produces the
	// fan-out message; delivering it (as the runtime does) must land
	// on the fan-out screen.
	_, pickCmd := pick.Update(enter())
	if pickCmd == nil {
		t.Fatalf("enter on a candidate must start the fan-out")
	}
	msg := pickCmd()
	if _, ok := msg.(shikiFanOutMsg); !ok {
		t.Fatalf("expected shikiFanOutMsg, got %T", msg)
	}
	model = drive(model, msg)
	if topOf(model).ID() != "shiki_fanout" {
		t.Fatalf("fan-out message must replace the pick screen, stayed %q", topOf(model).ID())
	}

	// The fan-out Init fans out to every provider with the resolved
	// variants; the table settles with live statuses.
	model = drainCmds(model)
	v := topOf(model).View().Content
	for _, want := range []string{"AnimeGO", "AniLib", "Завершено"} {
		if !strings.Contains(v, want) {
			t.Errorf("fan-out table missing %q, got:\n%s", want, v)
		}
	}

	// Enter on the settled table advances to the grouping checklist.
	_, advance := topOf(model).Update(enter())
	if advance == nil {
		t.Fatalf("enter on the settled fan-out must advance")
	}
	switch m := advance().(type) {
	case pushMsg:
		if m.screen.ID() != searchGroupID {
			t.Fatalf("fan-out must advance to grouping, got %q", m.screen.ID())
		}
	case replaceMsg:
		if m.screen.ID() != searchGroupID {
			t.Fatalf("fan-out must advance to grouping, got %q", m.screen.ID())
		}
	default:
		t.Fatalf("unexpected advance message %#v", advance())
	}
}

// TestShikiPickFanOutVariants: the fan-out queries providers with the
// canonical title first and falls through to the resolved alternative
// names when the canonical yields nothing.
func TestShikiPickFanOutVariants(t *testing.T) {
	fs, deps := shikiFlowDeps(testLogger())
	// Canonical title: empty answer; alias: the hit. Providers must
	// walk the variant set in order (PR24 early stop after a hit).
	fs.variantResults = map[string][]contracts.SearchResult{}
	fs.results["animego"] = nil
	fs.variantResults["Naruto"] = []contracts.SearchResult{{Title: "Naruto", URL: "u1", SourceID: "animego"}}
	app := NewApp(NewRootScreen(deps), deps, testLogger())

	model := drive(app, pushMsg{screen: NewShikiPickScreen(deps, "наруто")})
	pick := topOf(model).(*shikiPickScreen)
	model = drive(model, pick.Init())

	_, pickCmd := pick.Update(enter())
	model = drive(model, pickCmd())
	drainCmds(model)

	got := fs.queries["animego"]
	if len(got) == 0 || got[0] != "Наруто" {
		t.Fatalf("providers must see the canonical title first, got %v", got)
	}
	sawAlt := false
	for _, q := range got {
		if q == "Naruto" {
			sawAlt = true
		}
	}
	if !sawAlt {
		t.Fatalf("the resolved alias must be part of the variant set, got %v", got)
	}
}

// TestShikiFanOutScreenIgnoresStaleFanOutMsg: once the fan-out table
// is up, a stale shikiFanOutMsg (e.g. a duplicate command resolving
// after the screen swap) must NOT restart the fan-out — the screen
// delegates unknown messages to the progress table (PR28: the old
// handler replaced the screen with a fresh instance, re-running
// Init).
func TestShikiFanOutScreenIgnoresStaleFanOutMsg(t *testing.T) {
	_, deps := shikiFlowDeps(testLogger())
	screen := NewShikiFanOut(deps, "Наруто", 21, []string{"Наруто", "Naruto"})
	next, cmd := screen.Update(shikiFanOutMsg{title: "Наруто", shikimoriID: 21, variants: []string{"Наруто"}})
	if cmd != nil {
		t.Fatalf("a stale fan-out message must be a no-op, got cmd %T", cmd)
	}
	if next == nil || next.ID() != "shiki_fanout" {
		t.Fatalf("the fan-out screen must stay put, got %v", next)
	}
}

// TestShikiPickFlowLogs: the pick and fan-out Init phases log their
// start so production diagnostics (deps.Log) can trace the flow that
// used to be silent (PR28: Deps.Log was never wired, the search
// emitted nothing).
func TestShikiPickFlowLogs(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	_, deps := shikiFlowDeps(log)

	app := NewApp(NewRootScreen(deps), deps, log)
	model := drive(app, pushMsg{screen: NewShikiPickScreen(deps, "наруто")})
	pick := topOf(model).(*shikiPickScreen)
	model = drive(model, pick.Init())

	_, pickCmd := pick.Update(enter())
	model = drive(model, pickCmd())
	drainCmds(model)

	out := buf.String()
	for _, want := range []string{
		`msg="search: shiki pick" query=наруто`,
		`msg="search: shiki fan-out" title=Наруто`,
		`providers=2`,
		`msg="search: provider settled" provider=animego`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log output missing %q, got:\n%s", want, out)
		}
	}
}

// TestHistoryResumeRendersTableWithShiki: the 📜 Списки resume
// fan-out (the old searchProgress surface) renders its provider table
// with Shikimori enrichment active — the production shape.
func TestHistoryResumeRendersTableWithShiki(t *testing.T) {
	_, deps := shikiFlowDeps(testLogger())
	rec := &storage.AnimeProgress{Title: "Наруто", SourceID: "animego", SourceURL: "u1"}

	app := NewApp(NewRootScreen(deps), deps, testLogger())
	model := drive(app, pushMsg{screen: newHistoryResume(deps, rec)})
	model = drainCmds(model)

	v := topOf(model).View().Content
	for _, want := range []string{"AnimeGO", "AniLib", "Завершено", "Ответившие: 2/2"} {
		if !strings.Contains(v, want) {
			t.Errorf("resume table missing %q, got:\n%s", want, v)
		}
	}
}
