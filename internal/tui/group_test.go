package tui

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/providers"
)

// TestMergeEpisodeLists: the session episode merge across providers
// (python session_loop merged_episodes_map port): first source wins
// the episode slot, later sources append embeds under prefixed keys
// and compose raw ids.
func TestMergeEpisodeLists(t *testing.T) {
	mk := func(embeds map[string][]string) contracts.Episode {
		return contracts.Episode{Num: "1", RawID: "go-1", RawEmbeds: embeds}
	}

	t.Run("single source keeps keys unprefixed inside its own scope", func(t *testing.T) {
		merged, order := MergeEpisodeLists([]SourceEpisodes{{
			SourceID: "animego",
			Episodes: []contracts.Episode{mk(map[string][]string{"studio A": {"u1"}})},
		}})
		if len(order) != 1 || order[0] != "1" {
			t.Fatalf("want episode 1, got %v", order)
		}
		ep := merged["1"]
		if ep.RawEmbeds["[animego] studio A"] == nil {
			t.Fatalf("embed key must carry the provider prefix, got %v", ep.RawEmbeds)
		}
		if ep.RawID != "animego:go-1" {
			t.Fatalf("raw id must be provider-scoped, got %q", ep.RawID)
		}
	})

	t.Run("two sources merge embeds and ids", func(t *testing.T) {
		merged, _ := MergeEpisodeLists([]SourceEpisodes{
			{SourceID: "animego", Episodes: []contracts.Episode{
				{Num: "1", RawID: "a1", RawEmbeds: map[string][]string{"dubA": {"ua"}}},
			}},
			{SourceID: "anilib", Episodes: []contracts.Episode{
				{Num: "1", RawID: "b1", RawEmbeds: map[string][]string{"dubB": {"ub"}}},
				{Num: "2", RawID: "b2", RawEmbeds: map[string][]string{"dubB": {"ub2"}}},
			}},
		})
		if len(merged) != 2 {
			t.Fatalf("want episodes 1 and 2, got %d", len(merged))
		}
		ep1 := merged["1"]
		if ep1.RawEmbeds["[animego] dubA"] == nil || ep1.RawEmbeds["[anilib] dubB"] == nil {
			t.Fatalf("episode 1 must merge both sources' embeds, got %v", ep1.RawEmbeds)
		}
		if ep1.RawID != "animego:a1|anilib:b1" {
			t.Fatalf("composed raw id wrong: %q", ep1.RawID)
		}
	})

	t.Run("episode numbers sort numerically with fractional and junk", func(t *testing.T) {
		merged, order := MergeEpisodeLists([]SourceEpisodes{{
			SourceID: "x",
			Episodes: []contracts.Episode{
				{Num: "10", RawID: "a", RawEmbeds: map[string][]string{}},
				{Num: "2.5", RawID: "b", RawEmbeds: map[string][]string{}},
				{Num: "2", RawID: "c", RawEmbeds: map[string][]string{}},
				{Num: "OVA", RawID: "d", RawEmbeds: map[string][]string{}},
			},
		}})
		if len(order) != 4 {
			t.Fatalf("want 4 episodes, got %d", len(merged))
		}
		// Python sorts junk labels by key 0.0, i.e. FIRST
		// (bug-compatible parity with session_loop).
		want := []string{"OVA", "2", "2.5", "10"}
		if !reflect.DeepEqual(order, want) {
			t.Fatalf("numeric sort with junk keyed 0 first: want %v, got %v", want, order)
		}
	})
}

// TestDubStats: embed-key occurrence counts across merged episodes.
func TestDubStats(t *testing.T) {
	episodes := []contracts.Episode{
		{Num: "1", RawEmbeds: map[string][]string{
			"[a] dub1": {"u"}, "[a] dub2": {"u"},
		}},
		{Num: "2", RawEmbeds: map[string][]string{
			"[a] dub1": {"u"}, "[b] dub3": {"u"},
		}},
	}
	stats := DubStats(episodes)
	if stats["[a] dub1"] != 2 || stats["[a] dub2"] != 1 || stats["[b] dub3"] != 1 {
		t.Fatalf("wrong dub stats: %v", stats)
	}
}

