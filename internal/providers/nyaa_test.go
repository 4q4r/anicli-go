package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
)

func TestNyaaSearchParsesRSS(t *testing.T) {
	t.Parallel()

	p := newNyaaFixtureAt(t, nyaaServer(t, string(fixture(t, "nyaa_search_rss.xml")), nil))
	results, err := p.Search(context.Background(), "test show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}

	batch := results[0]
	if batch.Title != "[SubsPlease] Test Show (01-12) (1080p) [AAC].mkv" {
		t.Errorf("title = %q", batch.Title)
	}
	if batch.SourceID != "nyaa" {
		t.Errorf("source id = %q, want nyaa", batch.SourceID)
	}
	// Magnet from the RSS infoHash — the engine ingests the infohash
	// directly (no .torrent download needed for ingestion).
	wantMagnet := "magnet:?xt=urn:btih:abcdef0123456789abcdef0123456789abcdef01&dn=" +
		url.QueryEscape(batch.Title)
	if batch.URL != wantMagnet {
		t.Errorf("url = %q, want magnet %q", batch.URL, wantMagnet)
	}
	if batch.Meta[SearchMetaSize] != "7.4 GiB" {
		t.Errorf("size meta = %v", batch.Meta[SearchMetaSize])
	}
	if batch.Meta[SearchMetaSeeders] != "421" || batch.Meta[SearchMetaLeechers] != "33" {
		t.Errorf("seeders/leechers meta = %v/%v", batch.Meta[SearchMetaSeeders], batch.Meta[SearchMetaLeechers])
	}
	if batch.Meta[SearchMetaQuality] != "1080p" {
		t.Errorf("quality meta = %v, want the PR35 badge 1080p", batch.Meta[SearchMetaQuality])
	}

	// No infoHash on the wire → the .torrent URL is the link.
	fallback := results[1]
	if fallback.URL != "https://nyaa.si/download/2222222.torrent" {
		t.Errorf("fallback url = %q, want the RSS <link>", fallback.URL)
	}
	if fallback.Meta[SearchMetaQuality] != "2160p" {
		t.Errorf("fallback quality meta = %v, want the PR35 badge 2160p", fallback.Meta[SearchMetaQuality])
	}
}

func TestNyaaSearchRequestParams(t *testing.T) {
	t.Parallel()

	var gotQuery url.Values
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		gotPath = r.URL.Path
		_, _ = w.Write(fixture(t, "nyaa_search_rss.xml"))
	}))
	t.Cleanup(srv.Close)
	p := newNyaaFixtureAt(t, srv.URL)

	if _, err := p.Search(context.Background(), "fate stay night"); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotPath != "/" {
		t.Errorf("path = %q, want the site root", gotPath)
	}
	if gotQuery.Get("page") != "rss" {
		t.Errorf("page = %q, want rss", gotQuery.Get("page"))
	}
	if gotQuery.Get("q") != "fate stay night" {
		t.Errorf("q = %q, want the raw query", gotQuery.Get("q"))
	}
	// Controller-verified defaults: English-translated anime, no
	// filter, seeded-first ordering.
	if gotQuery.Get("c") != "1_2" {
		t.Errorf("c = %q, want 1_2 (anime english-translated)", gotQuery.Get("c"))
	}
	if gotQuery.Get("f") != "0" {
		t.Errorf("f = %q, want 0 (no filter)", gotQuery.Get("f"))
	}
	if gotQuery.Get("s") != "seeders" || gotQuery.Get("o") != "desc" {
		t.Errorf("s/o = %q/%q, want seeders/desc", gotQuery.Get("s"), gotQuery.Get("o"))
	}
}

func TestNyaaSearchEmptyQueryFailsLoud(t *testing.T) {
	t.Parallel()

	hits := 0
	p := newNyaaFixtureAt(t, nyaaServer(t, string(fixture(t, "nyaa_search_rss.xml")), &hits))
	for _, query := range []string{"", "   "} {
		_, err := p.Search(context.Background(), query)
		if err == nil {
			t.Fatalf("query %q: empty search must fail loud", query)
		}
		if !errors.Is(err, contracts.ErrInvalidInput) {
			t.Errorf("query %q: error = %v, want ErrInvalidInput", query, err)
		}
	}
	if hits != 0 {
		t.Errorf("server hits = %d, want 0 (no network on the guard)", hits)
	}
}

