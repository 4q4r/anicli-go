package shikimori

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/config"
)

// autocompleteHTML mirrors shikimori's /animes/autocomplete/v2 payload
// shape: a JSON object whose "content" carries the result-list HTML.
const autocompleteHTML = `
<div class="b-db_entry-variant-list_item" data-type="manga" data-id="1" data-text="Manga Thing">
  <div class="info"><div class="name"><a href="/mangas/1">M</a></div></div>
</div>
<div class="b-db_entry-variant-list_item" data-type="anime" data-id="5114" data-text="Fullmetal Alchemist">
  <div class="info"><div class="name">
    <a href="/animes/5114-fullmetal-alchemist" class="b-link">Стальной алхимик <span class="b-separator inline" data-replace=" · "></span>Fullmetal Alchemist</a>
  </div></div>
  <picture><img src="/system/animes/preview/5114.jpg" srcset="/system/animes/x96/5114.jpg 96w, /system/animes/original/5114.jpg 400w"></picture>
  <div class="b-tag" data-href="/animes/kind/tv">ТВ</div>
  <div class="b-tag" data-href="/animes/season/2003">2003</div>
</div>
<div class="b-db_entry-variant-list_item" data-type="anime" data-id="5114" data-text="Fullmetal Alchemist (dup)">
  <div class="info"><div class="name"><a href="/animes/5114">Стальной алхимик</a></div></div>
  <picture><img src="/system/animes/preview/5114.jpg"></picture>
</div>
<div class="b-db_entry-variant-list_item" data-type="anime" data-id="21" data-text="One Piece">
  <div class="info"><div class="name"><a href="/animes/21">One Piece / Ван-Пис</a></div></div>
  <img data-src="/system/animes/original/21.jpg">
  <div class="b-tag" data-href="/animes/kind/tv">ТВ</div>
</div>
`

func autocompleteHandler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/animes/autocomplete/v2" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("search") == "" {
			http.Error(w, "missing search", http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]string{"content": autocompleteHTML})
	}
}

func TestSearchIDs(t *testing.T) {
	c, log := newTestClient(t, configEnabled(), autocompleteHandler(t))

	got, err := c.SearchIDs(context.Background(), "alchemist")
	if err != nil {
		t.Fatalf("SearchIDs: %v", err)
	}
	// Port of shikimori.py:88-122: dict {data-text: data-id} for every
	// anime row — three distinct data-text values in the fixture.
	if len(got) != 3 {
		t.Fatalf("got %d results, want 3: %v", len(got), got)
	}
	if got["Fullmetal Alchemist"] != 5114 {
		t.Fatalf("Fullmetal Alchemist -> %d, want 5114", got["Fullmetal Alchemist"])
	}
	if got["One Piece"] != 21 {
		t.Fatalf("One Piece -> %d, want 21", got["One Piece"])
	}
	if got["Fullmetal Alchemist (dup)"] != 5114 {
		t.Fatalf("duplicate data-text must map too (python dict semantics)")
	}
	if n := log.count("/animes/autocomplete/v2"); n != 1 {
		t.Fatalf("autocomplete hit %d times, want 1", n)
	}
}

