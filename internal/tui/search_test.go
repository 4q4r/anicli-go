package tui

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/providers"
)

// fakeSearch implements SearchService with per-provider behavior.
// Queries are recorded per provider in call order (the hybrid fan-out
// may issue several variants); variantResults keys per-QUERY fixtures
// (presence matters: a nil slice is a legit empty answer); block makes
// a provider sleep to exercise timeout budgets.
type fakeSearch struct {
	providers      []ProviderMeta
	results        map[string][]contracts.SearchResult
	variantResults map[string][]contracts.SearchResult
	errs           map[string]error
	panics         map[string]bool
	queries        map[string][]string
	disabled       []providers.DisabledProvider
	block          map[string]time.Duration
	// namePrefs carries the NamePreferenceProvider pins (PR42).
	namePrefs map[string]contracts.NamePreference
	// variantErrs fails one specific query variant (PR97 merge
	// fan-out: per-variant errors continue, per-provider errs fail
	// the whole row).
	variantErrs map[string]error
}

func newFakeSearch() *fakeSearch {
	return &fakeSearch{
		providers: []ProviderMeta{
			{ID: "animego", Name: "AnimeGO"},
			{ID: "anilib", Name: "AniLib"},
			{ID: "broken", Name: "Broken"},
		},
		results:        map[string][]contracts.SearchResult{},
		variantResults: map[string][]contracts.SearchResult{},
		errs:           map[string]error{},
		panics:         map[string]bool{},
		queries:        map[string][]string{},
		disabled:       nil,
		block:          map[string]time.Duration{},
		namePrefs:      map[string]contracts.NamePreference{},
		variantErrs:    map[string]error{},
	}
}

func (f *fakeSearch) Providers() []ProviderMeta { return f.providers }

// NamePreference surfaces the provider name-preference pins (PR42).
func (f *fakeSearch) NamePreference(providerID string) contracts.NamePreference {
	return f.namePrefs[providerID]
}

func (f *fakeSearch) DisabledProviders() []providers.DisabledProvider { return f.disabled }

