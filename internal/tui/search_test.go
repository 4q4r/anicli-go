package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/providers"
)

// fakeSearch implements SearchService with per-provider behavior.
type fakeSearch struct {
	providers []ProviderMeta
	results   map[string][]contracts.SearchResult
	errs      map[string]error
	panics    map[string]bool
	queries   map[string]string
	disabled  []providers.DisabledProvider
}

func newFakeSearch() *fakeSearch {
	return &fakeSearch{
		providers: []ProviderMeta{
			{ID: "animego", Name: "AnimeGO"},
			{ID: "anilib", Name: "AniLib"},
			{ID: "broken", Name: "Broken"},
		},
		results:  map[string][]contracts.SearchResult{},
		errs:     map[string]error{},
		panics:   map[string]bool{},
		queries:  map[string]string{},
		disabled: nil,
	}
}

func (f *fakeSearch) Providers() []ProviderMeta { return f.providers }

func (f *fakeSearch) DisabledProviders() []providers.DisabledProvider { return f.disabled }

func (f *fakeSearch) Search(_ context.Context, providerID, query string) ([]contracts.SearchResult, error) {
	f.queries[providerID] = query
	if f.panics[providerID] {
		panic("provider exploded mid-search")
	}
	if err := f.errs[providerID]; err != nil {
		return nil, err
	}
	return f.results[providerID], nil
}

var _ SearchService = (*fakeSearch)(nil)

// searchFlowApp drives a full search through the app model with fake
// deps.
func searchFlowApp(fs *fakeSearch) App {
	deps := &Deps{Search: fs}
	return NewApp(NewRootScreen(deps), deps, testLogger())
}

// TestSearchFanOutProgress: the search screen issues one command per
// provider; statuses transition live; a panicking provider surfaces
// the error screen without killing the app.
func TestSearchFanOutProgress(t *testing.T) {
	fs := newFakeSearch()
	fs.results["animego"] = []contracts.SearchResult{{Title: "Наруто", URL: "u1", SourceID: "animego"}}
	fs.results["anilib"] = []contracts.SearchResult{{Title: "Наруто", URL: "u2", SourceID: "anilib"}}
	fs.errs["broken"] = errors.New("timeout")
	fs.panics["broken"] = true

	app := searchFlowApp(fs)

	// Navigate: root -> search input -> type query -> enter.
	model := drive(app, pushMsg{screen: NewSearchInput(app.deps)})
	input := topOf(model).(*TextPrompt)
	input.typeText("наруто")
	model = drive(model, pushMsg{screen: NewSearchProgress(app.deps, "наруто")})
	_ = input

	// The progress screen should have emitted per-provider commands on
	// Init; execute the fan-out by driving a full command drain.
	model = drainCmds(model)

	progress := topOf(model)
	v := progress.View().Content
	if !strings.Contains(v, "AnimeGO") || !strings.Contains(v, "AniLib") {
		t.Fatalf("progress table must list providers, got:\n%s", v)
	}
	if !strings.Contains(v, "Найдено") {
		t.Fatalf("successful providers must show a found count, got:\n%s", v)
	}
	if !strings.Contains(v, "timeout") && !strings.Contains(v, "Ошибка") {
		t.Fatalf("failed provider must show its error, got:\n%s", v)
	}

	// The panicking provider degraded to a row error (python
	// try/except semantics): the app continues, no modal, no crash.
	if progress.ID() == errorScreenID {
		t.Fatalf("a provider panic must not replace the fan-out screen")
	}
	if !strings.Contains(progress.View().Content, "recovered panic") &&
		!strings.Contains(progress.View().Content, "Ошибка") {
		t.Fatalf("the panicking provider must show a row error")
	}
	// The app-level net still routes injected errMsg to the modal.
	model2 := drive(model, errMsg{screen: searchProgressID, err: errors.New("boom")})
	if topOf(model2).ID() != errorScreenID {
		t.Fatalf("errMsg must surface the error screen")
	}

	// All providers were queried with the same query.
	for _, id := range []string{"animego", "anilib"} {
		if fs.queries[id] != "наруто" {
			t.Fatalf("provider %s must see the query, got %q", id, fs.queries[id])
		}
	}
}

