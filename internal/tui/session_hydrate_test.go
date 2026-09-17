package tui

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// fakeHydrate records hydration calls; embeds maps provider-local dub
// keys onto links returned for every episode of that provider.
type hydrateFixture struct {
	embeds map[string]map[string][]string
	errs   map[string]error

	mu    sync.Mutex
	calls []string // providerIDs in call order
}

// hydratingEpisode wraps fakeEpisode with the HydrateDubs capability.
type hydratingEpisode struct {
	fakeEpisode
	fix *hydrateFixture
}

func (h *hydratingEpisode) HydrateDubs(_ context.Context, providerID string, episode contracts.Episode) (contracts.Episode, error) {
	h.fix.mu.Lock()
	h.fix.calls = append(h.fix.calls, providerID)
	h.fix.mu.Unlock()
	if err := h.fix.errs[providerID]; err != nil {
		return episode, err
	}
	for dub, links := range h.fix.embeds[providerID] {
		episode.RawEmbeds[dub] = links
	}
	return episode, nil
}

var _ EpisodeService = (*hydratingEpisode)(nil)

// newLazySession builds a single-source session whose provider lists
// episodes with NO embeds (the lazily-hydrating reality) and runs the
// REAL Init fetch phase: the returned session is exactly what the user
// is shown after loading — the list surfaced, nothing resolved.
func newLazySession(t *testing.T, fix *hydrateFixture, eps map[string][]contracts.Episode) *sessionScreen {
	t.Helper()
	deps := &Deps{
		Episode: &hydratingEpisode{
			fakeEpisode: fakeEpisode{episodes: eps},
			fix:         fix,
		},
		Search: &fakeSearch{providers: []ProviderMeta{{ID: "anilib", Name: "AnimeLib"}}},
		Log:    discardLogger(),
	}
	if fix.embeds == nil {
		fix.embeds = map[string]map[string][]string{
			"anilib": {"AniLib (AnimeLib)": {"internal:x"}},
		}
	}
	group := []contracts.SearchResult{{Title: "Тайтл", URL: "u", SourceID: "anilib"}}
	s := NewSessionScreen(deps, group[0], group)
	settleInit(t, s)
	return s
}

// settleInit runs the session's Init command batch (the episode-fetch
// phase) and feeds every settle message back into Update, including
// the merge finalization the last episodePartMsg schedules.
func settleInit(t *testing.T, s *sessionScreen) {
	t.Helper()
	cmd := s.Init()
	if cmd == nil {
		t.Fatal("init must schedule the fetch phase")
	}
	var msgs []tea.Msg
	switch m := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range m {
			msgs = append(msgs, c())
		}
	default:
		msgs = append(msgs, m)
	}
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		_, next := s.Update(msg)
		if next != nil {
			if follow := next(); follow != nil {
				s.Update(follow)
			}
		}
	}
}

// TestEpisodeListSurfacesWithoutHydration pins the PR44 owner model:
// the episode LIST surfaces immediately — the fetch phase performs NO
// hydration (streams are temporary; resolving 1000+ episodes up front
// is pointless). The honest 0-state names its cause.
func TestEpisodeListSurfacesWithoutHydration(t *testing.T) {
	fix := &hydrateFixture{}
	s := newLazySession(t, fix, map[string][]contracts.Episode{
		"anilib": {
			{Num: "1", RawID: "17166", RawEmbeds: map[string][]string{}},
			{Num: "2", RawID: "17167", RawEmbeds: map[string][]string{}},
		},
	})

	if s.state != sessionStateMenu {
		t.Fatalf("state = %s, want the menu right after loading", s.state)
	}
	if len(s.order) != 2 {
		t.Fatalf("episode order = %v, want the full list surfaced", s.order)
	}
	fix.mu.Lock()
	calls := len(fix.calls)
	fix.mu.Unlock()
	if calls != 0 {
		t.Fatalf("hydration calls = %d, want 0 (nothing resolves in bulk)", calls)
	}
	got := s.renderHeader()
	if !strings.Contains(got, "Ист: 0") {
		t.Fatalf("header = %q, want the honest 0-state", got)
	}
	if !strings.Contains(got, "источники не запрашивались") {
		t.Fatalf("header = %q, want the not-attempted cause", got)
	}
}

