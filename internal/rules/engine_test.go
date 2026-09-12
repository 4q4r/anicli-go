package rules

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// anilibriaSearchFixture mirrors the aniliberty.top /api/v1 search style
// (hand-crafted, provider-shape-like; real parity fixtures ship with each
// provider PR at G3).
const anilibriaSearchFixture = `{
  "data": [
    {"id": 123, "names": {"ru": "Клинок, рассекающий демонов", "en": "Demon Slayer"}, "code": "demon-slayer"},
    {"id": 456, "names": {"ru": "Твоё имя", "en": "Your Name"}, "code": "tvoe-imya"},
    {"id": 789, "names": {"en": "No Russian title"}, "code": "no-ru-title"},
    {"id": 111, "names": {"ru": "Без кода"}, "code": ""}
  ]
}`

// anilibria search rules in the declarative shape of anicli-py sources.toml:
// search_path=data, search_title=names.ru, search_link=code with
// link_prefix/link_postfix assembling the release URL.
func anilibriaSearchRules() []Rule {
	return []Rule{
		{Path: "data", Multiple: true},
		{Path: "names.ru", Attr: AttrTitle},
		{Path: "code", Attr: AttrURL, Prefix: "https://anilibria.top/release/", Postfix: ".html"},
	}
}

// anilibEpisodesFixture mirrors the api.cdnlibs.org episodes style.
const anilibEpisodesFixture = `{
  "data": [
    {"number": 3, "html_video": "//player.anilib.me/video/3"},
    {"number": 1, "html_video": "https://player.anilib.me/video/1"},
    {"number": 2, "html_video": "//player.anilib.me/video/2"}
  ]
}`

// anilib episode rules: episode_context_path=data, episode_num=number,
// episode_link=html_video with python's protocol-relative URL fix.
func anilibEpisodeRules() []Rule {
	return []Rule{
		{Path: "data", Multiple: true},
		{Path: "number", Attr: AttrNum},
		{Path: "html_video", Attr: AttrURL, Transform: TransformProtocolRelative},
	}
}

func mustJSON(t *testing.T, raw string) any {
	t.Helper()
	var data any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	return data
}

func TestExtractSearchGoldenAnilibria(t *testing.T) {
	t.Parallel()

	got, err := ExtractSearch(mustJSON(t, anilibriaSearchFixture), anilibriaSearchRules(), "anilibria")
	if err != nil {
		t.Fatalf("ExtractSearch: %v", err)
	}

	// Items 3 and 4 are dropped: python requires both title and link
	// truthy (`if title and link_val`).
	want := []contracts.SearchResult{
		{
			Title:    "Клинок, рассекающий демонов",
			URL:      "https://anilibria.top/release/demon-slayer.html",
			SourceID: "anilibria",
		},
		{
			Title:    "Твоё имя",
			URL:      "https://anilibria.top/release/tvoe-imya.html",
			SourceID: "anilibria",
		},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d results, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Title != want[i].Title || got[i].URL != want[i].URL || got[i].SourceID != want[i].SourceID {
			t.Errorf("result[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestExtractEpisodesGoldenAnilib(t *testing.T) {
	t.Parallel()

	got, err := ExtractEpisodes(mustJSON(t, anilibEpisodesFixture), anilibEpisodeRules())
	if err != nil {
		t.Fatalf("ExtractEpisodes: %v", err)
	}

	// Numeric episode numbers sort ascending (python episodes.sort); the
	// two protocol-relative links get the https: prefix.
	want := []contracts.Episode{
		{Num: "1", RawID: "https://player.anilib.me/video/1"},
		{Num: "2", RawID: "https://player.anilib.me/video/2"},
		{Num: "3", RawID: "https://player.anilib.me/video/3"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d episodes, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Num != want[i].Num || got[i].RawID != want[i].RawID {
			t.Errorf("episode[%d] = {num %q raw_id %q}, want {num %q raw_id %q}",
				i, got[i].Num, got[i].RawID, want[i].Num, want[i].RawID)
		}
	}
}

func TestExtractEpisodesOrderPreservedWhenNonNumeric(t *testing.T) {
	t.Parallel()

	// python: episodes.sort(key=float) inside contextlib.suppress(ValueError)
	// — one unparseable number cancels the whole sort.
	fixture := `{"data": [
        {"number": 3,  "html_video": "https://p.example/3"},
        {"number": "OVA", "html_video": "https://p.example/ova"},
        {"number": 1,  "html_video": "https://p.example/1"}
    ]}`
	got, err := ExtractEpisodes(mustJSON(t, fixture), anilibEpisodeRules())
	if err != nil {
		t.Fatalf("ExtractEpisodes: %v", err)
	}
	wantNums := []string{"3", "OVA", "1"}
	for i, n := range wantNums {
		if got[i].Num != n {
			t.Errorf("episode[%d].Num = %q, want %q (order must be preserved)", i, got[i].Num, n)
		}
	}
}

func TestExtractNoContextRule(t *testing.T) {
	t.Parallel()

	rules := []Rule{{Path: "title", Attr: AttrTitle}} // no Multiple rule
	if _, err := Extract(mustJSON(t, `{}`), rules); err == nil {
		t.Fatal("rules without a multiple (context) rule must fail loud")
	}
}

func TestExtractInvalidPathFailsWithContext(t *testing.T) {
	t.Parallel()

	rules := []Rule{
		{Path: "items[*.title", Attr: AttrTitle}, // invalid jmespath
		{Path: "items", Multiple: true},
	}
	_, err := Extract(mustJSON(t, `{"items": []}`), rules)
	if err == nil {
		t.Fatal("invalid jmespath must fail loud")
	}
	if want := "items[*.title"; !strings.Contains(err.Error(), want) {
		t.Errorf("error must name the offending path %q, got: %v", want, err)
	}
}

func TestExtractUnknownAttrFails(t *testing.T) {
	t.Parallel()

	rules := []Rule{
		{Path: "items", Multiple: true},
		{Path: "title", Attr: Attr("poster")},
	}
	if _, err := Extract(mustJSON(t, `{"items": []}`), rules); err == nil {
		t.Fatal("unknown attr must fail loud (config typo protection)")
	}
}

func TestExtractNoMatchYieldsEmpty(t *testing.T) {
	t.Parallel()

	// python: `jmespath.search(...) or []` — a path that matches nothing
	// is an empty result, not an error.
	got, err := Extract(mustJSON(t, `{"other": 1}`), anilibriaSearchRules())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("no-match path must yield empty results, got %+v", got)
	}
}

func TestExtractStringification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		json string
		path string
		want string
	}{
		{name: "string passthrough", json: `{"v": "abc"}`, path: "v", want: "abc"},
		{name: "integral number renders without decimal", json: `{"v": 5}`, path: "v", want: "5"},
		{name: "fractional number keeps decimals", json: `{"v": 2.5}`, path: "v", want: "2.5"},
		{name: "python bool capitalization", json: `{"v": true}`, path: "v", want: "True"},
		{name: "null renders empty", json: `{"v": null}`, path: "v", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rules := []Rule{
				{Path: "items", Multiple: true},
				{Path: tt.path, Attr: AttrTitle},
			}
			got, err := Extract(mustJSON(t, `{"items": [`+tt.json+`]}`), rules)
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if len(got) != 1 || got[0].Title != tt.want {
				t.Errorf("stringify = %+v, want title %q", got, tt.want)
			}
		})
	}
}

