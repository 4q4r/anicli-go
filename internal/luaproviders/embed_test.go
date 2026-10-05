package luaproviders

import (
	"sort"
	"testing"
)

// TestSourcesShape pins the embed contract: every entry carries the
// "bundled" origin label, unique ids, sorted deterministically (the
// load-precedence order's bundled segment). The migrated roster ids
// pin as they land (PR116 wave: anitokyo, animedia, animevib).
func TestSourcesShape(t *testing.T) {
	srcs := Sources()
	seen := map[string]bool{}
	var ids []string
	for _, s := range srcs {
		if s.Dir != "bundled" {
			t.Errorf("source %q: Dir = %q, want bundled", s.ID, s.Dir)
		}
		if s.Src == "" {
			t.Errorf("source %q: empty script", s.ID)
		}
		if seen[s.ID] {
			t.Errorf("duplicate embedded id %q", s.ID)
		}
		seen[s.ID] = true
		ids = append(ids, s.ID)
	}
	if !sort.SliceIsSorted(ids, func(i, j int) bool { return ids[i] < ids[j] }) {
		t.Errorf("sources not sorted: %v", ids)
	}
	for _, want := range []string{"anitokyo", "animedia", "animevib"} {
		if !seen[want] {
			t.Errorf("bundled scripts missing the migrated provider %q (have %v)", want, ids)
		}
	}
}
