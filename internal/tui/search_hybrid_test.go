package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// fakeMetadata implements MetadataService with per-query fixtures.
type fakeMetadata struct {
	aliases map[string][]string
	err     error
	queries []string
}

func (f *fakeMetadata) SearchAlternativeTitles(_ context.Context, query string) ([]string, error) {
	f.queries = append(f.queries, query)
	if f.err != nil {
		return nil, f.err
	}
	return f.aliases[query], nil
}

var _ MetadataService = (*fakeMetadata)(nil)

// hybridDeps builds the fake service set for the hybrid flow tests.
// Nil fakes are left unset (a typed nil in an interface would panic).
func hybridDeps(fs *fakeSearch, shiki *fakeShiki, md *fakeMetadata, langs map[string]string) *Deps {
	deps := &Deps{
		Search:  fs,
		Episode: &fakeEpisode{langs: langs},
		Log:     testLogger(),
	}
	if shiki != nil {
		deps.Shiki = shiki
	}
	if md != nil {
		deps.Metadata = md
	}
	return deps
}

// TestProviderQueryLanguageRouting: the RU/ja query ordering rule —
// Cyrillic variants first for Russian providers, Latin-first for the
// rest; unknown languages keep the original order.
func TestProviderQueryLanguageRouting(t *testing.T) {
	variants := []string{"наруто", "Naruto", "Наруто: Ураганные хроники", "naruto"}

	t.Run("ru providers get Cyrillic variants first", func(t *testing.T) {
		got := queriesForLanguage(variants, "ru")
		if got[0] != "наруто" || !hasCyrillic.MatchString(got[0]) {
			t.Fatalf("ru first query must be Cyrillic, got %v", got)
		}
		if len(got) != len(variants) {
			t.Fatalf("routing must not drop variants: %v", got)
		}
	})

	t.Run("non-ru providers get Latin variants first", func(t *testing.T) {
		got := queriesForLanguage(variants, "ja")
		if hasCyrillic.MatchString(got[0]) {
			t.Fatalf("ja first query must be Latin (gogoanime EOFs on Cyrillic), got %v", got)
		}
		if got[0] != "Naruto" {
			t.Fatalf("first Latin variant must lead, got %v", got)
		}
	})

	t.Run("unknown language keeps the original order", func(t *testing.T) {
		got := queriesForLanguage(variants, "")
		if strings.Join(got, "|") != strings.Join(variants, "|") {
			t.Fatalf("unknown language must not reorder: %v", got)
		}
	})
}

// TestHybridSearchEnrichesViaShikimori: enabled Shikimori seeds the
// variant set — SearchIDs finds the matched title, the metadata
// manager contributes aliases, and the fan-out routes Cyrillic to ru
// providers and Latin to non-ru ones (PR24 hybrid flow).
func TestHybridSearchEnrichesViaShikimori(t *testing.T) {
	fs := newFakeSearch()
	fs.providers = []ProviderMeta{
		{ID: "animego", Name: "AnimeGO"},
		{ID: "gogoanime", Name: "GogoAnime"},
	}
	fs.results["animego"] = []contracts.SearchResult{{Title: "Наруто", URL: "u1", SourceID: "animego"}}
	fs.results["gogoanime"] = []contracts.SearchResult{{Title: "Naruto", URL: "u2", SourceID: "gogoanime"}}

	shiki := &fakeShiki{enabled: true, ids: map[string]int64{"Наруто": 21}}
	md := &fakeMetadata{aliases: map[string][]string{
		"Наруто": {"Naruto", "NARUTO"},
	}}
	deps := hybridDeps(fs, shiki, md, map[string]string{
		"animego":   "ru",
		"gogoanime": "ja",
	})

	model := drive(NewApp(NewRootScreen(deps), deps, testLogger()),
		pushMsg{screen: NewSearchProgress(deps, "наруто")})
	model = drainCmds(model)
	progress := topOf(model).(*searchProgress)

	// Shikimori was consulted with the original query.
	if len(shiki.queries) != 1 || shiki.queries[0] != "наруто" {
		t.Fatalf("shikimori must resolve the original query, got %v", shiki.queries)
	}
	// The metadata manager was consulted with the MATCHED title.
	if len(md.queries) != 1 || md.queries[0] != "Наруто" {
		t.Fatalf("metadata must enrich the matched title, got %v", md.queries)
	}
	// ru provider saw a Cyrillic query first.
	if got := fs.queries["animego"]; len(got) == 0 || !hasCyrillic.MatchString(got[0]) {
		t.Fatalf("animego (ru) must be queried with Cyrillic first, got %v", got)
	}
	// ja provider saw a Latin query first.
	if got := fs.queries["gogoanime"]; len(got) == 0 || hasCyrillic.MatchString(got[0]) {
		t.Fatalf("gogoanime (ja) must be queried with Latin first, got %v", got)
	}
	// Variants cap at 8 (metadata.QueryVariants contract).
	if len(progress.variants) > 8 {
		t.Fatalf("variants must cap at 8, got %v", progress.variants)
	}
}

