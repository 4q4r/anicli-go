package tui

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
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

// newEagerSession builds a single-source AnimeLib-style session whose
// episodes carry NO embeds (the lazy-hydration provider reality) and
// runs the REAL Init fetch phase: the returned session has its merge
// finalized, i.e. it is exactly what the user is shown after loading.
func newEagerSession(t *testing.T, fix *hydrateFixture, eps map[string][]contracts.Episode) *sessionScreen {
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

// TestInitEagerlyHydratesEmptyEmbeds pins the PR44 invariant: the
// episode-fetch phase hydrates lazily-listed episodes EAGERLY, so the
// merge finalizes with embeds in place and the header shows the real
// source count — the «Ист: 0» steady state is gone from the flow.
func TestInitEagerlyHydratesEmptyEmbeds(t *testing.T) {
	fix := &hydrateFixture{}
	s := newEagerSession(t, fix, map[string][]contracts.Episode{
		"anilib": {
			{Num: "1", RawID: "17166", RawEmbeds: map[string][]string{}},
			{Num: "2", RawID: "17167", RawEmbeds: map[string][]string{}},
		},
	})

	if got := s.renderHeader(); !strings.Contains(got, "Ист: 1 (AnimeLib)") {
		t.Fatalf("header = %q, want the hydrated source count", got)
	}
	ep := s.episodes["1"]
	if len(ep.RawEmbeds) != 1 || ep.RawEmbeds["[anilib] AniLib (AnimeLib)"] == nil {
		t.Fatalf("hydrated embeds not merged with provider prefix: %v", ep.RawEmbeds)
	}
	if s.dubStats["[anilib] AniLib (AnimeLib)"] != 2 {
		t.Fatalf("hydrated dub missing from stats: %v", s.dubStats)
	}
}

// TestInitEagerHydrationIsBounded pins the concurrency cap: the eager
// hydration of one source never fires more than
// eagerHydrateConcurrency parallel requests, yet hydrates EVERY empty
// episode.
func TestInitEagerHydrationIsBounded(t *testing.T) {
	fix := &hydrateFixture{embeds: map[string]map[string][]string{
		"anilib": {"AniLib (AnimeLib)": {"internal:x"}},
	}}
	eps := make([]contracts.Episode, 0, 20)
	for i := range 20 {
		eps = append(eps, contracts.Episode{
			Num:       itoa2(i + 1),
			RawID:     itoa2(17166 + i),
			RawEmbeds: map[string][]string{},
		})
	}
	probe := &flightProbe{EpisodeService: &hydratingEpisode{
		fakeEpisode: fakeEpisode{episodes: map[string][]contracts.Episode{"anilib": eps}},
		fix:         fix,
	}}
	deps := &Deps{
		Episode: probe,
		Search:  &fakeSearch{providers: []ProviderMeta{{ID: "anilib", Name: "AnimeLib"}}},
		Log:     discardLogger(),
	}
	group := []contracts.SearchResult{{Title: "Тайтл", URL: "u", SourceID: "anilib"}}
	s := NewSessionScreen(deps, group[0], group)
	settleInit(t, s)

	if probe.max > eagerHydrateConcurrency {
		t.Fatalf("max parallel hydration = %d, want ≤ %d", probe.max, eagerHydrateConcurrency)
	}
	if probe.max < 2 {
		t.Fatalf("max parallel hydration = %d, want > 1 (the cap must allow concurrency)", probe.max)
	}
	for i := range 20 {
		ep, ok := s.episodes[itoa2(i+1)]
		if !ok || len(ep.RawEmbeds) == 0 {
			t.Fatalf("episode %s was not hydrated: %v", itoa2(i+1), ep.RawEmbeds)
		}
	}
}

// flightProbe wraps an EpisodeService counting the in-flight
// HydrateDubs calls (bounded-concurrency assertion).
type flightProbe struct {
	EpisodeService

	mu    sync.Mutex
	cur   int
	max   int
	total int
}

func (p *flightProbe) HydrateDubs(ctx context.Context, providerID string, episode contracts.Episode) (contracts.Episode, error) {
	p.mu.Lock()
	p.cur++
	p.total++
	if p.cur > p.max {
		p.max = p.cur
	}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.cur--
		p.mu.Unlock()
	}()
	return p.EpisodeService.HydrateDubs(ctx, providerID, episode)
}

