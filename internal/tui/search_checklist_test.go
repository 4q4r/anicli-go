package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// PR31: the settled search phase renders EVERY result as its own
// checklist row (python questionary.checkbox parity) — no similarity
// grouping, exact-title+provider dedup only, space/a/i toggles, enter
// resolves the selection into the flow.

// settleSearch feeds provider settlements directly until every row
// settles, so checklist tests need no commands or spinner plumbing.
func settleSearch(sp *searchProgress, settlements ...providerResultMsg) *searchProgress {
	for _, msg := range settlements {
		next, _ := sp.Update(msg)
		sp = next.(*searchProgress)
	}
	return sp
}

// checklistTestDeps builds a two-provider deps set for the checklist
// tests.
func checklistTestDeps() (*fakeSearch, *Deps) {
	fs := newFakeSearch()
	fs.providers = []ProviderMeta{
		{ID: "animego", Name: "AnimeGO"},
		{ID: "anilib", Name: "AniLib"},
	}
	return fs, &Deps{Search: fs}
}

// TestSearchSettledResultsNoGrouping (PR31): 12 results render as 12
// separate checklist rows — similar titles from different providers
// never share a row; only identical strings could, and they don't
// here.
func TestSearchSettledResultsNoGrouping(t *testing.T) {
	fs, deps := checklistTestDeps()
	titles := []string{
		"Наруто", "Наруто (TV)", "Наруто Shippuuden", "Naruto",
		"Наруто: Фильм", "Наруто 2",
	}
	animego := make([]contracts.SearchResult, 0, len(titles))
	anilib := make([]contracts.SearchResult, 0, len(titles))
	for i, title := range titles {
		animego = append(animego, contracts.SearchResult{Title: title, URL: fmt.Sprintf("a%d", i), SourceID: "animego"})
		anilib = append(anilib, contracts.SearchResult{Title: title, URL: fmt.Sprintf("l%d", i), SourceID: "anilib"})
	}

	sp := NewSearchProgress(deps, "наруто")
	sp = settleSearch(sp,
		providerResultMsg{provider: fs.providers[0], results: animego},
		providerResultMsg{provider: fs.providers[1], results: anilib})

	if sp.resultCheck == nil {
		t.Fatalf("settled search must build the provider checklist")
	}
	if got := len(sp.resultCheck.items); got != 12 {
		t.Fatalf("12 results must render as 12 rows (no grouping), got %d", got)
	}
	v := sp.View().Content
	for _, want := range []string{
		"Выберите провайдеры",
		"AnimeGO — Наруто",
		"AniLib — Наруто",
	} {
		if !contains(v, want) {
			t.Errorf("settled checklist missing %q, got:\n%s", want, v)
		}
	}
	// The same title from two providers stays two distinct rows.
	labels := make(map[string]int, len(sp.resultCheck.items))
	for _, item := range sp.resultCheck.items {
		labels[item.Label]++
	}
	if labels["AnimeGO — Наруто"] != 1 || labels["AniLib — Наруто"] != 1 {
		t.Fatalf("Naruto must stay a separate row per provider, labels: %v", labels)
	}
}

// TestSearchSettledExactDuplicateDedup (PR31): only results with an
// IDENTICAL title from the SAME provider collapse (keep first); the
// same title from a different provider keeps its own row.
func TestSearchSettledExactDuplicateDedup(t *testing.T) {
	fs, deps := checklistTestDeps()

	sp := NewSearchProgress(deps, "наруто")
	sp = settleSearch(sp,
		providerResultMsg{provider: fs.providers[0], results: []contracts.SearchResult{
			{Title: "Наруто", URL: "u1", SourceID: "animego"},
			{Title: "Наруто", URL: "u2", SourceID: "animego"},
		}},
		providerResultMsg{provider: fs.providers[1], results: []contracts.SearchResult{
			{Title: "Наруто", URL: "u3", SourceID: "anilib"},
		}})

	if sp.resultCheck == nil {
		t.Fatalf("settled search must build the provider checklist")
	}
	if got := len(sp.resultCheck.items); got != 2 {
		t.Fatalf("exact title+provider duplicates must dedup to 1 row (2 total here), got %d", got)
	}
	first := sp.resultCheck.items[0].Value.(contracts.SearchResult)
	if first.URL != "u1" {
		t.Fatalf("dedup must keep the FIRST duplicate, got %+v", first)
	}
}