// TestNyaaSearchSkipsLinklessItems: an item with neither a usable
// infoHash nor a <link> has no torrent link at all — it is dropped
// like an empty title, never handed downstream as a dead result.
func TestNyaaSearchSkipsLinklessItems(t *testing.T) {
	t.Parallel()

	const body = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:nyaa="https://nyaa.si/xmlns/nyaa">
  <channel>
    <item>
      <title>[Good] Show - 01 (1080p).mkv</title>
      <link>https://nyaa.si/download/3333333.torrent</link>
      <nyaa:infoHash>abcdef0123456789abcdef0123456789abcdef01</nyaa:infoHash>
      <nyaa:size>1.0 GiB</nyaa:size>
    </item>
    <item>
      <title>[Broken] No hash no link</title>
      <nyaa:size>1.0 GiB</nyaa:size>
    </item>
  </channel>
</rss>`

	p := newNyaaFixtureAt(t, nyaaServer(t, body, nil))
	results, err := p.Search(context.Background(), "test")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1 (the linkless item must be skipped)", len(results))
	}
	if results[0].URL == "" {
		t.Error("the kept result must carry its magnet link")
	}
}

func TestNyaaSearchMalformedRSSTypedError(t *testing.T) {
	t.Parallel()

	p := newNyaaFixtureAt(t, nyaaServer(t, "this is not xml at all", nil))
	_, err := p.Search(context.Background(), "test")
	if err == nil {
		t.Fatal("malformed RSS must fail loud")
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) || perr.Provider != "nyaa" || perr.Op != contracts.OpSearch {
		t.Errorf("error = %v, want a nyaa search ProviderError", err)
	}
}

func TestNyaaSearchHTTPErrorTypedError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	p := newNyaaFixtureAt(t, srv.URL)

	_, err := p.Search(context.Background(), "test")
	if err == nil {
		t.Fatal("HTTP failure must fail loud")
	}
	var perr *contracts.ProviderError
	if !errors.As(err, &perr) || perr.Provider != "nyaa" {
		t.Errorf("error = %v, want a nyaa-tagged ProviderError", err)
	}
}

// TestNyaaGetEpisodesDelegatesToEpisodesWait: the provider
// GetEpisodes path rides the base's bounded metadata wait (the search
// result resolves long after the search; unreachable metadata fails
// loud on the caller's deadline, never silent-empty).
func TestNyaaGetEpisodesDelegatesToEpisodesWait(t *testing.T) {
	t.Parallel()

	const dead = "magnet:?xt=urn:btih:fedcba9876543210fedcba9876543210fedcba98"
	p := newNyaaWithEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	eps, err := p.GetEpisodes(ctx, dead)
	if err == nil {
		t.Fatal("GetEpisodes on unreachable metadata must fail loud")
	}
	if len(eps) != 0 {
		t.Errorf("episodes = %v, want none", eps)
	}
}

func TestNyaaCapabilityAndRoster(t *testing.T) {
	t.Parallel()

	p := newNyaaFixtureAt(t, nyaaServer(t, string(fixture(t, "nyaa_search_rss.xml")), nil))
	if !p.IsTorrent() {
		t.Error("nyaa must carry the torrent capability")
	}
	if p.ID() != "nyaa" || p.Name() != "Nyaa" {
		t.Errorf("id/name = %q/%q", p.ID(), p.Name())
	}
	if p.SourceType() != contracts.SourceTypeBoth {
		t.Errorf("source type = %q, want both (JA audio, acceptable video)", p.SourceType())
	}
	if got := p.ContentLanguage(); got != "ja" {
		t.Errorf("content language = %q, want ja (JP audio with subs)", got)
	}
	if p.BaseURL() == "" {
		t.Error("base URL must be the site root, not empty")
	}
}

// TestNyaaNotUnconfiguredByDefault pins the no-credentials convention:
// nyaa has nothing to configure, so it must never appear in the
// disabled table with default settings.
func TestNyaaNotUnconfiguredByDefault(t *testing.T) {
	t.Parallel()

	for _, d := range UnconfiguredProviders(config.Default()) {
		if d.ID == "nyaa" {
			t.Fatalf("nyaa must not be unconfigured by default: %s", d.Reason)
		}
	}
}

// --- helpers ---

// nyaaServer serves body on every request (the RSS search endpoint);
// hits, when non-nil, counts requests.
func nyaaServer(t *testing.T, body string, hits *int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits != nil {
			*hits++
		}
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func newNyaaFixtureAt(t *testing.T, baseURL string) *Nyaa {
	t.Helper()
	return newNyaa(baseURL, testClient(t, "nyaa"), nil)
}

func newNyaaWithEngine(t *testing.T) *Nyaa {
	t.Helper()
	eng := newOfflineTestEngine(t)
	t.Cleanup(func() { _ = eng.Close() })
	return newNyaa(NyaaBase, testClient(t, "nyaa"), eng)
}