// itoa2 formats n in base 10 for episode fixtures (strconv.Itoa with
// a local name to avoid clashing with the pinlist test helper).
func itoa2(n int) string { return strconv.Itoa(n) }

// TestInitEagerHydrationFailureSurfacesCause: a failing eager
// hydration surfaces the per-provider error summary in the header
// cause — never the retired «резолв не выполнен» line.
func TestInitEagerHydrationFailureSurfacesCause(t *testing.T) {
	fix := &hydrateFixture{
		embeds: map[string]map[string][]string{},
		errs:   map[string]error{"anilib": errors.New("boom")},
	}
	s := newEagerSession(t, fix, map[string][]contracts.Episode{
		"anilib": {
			{Num: "1", RawID: "17166", RawEmbeds: map[string][]string{}},
		},
	})

	got := s.renderHeader()
	if !strings.Contains(got, "Ист: 0 — источники не найдены") {
		t.Fatalf("header = %q, want the honest 0-state", got)
	}
	if !strings.Contains(got, "anilib") || !strings.Contains(got, "boom") {
		t.Fatalf("header = %q, want the per-provider error summary", got)
	}
	if strings.Contains(got, "резолв не выполнен") {
		t.Fatalf("header = %q, the retired cause must be gone", got)
	}
}

// TestEagerGenuineNoResultsCause: an eager hydration that succeeds
// with zero embeds means the provider genuinely has nothing — the
// no-results verdict, not an error.
func TestEagerGenuineNoResultsCause(t *testing.T) {
	fix := &hydrateFixture{embeds: map[string]map[string][]string{"anilib": {}}}
	s := newEagerSession(t, fix, map[string][]contracts.Episode{
		"anilib": {
			{Num: "1", RawID: "17166", RawEmbeds: map[string][]string{}},
		},
	})

	got := s.renderHeader()
	if !strings.Contains(got, "Ист: 0 — источники не найдены") {
		t.Fatalf("header = %q, want the honest 0-state", got)
	}
	if !strings.Contains(got, "все провайдеры завершились без результатов") {
		t.Fatalf("header = %q, want the no-results cause", got)
	}
}

// TestStartWatchWithoutSourcesNeverOpensSelector: a sourceless
// episode keeps «Смотреть» dimmed; an attempt explains the recovery
// path and the format selector never opens.
func TestStartWatchWithoutSourcesNeverOpensSelector(t *testing.T) {
	fix := &hydrateFixture{embeds: map[string]map[string][]string{"anilib": {}}}
	s := newEagerSession(t, fix, map[string][]contracts.Episode{
		"anilib": {
			{Num: "1", RawID: "17166", RawEmbeds: map[string][]string{}},
		},
	})

	for _, item := range s.list.Menu().Items {
		if item.ID == "watch" && !item.Disabled {
			t.Fatal("«Смотреть» must be disabled when no sources exist")
		}
	}
	s.list.Jump(indexOfDayActionMenu(s, "watch"))
	_, _ = s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if s.state == sessionStateFormat {
		t.Fatal("the format selector must never open without sources")
	}
	if !strings.Contains(s.status, "Обновить источники") {
		t.Fatalf("status after disabled watch attempt = %q, want the refresh hint", s.status)
	}
}

// TestSessionRefreshSources: «🔄 Обновить источники» re-runs the
// per-episode hydration for the current episode (the recovery path
// for transient eager failures) and reports progress while running.
func TestSessionRefreshSources(t *testing.T) {
	fix := &hydrateFixture{embeds: map[string]map[string][]string{"anilib": {}}}
	s := newEagerSession(t, fix, map[string][]contracts.Episode{
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