// TestSearchSettledChecklistKeys (PR31): space toggles the row under
// the cursor, a toggles all/none, i inverts — driven through the
// search screen's key routing.
func TestSearchSettledChecklistKeys(t *testing.T) {
	fs, deps := checklistTestDeps()
	fs.providers = append(fs.providers, ProviderMeta{ID: "third", Name: "Third"})
	res := func(src string) []contracts.SearchResult {
		return []contracts.SearchResult{{Title: "Наруто", URL: "u-" + src, SourceID: src}}
	}

	sp := NewSearchProgress(deps, "наруто")
	sp = settleSearch(sp,
		providerResultMsg{provider: fs.providers[0], results: res("animego")},
		providerResultMsg{provider: fs.providers[1], results: res("anilib")},
		providerResultMsg{provider: fs.providers[2], results: res("third")})

	// All unchecked by default.
	for i := range sp.resultCheck.items {
		if sp.resultCheck.Checked(i) {
			t.Fatalf("rows must start unchecked, row %d checked", i)
		}
	}

	// space toggles the row under the cursor.
	next, _ := sp.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	sp = next.(*searchProgress)
	if !sp.resultCheck.Checked(0) {
		t.Fatalf("space must check the cursor row")
	}

	// a selects all, a again deselects all.
	next, _ = sp.Update(tea.KeyPressMsg{Code: 'a'})
	sp = next.(*searchProgress)
	if got := len(sp.resultCheck.CheckedItems()); got != 3 {
		t.Fatalf("a must select all, got %d", got)
	}
	next, _ = sp.Update(tea.KeyPressMsg{Code: 'a'})
	sp = next.(*searchProgress)
	if got := len(sp.resultCheck.CheckedItems()); got != 0 {
		t.Fatalf("a again must deselect all, got %d", got)
	}

	// i inverts (none -> all, then partial -> inverse).
	next, _ = sp.Update(tea.KeyPressMsg{Code: 'i'})
	sp = next.(*searchProgress)
	if got := len(sp.resultCheck.CheckedItems()); got != 3 {
		t.Fatalf("i from none must check all, got %d", got)
	}
	next, _ = sp.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	next, _ = next.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	sp = next.(*searchProgress)
	if got := len(sp.resultCheck.CheckedItems()); got != 2 {
		t.Fatalf("space off one row must leave 2, got %d", got)
	}
	next, _ = sp.Update(tea.KeyPressMsg{Code: 'i'})
	sp = next.(*searchProgress)
	if got := len(sp.resultCheck.CheckedItems()); got != 1 {
		t.Fatalf("i must invert 2 checked into 1, got %d", got)
	}
}

// TestSearchEnterOneSelectionGoesToSession (PR31): one checked
// provider skips the picker and enters the session directly; enter
// with NOTHING checked is a no-op.
func TestSearchEnterOneSelectionGoesToSession(t *testing.T) {
	fs, deps := checklistTestDeps()
	fs.providers = fs.providers[:1] // one provider, one result

	sp := NewSearchProgress(deps, "наруто")
	sp = settleSearch(sp, providerResultMsg{
		provider: fs.providers[0],
		results:  []contracts.SearchResult{{Title: "Наруто", URL: "u1", SourceID: "animego"}},
	})

	// Enter with nothing checked: stays put.
	_, cmd := sp.Update(enter())
	if cmd != nil {
		t.Fatalf("enter with no selection must be a no-op, got %T", firstMsg(cmd))
	}

	// Check the single row, enter: straight into the session.
	next, _ := sp.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	sp = next.(*searchProgress)
	_, cmd = sp.Update(enter())
	if cmd == nil {
		t.Fatalf("enter with one selection must advance")
	}
	rm, ok := firstMsg(cmd).(replaceMsg)
	if !ok {
		t.Fatalf("one selection must replace with the session, got %T", firstMsg(cmd))
	}
	sess, ok := rm.screen.(*sessionScreen)
	if !ok {
		t.Fatalf("one selection must enter the session directly, got %T", rm.screen)
	}
	if len(sess.group) != 1 || sess.group[0].URL != "u1" {
		t.Fatalf("session must carry the selected provider, got %+v", sess.group)
	}
}