// TestHybridSearchShikimoriDisabledFallsBack: without Shikimori the
// fan-out runs on the original query only, no enrichment.
func TestHybridSearchShikimoriDisabledFallsBack(t *testing.T) {
	fs := newFakeSearch()
	fs.providers = fs.providers[:1]
	fs.results["animego"] = []contracts.SearchResult{{Title: "Наруто", URL: "u1", SourceID: "animego"}}

	shiki := &fakeShiki{enabled: false, ids: map[string]int64{"Наруто": 21}}
	md := &fakeMetadata{}
	deps := hybridDeps(fs, shiki, md, map[string]string{"animego": "ru"})

	model := drive(NewApp(NewRootScreen(deps), deps, testLogger()),
		pushMsg{screen: NewSearchProgress(deps, "наруто")})
	model = drainCmds(model)
	progress := topOf(model).(*searchProgress)

	if got := fs.queries["animego"]; len(got) != 1 || got[0] != "наруто" {
		t.Fatalf("disabled shikimori must fall back to the bare query, got %v", got)
	}
	if len(progress.variants) != 1 {
		t.Fatalf("variants must stay the bare query, got %v", progress.variants)
	}
	if len(md.queries) != 0 {
		t.Fatalf("metadata must not be consulted without a shikimori match, got %v", md.queries)
	}
}

// TestHybridSearchNoConfidentMatchFallsBack: a Shikimori answer whose
// best ratio misses the threshold yields the bare query.
func TestHybridSearchNoConfidentMatchFallsBack(t *testing.T) {
	fs := newFakeSearch()
	fs.providers = fs.providers[:1]
	shiki := &fakeShiki{enabled: true, ids: map[string]int64{"Совсем Другое Аниме": 99}}
	md := &fakeMetadata{}
	deps := hybridDeps(fs, shiki, md, nil)

	model := drive(NewApp(NewRootScreen(deps), deps, testLogger()),
		pushMsg{screen: NewSearchProgress(deps, "наруто")})
	model = drainCmds(model)
	progress := topOf(model).(*searchProgress)

	if got := fs.queries["animego"]; len(got) != 1 || got[0] != "наруто" {
		t.Fatalf("a low-ratio match must fall back to the bare query, got %v", got)
	}
	if len(progress.variants) != 1 {
		t.Fatalf("variants must stay the bare query, got %v", progress.variants)
	}
}