// TestSearchProgressAssembly: after the fan-out completes, the screen
// assembles the flat result set and moves to the grouping checklist.
func TestSearchProgressAssembly(t *testing.T) {
	fs := newFakeSearch()
	fs.results["animego"] = []contracts.SearchResult{{Title: "Наруто", URL: "u1", SourceID: "animego"}}
	fs.providers = fs.providers[:1] // keep it to one provider

	app := searchFlowApp(fs)
	model := drive(app, pushMsg{screen: NewSearchProgress(app.deps, "наруто")})
	model = drainCmds(model)

	progress := topOf(model).(*searchProgress)
	if len(progress.results) != 1 {
		t.Fatalf("results must be assembled, got %d", len(progress.results))
	}

	// When all providers settle, the screen offers continuation.
	_, cmd := progress.Update(enter())
	if cmd == nil {
		t.Fatalf("enter on settled progress must advance")
	}
	msg := cmd()
	if pm, ok := msg.(pushMsg); ok {
		if pm.screen.ID() != searchGroupID {
			t.Fatalf("progress must push the grouping screen, got %q", pm.screen.ID())
		}
	} else if rm, ok := msg.(replaceMsg); ok {
		if rm.screen.ID() != searchGroupID {
			t.Fatalf("progress must replace with grouping screen, got %q", rm.screen.ID())
		}
	} else {
		t.Fatalf("unexpected advance message %#v", msg)
	}
}

// TestSearchEmptyResults: zero hits is legal (I3): the progress
// screen renders an empty-state and enter returns one level up.
func TestSearchEmptyResults(t *testing.T) {
	fs := newFakeSearch()
	fs.providers = fs.providers[:1]

	app := searchFlowApp(fs)
	model := drive(app, pushMsg{screen: NewSearchProgress(app.deps, "ничего")})
	model = drainCmds(model)

	sp := topOf(model).(*searchProgress)
	if !contains(sp.View().Content, "Ничего не найдено") {
		t.Fatalf("empty fan-out must show the empty state, got:\n%s", sp.View().Content)
	}
	_, cmd := sp.Update(enter())
	if cmd == nil {
		t.Fatalf("enter on empty results must leave the screen")
	}
	if _, ok := cmd().(popMsg); !ok {
		t.Fatalf("empty results + enter must pop, got %#v", cmd())
	}
}

// TestSearchGroupingFlow: checkbox grouping -> merged group ->
// source pick pushes the session screen.
func TestSearchGroupingFlow(t *testing.T) {
	fs := newFakeSearch()
	results := []contracts.SearchResult{
		{Title: "Наруто", URL: "u1", SourceID: "animego"},
		{Title: "Наруто (TV)", URL: "u2", SourceID: "anilib"},
		{Title: "Bleach", URL: "u3", SourceID: "animego"},
	}

	app := searchFlowApp(fs)
	group := NewSearchGroup(app.deps, results)

	// Check the two Naruto entries (items 0 and 1; the cursor starts
	// on item 0).
	group.check.Toggle()
	group.check.MoveDown()
	group.check.Toggle()

	checked := group.check.CheckedItems()
	if len(checked) != 2 {
		t.Fatalf("want two checked results, got %+v", checked)
	}
	if checked[0].Value.(contracts.SearchResult).URL != "u1" ||
		checked[1].Value.(contracts.SearchResult).URL != "u2" {
		t.Fatalf("want the two Naruto results checked, got %+v", checked)
	}

	// Enter resolves to the source pick screen.
	_, cmd := group.Update(enter())
	msg := cmd()
	switch m := msg.(type) {
	case pushMsg:
		if m.screen.ID() != searchSourceID {
			t.Fatalf("grouping must push source pick, got %q", m.screen.ID())
		}
	case replaceMsg:
		if m.screen.ID() != searchSourceID {
			t.Fatalf("grouping must replace with source pick, got %q", m.screen.ID())
		}
	default:
		t.Fatalf("unexpected message %#v", msg)
	}
}

// helpers

// drive runs one message through the model and then executes every
// command the updates returned (the way the bubbletea runtime would),
// so Cmd-driven navigation lands synchronously in tests. Spinner tick
// loops are cut; batches fan out.
func drive(model tea.Model, msg tea.Msg) tea.Model {
	m, cmd := model.Update(msg)
	return drain(m, cmd)
}

// drain executes a command chain, fanning batches out like the
// bubbletea runtime.
func drain(m tea.Model, cmd tea.Cmd) tea.Model {
	for i := 0; cmd != nil && i < 64; i++ {
		msg := cmd()
		switch v := msg.(type) {
		case spinner.TickMsg:
			return m // never follow the blink loop
		case tea.BatchMsg:
			for _, sub := range v {
				m = drain(m, sub)
			}
			return m
		}
		var next tea.Cmd
		m, next = m.Update(msg)
		cmd = next
	}
	return m
}

func topOf(model tea.Model) Screen {
	return model.(App).stack[len(model.(App).stack)-1]
}

// drainCmds flushes any commands queued on the model's current
// screens by re-driving a benign message.
func drainCmds(model tea.Model) tea.Model {
	return drive(model, tea.WindowSizeMsg{Width: 100, Height: 30})
}