// TestWatchTriggersOnDemandHydration: «▶ Смотреть» on an unopened
// episode triggers the hydration of THAT episode only; after it
// settles, the next watch opens the format selector.
func TestWatchTriggersOnDemandHydration(t *testing.T) {
	fix := &hydrateFixture{}
	s := newLazySession(t, fix, map[string][]contracts.Episode{
		"anilib": {
			{Num: "1", RawID: "17166", RawEmbeds: map[string][]string{}},
			{Num: "2", RawID: "17167", RawEmbeds: map[string][]string{}},
		},
	})

	// First watch: the on-demand hydration of episode 1.
	s.list.Jump(indexOfDayActionMenu(s, "watch"))
	for _, item := range s.list.Menu().Items {
		if item.ID == "watch" && item.Disabled {
			t.Fatal("«Смотреть» must stay enabled before an attempt")
		}
	}
	_, cmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("watch on an unopened episode must schedule hydration")
	}
	if s.status != "Ищу источники…" {
		t.Fatalf("status = %q, want «Ищу источники…»", s.status)
	}
	msg := cmd()
	done, ok := msg.(hydrateDoneMsg)
	if !ok {
		t.Fatalf("watch settled %T, want hydrateDoneMsg", msg)
	}
	if _, cmd := s.Update(done); cmd != nil {
		t.Fatalf("hydration settle returned unexpected cmd %T", cmd)
	}
	fix.mu.Lock()
	calls := len(fix.calls)
	fix.mu.Unlock()
	if calls != 1 {
		t.Fatalf("hydration calls = %d, want 1 (the opened episode only)", calls)
	}
	if got := s.renderHeader(); !strings.Contains(got, "Ист: 1 (AnimeLib)") {
		t.Fatalf("post-hydration header = %q, want the real source count", got)
	}

	// Second watch: embeds are known — the format selector opens
	// WITHOUT another hydration.
	s.list.Jump(indexOfDayActionMenu(s, "watch"))
	if _, cmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil {
		t.Fatalf("watch cmd: %T", cmd)
	}
	if s.state != sessionStateFormat {
		t.Fatalf("state = %s, want the format selector", s.state)
	}
	fix.mu.Lock()
	calls = len(fix.calls)
	fix.mu.Unlock()
	if calls != 1 {
		t.Fatalf("hydration calls = %d, want still 1", calls)
	}
}

// TestWatchAttemptedEmptyDimsAndHints: a hydration that legitimately
// found nothing dims «Смотреть», names the genuine no-results cause
// and routes recovery through «🔄 Обновить источники».
func TestWatchAttemptedEmptyDimsAndHints(t *testing.T) {
	fix := &hydrateFixture{embeds: map[string]map[string][]string{"anilib": {}}}
	s := newLazySession(t, fix, map[string][]contracts.Episode{
		"anilib": {
			{Num: "1", RawID: "17166", RawEmbeds: map[string][]string{}},
		},
	})

	// The attempt: watch triggers the on-demand hydration.
	s.list.Jump(indexOfDayActionMenu(s, "watch"))
	_, cmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	s.Update(cmd())

	for _, item := range s.list.Menu().Items {
		if item.ID == "watch" && !item.Disabled {
			t.Fatal("«Смотреть» must be dimmed after an empty attempt")
		}
	}
	if got := s.renderHeader(); !strings.Contains(got, "все провайдеры завершились без результатов") {
		t.Fatalf("header = %q, want the genuine no-results cause", got)
	}
	s.list.Jump(indexOfDayActionMenu(s, "watch"))
	_, _ = s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if s.state == sessionStateFormat {
		t.Fatal("the format selector must never open without sources")
	}
	if !strings.Contains(s.status, "Обновить источники") {
		t.Fatalf("status = %q, want the refresh hint", s.status)
	}
}

// TestWatchHydrationErrorSurfacesCause: a failing watch-triggered
// hydration surfaces the per-provider error summary.
func TestWatchHydrationErrorSurfacesCause(t *testing.T) {
	fix := &hydrateFixture{
		embeds: map[string]map[string][]string{},
		errs:   map[string]error{"anilib": errors.New("boom")},
	}
	s := newLazySession(t, fix, map[string][]contracts.Episode{
		"anilib": {
			{Num: "1", RawID: "17166", RawEmbeds: map[string][]string{}},
		},
	})

	s.list.Jump(indexOfDayActionMenu(s, "watch"))
	_, cmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	s.Update(cmd())

	got := s.renderHeader()
	if !strings.Contains(got, "anilib") || !strings.Contains(got, "boom") {
		t.Fatalf("header = %q, want the per-provider error summary", got)
	}
	if strings.Contains(got, "источники не запрашивались") {
		t.Fatalf("header = %q, the not-attempted cause must be gone after an attempt", got)
	}
}

