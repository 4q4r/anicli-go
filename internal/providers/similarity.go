package providers

// SimilarityRatio exposes the CPython SequenceMatcher(None, a, b)
// ratio port used for history-rehydration matching (the python
// _find_best_similar_group difflib usage); the port itself and its
// goldens live in sequencematch.go.
func SimilarityRatio(a, b string) float64 {
	return sequenceMatcherRatio(a, b)
}