// TestHybridSearchEarlyStop: a provider is searched with its variants
// in order and stops at the first one that yields results.
func TestHybridSearchEarlyStop(t *testing.T) {
	fs := newFakeSearch()
	fs.providers = fs.providers[:1]
	fs.errs["animego"] = nil

	// Variant behavior: "наруто" → empty, "Наруто" → hit.
	fs.variantResults = map[string][]contracts.SearchResult{
		"Наруто": {{Title: "Наруто", URL: "u1", SourceID: "animego"}},
	}

	shiki := &fakeShiki{enabled: true, ids: map[string]int64{"Наруто": 21}}
	md := &fakeMetadata{aliases: map[string][]string{"Наруто": {"Наруто"}}}
	deps := hybridDeps(fs, shiki, md, nil)

	model := drive(NewApp(NewRootScreen(deps), deps, testLogger()),
		pushMsg{screen: NewSearchProgress(deps, "наруто")})
	model = drainCmds(model)
	progress := topOf(model).(*searchProgress)

	got := fs.queries["animego"]
	if len(got) < 2 {
		t.Fatalf("the empty first variant must be followed by the second, got %v", got)
	}
	if got[0] != "наруто" || got[1] != "Наруто" {
		t.Fatalf("variants must run in order, got %v", got)
	}
	if len(progress.results) != 1 {
		t.Fatalf("the hit variant's results must be assembled once, got %d", len(progress.results))
	}
}

// TestHybridSearchTimeoutStatus: a provider exceeding the per-provider
// budget renders a dedicated timeout row instead of hanging the table.
func TestHybridSearchTimeoutStatus(t *testing.T) {
	fs := newFakeSearch()
	fs.providers = fs.providers[:1]
	fs.block["animego"] = 300 * time.Millisecond

	deps := hybridDeps(fs, nil, nil, nil)
	deps.SearchTimeout = 50 * time.Millisecond

	model := drive(NewApp(NewRootScreen(deps), deps, testLogger()),
		pushMsg{screen: NewSearchProgress(deps, "наруто")})
	model = drainCmds(model)

	v := topOf(model).View().Content
	if !strings.Contains(v, "Таймаут") {
		t.Fatalf("a blown budget must render a timeout status, got:\n%s", v)
	}
}

// TestSearchLiveTableRendering (PR24): the fan-out table renders the
// three columns plus the centered overall counter.
func TestSearchLiveTableRendering(t *testing.T) {
	fs := newFakeSearch()
	fs.providers = []ProviderMeta{
		{ID: "animego", Name: "AnimeGO"},
		{ID: "anilib", Name: "AniLib"},
		{ID: "broken", Name: "Broken"},
	}
	fs.results["animego"] = []contracts.SearchResult{
		{Title: "Наруто", URL: "u1", SourceID: "animego"},
		{Title: "Наруто (TV)", URL: "u2", SourceID: "animego"},
	}
	fs.results["anilib"] = []contracts.SearchResult{{Title: "Наруто", URL: "u3", SourceID: "anilib"}}
	fs.errs["broken"] = errors.New("boom")

	deps := hybridDeps(fs, nil, nil, nil)
	model := drive(NewApp(NewRootScreen(deps), deps, testLogger()),
		pushMsg{screen: NewSearchProgress(deps, "наруто")})
	model = drainCmds(model)
	v := topOf(model).View().Content

	for _, want := range []string{
		"Провайдер", "Статус", "Результатов",
		"AnimeGO", "AniLib",
		"Завершено",
		"Ответившие: 2/3 провайдеров",
		"Всего результатов: 3",
	} {
		if !strings.Contains(v, want) {
			t.Errorf("live table missing %q, got:\n%s", want, v)
		}
	}
}

// TestSearchProgressScreenNotPinnedByEnrichment: the spinner ticks do
// not break the enrichment->fan-out hand-off (the drain helper cuts
// spinner loops; a second benign drain must be a no-op).
func TestSearchProgressScreenNotPinnedByEnrichment(t *testing.T) {
	fs := newFakeSearch()
	fs.providers = fs.providers[:1]
	shiki := &fakeShiki{enabled: true, ids: map[string]int64{"Наруто": 21}}
	deps := hybridDeps(fs, shiki, &fakeMetadata{}, nil)

	model := drive(NewApp(NewRootScreen(deps), deps, testLogger()),
		pushMsg{screen: NewSearchProgress(deps, "наруто")})
	model = drainCmds(model)
	progress := topOf(model).(*searchProgress)

	// After the enrichment settled, enter must advance (all rows done).
	_, cmd := progress.Update(enter())
	if cmd == nil {
		t.Fatalf("enter on the settled progress must advance")
	}
}
