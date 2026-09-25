package tui

// PR111 freshness tests (owner defect 2): stream links rotate
// server-side, but the session resolved ONCE and trusted the cached
// embeds/entries forever — the python version re-resolved fresh on
// every open. These tests pin the always-fresh pipeline: every
// episode open, redub pick and download probe re-resolves from the
// provider's CURRENT links; the ResolveStream cache is banned.
//
// The fixture simulates the rotation: every fetch or hydration round
// hands out a NEW embed URL, and ResolveStream records exactly which
// embeds it was asked to extract from, echoing the first into the
// playable URL — so every assertion can see whose links played.

import (
	"context"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// openThroughUI drives the real watch path: menu «Смотреть» → the
// format selector → «Потоковый» → the merged resolve settles into the
// quality picker.
func openThroughUI(t *testing.T, s *sessionScreen) {
	t.Helper()
	s.setState(sessionStateMenu)
	s.buildActionMenu()
	s.list.Jump(sessionActionIndex(s, "watch"))
	next, cmd := s.handleMenuKey(enter())
	if ss, ok := next.(*sessionScreen); ok {
		s = ss
	}
	if cmd != nil {
		runCmdTree(t, s, cmd) // the on-demand hydration of a sourceless episode
	}
	if s.state != sessionStateFormat {
		t.Fatalf("state = %s, want the format selector", s.state)
	}
	s.formatList.Jump(0) // «Потоковый»
	next, cmd = s.handleFormatKey(enter())
	if ss, ok := next.(*sessionScreen); ok {
		s = ss
	}
	runCmdTree(t, s, cmd)
	if s.state != sessionStateQuality {
		t.Fatalf("state = %s, want the quality picker after the resolve settle", s.state)
	}
}

// TestFreshResolveOnEveryOpen (owner defect 2): stream links rotate
// server-side; EVERY episode open must re-resolve from FRESH embeds —
// the cached links are never trusted for playback. Second open must
// not replay the first round's embeds, and the fresh round replaces
// the session cache.
func TestFreshResolveOnEveryOpen(t *testing.T) {
	s, fix := newRotatingSession(t)

	openThroughUI(t, s)
	if got := fix.lastResolve().embeds; len(got) != 1 || got[0] != "https://embed/h1" {
		t.Fatalf("first open resolved from %v, want the fresh hydration round h1 (not the cached fetch f1)", got)
	}
	if got := s.episodes["1"].RawEmbeds["[kodik] Kodik"]; len(got) != 1 || got[0] != "https://embed/h1" {
		t.Fatalf("session cache after open = %v, want the fresh round h1 replacing the fetch f1", got)
	}

	// The server rotated again (every hydration round yields a new
	// URL by construction). Second open: fresh links again.
	openThroughUI(t, s)
	if got := fix.lastResolve().embeds; len(got) != 1 || got[0] != "https://embed/h2" {
		t.Fatalf("second open resolved from %v, want the NEW round h2 — the cached h1 links are dead", got)
	}
	if got := s.episodes["1"].RawEmbeds["[kodik] Kodik"]; len(got) != 1 || got[0] != "https://embed/h2" {
		t.Fatalf("session cache after second open = %v, want h2", got)
	}
	if fix.resolveCount() != 2 {
		t.Fatalf("resolve calls = %d, want one per open", fix.resolveCount())
	}
}

// TestPickRedubResolvesFresh (the banned ResolveStream cache): a redub
// pick must NEVER launch the cached entries — it re-resolves fresh
// and launches the fresh URL (the provider rotated since the picker
// was filled).
func TestPickRedubResolvesFresh(t *testing.T) {
	s, _ := newRotatingSession(t)
	openThroughUI(t, s) // picker filled from the h1 round
	pb := s.deps.Playback.(*fakePlayback)

	s.videoDub, s.audioDub = "[kodik] Kodik", "[kodik] Kodik"
	next, cmd := s.pickRedub("[kodik] Kodik")
	if ss, ok := next.(*sessionScreen); ok {
		s = ss
	}
	runCmdTree(t, s, cmd)

	if len(pb.played) != 1 {
		t.Fatalf("plays = %d, want the redub pick to launch playback", len(pb.played))
	}
	if got := pb.played[0].URL; got != "play:https://embed/h2" {
		t.Fatalf("played URL = %q, want the FRESH round h2 — launching the cached %q is the banned cache", got, pb.played[0].URL)
	}
}

// TestRedubMenuItemResolvesFresh: «Сменить озвучку» opens the dub menu
// from a FRESH resolve (redubPending), never from the entries cached
// by an earlier round.
func TestRedubMenuItemResolvesFresh(t *testing.T) {
	s, fix := newRotatingSession(t)
	openThroughUI(t, s)
	n := fix.resolveCount()

	s.setState(sessionStateMenu)
	s.buildActionMenu()
	s.list.Jump(sessionActionIndex(s, "redub"))
	_, cmd := s.handleMenuKey(enter())
	if cmd == nil {
		t.Fatal("«Сменить озвучку» must schedule a fresh resolve, not open from the cached entries")
	}
	runCmdTree(t, s, cmd)
	if fix.resolveCount() <= n {
		t.Fatalf("resolve calls = %d, want the fresh round before the menu", fix.resolveCount())
	}
	if s.state != sessionStateRedub {
		t.Fatalf("state = %s, want the dub menu after the fresh settle", s.state)
	}
}

// TestDownloadLadderResolvesFresh: the buffered download path
// re-resolves fresh too — the ladder must probe the rotated embeds,
// not the episode's cached ones.
func TestDownloadLadderResolvesFresh(t *testing.T) {
	fix := &rotateFixture{}
	deps := &Deps{Episode: &rotateProvider{fix: fix}, Log: discardLogger()}
	ep := contracts.Episode{
		Num:   "1",
		RawID: "kodik:release-1",
		RawEmbeds: map[string][]string{
			"[kodik] Kodik": {"https://embed/stale"},
		},
	}
	if got := resolveDownloadDub(context.Background(), deps, ep, ""); got == "" {
		t.Fatal("download ladder found no viable dub")
	}
	rec := fix.lastResolve()
	if len(rec.embeds) != 1 || rec.embeds[0] == "https://embed/stale" {
		t.Fatalf("ladder resolved from %v, want the FRESH hydration round — the cached stale embed is the owner's dead-link defect", rec.embeds)
	}
}

// TestFreshRoundKeepsCachedWhenProviderYieldsNothing: the fresh round
// replaces a provider's embeds only when it actually returned links —
// a no-capability (eager) provider or an empty round keeps the cached
// embeds (fail-soft; the resolve remains the truth-teller).
func TestFreshRoundKeepsCachedWhenProviderYieldsNothing(t *testing.T) {
	fix := &rotateFixture{}
	// HydrateDubs yields NOTHING (the no-capability shape): embeds
	// stay whatever the fetch delivered.
	deps := &Deps{Episode: &noHydratorEpisode{rotateProvider: rotateProvider{fix: fix}}, Log: discardLogger()}
	group := []contracts.SearchResult{{Title: "Тайтл", URL: "u", SourceID: "kodik"}}
	s := NewSessionScreen(deps, group[0], group)
	settleInit(t, s)

	openThroughUI(t, s)
	rec := fix.lastResolve()
	if len(rec.embeds) != 1 || rec.embeds[0] != "https://embed/f1" {
		t.Fatalf("resolve consumed %v, want the CACHED fetch embeds (the provider has no hydration capability)", rec.embeds)
	}
}

// noHydratorEpisode keeps the rotating fetch but no hydration
// capability: HydrateDubs returns the episode unchanged (empty embeds
// — real.go's no-capability pass-through).
type noHydratorEpisode struct {
	rotateProvider
}

func (n *noHydratorEpisode) HydrateDubs(_ context.Context, _ string, episode contracts.Episode) (contracts.Episode, error) {
	return episode, nil
}
