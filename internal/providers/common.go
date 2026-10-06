package providers

import (
	"sort"
)

// keys lists a string-map's keys sorted (test diagnostics).
func keys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
