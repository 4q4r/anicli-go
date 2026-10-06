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
// PR126: animeheaven; PR127: anikoto; PR128: gogoanime;
// PR129: kickassanime; PR130: anizone; PR131: sameband; PR132:
// anidub; PR133: anikado; PR134: animiku; PR135: anifilm;
// PR137: animemobi; PR138: anistar; PR139: anipub; PR140: kodik).
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
	for _, want := range []string{"anidub", "anilib", "anilibria", "animeheaven", "anikado", "anifilm", "animemobi", "animiku", "anipub", "anistar", "anitokyo", "animedia", "animevib", "animevost", "animego", "anikoto", "gogoanime", "kickassanime", "kodik", "anizone", "sameband", "shiza", "yummy"} {
		if !seen[want] {
			t.Errorf("bundled scripts missing the migrated provider %q (have %v)", want, ids)
		}
	}
}
