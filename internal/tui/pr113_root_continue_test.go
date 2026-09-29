package tui

// PR113 «Продолжить просмотр»: the root menu gains a sixth row that
// jumps straight into the last-watched anime at the episode to
// continue with. The pins here:
//   - the row NEVER displaces the existing entries (count+order pins
//     below) and «Выход» stays the pinned bottom row (I1);
//   - the label reminds the title and the episode (owner ruling 2);
//   - the target episode is N+1 — the next unwatched — staying on N
//     when the episode was left incomplete or no next episode exists;
//   - the row reuses the ▶ watch action id (PR74: one emoji, one
//     action — continuing IS watching).

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/storage"
)

// pr113StrPtr is a one-line pointer helper for optional record fields.
func pr113StrPtr(s string) *string { return &s }

// TestContinueTargetEpisode: the pure target computation.
func TestContinueTargetEpisode(t *testing.T) {
	cases := []struct {
		name string
		rec  storage.AnimeProgress
		want string
	}{
		{
			name: "next episode after the watched one",
			rec:  storage.AnimeProgress{CurrentEpisode: "2"},
			want: "3",
		},
		{
			name: "known total not exhausted still advances",
			rec:  storage.AnimeProgress{CurrentEpisode: "5", TotalEpisodes: 12},
			want: "6",
		},
		{
			name: "known total exhausted stays on the final episode",
			rec:  storage.AnimeProgress{CurrentEpisode: "5", TotalEpisodes: 5},
			want: "5",
		},
		{
			name: "mid-episode stays to finish the episode (mpv restores position)",
			rec:  storage.AnimeProgress{CurrentEpisode: "5", ProgressSeconds: 120, TotalSeconds: 1440},
			want: "5",
		},
		{
			name: "fully-counted episode advances",
			rec:  storage.AnimeProgress{CurrentEpisode: "3", ProgressSeconds: 1440, TotalSeconds: 1440},
			want: "4",
		},
		{
			name: "zero progress does not count as mid-episode",
			rec:  storage.AnimeProgress{CurrentEpisode: "3", ProgressSeconds: 0, TotalSeconds: 1440},
			want: "4",
		},
		{
			name: "non-numeric episode label stays",
			rec:  storage.AnimeProgress{CurrentEpisode: "OVA"},
			want: "OVA",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ContinueTarget(tc.rec); got != tc.want {
				t.Fatalf("ContinueTarget(%+v) = %q, want %q", tc.rec, got, tc.want)
			}
		})
	}
}

// TestContinueLabel: the rendered row text — empty form without
// history, title+episode with it; the bound title wins over the raw
// provider title (the same name the manual history flow resumes by).
func TestContinueLabel(t *testing.T) {
	if got := ContinueLabel(nil); got != "▶ Continue: —" {
		t.Fatalf("empty label = %q, want %q", got, "▶ Continue: —")
	}
	rec := storage.AnimeProgress{Title: "Raw Title", CurrentEpisode: "3"}
	if got, want := ContinueLabel(&rec), "▶ Continue: Raw Title — ep. 4"; got != want {
		t.Fatalf("label = %q, want %q", got, want)
	}
	bound := storage.AnimeProgress{
		Title:          "Raw Title",
		BoundTitle:     pr113StrPtr("Bound Title"),
		CurrentEpisode: "1",
	}
	if got, want := ContinueLabel(&bound), "▶ Continue: Bound Title — ep. 2"; got != want {
		t.Fatalf("label = %q, want %q (bound title preferred)", got, want)
	}
}

// TestRootMenuContinueRowRendering: the row renders dim-empty without
// history and with the latest record's title+episode with history.
// PR113b: the row is a special header ABOVE the menu (never a menu
// item), so the assertions are on the rendered view only.
func TestRootMenuContinueRowRendering(t *testing.T) {
	t.Run("no history renders the dim dash row", func(t *testing.T) {
		root := NewRootScreen(&Deps{})
		view := root.View().Content
		if !strings.Contains(view, "▶ Continue: —") {
			t.Fatalf("root view must contain the empty continue row, got:\n%s", view)
		}
		if indexOfChoice(root.MenuScreen, "watch") >= 0 {
			t.Fatalf("the continue row must NOT be a menu item (PR113b header row)")
		}
	})

	t.Run("history renders title and next episode", func(t *testing.T) {
		deps := &Deps{History: &fakeHistory{items: []storage.AnimeProgress{{
			ID: 7, Title: "Anime X", CurrentEpisode: "3",
			SourceID: "animego", SourceURL: "u1",
		}}}}
		root := NewRootScreen(deps)
		view := root.View().Content
		if !strings.Contains(view, "▶ Continue: Anime X — ep. 4") {
			t.Fatalf("root view must contain the continue row with title+episode, got:\n%s", view)
		}
	})

	t.Run("the newest record wins", func(t *testing.T) {
		deps := &Deps{History: &fakeHistory{items: []storage.AnimeProgress{
			{ID: 1, Title: "Latest Anime", CurrentEpisode: "1", SourceID: "a", SourceURL: "u"},
			{ID: 2, Title: "Older Anime", CurrentEpisode: "9", SourceID: "b", SourceURL: "v"},
		}}}
		root := NewRootScreen(deps)
		if view := root.View().Content; !strings.Contains(view, "▶ Continue: Latest Anime — ep. 2") {
			t.Fatalf("the continue row must reflect the FIRST (newest) history row, got:\n%s", view)
		}
	})
}

