package tui

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// fakeHydrate records hydration calls; embeds maps provider-local dub
// keys onto links returned for every episode of that provider.
type hydrateFixture struct {
	embeds map[string]map[string][]string
	errs   map[string]error
	calls  []string // providerIDs in call order
}

// hydratingEpisode wraps fakeEpisode with the HydrateDubs capability.
type hydratingEpisode struct {
	fakeEpisode
	fix *hydrateFixture
}

func (h *hydratingEpisode) HydrateDubs(_ context.Context, providerID string, episode contracts.Episode) (contracts.Episode, error) {
	h.fix.calls = append(h.fix.calls, providerID)
	if err := h.fix.errs[providerID]; err != nil {
		return episode, err
	}
	for dub, links := range h.fix.embeds[providerID] {
		episode.RawEmbeds[dub] = links
	}
	return episode, nil
}

var _ EpisodeService = (*hydratingEpisode)(nil)

// newHydrateSession builds a single-source AnimeLib-style session whose
// episodes carry NO embeds (the lazy-hydration reality) and wires the
// hydrating fake. It returns the merge-finalization command — the
// hydration the merge schedules.
func newHydrateSession(t *testing.T, fix *hydrateFixture) (*sessionScreen, tea.Cmd) {
	t.Helper()
	deps := &Deps{
		Episode: &hydratingEpisode{
			fakeEpisode: fakeEpisode{episodes: map[string][]contracts.Episode{
				"anilib": {
					{Num: "1", RawID: "17166", RawEmbeds: map[string][]string{}},
					{Num: "2", RawID: "17167", RawEmbeds: map[string][]string{}},
				},
			}},
			fix: fix,
		},
		Search: &fakeSearch{providers: []ProviderMeta{{ID: "anilib", Name: "AnimeLib"}}},
		Log:     discardLogger(),
	}
	if fix.embeds == nil {
		fix.embeds = map[string]map[string][]string{
			"anilib": {"AniLib (AnimeLib)": {"internal:x"}},
		}
	}
	group := []contracts.SearchResult{{Title: "Тайтл", URL: "u", SourceID: "anilib"}}
	s := NewSessionScreen(deps, group[0], group)
	eps, err := deps.Episode.GetEpisodes(context.Background(), "anilib", "u")
	if err != nil {
		t.Fatalf("fixture episodes: %v", err)
	}
	s.Update(episodePartMsg{sourceID: "anilib", episodes: eps})
	_, cmd := s.Update(episodesDoneMsg{})
	return s, cmd
}

// runHydration settles one hydration round: run the command and feed
// the settle message back into Update.
func runHydration(s *sessionScreen, cmd tea.Cmd) {
	msg := cmd()
	if msg == nil {
		return
	}
	s.Update(msg)
}

// TestSessionAutoHydratesEmptyEmbedEpisode pins the PR43 root-cause
// fix: an AnimeLib-style episode list arrives with EMPTY RawEmbeds, so
// the session hydrates the current episode on demand and the header
// shows the real source count with provider names.
func TestSessionAutoHydratesEmptyEmbedEpisode(t *testing.T) {
	fix := &hydrateFixture{}
	s, cmd := newHydrateSession(t, fix)

	if got := s.renderHeader(); !strings.Contains(got, "Ист: 0") {
		t.Fatalf("pre-hydration header = %q, want Ист: 0", got)
	}
	// finalizeMerge must schedule the hydration of episode 1.
	if cmd == nil {
		t.Fatal("merge with empty embeds must schedule hydration")
	}
	msg := cmd()
	done, ok := msg.(hydrateDoneMsg)
	if !ok {
		t.Fatalf("hydration cmd settled %T, want hydrateDoneMsg", msg)
	}
	if _, cmd := s.Update(done); cmd != nil {
		t.Fatalf("hydration settle returned unexpected cmd %T", cmd)
	}
	if got := s.renderHeader(); !strings.Contains(got, "Ист: 1 (AnimeLib)") {
		t.Fatalf("post-hydration header = %q, want Ист: 1 (AnimeLib)", got)
	}
	ep := s.episodes["1"]
	if len(ep.RawEmbeds) != 1 || ep.RawEmbeds["[anilib] AniLib (AnimeLib)"] == nil {
		t.Fatalf("hydrated embeds not merged with provider prefix: %v", ep.RawEmbeds)
	}
	if s.dubStats["[anilib] AniLib (AnimeLib)"] != 1 {
		t.Fatalf("hydrated dub missing from stats: %v", s.dubStats)
	}
}

// TestSessionHydrationEmptyHonestState: hydration that returns zero
// embeds must surface the honest 0-state, disable «Смотреть» and
// explain on an attempt — never a silent dead end.
func TestSessionHydrationEmptyHonestState(t *testing.T) {
	fix := &hydrateFixture{embeds: map[string]map[string][]string{"anilib": {}}}
	s, cmd := newHydrateSession(t, fix)
	runHydration(s, cmd)

	if got := s.renderHeader(); !strings.Contains(got, "Ист: 0 — источники не найдены") {
		t.Fatalf("header = %q, want the honest 0-state", got)
	}
	if got := s.renderHeader(); !strings.Contains(got, "все провайдеры завершились без результатов") {
		t.Fatalf("header = %q, want the no-results cause", got)
	}
	for _, item := range s.list.Menu().Items {
		if item.ID == "watch" && !item.Disabled {
			t.Fatal("«Смотреть» must be disabled when no sources exist")
		}
	}
	// Enter on the disabled action must explain instead of acting.
	_, _ = s.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // cursor sits on «Смотреть»
	if !strings.Contains(s.status, "Обновить источники") {
		t.Fatalf("status after disabled watch attempt = %q, want the refresh hint", s.status)
	}
}

// TestSessionHydrationErrorCause: a failing hydration surfaces a
// truncated per-provider error summary in the header cause.
func TestSessionHydrationErrorCause(t *testing.T) {
	fix := &hydrateFixture{errs: map[string]error{"anilib": context.DeadlineExceeded}, embeds: map[string]map[string][]string{}}
	s, cmd := newHydrateSession(t, fix)
	runHydration(s, cmd)

	got := s.renderHeader()
	if !strings.Contains(got, "Ист: 0 — источники не найдены") {
		t.Fatalf("header = %q, want the honest 0-state", got)
	}
	if !strings.Contains(got, "anilib") || !strings.Contains(got, "deadline") {
		t.Fatalf("header = %q, want the per-provider error summary", got)
	}
}

// TestSessionRefreshSources: «🔄 Обновить источники» re-runs the
// resolve for the current episode and reports progress while running.
func TestSessionRefreshSources(t *testing.T) {
	fix := &hydrateFixture{embeds: map[string]map[string][]string{"anilib": {}}}
	s, cmd := newHydrateSession(t, fix)
	runHydration(s, cmd)

	s.list.Jump(indexOfDayActionMenu(s, "refresh"))
	_, cmd = s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
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
