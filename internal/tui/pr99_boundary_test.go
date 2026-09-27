// PR99 owner defect: the boundary episode actions never hide. After
// watching ALL episodes the session sits on the LAST one and
// «⏭ Next» is still offered (its pick no-oped since PR63's clamp);
// on ep 1 «⏮ Prev» is offered the same way. The owner's intent:
// the items must be EXCLUDED from the menu when no target episode
// exists — first episode hides «Пред.», last hides «След.», a
// single-episode title hides both.

package tui

import (
	"fmt"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// multiEpisodeSet builds one provider's episode list: nums 1..n, each
// carrying one eager dub so the menu fixtures match the real merge.
func multiEpisodeSet(n int) map[string][]contracts.Episode {
	eps := make([]contracts.Episode, 0, n)
	for i := 1; i <= n; i++ {
		num := fmt.Sprintf("%d", i)
		eps = append(eps, contracts.Episode{
			Num:       num,
			RawID:     "p" + num,
			RawEmbeds: map[string][]string{"Дубль 1": {"u" + num + "v"}},
		})
	}
	return map[string][]contracts.Episode{"animego": eps}
}

// newBoundedSession builds a session over an n-episode single-provider
// title with the merge finalized synchronously.
func newBoundedSession(t *testing.T, n int) *sessionScreen {
	t.Helper()
	deps := &Deps{Episode: &fakeEpisode{episodes: multiEpisodeSet(n)}}
	group := []contracts.SearchResult{{Title: "Тайтл", URL: "u1", SourceID: "animego"}}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()
	return s
}

// TestPR99FirstEpisodeHidesPrev: on ep 1 «⏮ Prev» must be EXCLUDED
// from the action menu (hidden, not dimmed) while «⏭ Next» stays.
func TestPR99FirstEpisodeHidesPrev(t *testing.T) {
	s := newBoundedSession(t, 3) // currentIdx=0 → ep 1
	v := s.View().Content
	if contains(v, "⏮ Prev") {
		t.Fatalf("«Пред.» must be hidden on the first episode:\n%s", v)
	}
	if !contains(v, "⏭ Next") {
		t.Fatalf("«След.» must stay on the first episode:\n%s", v)
	}
	if sessionActionIndex(s, "prev") != -1 {
		t.Fatalf("«Пред.» must be excluded, not disabled (owner intent)")
	}
}

// TestPR99LastEpisodeHidesNext: on the last episode «⏭ Next» must be
// EXCLUDED from the action menu while «⏮ Prev» stays.
func TestPR99LastEpisodeHidesNext(t *testing.T) {
	s := newBoundedSession(t, 3)
	s.jumpTo("3")
	v := s.View().Content
	if contains(v, "⏭ Next") {
		t.Fatalf("«След.» must be hidden on the last episode:\n%s", v)
	}
	if !contains(v, "⏮ Prev") {
		t.Fatalf("«Пред.» must stay on the last episode:\n%s", v)
	}
	if sessionActionIndex(s, "next") != -1 {
		t.Fatalf("«След.» must be excluded, not disabled (owner intent)")
	}
}

// TestPR99MiddleEpisodeKeepsBoth: between the boundaries both actions
// remain offered.
func TestPR99MiddleEpisodeKeepsBoth(t *testing.T) {
	s := newBoundedSession(t, 3)
	s.jumpTo("2")
	v := s.View().Content
	if !contains(v, "⏭ Next") || !contains(v, "⏮ Prev") {
		t.Fatalf("both boundary actions must stay mid-list:\n%s", v)
	}
}

// TestPR99SingleEpisodeHidesBoth: a single-episode title has no
// neighbor in either direction — both items are excluded.
func TestPR99SingleEpisodeHidesBoth(t *testing.T) {
	s := newBoundedSession(t, 1)
	v := s.View().Content
	if contains(v, "⏭ Next") || contains(v, "⏮ Prev") {
		t.Fatalf("single-episode title must hide both boundary actions:\n%s", v)
	}
	if sessionActionIndex(s, "next") != -1 || sessionActionIndex(s, "prev") != -1 {
		t.Fatalf("both boundary actions must be excluded on a single-episode title")
	}
}

// TestPR99WrapSanityAfterRemoval: with items removed the menu is a
// shorter list — PR83's wrap machinery must still hold: the pinned
// Back row stays last, down-wrap from it lands on item 0, up-wrap
// from item 0 lands on the pinned row, and every landed index
// resolves to a real action (never out of range).
func TestPR99WrapSanityAfterRemoval(t *testing.T) {
	s := newBoundedSession(t, 3)
	s.jumpTo("3")

	items := s.list.Menu().Items
	if last := items[len(items)-1]; last.ID != BackID {
		t.Fatalf("pinned last row = %q, want Back", last.ID)
	}
	wantRows := 9 // watch, prev, jump, redub, info, download, refresh, exit (8) + Back
	if got := len(items); got != wantRows {
		t.Fatalf("last-episode menu rows = %d, want %d", got, wantRows)
	}

	s.list.Jump(len(items) - 1) // park on the pinned Back row
	s.list.MoveDown()           // down-wrap → item 0
	if got := s.list.Cursor(); got != 0 {
		t.Fatalf("down-wrap from the pinned row must land on item 0, got %d", got)
	}
	if resolved := ResolveKey(s.list.Menu(), s.list.Cursor(), enter()); resolved != "watch" {
		t.Fatalf("wrapped cursor must resolve to a real action, got %#v", resolved)
	}

	s.list.MoveUp() // up-wrap from item 0 → pinned row
	if got := s.list.Cursor(); got != len(items)-1 {
		t.Fatalf("up-wrap from item 0 must land on the pinned row, got %d", got)
	}
	if resolved := ResolveKey(s.list.Menu(), s.list.Cursor(), enter()); resolved != Back {
		t.Fatalf("wrapped-up cursor must resolve to Back, got %#v", resolved)
	}
}

// TestPR99NavigationAcrossBoundaryRebuilds: stepping prev from the
// last episode rebuilds the menu so «След.» re-appears — the boundary
// follows the live position, not the construction-time one. A second
// prev step crosses into ep 1, where «Пред.» hides again.
func TestPR99NavigationAcrossBoundaryRebuilds(t *testing.T) {
	s := newBoundedSession(t, 3)
	s.jumpTo("3")
	if sessionActionIndex(s, "next") != -1 {
		t.Fatalf("precondition: «След.» hidden on the last episode")
	}
	idx := sessionActionIndex(s, "prev")
	s.list.Jump(idx)
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	if ss.currentEpisode() != "2" {
		t.Fatalf("prev must step to ep 2, got %q", ss.currentEpisode())
	}
	if sessionActionIndex(ss, "next") == -1 {
		t.Fatalf("after stepping off the boundary «След.» must return:\n%s", ss.View().Content)
	}
	if sessionActionIndex(ss, "prev") == -1 {
		t.Fatalf("mid-list ep 2 must keep «Пред.»:\n%s", ss.View().Content)
	}

	// One more prev lands on ep 1: the lower boundary hides «Пред.».
	idx = sessionActionIndex(ss, "prev")
	ss.list.Jump(idx)
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)
	if ss.currentEpisode() != "1" {
		t.Fatalf("second prev must step to ep 1, got %q", ss.currentEpisode())
	}
	if sessionActionIndex(ss, "prev") != -1 {
		t.Fatalf("ep 1's menu must hide «Пред.» again:\n%s", ss.View().Content)
	}
	if sessionActionIndex(ss, "next") == -1 {
		t.Fatalf("ep 1's menu must keep «След.»:\n%s", ss.View().Content)
	}
}

// TestPR99Live13EpisodeRenders: the owner's 13-episode shape. Renders
// the action menu on the last (13), first (1) and a middle (7)
// episode; the renders are logged for the PR report.
func TestPR99Live13EpisodeRenders(t *testing.T) {
	s := newBoundedSession(t, 13)

	cases := []struct {
		ep     string
		noPrev bool
		noNext bool
	}{
		{"13", false, true},
		{"1", true, false},
		{"7", false, false},
	}
	for _, tc := range cases {
		t.Run("ep "+tc.ep, func(t *testing.T) {
			s.jumpTo(tc.ep)
			v := s.View().Content
			if tc.noNext && contains(v, "⏭ Next") {
				t.Fatalf("ep %s: «След.» must be hidden:\n%s", tc.ep, v)
			}
			if tc.noPrev && contains(v, "⏮ Prev") {
				t.Fatalf("ep %s: «Пред.» must be hidden:\n%s", tc.ep, v)
			}
			t.Logf("PR99 render @ ep %s:\n%s", tc.ep, v)
		})
	}
}
