package lua

import (
	"context"
	"errors"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// The capability adapters (PR116): a script may declare content_lang,
// smoke_query and name_preference — the optional surfaces the compiled
// providers expose as Go interfaces (registry duck-typing). The
// adapters wrap ONLY what the script actually declares: an undeclared
// capability must stay invisible to the duck-typed lookups (an
// always-implemented SmokeQuery would make the parity smoke probe
// every user script with an empty query).

// capsScript builds a loadable provider script with the given table
// fields appended (must end without a closing brace).
func capsScript(id, extraFields string) string {
	return "return {\n" +
		"\tid = \"" + id + "\",\n" +
		"\tsearch = function(query) return {} end,\n" +
		"\tepisodes = function(anime_url) return {} end,\n" +
		"\tstreams = function(episode_url, dub) return { dub_name = dub, links = {} } end,\n" +
		extraFields +
		"}"
}

func mustLoad(t *testing.T, id, src string) contracts.Provider {
	t.Helper()
	p, err := NewEngine(DefaultConfig(), mustLogger(t)).LoadProvider(id, src)
	if err != nil {
		t.Fatalf("LoadProvider: %v", err)
	}
	return p.Adapt()
}

func TestAdaptContentLanguage(t *testing.T) {
	p := mustLoad(t, "capped", capsScript("capped", "\tcontent_lang = \"ru\",\n"))
	lc, ok := p.(interface{ ContentLanguage() string })
	if !ok {
		t.Fatal("the adapted provider lost the ContentLanguage capability")
	}
	if got := lc.ContentLanguage(); got != "ru" {
		t.Errorf("ContentLanguage = %q, want ru", got)
	}
}

func TestAdaptSmokeQuery(t *testing.T) {
	p := mustLoad(t, "capped", capsScript("capped", "\tsmoke_query = \"дандадан\",\n"))
	sq, ok := p.(contracts.SmokeQueryProvider)
	if !ok {
		t.Fatal("the adapted provider lost the SmokeQueryProvider capability")
	}
	if got := sq.SmokeQuery(); got != "дандадан" {
		t.Errorf("SmokeQuery = %q, want дандадан", got)
	}
}

func TestAdaptNamePreferenceLatin(t *testing.T) {
	p := mustLoad(t, "capped", capsScript("capped", "\tname_preference = \"latin\",\n"))
	np, ok := p.(contracts.NamePreferenceProvider)
	if !ok {
		t.Fatal("the adapted provider lost the NamePreferenceProvider capability")
	}
	if got := np.NamePreference(); got != contracts.NamePrefLatin {
		t.Errorf("NamePreference = %v, want NamePrefLatin", got)
	}
}

func TestAdaptUnknownNamePreferenceFailsLoad(t *testing.T) {
	_, err := NewEngine(DefaultConfig(), mustLogger(t)).LoadProvider("capped",
		capsScript("capped", "\tname_preference = \"cyrillic\",\n"))
	if err == nil {
		t.Fatal("error = nil, want the unknown name_preference rejection")
	}
}

// TestAdaptUndeclaredStaysInvisible pins the honest duck-typing rule:
// a script declaring nothing wraps to itself, so the registry's
// capability lookups see exactly what the script declared.
func TestAdaptUndeclaredStaysInvisible(t *testing.T) {
	p := mustLoad(t, "bare", capsScript("bare", ""))
	if _, ok := p.(contracts.SmokeQueryProvider); ok {
		t.Error("undeclared smoke_query must not surface SmokeQueryProvider")
	}
	if _, ok := p.(contracts.NamePreferenceProvider); ok {
		t.Error("undeclared name_preference must not surface NamePreferenceProvider")
	}
	if _, ok := p.(interface{ ContentLanguage() string }); ok {
		t.Error("undeclared content_lang must not surface ContentLanguage")
	}
}

// TestAdaptPartialDeclarationZeroEquivalence pins the composite's
// zero-value semantics: a script declaring SOME capabilities wraps in
// the adapter whose undeclared surfaces return the zero values —
// observationally identical to not-implemented at every consumer
// (registry "" / NamePrefDefault, the parity smoke's declared != ""
// guard).
func TestAdaptPartialDeclarationZeroEquivalence(t *testing.T) {
	p := mustLoad(t, "partial", capsScript("partial", "\tsmoke_query = \"дандадан\",\n"))
	sq, ok := p.(contracts.SmokeQueryProvider)
	if !ok {
		t.Fatal("the declared smoke_query must surface SmokeQueryProvider")
	}
	if sq.SmokeQuery() != "дандадан" {
		t.Errorf("SmokeQuery = %q", sq.SmokeQuery())
	}
	lc := p.(interface{ ContentLanguage() string })
	if got := lc.ContentLanguage(); got != "" {
		t.Errorf("undeclared ContentLanguage = %q, want \"\" (the registry's not-declared value)", got)
	}
	np := p.(contracts.NamePreferenceProvider)
	if got := np.NamePreference(); got != contracts.NamePrefDefault {
		t.Errorf("undeclared NamePreference = %v, want NamePrefDefault", got)
	}
}

// TestSetLoggerReachesEngine pins the logger seam (PR62 #4): the
// registry's SetLogger loop routes provider diagnostics to the
// configured sink — a Lua provider's engine print/SDK logs must
// follow (never stderr in the TUI).
func TestSetLoggerReachesEngine(t *testing.T) {
	p, err := NewEngine(DefaultConfig(), mustLogger(t)).LoadProvider("capped",
		capsScript("capped", "\tsearch = function(query) print(\"probe-marker\") return {} end,\n"))
	if err != nil {
		t.Fatalf("LoadProvider: %v", err)
	}

	log, buf := testLogger(t)
	p.SetLogger(log)

	_, err = p.Search(context.Background(), "q")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !logContains(buf, "probe-marker") {
		t.Error("print output missed the swapped logger")
	}
}

// TestTypedErrorsPreserveSentinels pins the typed-error flow (PR116):
// anicli.fail(kind, message) raises inside the VM and the adapter's
// error classification re-attaches the matching contracts sentinel —
// consumers' errors.Is branches (tui/real.go ErrNotFound handling)
// must work identically for Lua providers.
func TestTypedErrorsPreserveSentinels(t *testing.T) {
	src := "return {\n" +
		"\tid = \"typed\",\n" +
		"\tsearch = function(query) anicli.fail(\"extract_failed\", \"no cards\") return {} end,\n" +
		"\tepisodes = function(anime_url) anicli.fail(\"not_found\", \"no player data\") return {} end,\n" +
		"\tstreams = function(episode_url, dub) anicli.fail(\"invalid_input\", \"no such dub\") return {} end,\n" +
		"}"
	p, err := NewEngine(DefaultConfig(), mustLogger(t)).LoadProvider("typed", src)
	if err != nil {
		t.Fatalf("LoadProvider: %v", err)
	}

	ctx := context.Background()
	if _, err := p.Search(ctx, "q"); !errors.Is(err, contracts.ErrExtractFailed) {
		t.Errorf("Search error = %v, want ErrExtractFailed sentinel", err)
	}
	if _, err := p.GetEpisodes(ctx, "u"); !errors.Is(err, contracts.ErrNotFound) {
		t.Errorf("GetEpisodes error = %v, want ErrNotFound sentinel", err)
	}
	_, err = p.ResolveStream(ctx, contracts.Episode{Num: "1", RawID: "1"}, "dub")
	if !errors.Is(err, contracts.ErrInvalidInput) {
		t.Errorf("ResolveStream error = %v, want ErrInvalidInput sentinel", err)
	}
}

// TestAdaptPreservesProviderSurface pins that the adapters stay
// contracts.Provider: search still flows through the wrapped chain.
func TestAdaptPreservesProviderSurface(t *testing.T) {
	p := mustLoad(t, "capped", capsScript("capped", "\tcontent_lang = \"ru\",\n"))
	results, err := p.Search(context.Background(), "q")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if results == nil {
		t.Error("Search returned nil results, want an empty slice")
	}
}