// TestRehydrateExact: strategy 1 of _rehydrate_group — the group
// containing the exact (source_id, url) record wins.
func TestRehydrateExact(t *testing.T) {
	groups := [][]contracts.SearchResult{
		{
			{Title: "Naruto", URL: "u1", SourceID: "animego"},
			{Title: "Наруто", URL: "u2", SourceID: "anilib"},
		},
		{
			{Title: "Bleach", URL: "u3", SourceID: "animego"},
		},
	}

	t.Run("exact hit", func(t *testing.T) {
		got := FindExactGroup(groups, "anilib", "u2")
		if len(got) != 2 || got[0].Title != "Naruto" {
			t.Fatalf("exact match must return the holding group, got %v", got)
		}
	})

	t.Run("no hit yields nil", func(t *testing.T) {
		if got := FindExactGroup(groups, "animego", "gone"); got != nil {
			t.Fatalf("absent record must yield nil, got %v", got)
		}
	})
}

// TestRehydrateBestSimilar: strategy 2 — the group whose longest title
// best matches the canonical (shikimori) title, above the threshold.
func TestRehydrateBestSimilar(t *testing.T) {
	groups := [][]contracts.SearchResult{
		{{Title: "Ван Панч: Ванпанчмен", URL: "u1", SourceID: "animego"}},
		{{Title: "Bleach", URL: "u2", SourceID: "animego"}},
	}

	t.Run("similar group wins", func(t *testing.T) {
		got := FindBestSimilarGroup(groups, "Ванпанчмен", 0.6)
		if got == nil || got[0].URL != "u1" {
			t.Fatalf("want the One Punch group, got %v", got)
		}
	})

	t.Run("below threshold yields nil", func(t *testing.T) {
		if got := FindBestSimilarGroup(groups, "Совсем другое аниме", 0.6); got != nil {
			t.Fatalf("weak similarity must yield nil, got %v", got)
		}
	})
}

// TestBestDisplayTitle: longest Cyrillic title wins, else longest.
func TestBestDisplayTitle(t *testing.T) {
	group := []contracts.SearchResult{
		{Title: "One Punch Man"},
		{Title: "Ванпанчмен"},
		{Title: "Ван Панч — самый сильный герой"},
	}
	if got := BestDisplayTitle(group); got != "Ван Панч — самый сильный герой" {
		t.Fatalf("want longest cyrillic, got %q", got)
	}
	latin := []contracts.SearchResult{
		{Title: "OPM"},
		{Title: "One Punch Man"},
	}
	if got := BestDisplayTitle(latin); got != "One Punch Man" {
		t.Fatalf("want longest latin fallback, got %q", got)
	}
}

// TestParseRange: download range parsing (python _parse_range port).
func TestParseRange(t *testing.T) {
	t.Run("mixed ranges and singles", func(t *testing.T) {
		got := ParseRange("1-3, 5, 10-11")
		want := []int{1, 2, 3, 5, 10, 11}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("want %v, got %v", want, got)
		}
	})

	t.Run("garbage and duplicates are dropped", func(t *testing.T) {
		got := ParseRange("abc, 2, 2, x-y, 4-4")
		want := []int{2, 4}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("want %v, got %v", want, got)
		}
	})

	t.Run("empty yields empty", func(t *testing.T) {
		if got := ParseRange(""); len(got) != 0 {
			t.Fatalf("empty input must yield empty, got %v", got)
		}
	})
}

// TestEpisodeSortKey: numeric parsing with junk fallback.
func TestEpisodeSortKey(t *testing.T) {
	if EpisodeSortKey("3") != 3 || EpisodeSortKey("2.5") != 2.5 || EpisodeSortKey("OVA") != 0 {
		t.Fatalf("sort keys wrong")
	}
}

