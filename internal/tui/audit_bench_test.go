package tui

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// PR81 audit benchmarks: quantifies the complexity findings from the
// algorithmic sweep (grouping/sorting/joining hot paths) so each has a
// measured before/after baseline.

// benchTitles builds n distinct realistic result titles.
func benchTitles(n int) []contracts.SearchResult {
	out := make([]contracts.SearchResult, 0, n)
	for i := range n {
		out = append(out, contracts.SearchResult{
			Title:    fmt.Sprintf("Ван Пис Wan Pisu серия %d [AniLibria]", i),
			SourceID: "prov",
			URL:      "https://prov.example/" + strconv.Itoa(i),
		})
	}
	return out
}

var benchSinkGrouped [][]contracts.SearchResult

// BenchmarkGroupByTitle30 — the rebind-flow grouper at a realistic
// roster size (30 results).
func BenchmarkGroupByTitle30(b *testing.B) {
	b.ReportAllocs()
	results := benchTitles(30)
	for b.Loop() {
		benchSinkGrouped = GroupByTitle(results, 0.6)
	}
}

// BenchmarkGroupByTitle100 — 100 results: the quadratic pairwise
// similarity cost becomes visible here.
func BenchmarkGroupByTitle100(b *testing.B) {
	b.ReportAllocs()
	results := benchTitles(100)
	for b.Loop() {
		benchSinkGrouped = GroupByTitle(results, 0.6)
	}
}

// BenchmarkGroupByTitleDuplicates100 — the REALISTIC rebind shape: the
// same title re-emitted by 4 providers. The exact-match fast path (PR82
// P1#2) collapses these pairs before SimilarityRatio runs.
func BenchmarkGroupByTitleDuplicates100(b *testing.B) {
	b.ReportAllocs()
	results := make([]contracts.SearchResult, 0, 100)
	for i := range 25 {
		for _, prov := range []string{"animego", "anilib", "shiza", "kodik"} {
			results = append(results, contracts.SearchResult{
				Title:    fmt.Sprintf("One Piece Wan Pisu %d", i),
				SourceID: prov,
				URL:      fmt.Sprintf("%s/%d", prov, i),
			})
		}
	}
	for b.Loop() {
		benchSinkGrouped = GroupByTitle(results, 0.6)
	}
}

// BenchmarkDownloadRangeJoin1178 mirrors handleDownloadRangeKey's
// nested loop VERBATIM (session.go:1917-1925 — no break: the inner
// scan runs to the end of the order slice on every num, faithful to
// production): ParseRange("1-1178") (1178 nums) joined against a
// 1178-entry episode order by repeated linear scan (the One Piece
// batch-download case).
func BenchmarkDownloadRangeJoin1178(b *testing.B) {
	b.ReportAllocs()
	order := make([]string, 0, 1178)
	for n := 1; n <= 1178; n++ {
		order = append(order, strconv.Itoa(n))
	}
	nums := ParseRange("1-1178")
	for b.Loop() {
		downloadEpisodes := make([]string, 0, len(nums))
		for _, n := range nums {
			label := strconv.Itoa(n)
			for _, o := range order {
				if o == label {
					downloadEpisodes = append(downloadEpisodes, label)
				}
			}
		}
		benchSinkJoined = downloadEpisodes
		if len(benchSinkJoined) != 1178 {
			b.Fatal("join lost episodes")
		}
	}
}

var benchSinkJoined []string