// TestRootContinueHeaderAboveMenuWithSeparator pins the PR113b layout:
// the continue row is the FIRST rendered line below the title, a full
// horizontal line separates it from the menu list, and the feature
// items follow below the line (owner mock: a special top row, visually
// separated like the pinned «Выход» row at the bottom).
func TestRootContinueHeaderAboveMenuWithSeparator(t *testing.T) {
	deps := &Deps{History: &fakeHistory{items: []storage.AnimeProgress{{
		ID: 7, Title: "Anime X", CurrentEpisode: "3",
		SourceID: "animego", SourceURL: "u1",
	}}}}
	root := NewRootScreen(deps)
	root.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	view := root.View().Content

	contIdx := strings.Index(view, "▶ Continue: Anime X — ep. 4")
	listsIdx := strings.Index(view, "📜 Lists")
	sepIdx := strings.Index(view, strings.Repeat("─", 60))
	if contIdx < 0 || listsIdx < 0 || sepIdx < 0 {
		t.Fatalf("view must contain the continue row, the separator line and the menu, got:\n%s", view)
	}
	if contIdx >= sepIdx || sepIdx >= listsIdx {
		t.Fatalf("order must be continue row → separator → menu list, got:\n%s", view)
	}
	// The line sits between header and list only — the pinned «Выход»
	// row below keeps its own pre-existing separator.
	if strings.Count(view, strings.Repeat("─", 60)) != 1 {
		t.Fatalf("exactly one width-wide separator expected, got:\n%s", view)
	}
}

// TestRootContinueHeaderTracksTerminalWidth pins the PR98-style width
// contract: the header separator re-renders against the actual window
// width — a line wider than the terminal would wrap and corrupt
// bubbletea's cursor tracking.
func TestRootContinueHeaderTracksTerminalWidth(t *testing.T) {
	root := NewRootScreen(&Deps{})
	if view := root.View().Content; !strings.Contains(view, strings.Repeat("─", 40)) {
		t.Fatalf("before any resize the separator uses the 40-column fallback, got:\n%s", view)
	}
	root.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if view := root.View().Content; !strings.Contains(view, strings.Repeat("─", 100)) {
		t.Fatalf("after WindowSizeMsg(100) the separator must be 100 columns, got:\n%s", view)
	}
}

// TestRootMenuExistingItemsUnchanged: PR113b must not displace the
// existing entries — the continue row LEFT the menu (it is the header
// above it now), the feature rows keep their relative order and
// «Выход» stays the pinned LAST row (I1).
func TestRootMenuExistingItemsUnchanged(t *testing.T) {
	root := NewRootScreen(&Deps{})
	items := root.list.Menu().Items
	wantIDs := []string{"lists", "downloads", "db", "check", "season", "exit"}
	if len(items) != len(wantIDs) {
		t.Fatalf("root menu must hold %d items, got %d: %+v", len(wantIDs), len(items), items)
	}
	for i, want := range wantIDs {
		if items[i].ID != want {
			t.Fatalf("root item %d = %q, want %q (order pin)", i, items[i].ID, want)
		}
	}
	if last := items[len(items)-1]; last.ID != "exit" {
		t.Fatalf("«Выход» must remain the trailing pinned row, got %+v", last)
	}
	if indexOfChoice(root.MenuScreen, "watch") >= 0 {
		t.Fatalf("«Продолжить» must not be a MenuScreen item (PR113b header row)")
	}
}