func TestSearchIDsServerErrorReturnsEmpty(t *testing.T) {
	c, _ := newTestClient(t, configEnabled(), func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	got, err := c.SearchIDs(context.Background(), "q")
	if err != nil {
		t.Fatalf("python parity: exceptions surface as empty map, got err %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty map, got %v", got)
	}
}

func TestAutocomplete(t *testing.T) {
	c, _ := newTestClient(t, configEnabled(), autocompleteHandler(t))

	items, err := c.Autocomplete(context.Background(), "alchemist", 7)
	if err != nil {
		t.Fatalf("Autocomplete: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items (dedup by data-id), want 2", len(items))
	}

	first := items[0]
	if first.ShikimoriID != 5114 {
		t.Fatalf("first item id = %d, want 5114", first.ShikimoriID)
	}
	if first.TitleEn == nil || *first.TitleEn != "Fullmetal Alchemist" {
		t.Fatalf("title_en = %v, want data-text", first.TitleEn)
	}
	if first.TitleRu == nil || *first.TitleRu != "Стальной алхимик" {
		t.Fatalf("title_ru = %v (anchor text before b-separator inline), got %v", first.TitleRu, first.TitleRu)
	}
	if first.PosterURL == nil || !strings.HasSuffix(*first.PosterURL, "/system/animes/original/5114.jpg") {
		t.Fatalf("poster_url = %v, want srcset-last original", first.PosterURL)
	}
	if !strings.HasPrefix(*first.PosterURL, "http") {
		t.Fatalf("poster_url must resolve absolute, got %v", first.PosterURL)
	}
	if first.Type == nil || *first.Type != "ТВ" {
		t.Fatalf("type = %v, want kind tag text", first.Type)
	}
	if first.Year == nil || *first.Year != 2003 {
		t.Fatalf("year = %v, want 2003", first.Year)
	}
	if first.URL != "/anime/5114" {
		t.Fatalf("url = %q, want /anime/5114", first.URL)
	}

	second := items[1]
	if second.ShikimoriID != 21 {
		t.Fatalf("second item id = %d, want 21", second.ShikimoriID)
	}
	// Python: inner html exists and carries no separator span, so the
	// whole cleaned text is the ru title (the "/" split only fires on
	// the text() fallback path).
	if second.TitleRu == nil || *second.TitleRu != "One Piece / Ван-Пис" {
		t.Fatalf("title_ru = %v", second.TitleRu)
	}
	if second.PosterURL == nil || !strings.HasSuffix(*second.PosterURL, "/system/animes/original/21.jpg") {
		t.Fatalf("poster_url must use data-src fallback, got %v", second.PosterURL)
	}
	if second.Year != nil {
		t.Fatalf("year = %v, want nil without season tag", *second.Year)
	}
}

func TestAutocompleteLimit(t *testing.T) {
	c, _ := newTestClient(t, configEnabled(), autocompleteHandler(t))
	items, err := c.Autocomplete(context.Background(), "q", 1)
	if err != nil {
		t.Fatalf("Autocomplete: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("limit not respected: %d items", len(items))
	}
}

func TestGetAnimesInfoChunking(t *testing.T) {
	ids := make([]int64, 0, 120)
	for i := int64(1); i <= 120; i++ {
		ids = append(ids, i)
	}

	c, log := newTestClient(t, bearerCfg("tok"), func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/animes" {
			http.NotFound(w, r)
			return
		}
		idParam := r.URL.Query().Get("ids")
		if idParam == "" {
			http.Error(w, "ids required", http.StatusBadRequest)
			return
		}
		parts := strings.Split(idParam, ",")
		if len(parts) > 50 {
			http.Error(w, "too many ids", http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("limit") != "50" {
			http.Error(w, "limit must be 50", http.StatusBadRequest)
			return
		}
		out := make([]map[string]any, 0, len(parts))
		for _, p := range parts {
			out = append(out, map[string]any{"id": jsonInt(t, p), "name": "A" + p, "russian": "Р" + p})
		}
		writeJSON(w, out)
	})

	animes, err := c.GetAnimesInfo(context.Background(), ids)
	if err != nil {
		t.Fatalf("GetAnimesInfo: %v", err)
	}
	if len(animes) != 120 {
		t.Fatalf("got %d animes, want 120", len(animes))
	}
	if animes[0].ID != 1 || animes[119].ID != 120 {
		t.Fatalf("ordering broken: first=%d last=%d", animes[0].ID, animes[119].ID)
	}
	// 120 ids at chunk size 50 -> exactly 3 requests.
	if n := log.count("/api/animes"); n != 3 {
		t.Fatalf("chunked into %d requests, want 3", n)
	}
	// Bearer auth must ride (api_headers).
	if log.snapshot()[0].Auth != "Bearer tok" {
		t.Fatalf("Authorization header missing: %q", log.snapshot()[0].Auth)
	}
}

func TestGetAnimesInfoEmpty(t *testing.T) {
	c, log := newTestClient(t, bearerCfg("tok"), func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no request expected for empty ids")
	})
	got, err := c.GetAnimesInfo(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetAnimesInfo(nil): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty, got %d", len(got))
	}
	if n := len(log.snapshot()); n != 0 {
		t.Fatalf("made %d requests, want 0", n)
	}
}

const ongoingHTML = `
<div class="cc-entries">
  <article class="b-catalog_entry c-anime" id="5114">
    <a class="cover" href="/animes/5114-fullmetal-alchemist">
      <picture><img src="/system/animes/preview/5114.jpg" srcset="/system/animes/x96/5114.jpg 96w, /system/animes/original/5114.jpg 400w"></picture>
      <div class="title"><span class="name-en">Fullmetal Alchemist</span><span class="name-ru">Стальной алхимик</span></div>
    </a>
    <div class="misc"><span>ТВ</span><span>2003</span></div>
  </article>
  <article class="b-catalog_entry c-anime" id="21">
    <a class="cover" href="https://shikimori.one/animes/21-one-piece">
      <picture><img src="/system/animes/preview/21.jpg"></picture>
      <div class="title"><span class="name-en">One Piece</span></div>
    </a>
  </article>
  <article class="b-catalog_entry c-anime" id="5114">
    <div class="title"><span class="name-ru">Дубль</span></div>
  </article>
  <article class="b-catalog_entry c-manga" id="999">
    <div class="title"><span class="name-en">Manga</span></div>
  </article>
</div>
`

func TestFetchOngoingCandidates(t *testing.T) {
	c, log := newTestClient(t, configEnabled(), func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/animes/status/ongoing" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(ongoingHTML))
	})

	items, err := c.FetchOngoingCandidates(context.Background())
	if err != nil {
		t.Fatalf("FetchOngoingCandidates: %v", err)
	}
	// Dedup by id: 5114 twice -> once; manga entry filtered by selector.
	if len(items) != 2 {
		t.Fatalf("got %d candidates, want 2", len(items))
	}

	first := items[0]
	if first.ShikimoriID != 5114 {
		t.Fatalf("first id = %d", first.ShikimoriID)
	}
	if first.TitleEn == nil || *first.TitleEn != "Fullmetal Alchemist" {
		t.Fatalf("title_en = %v", first.TitleEn)
	}
	if first.TitleRu == nil || *first.TitleRu != "Стальной алхимик" {
		t.Fatalf("title_ru = %v", first.TitleRu)
	}
	if first.PosterURL == "" || !strings.HasSuffix(first.PosterURL, "/system/animes/original/5114.jpg") {
		t.Fatalf("poster (srcset preferred) = %q", first.PosterURL)
	}
	if !strings.HasPrefix(first.PosterURL, "http") {
		t.Fatalf("poster must resolve absolute, got %q", first.PosterURL)
	}
	if first.Year == nil || *first.Year != 2003 {
		t.Fatalf("year (last .misc span) = %v, want 2003", first.Year)
	}
	if first.SourceURL == "" || !strings.HasSuffix(first.SourceURL, "/animes/5114-fullmetal-alchemist") {
		t.Fatalf("source_url (cover href) = %q", first.SourceURL)
	}

	second := items[1]
	if second.ShikimoriID != 21 {
		t.Fatalf("second id = %d", second.ShikimoriID)
	}
	if second.TitleRu != nil {
		t.Fatalf("absent name-ru must stay nil, got %v", *second.TitleRu)
	}
	if second.Year != nil {
		t.Fatalf("no .misc -> nil year, got %v", *second.Year)
	}
	if n := log.count("/animes/status/ongoing"); n != 1 {
		t.Fatalf("ongoing fetched %d times", n)
	}
}

func TestFetchOngoingCandidatesServerError(t *testing.T) {
	c, _ := newTestClient(t, configEnabled(), func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	items, err := c.FetchOngoingCandidates(context.Background())
	if err != nil {
		t.Fatalf("python parity: fetch failure yields empty list, got %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("want empty, got %d", len(items))
	}
}

// configEnabled returns an enabled, credential-free client config
// (public reads only).
func configEnabled() config.Shikimori {
	return config.Shikimori{Enabled: true}
}

// jsonInt parses a decimal string for test fixtures.
func jsonInt(t *testing.T, s string) int64 {
	t.Helper()
	var v int64
	if _, err := fmt.Sscanf(s, "%d", &v); err != nil {
		t.Fatalf("bad id fixture %q: %v", s, err)
	}
	return v
}
