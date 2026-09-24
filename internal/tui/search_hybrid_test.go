package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/shikimori"
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

// autocompleteItems builds autocomplete records from ru/en name pairs
// (nil name = the field stays unset).
func autocompleteItems(pairs ...[2]string) []shikimori.AutocompleteItem {
	items := make([]shikimori.AutocompleteItem, 0, len(pairs))
	for i, p := range pairs {
		item := shikimori.AutocompleteItem{ShikimoriID: int64(100 + i)}
		if p[0] != "" {
			ru := p[0]
			item.TitleRu = &ru
		}
		if p[1] != "" {
			en := p[1]
			item.TitleEn = &en
		}
		items = append(items, item)
	}
	return items
}

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
// variant set — the TOP autocomplete card (Shikimori's own relevance
// rank, no local matcher) is fetched by ID and EVERY name of the card
// (russian, original, english[], japanese[], synonyms[]) joins the
// variant pool with the metadata aliases (PR97 all-names binding;
// fixture = the verbatim 51553 capture shape).
func TestHybridSearchEnrichesViaShikimori(t *testing.T) {
	fs := newFakeSearch()
	fs.providers = []ProviderMeta{
		{ID: "animego", Name: "AnimeGO"},
		{ID: "gogoanime", Name: "GogoAnime"},
	}
	fs.results["animego"] = []contracts.SearchResult{{Title: "Ателье колдовских колпаков", URL: "u1", SourceID: "animego"}}
	fs.results["gogoanime"] = []contracts.SearchResult{{Title: "Witch Hat Atelier", URL: "u2", SourceID: "gogoanime"}}

	// The verbatim 51553 capture shape: 5 distinct names on one card.
	card := &shikimori.Anime{
		ID:       100,
		Name:     "Tongari Boushi no Atelier",
		Russian:  "Ателье колдовских колпаков",
		English:  []string{"Witch Hat Atelier"},
		Japanese: []string{"とんがり帽子のアトリエ"},
		Synonyms: []string{"Atelier of Witch Hat"},
	}
	shiki := &fakeShiki{
		enabled: true,
		items:   autocompleteItems([2]string{"Ателье колдовских колпаков", "Witch Hat Atelier"}),
		animes:  map[int64]*shikimori.Anime{100: card},
	}
	md := &fakeMetadata{aliases: map[string][]string{
		"Tongari Boushi no Atelier": {"Atelier no Witch Hat"},
	}}
	deps := hybridDeps(fs, shiki, md, map[string]string{
		"animego":   "ru",
		"gogoanime": "ja",
	})

	model := drive(NewApp(NewRootScreen(deps), deps, testLogger()),
		pushMsg{screen: NewSearchProgress(deps, "Witch Hat")})
	model = drainCmds(model)
	progress := topOf(model).(*searchProgress)

	// Shikimori was consulted with the original query.
	if len(shiki.queries) != 1 || shiki.queries[0] != "Witch Hat" {
		t.Fatalf("shikimori must resolve the original query, got %v", shiki.queries)
	}
	// The top card was fetched by its Shikimori ID.
	if len(shiki.animeCalls) != 1 || shiki.animeCalls[0] != 100 {
		t.Fatalf("GetAnime must fetch the top card by ID, got %v", shiki.animeCalls)
	}
	// The metadata manager was consulted with the card's original name.
	if len(md.queries) != 1 || md.queries[0] != "Tongari Boushi no Atelier" {
		t.Fatalf("metadata must enrich the card's original name, got %v", md.queries)
	}
	// Every name of the card surfaced as a variant, in collection
	// order: original query, russian, original, english, japanese,
	// synonyms, metadata alias — then the lowercase mirrors (capped
	// at 16).
	want := []string{
		"Witch Hat",
		"Ателье колдовских колпаков",
		"Tongari Boushi no Atelier",
		"Witch Hat Atelier",
		"とんがり帽子のアトリエ",
		"Atelier of Witch Hat",
		"Atelier no Witch Hat",
		"witch hat",
		"ателье колдовских колпаков",
		"tongari boushi no atelier",
		"witch hat atelier",
		// NOTE: the Japanese name has no letter case — its lowercase
		// mirror is identical and deduped away (QueryVariants contract).
		"atelier of witch hat",
		"atelier no witch hat",
	}
	if strings.Join(progress.variants, "|") != strings.Join(want, "|") {
		t.Fatalf("variants must carry every card name in order, got:\n%v\nwant:\n%v", progress.variants, want)
	}
	if len(progress.variants) > 16 {
		t.Fatalf("variants must cap at 16, got %v", progress.variants)
	}
	// ru provider saw a Cyrillic query first.
	if got := fs.queries["animego"]; len(got) == 0 || !hasCyrillic.MatchString(got[0]) {
		t.Fatalf("animego (ru) must be queried with Cyrillic first, got %v", got)
	}
	// ja provider saw a Latin query first.
	if got := fs.queries["gogoanime"]; len(got) == 0 || hasCyrillic.MatchString(got[0]) {
		t.Fatalf("gogoanime (ja) must be queried with Latin first, got %v", got)
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

// TestHybridSearchBindsTopCardOnAnyQuery pins the PR97 owner ruling —
// the local matcher is GONE: the TOP autocomplete record (Shikimori's
// own relevance rank) binds whatever the query is. Documented
// honestly: a nonsense query binds to whatever Shikimori ranked first
// and surfaces its full name inventory as variants — that is the
// owner's explicit choice (recall over precision).
func TestHybridSearchBindsTopCardOnAnyQuery(t *testing.T) {
	fs := newFakeSearch()
	fs.providers = fs.providers[:1]
	card := &shikimori.Anime{
		ID:      99,
		Name:    "Totally Unrelated Anime",
		Russian: "Совсем Другое Аниме",
	}
	shiki := &fakeShiki{
		enabled: true,
		items:   autocompleteItems([2]string{"Совсем Другое Аниме", "Totally Unrelated Anime"}),
		animes:  map[int64]*shikimori.Anime{100: card},
	}
	md := &fakeMetadata{}
	deps := hybridDeps(fs, shiki, md, nil)

	model := drive(NewApp(NewRootScreen(deps), deps, testLogger()),
		pushMsg{screen: NewSearchProgress(deps, "абракадабра")})
	model = drainCmds(model)
	progress := topOf(model).(*searchProgress)

	// The top card still binds: its names join the variant pool.
	joined := strings.Join(progress.variants, "|")
	if !strings.Contains(joined, "Совсем Другое Аниме") || !strings.Contains(joined, "Totally Unrelated Anime") {
		t.Fatalf("the top card must bind regardless of query similarity, got %v", progress.variants)
	}
	if len(progress.variants) <= 1 {
		t.Fatalf("the bound card must expand the variant pool, got %v", progress.variants)
	}
}

// TestHybridSearchGetAnimeFailureFallsBackToAutocompleteNames pins the
// documented fail-soft fallback (PR97 fast-follow): a card-fetch error
// must NOT collapse the variant pool — the autocomplete record's own
// two names (russian + english) still seed it, and the metadata
// manager keys on the english fallback name.
func TestHybridSearchGetAnimeFailureFallsBackToAutocompleteNames(t *testing.T) {
	fs := newFakeSearch()
	fs.providers = fs.providers[:1]
	shiki := &fakeShiki{
		enabled:  true,
		items:    autocompleteItems([2]string{"Наруто", "Naruto"}),
		animeErr: errors.New("card fetch exploded"),
	}
	md := &fakeMetadata{aliases: map[string][]string{
		"Naruto": {"Naruto: Shippuuden"},
	}}
	deps := hybridDeps(fs, shiki, md, nil)

	model := drive(NewApp(NewRootScreen(deps), deps, testLogger()),
		pushMsg{screen: NewSearchProgress(deps, "наруто")})
	model = drainCmds(model)
	progress := topOf(model).(*searchProgress)

	joined := strings.Join(progress.variants, "|")
	if !strings.Contains(joined, "Наруто") || !strings.Contains(joined, "Naruto") {
		t.Fatalf("the autocomplete names must seed the pool on a card-fetch failure, got %v", progress.variants)
	}
	if !strings.Contains(joined, "Naruto: Shippuuden") {
		t.Fatalf("metadata must still enrich via the english fallback name, got %v", progress.variants)
	}
	// The metadata manager keyed on the english fallback name (the
	// card's original name is unavailable).
	if len(md.queries) != 1 || md.queries[0] != "Naruto" {
		t.Fatalf("metadata must key on the english fallback, got %v", md.queries)
	}
}

// TestHybridSearchMergesAllVariants pins the PR97 fan-out redesign:
// EVERY language-routed variant runs (no early exit on the first
// non-empty), the per-variant result lists merge in first-seen
// order, and duplicates dedupe by exact title within the provider.
// The per-provider budget still bounds the whole loop.
func TestHybridSearchMergesAllVariants(t *testing.T) {
	fs := newFakeSearch()
	fs.providers = fs.providers[:1]

	// Variants: "наруто" hits, "Наруто" hits with one duplicate of
	// the first hit plus a new row, "Naruto" adds a latin row,
	// "naruto" comes back empty — the empty tail must still run.
	fs.variantResults = map[string][]contracts.SearchResult{
		"наруто": {{Title: "Наруто", URL: "u1", SourceID: "animego"}},
		"Наруто": {
			{Title: "Наруто", URL: "u1-dup", SourceID: "animego"},
			{Title: "Наруто: Ураганные хроники", URL: "u2", SourceID: "animego"},
		},
		"Naruto": {{Title: "Naruto", URL: "u3", SourceID: "animego"}},
	}

	shiki := &fakeShiki{enabled: true, items: autocompleteItems([2]string{"Наруто", "Naruto"})}
	md := &fakeMetadata{aliases: map[string][]string{"Наруто": {"Naruto"}}}
	deps := hybridDeps(fs, shiki, md, nil)

	model := drive(NewApp(NewRootScreen(deps), deps, testLogger()),
		pushMsg{screen: NewSearchProgress(deps, "наруто")})
	model = drainCmds(model)
	progress := topOf(model).(*searchProgress)

	got := fs.queries["animego"]
	// QueryVariants("наруто", [Наруто, Naruto]) = [наруто, Наруто,
	// Naruto, naruto] — ALL four must have been queried.
	if strings.Join(got, "|") != "наруто|Наруто|Naruto|naruto" {
		t.Fatalf("every variant must run without early exit, got %v", got)
	}
	// Merge: first-seen order, exact-title dedup drops the variant-2
	// duplicate of the variant-1 hit.
	wantTitles := "Наруто|Наруто: Ураганные хроники|Naruto"
	titles := make([]string, 0, len(progress.results))
	for _, r := range progress.results {
		titles = append(titles, r.Title)
	}
	if strings.Join(titles, "|") != wantTitles {
		t.Fatalf("merged results must keep first-seen order with exact-title dedup, got %v", titles)
	}
}

// TestHybridSearchContinuesPastVariantErrors pins the PR97 error
// semantics: one variant's failure does not abort the remaining
// variants — as long as SOME variant yields results, the row settles
// with the merged set (recall over fail-fast; the owner wants all
// results from all names).
func TestHybridSearchContinuesPastVariantErrors(t *testing.T) {
	fs := newFakeSearch()
	fs.providers = fs.providers[:1]
	fs.variantErrs["наруто"] = errors.New("first variant exploded")
	// The second variant answers; the first one's error must not
	// abort the loop before it runs.
	fs.variantResults["Наруто"] = []contracts.SearchResult{{Title: "Наруто", URL: "u1", SourceID: "animego"}}

	shiki := &fakeShiki{enabled: true, items: autocompleteItems([2]string{"Наруто", "Naruto"})}
	md := &fakeMetadata{aliases: map[string][]string{"Наруто": {"Naruto"}}}
	deps := hybridDeps(fs, shiki, md, nil)

	model := drive(NewApp(NewRootScreen(deps), deps, testLogger()),
		pushMsg{screen: NewSearchProgress(deps, "наруто")})
	model = drainCmds(model)
	progress := topOf(model).(*searchProgress)

	got := fs.queries["animego"]
	if len(got) < 2 {
		t.Fatalf("a failed variant must not abort the loop, got %v", got)
	}
	if len(progress.results) == 0 {
		t.Fatalf("results from later variants must still surface, got none")
	}
	if progress.status["animego"] != "Завершено" {
		t.Fatalf("a row with results must settle as Завершено, got %q", progress.status["animego"])
	}
}

// TestHybridSearchAllVariantsFailingFailsTheRow: when EVERY variant
// errors and nothing resolved, the row carries the first error (the
// honest failure state).
func TestHybridSearchAllVariantsFailingFailsTheRow(t *testing.T) {
	fs := newFakeSearch()
	fs.providers = fs.providers[:1]
	fs.errs["animego"] = errors.New("provider down")

	shiki := &fakeShiki{enabled: true, items: autocompleteItems([2]string{"Наруто", "Naruto"})}
	md := &fakeMetadata{aliases: map[string][]string{"Наруто": {"Naruto"}}}
	deps := hybridDeps(fs, shiki, md, nil)

	model := drive(NewApp(NewRootScreen(deps), deps, testLogger()),
		pushMsg{screen: NewSearchProgress(deps, "наруто")})
	model = drainCmds(model)
	progress := topOf(model).(*searchProgress)

	// PR98: the row keeps the raw first error (errs) for introspection
	// while the cell carries the short class label.
	if progress.errs["animego"] == nil || progress.errs["animego"].Error() != "provider down" {
		t.Fatalf("an all-error row must carry the first error, got %v", progress.errs["animego"])
	}
	if progress.status["animego"] != labelError {
		t.Fatalf("an all-error row must classify onto the short label, got %q", progress.status["animego"])
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
	if !strings.Contains(v, "⏱ таймаут") {
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

// TestProviderQueryLatinPreference pins the fan-out routing for the
// latin-only providers (PR42): a NamePrefLatin provider is queried
// with the latin variants ONLY — the Cyrillic ones are guaranteed-zero
// there. Without any resolved latin variant it falls back to the full
// set (fail-soft: search anyway, the provider returns 0); default
// providers keep the existing PR24 language ordering.
func TestProviderQueryLatinPreference(t *testing.T) {
	variants := []string{"Пираты «Чёрной лагуны»", "Пираты Чёрной лагуны", "Black Lagoon", "Burakku Ragūn"}
	fs := newFakeSearch()
	fs.namePrefs = map[string]contracts.NamePreference{"nyaa": contracts.NamePrefLatin}
	deps := hybridDeps(fs, nil, nil, map[string]string{"nyaa": "ja", "animego": "ru"})
	m := &searchProgress{deps: deps, variants: variants}

	t.Run("latin-only provider gets latin variants only", func(t *testing.T) {
		got := m.providerQueries("nyaa")
		for _, q := range got {
			if hasCyrillic.MatchString(q) {
				t.Errorf("latin-only provider must never see the Cyrillic query %q", q)
			}
		}
		if len(got) == 0 || got[0] != "Black Lagoon" {
			t.Fatalf("latin variants must ride in order, got %v", got)
		}
	})
	t.Run("default provider keeps the full ordered set", func(t *testing.T) {
		got := m.providerQueries("animego")
		if len(got) != len(variants) {
			t.Fatalf("routing must not drop variants for a default provider: %v", got)
		}
		if got[0] != variants[0] {
			t.Fatalf("a ru provider must keep the Cyrillic-first order, got %v", got)
		}
	})
	t.Run("no latin variants resolved falls back to the full set", func(t *testing.T) {
		cyrOnly := &searchProgress{deps: deps, variants: []string{"Пираты «Чёрной лагуны»"}}
		got := cyrOnly.providerQueries("nyaa")
		if len(got) != 1 || got[0] != "Пираты «Чёрной лагуны»" {
			t.Fatalf("fail-soft fallback must search anyway, got %v", got)
		}
	})
}

// TestHybridSearchCyrillicQueryRoutesLatinToTorrentProviders is the
// PR42 flow pin: a Cyrillic query binds through the russian name of
// the Shikimori record, and the resolved romaji/english name rides the
// variant pool — so the latin-only torrent providers are searched with
// the latin title (the owner's «Пираты «Чёрной лагуны»» → nyaa/
// animetosho/tokyotosho get "Black Lagoon", never the Cyrillic string
// that crashed tokyotosho's zero-result footer into a decode error).
func TestHybridSearchCyrillicQueryRoutesLatinToTorrentProviders(t *testing.T) {
	fs := newFakeSearch()
	fs.providers = []ProviderMeta{
		{ID: "animego", Name: "AnimeGO"},
		{ID: "nyaa", Name: "Nyaa"},
	}
	fs.namePrefs = map[string]contracts.NamePreference{"nyaa": contracts.NamePrefLatin}
	fs.results["animego"] = []contracts.SearchResult{{Title: "Пираты «Чёрной лагуны»", URL: "u1", SourceID: "animego"}}
	fs.results["nyaa"] = []contracts.SearchResult{{Title: "[Group] Black Lagoon", URL: "u2", SourceID: "nyaa"}}

	shiki := &fakeShiki{
		enabled: true,
		items:   autocompleteItems([2]string{"Пираты «Чёрной лагуны»", "Black Lagoon"}),
	}
	md := &fakeMetadata{}
	deps := hybridDeps(fs, shiki, md, map[string]string{"animego": "ru", "nyaa": "ja"})

	model := drive(NewApp(NewRootScreen(deps), deps, testLogger()),
		pushMsg{screen: NewSearchProgress(deps, "Пираты «Чёрной лагуны»")})
	drainCmds(model)

	// Shikimori was consulted with the original Cyrillic query.
	if len(shiki.queries) != 1 || shiki.queries[0] != "Пираты «Чёрной лагуны»" {
		t.Fatalf("shikimori must resolve the original query, got %v", shiki.queries)
	}
	// The latin-only provider saw ONLY the resolved latin names.
	got := fs.queries["nyaa"]
	if len(got) == 0 {
		t.Fatal("nyaa must have been searched")
	}
	for _, q := range got {
		if hasCyrillic.MatchString(q) {
			t.Errorf("the latin-only provider must never see the Cyrillic query %q", q)
		}
	}
	if got[0] != "Black Lagoon" {
		t.Fatalf("nyaa must be searched with the resolved romaji/english name first, got %v", got)
	}
	// The RU provider keeps the Cyrillic-first order (original query).
	ruQueries := fs.queries["animego"]
	if len(ruQueries) == 0 || !hasCyrillic.MatchString(ruQueries[0]) {
		t.Fatalf("the RU provider must be queried with Cyrillic first, got %v", ruQueries)
	}
}
