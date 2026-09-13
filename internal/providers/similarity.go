package providers

// SimilarityRatio exposes the CPython SequenceMatcher(None, a, b)
// ratio ported for allanime search sorting (see allanime_match.go for
// provenance and goldens). The TUI reuses it for history rehydration
// matching (python _find_best_similar_group difflib usage).
func SimilarityRatio(a, b string) float64 {
	return allanimeSimilarity(a, b)
}