// TestKnownDubKeysNeedNoHydration: with the release-scope dub keys
// (the tier-1 shape — keys with empty lists) the Ист count is known
// without resolving, and watch goes straight to the format selector.
func TestKnownDubKeysNeedNoHydration(t *testing.T) {
	fix := &hydrateFixture{}
	s := newLazySession(t, fix, map[string][]contracts.Episode{
		"anilib": {
			{Num: "1", RawID: "17166", RawEmbeds: map[string][]string{"AniLib (AnimeLib)": {}, "Studio Band (Kodik)": {}}},
		},
	})

	if got := s.renderHeader(); !strings.Contains(got, "Ист: 2 (AnimeLib)") {
		t.Fatalf("header = %q, want the tier-1 dub count", got)
	}
	s.list.Jump(indexOfDayActionMenu(s, "watch"))
	if _, cmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil {
		t.Fatalf("watch cmd: %T", cmd)
	}
	if s.state != sessionStateFormat {
		t.Fatalf("state = %s, want the format selector", s.state)
	}
	fix.mu.Lock()
	calls := len(fix.calls)
	fix.mu.Unlock()
	if calls != 0 {
		t.Fatalf("hydration calls = %d, want 0 (resolving happens in ResolveStream, not bulk)", calls)
	}
}

// TestSessionRefreshSources: «🔄 Обновить источники» re-runs the
// per-episode hydration for the current episode (the recovery path
// for transient failures) and reports progress while running.
func TestSessionRefreshSources(t *testing.T) {
	fix := &hydrateFixture{embeds: map[string]map[string][]string{"anilib": {}}}
	s := newLazySession(t, fix, map[string][]contracts.Episode{
		"anilib": {
			{Num: "1", RawID: "17166", RawEmbeds: map[string][]string{}},
		},
	})

	// The recovery now finds the dub (transient failure healed).
	fix.embeds["anilib"] = map[string]map[string][]string{"anilib": {"AniLib (AnimeLib)": {"internal:x"}}}["anilib"]
	fix.errs = nil
	s.list.Jump(indexOfDayActionMenu(s, "refresh"))
	_, cmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("refresh must schedule a re-resolve")
	}
	if s.status != "Ищу источники…" {
		t.Fatalf("status during refresh = %q, want «Ищу источники…»", s.status)
	}
	msg := cmd()
	if _, ok := msg.(hydrateDoneMsg); !ok {
		t.Fatalf("refresh settled %T, want hydrateDoneMsg", msg)
	}
	s.Update(msg)
	if got := s.renderHeader(); !strings.Contains(got, "Ист: 1") {
		t.Fatalf("post-refresh header = %q, want the recovered source", got)
	}
}

// TestRefreshSourcesHydratesKeyOnlyTier1Episodes (review MAJOR): the
// tier-1 dub list rides every episode as keys with EMPTY lists — that
// is the NORMAL post-GetEpisodes state, and «🔄 Обновить источники»
// must still call HydrateDubs for it (the skip guard counts only
// embeds with actual LINKS, not key-only entries).
func TestRefreshSourcesHydratesKeyOnlyTier1Episodes(t *testing.T) {
	fix := &hydrateFixture{}
	s := newLazySession(t, fix, map[string][]contracts.Episode{
		"anilib": {{
			Num:       "1",
			RawID:     "17166",
			RawEmbeds: map[string][]string{"AniLib (AnimeLib)": {}},
		}},
	})
	fix.mu.Lock()
	before := len(fix.calls)
	fix.mu.Unlock()

	s.list.Jump(indexOfDayActionMenu(s, "refresh"))
	_, cmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("refresh must schedule a re-resolve")
	}
	msg := cmd()
	done, ok := msg.(hydrateDoneMsg)
	if !ok {
		t.Fatalf("refresh settled %T, want hydrateDoneMsg", msg)
	}
	s.Update(done)

	fix.mu.Lock()
	calls := len(fix.calls)
	fix.mu.Unlock()
	if calls <= before {
		t.Fatalf("HydrateDubs calls = %d, want > %d: key-only tier-1 episodes must not be skipped as already-hydrated", calls, before)
	}
	if got := s.renderHeader(); !strings.Contains(got, "Ист: 1 (AnimeLib)") {
		t.Fatalf("post-refresh header = %q, want the recovered source", got)
	}
}

// indexOfDayActionMenu returns the cursor index of a menu item id.
func indexOfDayActionMenu(s *sessionScreen, id string) int {
	for i, item := range s.list.Menu().Items {
		if item.ID == id {
			return i
		}
	}
	return 0
}

// discardLogger silences diagnostics in tests (production routes them
// to the TUI file logger).
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
