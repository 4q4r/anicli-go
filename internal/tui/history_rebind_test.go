package tui

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

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

// TestRebindProgressRendersProviderTable (PR29): the rebind fan-out
// screen must render the same live provider table as the plain search
// flow — header columns, one row per provider, verdicts after
// settlement and the centered overall counter.
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
		pushMsg{screen: newRebindProgress(deps, rebindTestRecord(), "наруто")})
	model = drainCmds(model)
	v := topOf(model).View().Content

	for _, want := range []string{
		"Провайдер", "Статус", "Результатов",
		"AnimeGO", "AniLib", "Broken",
		"Завершено",
		"Ответившие: 2/3 провайдеров",
		"Всего результатов: 2",
	} {
		if !strings.Contains(v, want) {
			t.Errorf("rebind table missing %q, got:\n%s", want, v)
		}
	}
}

// TestRebindProgressEnterBlockedWhilePending (PR29): enter must not
// group partial results — the same pending guard as the plain search
// flow — and once every row settled, enter produces the grouped pick
// list.
func TestRebindProgressEnterBlockedWhilePending(t *testing.T) {
	fs := newFakeSearch()
	fs.providers = fs.providers[:1]
	fs.block["animego"] = 400 * time.Millisecond
	deps := hybridDeps(fs, nil, nil, nil)
	deps.SearchTimeout = 50 * time.Millisecond

	r := newRebindProgress(deps, rebindTestRecord(), "наруто")
	if _, cmd := r.Update(enter()); cmd != nil {
		t.Fatalf("enter must be blocked while rows are pending")
	}

	// Settle the single row (the blown budget lands as a timeout),
	// then enter must group.
	next, _ := r.Update(providerResultMsg{
		provider: fs.providers[0],
		err:      context.DeadlineExceeded,
	})
	r = next.(*rebindProgress)
	_, cmd := r.Update(enter())
	if cmd == nil {
		t.Fatalf("enter on the settled rebind table must advance to grouping")
	}
	r.buildGroupList(GroupByTitle(r.results, 0.6))
	v := r.View().Content
	if !strings.Contains(v, "Выберите правильный тайтл") {
		t.Fatalf("settled rebind must render the grouped pick list, got:\n%s", v)
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
		pushMsg{screen: newRebindProgress(deps, rebindTestRecord(), "наруто")})
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
		pushMsg{screen: newRebindProgress(deps, rec, "наруто")})
	model = drainCmds(model)
	progress := topOf(model).(*rebindProgress)

	// Metadata was consulted with the record's canonical title.
	if len(md.queries) != 1 || md.queries[0] != "Наруто" {
		t.Fatalf("metadata must enrich the record canonical title, got %v", md.queries)
	}
	// The ru provider reached its Cyrillic variant hit, the ja one its
	// Latin alias hit — both only exist as variants, not as the bare
	// query.
	if got := fs.queries["animego"]; len(got) == 0 || !containsQuery(got, "Наруто") {
		t.Fatalf("animego must be queried with the canonical variant, got %v", got)
	}
	if got := fs.queries["gogoanime"]; len(got) == 0 || !containsQuery(got, "Naruto") {
		t.Fatalf("gogoanime must be queried with the alias variant, got %v", got)
	}
	if len(progress.results) != 2 {
		t.Fatalf("variant hits must assemble, got %d results", len(progress.results))
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

// TestShikiFanOutLogsStart (PR29 B): the Shikimori-first fan-out logs
// its start with the resolved variant count.
func TestShikiFanOutLogsStart(t *testing.T) {
	fs := newFakeSearch()
	fs.providers = fs.providers[:1]
	logger, buf := captureLogger()
	deps := hybridDeps(fs, nil, nil, nil)
	deps.Log = logger

	s := NewShikiFanOut(deps, "Наруто", 21, []string{"Наруто", "Naruto"})
	_ = s.Init()

	logs := buf.String()
	if !strings.Contains(logs, "fan-out: starting") {
		t.Errorf("fan-out start must be logged, got:\n%s", logs)
	}
	if !strings.Contains(logs, "variants=2") {
		t.Errorf("fan-out start must log the variant count, got:\n%s", logs)
	}
}
