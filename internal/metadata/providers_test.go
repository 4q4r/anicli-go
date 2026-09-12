package metadata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// newTestNet builds the shared netclient with short timeouts.
func newTestNet(t *testing.T, provider string) *netclient.Client {
	t.Helper()
	cfg := config.Default().Network
	cfg.ProxyURL = ""
	cfg.RequestTimeout = 5 * time.Second
	net, err := netclient.New(cfg, netclient.WithProvider(provider))
	if err != nil {
		t.Fatalf("netclient.New: %v", err)
	}
	return net
}

// TestNormalizeTitle ports the Python _normalize_title contract:
// whitespace collapse, strip, and the 220-rune cap.
func TestNormalizeTitle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, in, want string
	}{
		{name: "collapse whitespace", in: "  Foo   Bar\t\nQux ", want: "Foo Bar Qux"},
		{name: "empty stays empty", in: "   ", want: ""},
		{name: "plain passes", in: "Naruto", want: "Naruto"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := normalizeTitle(tt.in); got != tt.want {
				t.Errorf("normalizeTitle(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestNormalizeTitleCapsLength pins the 220-rune MAX_TITLE_LENGTH cap
// (rune-safe, not byte-safe).
func TestNormalizeTitleCapsLength(t *testing.T) {
	t.Parallel()

	long := make([]rune, maxTitleLength+50)
	for i := range long {
		long[i] = 'あ'
	}
	got := normalizeTitle(string(long))
	if n := len([]rune(got)); n != maxTitleLength {
		t.Errorf("normalized length = %d runes, want %d", n, maxTitleLength)
	}
}

// TestAniListSearch pins the GraphQL contract: query shape, variables,
// title/synonym extraction and normalization.
func TestAniListSearch(t *testing.T) {
	t.Parallel()

	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		buf := make([]byte, 8192)
		n, _ := r.Body.Read(buf)
		body = string(buf[:n])
		_, _ = w.Write([]byte(`{
			"data": {"Page": {"media": [
				{"title": {"romaji": "Naruto", "english": "  Naruto   Shippuden ", "native": "ナルト"},
				 "synonyms": ["Naruto", "Наруто"]},
				{"title": {"romaji": "Bleach"}, "synonyms": []}
			]}}
		}`))
	}))
	defer srv.Close()

	c := NewAniListClient(newTestNet(t, "anilist"), srv.URL, nil)
	titles, err := c.SearchAlternativeTitles(context.Background(), "naruto")
	if err != nil {
		t.Fatalf("SearchAlternativeTitles: %v", err)
	}

	want := []string{"Bleach", "Naruto", "Naruto Shippuden", "ナルト", "Наруто"}
	if len(titles) != len(want) {
		t.Fatalf("titles = %v, want %v", titles, want)
	}
	set := map[string]bool{}
	for _, title := range titles {
		set[title] = true
	}
	for _, w := range want {
		if !set[w] {
			t.Errorf("titles missing %q: %v", w, titles)
		}
	}
	for _, sub := range []string{`"search":"naruto"`, `"page":1`, `"perPage":8`} {
		if !strings.Contains(body, sub) {
			t.Errorf("request body %q missing %q", body, sub)
		}
	}
}

// TestAniListSearchEmptyData pins: GraphQL replies without data yield an
// empty result, not an error (python returned set()).
func TestAniListSearchEmptyData(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"errors": [{"message": "not found"}]}`))
	}))
	defer srv.Close()

	c := NewAniListClient(newTestNet(t, "anilist"), srv.URL, nil)
	titles, err := c.SearchAlternativeTitles(context.Background(), "x")
	if err != nil {
		t.Fatalf("SearchAlternativeTitles: %v", err)
	}
	if len(titles) != 0 {
		t.Errorf("titles = %v, want empty", titles)
	}
}

// TestKitsuSearch pins the JSON:API contract: filter/page query params,
// vnd.api+json accept, canonicalTitle + titles + abbreviatedTitles.
func TestKitsuSearch(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/anime" {
			t.Errorf("path = %q, want /anime", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("filter[text]") != "naruto" || q.Get("page[limit]") != "8" {
			t.Errorf("query = %q, want filter[text]=naruto&page[limit]=8", r.URL.RawQuery)
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.api+json" {
			t.Errorf("accept = %q", got)
		}
		_, _ = w.Write([]byte(`{"data": [
			{"attributes": {
				"canonicalTitle": "Naruto",
				"titles": {"en": "Naruto", "en_jp": "Naruto", "ja_jp": "ナルト"},
				"abbreviatedTitles": ["NS"]}
			},
			{"attributes": {"canonicalTitle": "Bleach"}}
		]}`))
	}))
	defer srv.Close()

	c := NewKitsuClient(newTestNet(t, "kitsu"), srv.URL, nil)
	titles, err := c.SearchAlternativeTitles(context.Background(), "naruto")
	if err != nil {
		t.Fatalf("SearchAlternativeTitles: %v", err)
	}

	want := []string{"Bleach", "Naruto", "NS", "ナルト"}
	if len(titles) != len(want) {
		t.Fatalf("titles = %v, want %v", titles, want)
	}
	set := map[string]bool{}
	for _, title := range titles {
		set[title] = true
	}
	for _, w := range want {
		if !set[w] {
			t.Errorf("titles missing %q: %v", w, titles)
		}
	}
}

// TestAniSearchSearch pins the HTML contract: /anime/index?char=,
// anchor text + title attributes.
func TestAniSearchSearch(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/anime/index" {
			t.Errorf("path = %q, want /anime/index", r.URL.Path)
		}
		if q := r.URL.Query().Get("char"); q != "naruto" {
			t.Errorf("char = %q, want naruto", q)
		}
		_, _ = w.Write([]byte(`<html><body>
			<a href="/anime/naruto" title="NARUTO">Naruto</a>
			<a href="/anime/naruto-shippuuden">Naruto Shippuden</a>
			<a href="/manga/naruto">ignored</a>
			</body></html>`))
	}))
	defer srv.Close()

	c := NewAniSearchClient(newTestNet(t, "anisearch"), srv.URL, nil)
	titles, err := c.SearchAlternativeTitles(context.Background(), "naruto")
	if err != nil {
		t.Fatalf("SearchAlternativeTitles: %v", err)
	}
	want := []string{"NARUTO", "Naruto", "Naruto Shippuden"}
	if len(titles) != len(want) {
		t.Fatalf("titles = %v, want %v", titles, want)
	}
	set := map[string]bool{}
	for _, title := range titles {
		set[title] = true
	}
	for _, w := range want {
		if !set[w] {
			t.Errorf("titles missing %q: %v", w, titles)
		}
	}
}

// TestAniSearchCapsScrapedAliases pins MAX_SCRAPED_ALIASES=30 early stop.
func TestAniSearchCapsScrapedAliases(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		page := `<html><body>`
		for i := range 200 {
			page += `<a href="/anime/x` + strconv.Itoa(i) + `" title="T` + strconv.Itoa(i) + `">X` + strconv.Itoa(i) + `</a>`
		}
		page += `</body></html>`
		_, _ = w.Write([]byte(page))
	}))
	defer srv.Close()

	c := NewAniSearchClient(newTestNet(t, "anisearch"), srv.URL, nil)
	titles, err := c.SearchAlternativeTitles(context.Background(), "x")
	if err != nil {
		t.Fatalf("SearchAlternativeTitles: %v", err)
	}
	if n := len(titles); n > maxScrapedAliases {
		t.Errorf("scraped %d aliases, cap is %d", n, maxScrapedAliases)
	}
	if n := len(titles); n < maxScrapedAliases {
		t.Errorf("scraped only %d aliases, want the full cap %d", n, maxScrapedAliases)
	}
}

// TestAniDBSearch pins the HTML contract: /search/anime with
// adb.search + do.search params, href filter and .animetitle selector.
func TestAniDBSearch(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/anime" {
			t.Errorf("path = %q, want /search/anime", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("adb.search") != "naruto" || q.Get("do.search") != "1" {
			t.Errorf("query = %q, want adb.search=naruto&do.search=1", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`<html><body>
			<table><tr><td><a href="/anime/naruto" title="NARUTO">Naruto</a></td></tr></table>
			<a href="/anime/naruto-shippuuden">Naruto Shippuden</a>
			<div class="animetitle">Boruto</div>
			<a href="/manga/other">ignored</a>
			</body></html>`))
	}))
	defer srv.Close()

	c := NewAniDBClient(newTestNet(t, "anidb"), srv.URL, nil)
	titles, err := c.SearchAlternativeTitles(context.Background(), "naruto")
	if err != nil {
		t.Fatalf("SearchAlternativeTitles: %v", err)
	}
	want := []string{"Boruto", "NARUTO", "Naruto", "Naruto Shippuden"}
	if len(titles) != len(want) {
		t.Fatalf("titles = %v, want %v", titles, want)
	}
	set := map[string]bool{}
	for _, title := range titles {
		set[title] = true
	}
	for _, w := range want {
		if !set[w] {
			t.Errorf("titles missing %q: %v", w, titles)
		}
	}
}

// TestProvidersNormalizeOutput pins that every provider normalizes the
// titles it returns (whitespace collapse) — the manager dedupes on the
// normalized form.
func TestProvidersNormalizeOutput(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data": {"Page": {"media": [
			{"title": {"romaji": "  Spaced   Out "}, "synonyms": []}
		]}}}`))
	}))
	defer srv.Close()

	c := NewAniListClient(newTestNet(t, "anilist"), srv.URL, nil)
	titles, err := c.SearchAlternativeTitles(context.Background(), "x")
	if err != nil {
		t.Fatalf("SearchAlternativeTitles: %v", err)
	}
	if len(titles) != 1 || titles[0] != "Spaced Out" {
		t.Errorf("titles = %v, want [Spaced Out]", titles)
	}
}