// TestRootContinueEnterNoHistoryNoOp: the continue hotkey (c, PR113b —
// the row is the header above the menu, outside the cursor domain) on
// the dim row is a no-op answering with the «History is empty» hint;
// the hint clears on the next key press.
func TestRootContinueEnterNoHistoryNoOp(t *testing.T) {
	root := NewRootScreen(&Deps{})

	next, cmd := root.Update(continueKey())
	if cmd != nil {
		t.Fatalf("the continue hotkey on the empty row must not navigate, got %v", cmd)
	}
	if next.ID() != rootScreenID {
		t.Fatalf("the root screen must stay, got %q", next.ID())
	}
	if view := root.View().Content; !strings.Contains(view, "History is empty") {
		t.Fatalf("the empty continue row must answer with the hint, got:\n%s", view)
	}

	// The hint is transient: the next key press clears it.
	root.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if view := root.View().Content; strings.Contains(view, "History is empty") {
		t.Fatalf("the hint must clear on the next key press, got:\n%s", view)
	}
}

// TestRootContinueEnterPushesSessionAtTargetEpisode: the continue
// hotkey pushes the resumed session — the same screen manual history
// navigation lands on — restored onto the LABELED episode.
func TestRootContinueEnterPushesSessionAtTargetEpisode(t *testing.T) {
	deps := &Deps{
		History: &fakeHistory{items: []storage.AnimeProgress{{
			ID: 7, Title: "Anime X", CurrentEpisode: "1",
			SourceID: "animego", SourceURL: "u1",
		}}},
		Episode: &fakeEpisode{episodes: testEpisodeSet()},
	}
	root := NewRootScreen(deps)

	_, cmd := root.Update(continueKey())
	if cmd == nil {
		t.Fatalf("Enter on the continue row must schedule navigation")
	}
	pm, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("the continue row must push a screen, got %#v", cmd())
	}
	if pm.screen.ID() != sessionScreenID {
		t.Fatalf("want the session screen, got %q", pm.screen.ID())
	}
	ss, ok := pm.screen.(*sessionScreen)
	if !ok {
		t.Fatalf("want *sessionScreen, got %T", pm.screen)
	}
	if ss.resume == nil || ss.resume.CurrentEpisode != "2" {
		t.Fatalf("the resumed session must target episode 2 (label ep. 2), got %+v", ss.resume)
	}
	// The merge finalization restores the cursor onto that episode:
	// the single-source (animego) merged order of testEpisodeSet is
	// [1 2] → index 1.
	ss.loadEpisodesSync()
	if ss.currentIdx != 1 {
		t.Fatalf("the session must land on episode \"2\" (idx 1), got idx %d (order %v)",
			ss.currentIdx, ss.order)
	}
}

// TestRootContinueUnboundRecordRoutesToRebind: a record that carries
// no usable source takes the manual flow's path — the rebind search
// screen — instead of a doomed direct resume.
func TestRootContinueUnboundRecordRoutesToRebind(t *testing.T) {
	deps := &Deps{History: &fakeHistory{items: []storage.AnimeProgress{{
		ID: 7, Title: "Anime X", CurrentEpisode: "2", NeedsCorrection: true,
	}}}}
	root := NewRootScreen(deps)

	_, cmd := root.Update(continueKey())
	if cmd == nil {
		t.Fatalf("Enter must schedule navigation")
	}
	pm, ok := cmd().(pushMsg)
	if !ok {
		t.Fatalf("the unbound continue row must push a screen, got %#v", cmd())
	}
	if want := historyRebindID + "-search"; pm.screen.ID() != want {
		t.Fatalf("want the rebind screen %q, got %q", want, pm.screen.ID())
	}
}

// TestRootContinueRowRefreshesAfterPlayback: the label is computed at
// render time, not frozen at construction — after a new record
// appears the next View reflects it (popToRoot reuses the root
// screen instance).
func TestRootContinueRowRefreshesAfterPlayback(t *testing.T) {
	hist := &fakeHistory{}
	deps := &Deps{History: hist}
	root := NewRootScreen(deps)
	if view := root.View().Content; !strings.Contains(view, "▶ Continue: —") {
		t.Fatalf("fresh root must render the empty row, got:\n%s", view)
	}

	hist.items = []storage.AnimeProgress{{
		ID: 9, Title: "New Anime", CurrentEpisode: "7", SourceID: "a", SourceURL: "u",
	}}
	if view := root.View().Content; !strings.Contains(view, "▶ Continue: New Anime — ep. 8") {
		t.Fatalf("the SAME root instance must pick up the fresh record, got:\n%s", view)
	}
}
