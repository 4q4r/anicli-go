package tui

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/storage"
)

// captureLogger builds a logger writing into a buffer so tests can
// assert the PR29 rebind lifecycle log lines.
func captureLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

// rebindTestRecord builds a needs-correction record for the fan-out.
func rebindTestRecord() *storage.AnimeProgress {
	return &storage.AnimeProgress{ID: 7, Title: "Наруто", NeedsCorrection: true}
}

// TestRebindProgressRendersProviderTable (PR29/PR30): the catalog
// fan-out screen must render the same live provider table as the
// plain search flow — header columns, one row per provider, verdicts
// after settlement and the centered overall counter — and once every
// row settled, the grouped results appear BELOW the table
// automatically.
func TestRebindProgressRendersProviderTable(t *testing.T) {
	fs := newFakeSearch()
	fs.providers = []ProviderMeta{
		{ID: "animego", Name: "AnimeGO"},
		{ID: "anilib", Name: "AniLib"},
		{ID: "broken", Name: "Broken"},
	}
	fs.results["animego"] = []contracts.SearchResult{{Title: "Наруто", URL: "u1", SourceID: "animego"}}
	fs.results["anilib"] = []contracts.SearchResult{{Title: "Наруто", URL: "u2", SourceID: "anilib"}}
	fs.errs["broken"] = errors.New("boom")

	deps := hybridDeps(fs, nil, nil, nil)
	model := drive(NewApp(NewRootScreen(deps), deps, testLogger()),
		pushMsg{screen: newRebindProgress(deps, rebindTestRecord())})
	model = drainCmds(model)
	v := topOf(model).View().Content

	for _, want := range []string{
		"Провайдер", "Статус", "Результатов",
		"AnimeGO", "AniLib", "Broken",
		"Завершено",
		"Ответившие: 2/3 провайдеров",
		"Всего результатов: 2",
		"Выберите провайдеры",
		"AnimeGO — Наруто",
		"AniLib — Наруто",
	} {
		if !strings.Contains(v, want) {
			t.Errorf("rebind table missing %q, got:\n%s", want, v)
		}
	}
}

// TestRebindProgressSettlesWithoutEnterGate (PR30): enter is a no-op
// while rows are pending; once the last row settles, the grouped
// results render BELOW the table with NO enter press, and enter on
// the below-table list resumes the session.
func TestRebindProgressSettlesWithoutEnterGate(t *testing.T) {
	fs := newFakeSearch()
	fs.providers = fs.providers[:1]
	deps := hybridDeps(fs, nil, nil, nil)

	r := newRebindProgress(deps, rebindTestRecord())
	_ = r.Init()
	if _, cmd := r.Update(enter()); cmd != nil {
		t.Fatalf("enter must be blocked while rows are pending")
	}

	// Settle the single row with a hit: the provider checklist must
	// appear below the table WITHOUT any enter press.
	next, _ := r.Update(providerResultMsg{
		provider: fs.providers[0],
		results:  []contracts.SearchResult{{Title: "Наруто", URL: "u1", SourceID: "animego"}},
	})
	r = next.(*rebindProgress)
	v := r.View().Content
	if !strings.Contains(v, "Выберите провайдеры") {
		t.Fatalf("settled results must appear below the table without enter, got:\n%s", v)
	}
	if !strings.Contains(v, "AnimeGO — Наруто") {
		t.Fatalf("the settled result must render as its own row, got:\n%s", v)
	}

	// Check the row (space), enter: the resumed session replaces the
	// screen.
	r.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	_, cmd := r.Update(enter())
	if cmd == nil {
		t.Fatalf("enter on the below-table results must advance")
	}
	rm, ok := cmd().(replaceMsg)
	if !ok {
		t.Fatalf("enter on the results must resume the session, got %T", cmd())
	}
	if _, ok := rm.screen.(*sessionScreen); !ok {
		t.Fatalf("enter on the results must enter a session, got %T", rm.screen)
	}
}

// TestRebindProgressLogsLifecycle (PR29): the rebind fan-out logs
// through Deps.Log — starting, per-provider settled and complete — so
// /tmp/anicli-tui.log shows the flow the way the plain search does.
func TestRebindProgressLogsLifecycle(t *testing.T) {
	fs := newFakeSearch()
	fs.providers = fs.providers[:2]
	logger, buf := captureLogger()
	deps := hybridDeps(fs, nil, nil, nil)
	deps.Log = logger

	model := drive(NewApp(NewRootScreen(deps), deps, testLogger()),
		pushMsg{screen: newRebindProgress(deps, rebindTestRecord())})
	_ = drainCmds(model)

	logs := buf.String()
	for _, want := range []string{
		`msg="rebind: starting"`,
		`msg="rebind: provider settled"`,
		`msg="rebind: complete"`,
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("rebind lifecycle log missing %q, got:\n%s", want, logs)
		}
	}
}

// TestRebindProgressEnrichesFromRecord (PR29): a record bound to
// Shikimori seeds the fan-out variants with its canonical title plus
// the metadata alternative names (the Shikimori-first flow shape) —
// not just the bare prompt query.
func TestRebindProgressEnrichesFromRecord(t *testing.T) {
	fs := newFakeSearch()
	fs.providers = []ProviderMeta{
		{ID: "animego", Name: "AnimeGO"},
		{ID: "gogoanime", Name: "GogoAnime"},
	}
	// The bare query yields nothing; only the canonical/alias variants
	// hit.
	fs.variantResults = map[string][]contracts.SearchResult{
		"Наруто": {{Title: "Наруто", URL: "u1", SourceID: "animego"}},
		"Naruto": {{Title: "Naruto", URL: "u2", SourceID: "gogoanime"}},
	}

	md := &fakeMetadata{aliases: map[string][]string{"Наруто": {"Naruto"}}}
	deps := hybridDeps(fs, nil, md, map[string]string{"animego": "ru", "gogoanime": "ja"})

	rec := rebindTestRecord()
	canonical := "Наруто"
	rec.ShikimoriTitle = &canonical

	model := drive(NewApp(NewRootScreen(deps), deps, testLogger()),
		pushMsg{screen: newRebindProgress(deps, rec)})
	model = drainCmds(model)
	progress := topOf(model).(*rebindProgress)

	// Metadata was consulted with the record's canonical title.
	if len(md.queries) != 1 || md.queries[0] != "Наруто" {
		t.Fatalf("metadata must enrich the record canonical title, got %v", md.queries)
	}
	// PR97 merge fan-out: each provider runs ALL its language-routed
	// variants and merges — animego (ru) now also surfaces the latin
	// alias hit and gogoanime (ja) the Cyrillic canonical hit, so the
	// two-provider total is 4 rows (each unique within its provider).
	if got := fs.queries["animego"]; len(got) == 0 || !containsQuery(got, "Наруто") {
		t.Fatalf("animego must be queried with the canonical variant, got %v", got)
	}
	if got := fs.queries["gogoanime"]; len(got) == 0 || !containsQuery(got, "Naruto") {
		t.Fatalf("gogoanime must be queried with the alias variant, got %v", got)
	}
	if len(progress.results) != 4 {
		t.Fatalf("each provider must merge its variant hits, got %d results", len(progress.results))
	}
}

// containsQuery reports whether the query list holds the exact string.
func containsQuery(queries []string, want string) bool {
	for _, q := range queries {
		if q == want {
			return true
		}
	}
	return false
}