func (f *fakeSearch) Search(ctx context.Context, providerID, query string) ([]contracts.SearchResult, error) {
	f.queries[providerID] = append(f.queries[providerID], query)
	if d := f.block[providerID]; d > 0 {
		// Behave like a real HTTP client: an expired context fails
		// the request instead of sleeping past the budget.
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.panics[providerID] {
		panic("provider exploded mid-search")
	}
	if err, ok := f.variantErrs[query]; ok {
		return nil, err
	}
	if err := f.errs[providerID]; err != nil {
		return nil, err
	}
	if res, ok := f.variantResults[query]; ok {
		return res, nil
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

	// PR30: the free-text search input is gone (search runs strictly
	// through the catalog); the fan-out progress is driven directly.
	model := drive(app, pushMsg{screen: NewSearchProgress(app.deps, "наруто")})

	// The progress screen should have emitted per-provider commands on
	// Init; execute the fan-out by driving a full command drain.
	model = drainCmds(model)

	progress := topOf(model)
	v := progress.View().Content

	// PR110: the settled screen is header + summary + checklist — no
	// bordered table anywhere.
	for _, banned := range []string{"┌", "│", "└", "Ответившие"} {
		if strings.Contains(v, banned) {
			t.Fatalf("the fan-out view must not render table chrome %q:\n%s", banned, v)
		}
	}
	// The summary line: merged hits vs providers that found nothing.
	if want := "Найдено: 2 · Без результатов/ошибок: 1"; !strings.Contains(v, want) {
		t.Fatalf("summary missing %q:\n%s", want, v)
	}
	// The settled checklist below the summary.
	if !strings.Contains(v, "AnimeGO — Наруто") || !strings.Contains(v, "AniLib — Наруто") {
		t.Fatalf("settled checklist rows missing:\n%s", v)
	}

	// The panicking provider degraded to a row error (python
	// try/except semantics): the app continues, no modal, no crash.
	if progress.ID() == errorScreenID {
		t.Fatalf("a provider panic must not replace the fan-out screen")
	}
	// PR110: the full provider error goes to the FILE logger only —
	// the TUI view must not carry it.
	if strings.Contains(v, "timeout") {
		t.Errorf("the full provider error must stay out of the TUI, got:\n%s", v)
	}
	// The app-level net still routes injected errMsg to the modal.
	model2 := drive(model, errMsg{screen: searchProgressID, err: errors.New("boom")})
	if topOf(model2).ID() != errorScreenID {
		t.Fatalf("errMsg must surface the error screen")
	}

	// All providers were queried with the original query first (no
	// shikimori in these deps → no enrichment).
	for _, id := range []string{"animego", "anilib"} {
		got := fs.queries[id]
		if len(got) == 0 || got[0] != "наруто" {
			t.Fatalf("provider %s must see the query first, got %q", id, got)
		}
	}

	// PR31: once every row settled, EVERY result renders as its own
	// checklist row BELOW the table automatically — no enter press,
	// no grouping.
	v = progress.View().Content
	if !strings.Contains(v, "Выберите провайдеры") {
		t.Fatalf("settled fan-out must show the provider checklist, got:\n%s", v)
	}
	if !strings.Contains(v, "AnimeGO — Наруто") || !strings.Contains(v, "AniLib — Наруто") {
		t.Fatalf("the two provider hits must stay separate rows, got:\n%s", v)
	}
}

// TestSearchProgressAssembly (PR30/PR31): after the fan-out completes,
// every result renders as its own checklist row BELOW the table
// automatically — no enter gate — and enter with one checked provider
// enters the session directly.
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

	// Settled: the checklist appears WITHOUT enter.
	v := progress.View().Content
	if !strings.Contains(v, "Выберите провайдеры") {
		t.Fatalf("settled progress must show the checklist, got:\n%s", v)
	}
	if !strings.Contains(v, "AnimeGO — Наруто") {
		t.Fatalf("the result must render as its own row, got:\n%s", v)
	}

	// Enter with nothing checked stays put (empty selection is legal).
	if _, cmd := progress.Update(enter()); cmd != nil {
		t.Fatalf("enter with no checked provider must be a no-op, got %T", cmd())
	}

	// Check the row (space), enter: straight into the session.
	progress.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	_, cmd := progress.Update(enter())
	if cmd == nil {
		t.Fatalf("enter with one checked provider must advance")
	}
	rm, ok := cmd().(replaceMsg)
	if !ok {
		t.Fatalf("one checked provider must enter the session, got %T", cmd())
	}
	if _, ok := rm.screen.(*sessionScreen); !ok {
		t.Fatalf("one checked provider must enter the session directly, got %q", rm.screen.ID())
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
	// PR110 review: the all-failed/empty settle must render zero
	// counts on BOTH summary halves — found 0, not-found 1 (the
	// single provider returned nothing).
	if !contains(sp.View().Content, "Найдено: 0 · Без результатов/ошибок: 1") {
		t.Fatalf("empty fan-out must render zero counts, got:\n%s", sp.View().Content)
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

	// Enter resolves to the session directly (PR61: no provider gate).
	_, cmd := group.Update(enter())
	msg := cmd()
	switch m := msg.(type) {
	case pushMsg:
		if m.screen.ID() != sessionScreenID {
			t.Fatalf("grouping must push the session, got %q", m.screen.ID())
		}
	case replaceMsg:
		if m.screen.ID() != sessionScreenID {
			t.Fatalf("grouping must replace with the session, got %q", m.screen.ID())
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

// TestTorrentResultSuffix pins the torrent preview suffix (PR36):
// results carrying torrent meta render " · quality · size · seeds↑/
// leechers↓"; stream-provider results keep their plain labels.
func TestTorrentResultSuffix(t *testing.T) {
	t.Parallel()

	full := contracts.SearchResult{Meta: map[string]any{
		providers.SearchMetaQuality:  "1080p",
		providers.SearchMetaSize:     "7.4 GiB",
		providers.SearchMetaSeeders:  "421",
		providers.SearchMetaLeechers: "33",
	}}
	want := " · 1080p · 7.4 GiB · 421↑/33↓"
	if got := torrentResultSuffix(full); got != want {
		t.Errorf("suffix = %q, want %q", got, want)
	}

	partial := contracts.SearchResult{Meta: map[string]any{
		providers.SearchMetaSize:    "1.2 GiB",
		providers.SearchMetaQuality: 42, // non-string values are ignored, not shown
	}}
	if got := torrentResultSuffix(partial); got != " · 1.2 GiB" {
		t.Errorf("partial suffix = %q, want %q", got, " · 1.2 GiB")
	}

	plain := contracts.SearchResult{Title: "Naruto"}
	if got := torrentResultSuffix(plain); got != "" {
		t.Errorf("stream-provider suffix = %q, want empty", got)
	}
}

// TestSearchGroupLabelsCarryTorrentSuffix: the grouping screen's rows
// show the torrent preview for torrent results.
func TestSearchGroupLabelsCarryTorrentSuffix(t *testing.T) {
	t.Parallel()

	results := []contracts.SearchResult{
		{Title: "Batch Release", SourceID: "animetosho", Meta: map[string]any{
			providers.SearchMetaSize:    "7.4 GiB",
			providers.SearchMetaSeeders: "421",
		}},
		{Title: "Stream Release", SourceID: "anilibria"},
	}
	g := newSearchGroupNoted(nil, results, "")
	for _, item := range g.check.items {
		switch item.Value.(contracts.SearchResult).Title {
		case "Batch Release":
			if !strings.Contains(item.Label, "7.4 GiB · 421↑") {
				t.Errorf("torrent label = %q, want the torrent suffix", item.Label)
			}
		case "Stream Release":
			if strings.Contains(item.Label, "↑") || strings.Contains(item.Label, "GiB") {
				t.Errorf("stream label = %q, want no torrent suffix", item.Label)
			}
		}
	}
}

// TestSearchFanOutLoadingViewMinimal (PR110): during the fan-out the
// screen is ONE minimal loading line (spinner + «Ищу по N
// провайдерам…») under the title — no table, no checklist, no
// summary counts.
func TestSearchFanOutLoadingViewMinimal(t *testing.T) {
	fs := newFakeSearch()
	deps := &Deps{Search: fs}

	// drive() executes the fan-out commands synchronously, so the
	// in-flight phase is pinned on the bare screen: the same pending
	// state the runtime shows between the first frame and the last
	// provider settle.
	sp := NewSearchProgress(deps, "наруто")

	v := sp.View().Content
	if !strings.Contains(v, "Ищу по 3 провайдерам…") {
		t.Fatalf("loading line missing:\n%s", v)
	}
	for _, banned := range []string{"┌", "│", "└", "Выберите провайдеры", "Найдено:", "Ответившие"} {
		if strings.Contains(v, banned) {
			t.Fatalf("loading view must be minimal (%q found):\n%s", banned, v)
		}
	}

	// One provider settles: the loading line persists until the LAST
	// settle, still without table chrome or summary counts.
	sp.Update(providerResultMsg{provider: fs.providers[0],
		results: []contracts.SearchResult{{Title: "Наруто", URL: "u1", SourceID: "animego"}}})
	v = sp.View().Content
	if !strings.Contains(v, "Ищу по 3 провайдерам…") {
		t.Fatalf("loading line must persist while providers are pending:\n%s", v)
	}
	if strings.Contains(v, "Найдено:") || strings.Contains(v, "┌") {
		t.Fatalf("no summary or table while in flight:\n%s", v)
	}
}

// TestSearchProviderErrorsFileLogOnly (PR110): the per-provider
// settle error carries the FULL text in the file logger; the TUI view
// renders only the summary counts.
func TestSearchProviderErrorsFileLogOnly(t *testing.T) {
	fs := newFakeSearch()
	logBuf := &bytes.Buffer{}
	deps := &Deps{Search: fs, Log: slog.New(slog.NewTextHandler(logBuf, nil))}
	app := NewApp(NewRootScreen(deps), deps, testLogger())

	fs.errs["broken"] = errors.New("dial tcp: connection refused")

	model := drive(app, pushMsg{screen: NewSearchProgress(deps, "наруто")})
	model = drainCmds(model)

	if !strings.Contains(logBuf.String(), "connection refused") {
		t.Fatalf("the file log must carry the full provider error:\n%s", logBuf.String())
	}
	progress := topOf(model)
	if v := progress.View().Content; strings.Contains(v, "connection refused") {
		t.Fatalf("the TUI view must not carry the full error:\n%s", v)
	}
}