// TestGroupByTitle: the deterministic similarity grouper used by the
// rebind flow clusters above the threshold and separates below it.
func TestGroupByTitle(t *testing.T) {
	results := []contracts.SearchResult{
		{Title: "Ванпанчмен", URL: "u1", SourceID: "animego"},
		{Title: "Ванпанчмен (TV)", URL: "u2", SourceID: "anilib"},
		{Title: "Bleach", URL: "u3", SourceID: "animego"},
	}

	t.Run("similar titles cluster", func(t *testing.T) {
		groups := GroupByTitle(results, 0.6)
		if len(groups) != 2 {
			t.Fatalf("want 2 groups (one punch cluster + bleach), got %d: %+v", len(groups), groups)
		}
		if len(groups[0]) != 2 || groups[0][0].URL != "u1" || groups[0][1].URL != "u2" {
			t.Fatalf("the one punch pair must cluster together, got %+v", groups[0])
		}
	})

	t.Run("high threshold isolates everything", func(t *testing.T) {
		groups := GroupByTitle(results, 0.99)
		if len(groups) != len(results) {
			t.Fatalf("threshold 0.99 must isolate each result, got %d groups", len(groups))
		}
	})
}

// TestGroupByTitleMatchesReference is the PR82 P1#2 behavior-identity
// proof: the optimized grouper must produce byte-identical grouping
// (membership + intra-group order) with the ORIGINAL pairwise
// implementation, over a deterministic multi-source corpus that
// includes normalization collisions (case pairs, shared prefixes,
// noise-word variants) at several thresholds.
func TestGroupByTitleMatchesReference(t *testing.T) {
	reference := func(results []contracts.SearchResult, threshold float64) [][]contracts.SearchResult {
		var groups [][]contracts.SearchResult
		for _, res := range results {
			placed := false
			for gi, g := range groups {
				if providers.SimilarityRatio(
					strings.ToLower(res.Title),
					strings.ToLower(g[0].Title)) > threshold {
					groups[gi] = append(groups[gi], res)
					placed = true
					break
				}
			}
			if !placed {
				groups = append(groups, []contracts.SearchResult{res})
			}
		}
		return groups
	}

	corpus := []contracts.SearchResult{
		{Title: "Ванпанчмен", SourceID: "animego", URL: "u0"},
		{Title: "ванпанчмен", SourceID: "anilib", URL: "u1"},     // case collision
		{Title: "ВАНПАНЧМЕН 2", SourceID: "shiza", URL: "u2"},    // case + sequel
		{Title: "Ванпанчмен (TV)", SourceID: "yummy", URL: "u3"}, // variant
		{Title: "One Piece Wan Pisu", SourceID: "animego", URL: "u4"},
		{Title: "one piece wan pisu tv", SourceID: "anilib", URL: "u5"},
		{Title: "One Piece — Wan Pisu 1178", SourceID: "kodik", URL: "u6"},
		{Title: "Bleach Sennen Kessen-hen", SourceID: "animego", URL: "u7"},
		{Title: "bleach sennen kessen hen", SourceID: "anidub", URL: "u8"},
		{Title: "Naruto", SourceID: "animego", URL: "u9"},
		{Title: "Naruto: Shippuuden", SourceID: "anilib", URL: "u10"},
		{Title: "", SourceID: "animego", URL: "u11"}, // empty-title edge
		{Title: "", SourceID: "anilib", URL: "u12"},  // empty-empty collision
	}
	// A longer tail so the quadratic path is exercised with real
	// near-duplicates: titles share a long prefix with varying seasons.
	for i := range 40 {
		corpus = append(corpus, contracts.SearchResult{
			Title:    fmt.Sprintf("Boku no Hero Academia Season %d", i%7),
			SourceID: []string{"animego", "anilib", "shiza"}[i%3],
			URL:      fmt.Sprintf("u%d", 13+i),
		})
	}

	for _, threshold := range []float64{0.3, 0.5, 0.6, 0.8, 0.99, 1.0, 1.5} {
		got := GroupByTitle(append([]contracts.SearchResult(nil), corpus...), threshold)
		want := reference(append([]contracts.SearchResult(nil), corpus...), threshold)
		if len(got) != len(want) {
			t.Fatalf("threshold %v: %d groups, want %d", threshold, len(got), len(want))
		}
		for gi := range want {
			if len(got[gi]) != len(want[gi]) {
				t.Fatalf("threshold %v group %d: %d items, want %d", threshold, gi, len(got[gi]), len(want[gi]))
			}
			for ii := range want[gi] {
				if got[gi][ii].URL != want[gi][ii].URL {
					t.Fatalf("threshold %v group %d item %d: %s, want %s (membership/order changed)",
						threshold, gi, ii, got[gi][ii].URL, want[gi][ii].URL)
				}
			}
		}
	}
}
