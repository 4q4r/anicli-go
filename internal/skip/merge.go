package skip

import (
	"sort"
	"strconv"
)

// MergeByType smart-merges interval sets given in priority order (first
// set = highest priority provider).
//
// Rules (FEATURE_INVENTORY D ruling with the ML predictor removed):
//   - a skip type produced by several providers resolves to the single
//     entry of the highest-priority provider — the old API>ML authority
//     rule generalized to provider priority, whether the intervals
//     agree closely or conflict far apart;
//   - types the winner does not carry flow in from lower-priority
//     providers (python "only API exists" union branch);
//   - the merged set is ordered by start time.
//
// It returns the merged intervals and the indexes of contributing sets
// (rendered as "p0", "p1", ...).
func MergeByType(sets [][]Interval) ([]Interval, []string) {
	byType := make(map[string]Interval)
	order := make([]string, 0)
	contributed := make(map[int]struct{})

	for idx, set := range sets {
		for _, iv := range set {
			if _, exists := byType[iv.SkipType]; exists {
				// Same type already decided by a higher-priority
				// provider.
				continue
			}
			if iv.EndTime <= iv.StartTime {
				continue
			}
			byType[iv.SkipType] = iv
			order = append(order, iv.SkipType)
			contributed[idx] = struct{}{}
		}
	}

	merged := make([]Interval, 0, len(order))
	for _, skipType := range order {
		merged = append(merged, byType[skipType])
	}
	sort.SliceStable(merged, func(i, j int) bool {
		return merged[i].StartTime < merged[j].StartTime
	})

	contributors := make([]string, 0, len(contributed))
	for idx := range contributed {
		contributors = append(contributors, providerIndexLabel(idx))
	}
	sort.Strings(contributors)
	return merged, contributors
}

// providerIndexLabel renders a set index for contributor reporting.
func providerIndexLabel(idx int) string {
	return "p" + string(rune('0'+idx%10))
}

// parseProviderIndex decodes a providerIndexLabel back into its set
// index (the manager maps it through the per-consultation provider id
// list). The round trip is exact for the single-digit indexes produced
// here; malformed labels report false.
func parseProviderIndex(label string) (int, bool) {
	if len(label) != 2 || label[0] != 'p' {
		return 0, false
	}
	idx, err := strconv.Atoi(label[1:])
	return idx, err == nil
}
