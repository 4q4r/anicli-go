package lua

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// okScript returns a minimal valid provider script body for id.
func okScript(id string) string {
	return "return {\n" +
		"\tid = \"" + id + "\",\n" +
		"\tsearch = function(query) return { { title = \"hit\", url = \"/u\" } } end,\n" +
		"\tepisodes = function(anime_url) return {} end,\n" +
		"\tstreams = function(episode_url, dub) return { dub_name = dub, links = {} } end,\n" +
		"}"
}

// TestLoadSourcesDedupsFirstWins pins the precedence rule (PR116): the
// caller orders sources (bundled → user dir → config dir) and the
// FIRST occurrence of an id wins; later duplicates are skips, never
// silent replacements.
func TestLoadSourcesDedupsFirstWins(t *testing.T) {
	log, _ := testLogger(t)
	sources := []Source{
		{ID: "alpha", Src: okScript("alpha"), Dir: "bundled"},
		{ID: "alpha", Src: okScript("alpha"), Dir: "/user"}, // duplicate: skipped
		{ID: "beta", Src: okScript("beta"), Dir: "/user"},
	}

	provs, errs := LoadSources(DefaultConfig(), log, sources, nil)
	if len(errs) != 1 {
		t.Fatalf("skips = %d (%v), want 1 (the duplicate alpha)", len(errs), errs)
	}
	if errs[0].Dir != "alpha" {
		t.Errorf("skip.Dir = %q, want alpha", errs[0].Dir)
	}
	if len(provs) != 2 {
		t.Fatalf("loaded %d providers, want 2", len(provs))
	}
	if provs[0].ID() != "alpha" || provs[1].ID() != "beta" {
		t.Errorf("order = %q,%q — want the source order preserved", provs[0].ID(), provs[1].ID())
	}
}

// TestLoadSourcesIsolatesBrokenScripts pins the error-isolation rule:
// one broken script is a skip; the other providers still load.
func TestLoadSourcesIsolatesBrokenScripts(t *testing.T) {
	log, buf := testLogger(t)
	sources := []Source{
		{ID: "broken", Src: "return { id = \"broken\" --[[ no functions ]] }", Dir: "bundled"},
		{ID: "syntax", Src: "return }}}", Dir: "bundled"},
		{ID: "good", Src: okScript("good"), Dir: "bundled"},
	}

	provs, errs := LoadSources(DefaultConfig(), log, sources, nil)
	if len(provs) != 1 || provs[0].ID() != "good" {
		t.Fatalf("loaded = %v, want only good", providerIDs(provs))
	}
	if len(errs) != 2 {
		t.Fatalf("skips = %d (%v), want 2", len(errs), errs)
	}
	if !logContains(buf, "provider script skipped") {
		t.Error("the skips must be logged (never silent)")
	}
}

// TestLoadSourcesWiresHTTPPerProvider pins the transport wiring: the
// httpFor callback runs once per LOADED provider id and the built
// provider's SDK HTTP calls route through the returned client.
func TestLoadSourcesWiresHTTPPerProvider(t *testing.T) {
	var gotIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)

	script := "return {\n" +
		"\tid = \"wired\",\n" +
		"\tsearch = function(query)\n" +
		"\t\tlocal r = anicli.http.get(\"" + srv.URL + "\")\n" +
		"\t\treturn { { title = r.body, url = \"/u\" } }\n" +
		"\tend,\n" +
		"\tepisodes = function(anime_url) return {} end,\n" +
		"\tstreams = function(episode_url, dub) return { dub_name = dub, links = {} } end,\n" +
		"}"

	sources := []Source{
		{ID: "wired", Src: script, Dir: "bundled"},
		{ID: "unwired", Src: okScript("unwired"), Dir: "bundled"},
	}
	httpFor := func(id string) *netclient.Client {
		gotIDs = append(gotIDs, id)
		// Production-shaped network config: netclient wraps every
		// request in a RequestTimeout context — a zero-value Network
		// would expire instantly.
		c, err := netclient.New(config.Default().Network, netclient.WithProvider(id))
		if err != nil {
			t.Fatalf("netclient.New: %v", err)
		}
		return c
	}

	log, _ := testLogger(t)
	provs, errs := LoadSources(DefaultConfig(), log, sources, httpFor)
	if len(errs) != 0 {
		t.Fatalf("unexpected skips: %v", errs)
	}
	if len(gotIDs) != 2 || gotIDs[0] != "wired" || gotIDs[1] != "unwired" {
		t.Fatalf("httpFor ids = %v, want every loaded provider once", gotIDs)
	}

	results, err := provs[0].Search(context.Background(), "q")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].Title != "ok" {
		t.Fatalf("results = %+v, want the response body routed through the wired client", results)
	}
}

// providerIDs projects the loaded providers onto their ids.
func providerIDs(provs []contracts.Provider) []string {
	out := make([]string, 0, len(provs))
	for _, p := range provs {
		out = append(out, p.ID())
	}
	return out
}