// TestSearchEnterMultiSelectionOpensProviderPicker (PR31): three
// checked providers open the picker over exactly the selected rows;
// the picker pick then enters the session carrying all of them.
func TestSearchEnterMultiSelectionOpensProviderPicker(t *testing.T) {
	fs, deps := checklistTestDeps()
	fs.providers = append(fs.providers, ProviderMeta{ID: "third", Name: "Third"})
	res := func(src string) []contracts.SearchResult {
		return []contracts.SearchResult{{Title: "Наруто", URL: "u-" + src, SourceID: src}}
	}

	sp := NewSearchProgress(deps, "наруто")
	sp = settleSearch(sp,
		providerResultMsg{provider: fs.providers[0], results: res("animego")},
		providerResultMsg{provider: fs.providers[1], results: res("anilib")},
		providerResultMsg{provider: fs.providers[2], results: res("third")})

	next, _ := sp.Update(tea.KeyPressMsg{Code: 'a'})
	sp = next.(*searchProgress)
	_, cmd := sp.Update(enter())
	if cmd == nil {
		t.Fatalf("enter with three selections must advance")
	}
	rm, ok := firstMsg(cmd).(replaceMsg)
	if !ok {
		t.Fatalf("multi selection must replace with the provider picker, got %T", firstMsg(cmd))
	}
	src, ok := rm.screen.(*searchSource)
	if !ok {
		t.Fatalf("multi selection must open the provider picker, got %T", rm.screen)
	}
	if len(src.group) != 3 {
		t.Fatalf("picker must list the three selected providers, got %d", len(src.group))
	}
	if !strings.Contains(src.list.Menu().Title, "Выберите провайдера") {
		t.Fatalf("picker title must use provider terminology, got %q", src.list.Menu().Title)
	}

	// Picker enter: the session carries ALL selected providers.
	_, cmd = src.Update(enter())
	rm, ok = firstMsg(cmd).(replaceMsg)
	if !ok {
		t.Fatalf("picker enter must enter the session, got %T", firstMsg(cmd))
	}
	sess, ok := rm.screen.(*sessionScreen)
	if !ok {
		t.Fatalf("picker pick must enter the session, got %T", rm.screen)
	}
	if len(sess.group) != 3 {
		t.Fatalf("session must carry the selected providers, got %d", len(sess.group))
	}
}

// TestSearchEscFromSettledPopsBack (PR31): esc from the settled
// checklist pops one level — back to the catalog in the rebind flow,
// back to the caller in the plain flow.
func TestSearchEscFromSettledPopsBack(t *testing.T) {
	fs, deps := checklistTestDeps()

	sp := NewSearchProgress(deps, "наруто")
	sp = settleSearch(sp, providerResultMsg{
		provider: fs.providers[0],
		results:  []contracts.SearchResult{{Title: "Наруто", URL: "u1", SourceID: "animego"}},
	})
	_, cmd := sp.Update(esc())
	if cmd == nil {
		t.Fatalf("esc from the settled checklist must pop")
	}
	if _, ok := firstMsg(cmd).(popMsg); !ok {
		t.Fatalf("esc from the settled checklist must pop, got %#v", firstMsg(cmd))
	}

	// Catalog (rebind) flow: same pop semantics from the wrapped
	// screen.
	rec := rebindTestRecord()
	rp := newRebindProgress(deps, rec)
	next, _ := rp.Update(providerResultMsg{
		provider: fs.providers[0],
		results:  []contracts.SearchResult{{Title: "Наруто", URL: "u1", SourceID: "animego"}},
	})
	rp = next.(*rebindProgress)
	_, cmd = rp.Update(esc())
	if cmd == nil {
		t.Fatalf("esc from the settled catalog checklist must pop")
	}
	if _, ok := firstMsg(cmd).(popMsg); !ok {
		t.Fatalf("esc from the settled catalog checklist must pop, got %#v", firstMsg(cmd))
	}
}

// TestCatalogResumeMultiSelectionPickerResumes (PR31): in the catalog
// flow, several checked providers open the picker and the pick
// RESUMES the history record (saved episode/dubs) instead of a fresh
// session.
func TestCatalogResumeMultiSelectionPickerResumes(t *testing.T) {
	fs, deps := checklistTestDeps()
	rec := rebindTestRecord()
	saved := "5"
	rec.CurrentEpisode = saved

	rp := newRebindProgress(deps, rec)
	rp.searchProgress = settleSearch(rp.searchProgress,
		providerResultMsg{provider: fs.providers[0], results: []contracts.SearchResult{
			{Title: "Наруто", URL: "u1", SourceID: "animego"},
		}},
		providerResultMsg{provider: fs.providers[1], results: []contracts.SearchResult{
			{Title: "Наруто", URL: "u2", SourceID: "anilib"},
		}})

	next, _ := rp.Update(tea.KeyPressMsg{Code: 'a'})
	rp = next.(*rebindProgress)
	_, cmd := rp.Update(enter())
	rm, ok := firstMsg(cmd).(replaceMsg)
	if !ok {
		t.Fatalf("catalog multi selection must open the picker, got %T", firstMsg(cmd))
	}
	src, ok := rm.screen.(*searchSource)
	if !ok {
		t.Fatalf("catalog multi selection must open the provider picker, got %T", rm.screen)
	}

	_, cmd = src.Update(enter())
	rm, ok = firstMsg(cmd).(replaceMsg)
	if !ok {
		t.Fatalf("picker enter must resume the session, got %T", firstMsg(cmd))
	}
	sess, ok := rm.screen.(*sessionScreen)
	if !ok {
		t.Fatalf("picker pick must resume the session, got %T", rm.screen)
	}
	if sess.resume == nil || sess.resume.CurrentEpisode != saved {
		t.Fatalf("session must resume the record, got %+v", sess.resume)
	}
}

// firstMsg drains a command into its first concrete message (nil when
// the command is nil).
func firstMsg(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	return cmd()
}