// countCachePrefix counts cached expressions with the given path prefix.
// The prefix is unique to one test, making the count immune to parallel
// siblings sharing the package-level cache.
func countCachePrefix(prefix string) int {
	n := 0
	compiled.Range(func(k, _ any) bool {
		if strings.HasPrefix(k.(string), prefix) {
			n++
		}
		return true
	})
	return n
}

func TestCompileCache(t *testing.T) {
	t.Parallel()

	rules := []Rule{
		{Path: "cache_test_items", Multiple: true},
		{Path: "cache_test_title", Attr: AttrTitle},
		{Path: "cache_test_code", Attr: AttrURL},
	}
	data := mustJSON(t, `{"cache_test_items": [{"cache_test_title": "t", "cache_test_code": "c"}]}`)

	for range 3 {
		if _, err := Extract(data, rules); err != nil {
			t.Fatalf("Extract: %v", err)
		}
	}
	// Three runs over the same three distinct paths must compile each
	// expression exactly once.
	if got := countCachePrefix("cache_test_"); got != 3 {
		t.Errorf("cached expressions for unique prefix = %d after repeated extracts, want 3", got)
	}
}

func TestExtractConcurrent(t *testing.T) {
	data := mustJSON(t, anilibriaSearchFixture)

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ExtractSearch(data, anilibriaSearchRules(), "anilibria"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestExtractSearchWrapperFillsSourceID(t *testing.T) {
	t.Parallel()

	got, err := ExtractSearch(mustJSON(t, `{"data": [{"t": "x", "c": "y"}]}`), []Rule{
		{Path: "data", Multiple: true},
		{Path: "t", Attr: AttrTitle},
		{Path: "c", Attr: AttrURL},
	}, "anilib")
	if err != nil {
		t.Fatalf("ExtractSearch: %v", err)
	}
	if len(got) != 1 || got[0].SourceID != "anilib" {
		t.Fatalf("SourceID must be filled by the wrapper, got %+v", got)
	}
}

func TestExtractErrorUnwrap(t *testing.T) {
	t.Parallel()

	rules := []Rule{
		{Path: "items", Multiple: true},
		{Path: "@@@invalid", Attr: AttrTitle},
	}
	_, err := Extract(mustJSON(t, `{"items": [{}]}`), rules)
	if err == nil {
		t.Fatal("invalid path must error")
	}
	var ruleErr *RuleError
	if !errors.As(err, &ruleErr) {
		t.Fatalf("error must be (or wrap) *RuleError for programmatic context, got %T: %v", err, err)
	}
	if ruleErr.Path != "@@@invalid" {
		t.Errorf("RuleError.Path = %q, want %q", ruleErr.Path, "@@@invalid")
	}
}
