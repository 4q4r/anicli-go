package luaproviders

import (
	"sort"
	"testing"
)

// TestSourcesShape pins the embed contract: every entry carries the
// "bundled" origin label, unique ids, sorted deterministically (the
// load-precedence order's bundled segment). The migrated roster ids
// pin as they land (PR116 wave: anitokyo, animedia, animevib;
// PR120: anilibria; PR122: anilib; PR125: shiza (which also completed
// the animevost/yummy entries the earlier migrations had missed);
// PR126: animeheaven).
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
	for _, want := range []string{"anilib", "anilibria", "animeheaven", "anitokyo", "animedia", "animevib", "animevost", "animego", "shiza", "yummy"} {
		if !seen[want] {
			t.Errorf("bundled scripts missing the migrated provider %q (have %v)", want, ids)
		}
	}
}
