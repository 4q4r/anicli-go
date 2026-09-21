package tui

import (
	tea "charm.land/bubbletea/v2"
	"fmt"
	"strconv"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// PR81 offline benchmarks for the TUI hot paths: the multi-provider
// episode merge (the One Piece 1178 case), the type-to-search filter
// (per-keystroke cost), the list/checklist renders at realistic scales
// and the search fan-out settle build. All inputs are prebuilt before
// `for b.Loop()`; results sink into package-level vars.

// benchEpisodeSources builds the multi-dub episode rosters merged on
// the session screen: source `total` carries every episode, the other
// four carry overlapping subsets (realistic dub coverage), all with
// provider-namespaced embeds.
func benchEpisodeSources(total int) []SourceEpisodes {
	mk := func(sourceID string, from, step int) []contracts.Episode {
		eps := make([]contracts.Episode, 0, total/step+1)
		for n := from; n <= total; n += step {
			eps = append(eps, contracts.Episode{
				Num:       strconv.Itoa(n),
				Title:     fmt.Sprintf("Серия %d", n),
				RawID:     fmt.Sprintf("ep-%d", n),
				RawEmbeds: map[string][]string{"1080": {"https://" + sourceID + ".example/" + strconv.Itoa(n)}},
			})
		}
		return eps
	}
	return []SourceEpisodes{
		{SourceID: "anilib", Episodes: mk("anilib", 1, 1)},
		{SourceID: "animego", Episodes: mk("animego", 1, 2)},
		{SourceID: "anizone", Episodes: mk("anizone", 1, 3)},
		{SourceID: "shiza", Episodes: mk("shiza", 1, 4)},
		{SourceID: "yummy", Episodes: mk("yummy", 1, 5)},
	}
}

var (
	benchSinkMerged     map[string]contracts.Episode
	benchSinkOrder      []string
	benchSinkDubStats   map[string]int
	benchSinkChoices    []Choice
	benchSinkRender     string
	benchSinkGroups     [][]contracts.SearchResult
	benchSinkCheckList  *CheckList
	benchSinkSearchView string
)

// BenchmarkMergeEpisodeLists5x1178 merges five dub rosters over the
// One Piece 1178-episode scale (per-op: the whole merge+sort).
func BenchmarkMergeEpisodeLists5x1178(b *testing.B) {
	b.ReportAllocs()
	sources := benchEpisodeSources(1178)
	for b.Loop() {
		merged, order := MergeEpisodeLists(sources)
		benchSinkMerged = merged
		benchSinkOrder = order
	}
}

// BenchmarkMergeEpisodeLists5x600 merges five rosters over a
// mid-scale long-running title (600 episodes).
func BenchmarkMergeEpisodeLists5x600(b *testing.B) {
	b.ReportAllocs()
	sources := benchEpisodeSources(600)
	for b.Loop() {
		merged, order := MergeEpisodeLists(sources)
		benchSinkMerged = merged
		benchSinkOrder = order
	}
}

// BenchmarkDubStats1178 counts dub keys across a 1178-episode roster
// (the dub-picker badges).
func BenchmarkDubStats1178(b *testing.B) {
	b.ReportAllocs()
	_, order := MergeEpisodeLists(benchEpisodeSources(1178))
	_ = order
	merged, _ := MergeEpisodeLists(benchEpisodeSources(1178))
	eps := make([]contracts.Episode, 0, len(order))
	for _, num := range order {
		eps = append(eps, merged[num])
	}
	for b.Loop() {
		benchSinkDubStats = DubStats(eps)
	}
}

// BenchmarkFindBestSimilarGroup matches a canonical title against 64
// result groups (the history-rehydration path).
func BenchmarkFindBestSimilarGroup64(b *testing.B) {
	b.ReportAllocs()
	groups := make([][]contracts.SearchResult, 0, 64)
	for i := range 64 {
		groups = append(groups, []contracts.SearchResult{
			{Title: fmt.Sprintf("Ван Пис %d", i), SourceID: "p", URL: "u"},
		})
	}
	for b.Loop() {
		benchSinkGroups = nil
		g := FindBestSimilarGroup(groups, "ван пис", 0.4)
		if g != nil {
			benchSinkGroups = groups
		}
	}
}

// benchChoices builds n realistic list rows (provider-result checklist
// labels: «provider — title [suffix]»).
func benchChoices(n int) []Choice {
	out := make([]Choice, 0, n)
	for i := range n {
		out = append(out, Choice{
			ID:    "r" + strconv.Itoa(i),
			Label: fmt.Sprintf("AniLibriaTV — One Piece Wan Pisu %d [1080p]", i),
			Value: contracts.SearchResult{Title: fmt.Sprintf("One Piece %d", i)},
		})
	}
	return out
}

// BenchmarkFilterChoices64 measures one filter keystroke over a
// 64-item list (substring scan + lowercasing).
func BenchmarkFilterChoices64(b *testing.B) {
	b.ReportAllocs()
	items := benchChoices(64)
	for b.Loop() {
		benchSinkChoices = filterChoices(items, "one piece 1")
	}
}

// BenchmarkFilterChoices1178 — one keystroke over the 1178-item list.
func BenchmarkFilterChoices1178(b *testing.B) {
	b.ReportAllocs()
	items := benchChoices(1178)
	for b.Loop() {
		benchSinkChoices = filterChoices(items, "one piece 1")
	}
}

// BenchmarkPinListRender64 renders the episode-list viewport (64 rows
// behind the window) — the per-frame cost of the session list.
func BenchmarkPinListRender64(b *testing.B) {
	b.ReportAllocs()
	list := NewPinList(NewMenu("Выберите серию:", "Нет серий", benchChoices(64)...), defaultListHeight)
	for b.Loop() {
		benchSinkRender = list.Render()
	}
}

// BenchmarkPinListRender1178 — same render over 1178 rows: the
// viewport must keep this bounded (regression guard for the PR78/79
// scrolling fixes).
func BenchmarkPinListRender1178(b *testing.B) {
	b.ReportAllocs()
	list := NewPinList(NewMenu("Выберите серию:", "Нет серий", benchChoices(1178)...), defaultListHeight)
	for b.Loop() {
		benchSinkRender = list.Render()
	}
}

// benchCheckList builds the provider-results checklist at scale n.
func benchCheckList(b *testing.B, n int) *CheckList {
	b.Helper()
	return NewCheckList("Выберите провайдеры:", benchChoices(n))
}

// BenchmarkCheckListApplyFilter1178 — one filter keystroke over a
// 1178-item checklist (the PR78 type-to-search rebuild path).
func BenchmarkCheckListApplyFilter1178(b *testing.B) {
	b.ReportAllocs()
	items := benchChoices(1178)
	key := tea.KeyPressMsg{Code: 'o'}
	for b.Loop() {
		fresh := NewCheckList("Выберите провайдеры:", items)
		fresh.filter.consume(key, checklistBoundRunes)
		fresh.applyFilter()
		benchSinkCheckList = fresh
	}
}

// BenchmarkCheckListRender64 renders the multi-select checklist over
// 64 items.
func BenchmarkCheckListRender64(b *testing.B) {
	b.ReportAllocs()
	cl := benchCheckList(b, 64)
	for b.Loop() {
		benchSinkRender = cl.Render()
	}
}

// BenchmarkCheckListRender1178 — checklist over 1178 items.
func BenchmarkCheckListRender1178(b *testing.B) {
	b.ReportAllocs()
	cl := benchCheckList(b, 1178)
	for b.Loop() {
		benchSinkRender = cl.Render()
	}
}

// benchSearchResults builds n realistic fan-out results (title+source
// dedupe keys).
func benchSearchResults(n int) []contracts.SearchResult {
	out := make([]contracts.SearchResult, 0, n)
	for i := range n {
		out = append(out, contracts.SearchResult{
			Title:    fmt.Sprintf("One Piece Wan Pisu %d", i),
			URL:      "https://prov.example/anime/" + strconv.Itoa(i),
			SourceID: "prov" + strconv.Itoa(i%23),
			Poster:   "https://prov.example/p/" + strconv.Itoa(i) + ".jpg",
		})
	}
	return out
}

// BenchmarkSettleResults1000 builds the settled results checklist from
// 1000 fan-out results (the dedupe + label build after a search).
func BenchmarkSettleResults1000(b *testing.B) {
	b.ReportAllocs()
	results := benchSearchResults(1000)
	rows := make([]ProviderMeta, 0, 23)
	for i := range 23 {
		rows = append(rows, ProviderMeta{ID: "prov" + strconv.Itoa(i), Name: "Провайдер " + strconv.Itoa(i)})
	}
	for b.Loop() {
		m := &searchProgress{deps: nil, rows: rows, results: results}
		m.settleResults()
		benchSinkCheckList = m.resultCheck
		if m.resultCheck == nil {
			b.Fatal("settleResults built no checklist")
		}
	}
}

// BenchmarkSearchProgressView renders the settled search screen (live
// table + results checklist) at the 1000-result scale.
func BenchmarkSearchProgressView(b *testing.B) {
	b.ReportAllocs()
	results := benchSearchResults(1000)
	rows := make([]ProviderMeta, 0, 23)
	status := make(map[string]string, 23)
	counts := make(map[string]int, 23)
	responded := make(map[string]bool, 23)
	for i := range 23 {
		id := "prov" + strconv.Itoa(i)
		rows = append(rows, ProviderMeta{ID: id, Name: "Провайдер " + strconv.Itoa(i)})
		status[id] = "готово"
		counts[id] = 1000 / 23
		responded[id] = true
	}
	m := &searchProgress{deps: nil, rows: rows, status: status, counts: counts, responded: responded, results: results}
	m.settleResults()
	if m.resultCheck == nil {
		b.Fatal("settleResults built no checklist")
	}
	for b.Loop() {
		view := m.View()
		benchSinkSearchView = view.Content
	}
}
