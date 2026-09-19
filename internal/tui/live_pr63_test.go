//go:build live

package tui

// LIVE probes for PR63 (local progress at launch, «⏭ След.»
// auto-launch). Excluded from the hermetic default suite by the `live`
// build tag. Run manually:
//
//	go test -tags live -run TestLivePR63 -count=1 -v ./internal/tui/
//
// Real providers, real store, real mpv playback. mpv is bounded by a
// delegating playback wrapper: it hands the REAL player a context that
// expires after cap seconds — the player's own SIGTERM→SIGKILL ladder
// closes the process group. That is precisely the owner's "watched a
// bit, closed the player" path the launch-time save must survive.
// The changed-dubs fallback probe mutates the in-memory episode
// aggregate (a live provider cannot be forced to change its dub list).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/storage"
)

// cappedPlayback delegates to the real playback service with a hard
// watch budget; the deadline expiry counts as "the user closed the
// player". Every launch is recorded with its mpv title.
type cappedPlayback struct {
	*realPlayback
	cap  time.Duration
	seen []string
}

func (c *cappedPlayback) Play(ctx context.Context, req PlayRequest) error {
	c.seen = append(c.seen, req.Title)
	pctx, cancel := context.WithTimeout(context.Background(), c.cap)
	defer cancel()
	err := c.realPlayback.Play(pctx, req)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// live63Session fans the query out over the real roster, picks the
// sorted-first primary, seeds the owner's imported row (0/13) on the
// primary's source key and opens the resumed session (the PR62 #3
// re-entry shape). Episode lists load over the real network.
func live63Session(t *testing.T, deps *Deps, query string, store *storage.Store) *sessionScreen {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	bySource := liveSearch(t, deps, query)
	if len(bySource) < 1 {
		t.Fatalf("no live results for %q", query)
	}
	group := make([]contracts.SearchResult, 0, len(bySource))
	for _, r := range bySource {
		group = append(group, r)
	}
	group = stableGroup(group)
	primary := group[0]
	t.Logf("primary source: %s/%s (%q)", primary.SourceID, primary.URL, primary.Title)

	// The owner's imported row, keyed to the watched source (what the
	// PR62 #2/#3 flows leave behind), stuck at «0/13».
	rec := storage.AnimeProgress{
		Title: primary.Title, SourceID: primary.SourceID, SourceURL: primary.URL,
		CurrentEpisode: "0", TotalEpisodes: 13, ShikimoriID: ptrTo(int64(21)),
		ShikimoriStatus: "watching", UpdatedAt: nowUTC(),
	}
	if err := store.Progress.Upsert(ctx, &rec); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	s := newResumedSession(deps, primary, group, rec)
	s.loadEpisodesSync()
	t.Logf("merged episodes: %v", s.order)
	if len(s.order) < 2 {
		t.Fatalf("need a 2+ episode title for the next-proof, got %v", s.order)
	}
	return s
}

// liveWatchToLaunch drives the real watch to the launch command,
// running the on-demand hydration round when the first watch finds an
// empty tier-1 list (PR43 model).
func liveWatchToLaunch(t *testing.T, s *sessionScreen, format string) tea.Cmd {
	t.Helper()
	s.list.Jump(sessionActionIndex(s, "watch"))
	next, cmd := s.Update(enter())
	ss := next.(*sessionScreen)
	if ss.state != sessionStateFormat {
		// The hydration round: settle it and press «Смотреть» again.
		if cmd == nil {
			t.Fatalf("watch must either open the format list or hydrate")
		}
		msg := cmd()
		next, _ = ss.Update(msg)
		ss = next.(*sessionScreen)
		s.list.Jump(sessionActionIndex(s, "watch"))
		next, _ = ss.Update(enter())
		ss = next.(*sessionScreen)
		if ss.state != sessionStateFormat {
			t.Fatalf("the hydrated watch must open the format list, state=%v", ss.state)
		}
	}
	ss.formatList.Jump(indexOfDayFormatList(ss, format))
	_, cmd = ss.Update(enter())
	msg := cmd()
	sr, ok := msg.(streamResolvedMsg)
	if !ok {
		t.Fatalf("stream resolve expected, got %T", msg)
	}
	if sr.err != nil {
		t.Fatalf("live resolve failed: %v", sr.err)
	}
	next, _ = ss.Update(sr)
	ss = next.(*sessionScreen)
	if len(sr.entries) == 0 {
		t.Fatalf("no live stream entries for ep %q", ss.currentEpisode())
	}
	t.Logf("ep %s resolved %d entries, best: %s", ss.currentEpisode(),
		len(sr.entries), ss.streamEntryLabel(sr.entries[0]))
	ss.qualityList.Jump(0)
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)
	ss.dubList.Jump(0) // «⭐ Как видео»
	_, launch := ss.Update(enter())
	if launch == nil {
		t.Fatalf("the audio pick must launch playback")
	}
	return launch
}

// rowOf loads the watched-source row for assertions and printing.
func rowOf(t *testing.T, s *sessionScreen) *storage.AnimeProgress {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), statWriteTimeout)
	defer cancel()
	row, err := s.deps.History.(*realHistory).store.Progress.GetBySource(
		ctx, s.primary.SourceID, s.primary.URL)
	if err != nil {
		t.Fatalf("load row: %v", err)
	}
	return row
}

// libraryView renders the «Списки» row surface exactly as the owner
// sees it after the watch.
func libraryView(t *testing.T, s *sessionScreen) string {
	t.Helper()
	items, err := loadHistory(s.deps)
	if err != nil {
		t.Fatalf("load history: %v", err)
	}
	return NewHistoryList(s.deps, "", items).View().Content
}

// TestLivePR63LaunchSaveAndNextAutoLaunch: watching an episode records
// the local row AT LAUNCH (before the player even runs), the library
// surface renders it, and «⏭ След.» auto-launches the next episode
// over the remembered dubs.
func TestLivePR63LaunchSaveAndNextAutoLaunch(t *testing.T) {
	_, deps := liveDeps(t)
	store := deps.History.(*realHistory).store
	pb, ok := deps.Playback.(*realPlayback)
	if !ok {
		t.Fatalf("deps.Playback is %T, want *realPlayback", deps.Playback)
	}
	capped := &cappedPlayback{realPlayback: pb, cap: 6 * time.Second}
	deps.Playback = capped

	query := "черная лагуна"
	s := live63Session(t, deps, query, store)

	// --- Defect 1: the launch-time save over a REAL streaming watch.
	launch := liveWatchToLaunch(t, s, "stream")
	row := rowOf(t, s) // the save ran synchronously inside Update — no player, no settle yet
	t.Logf("ROW AFTER LAUNCH (before the player ran): ep=%q dub=%q updated=%s",
		row.CurrentEpisode, derefStr(row.VideoDub, ""), row.UpdatedAt.Format(time.RFC3339))
	if row.CurrentEpisode != s.currentEpisode() {
		t.Fatalf("launch must record the watched episode, got %q want %q",
			row.CurrentEpisode, s.currentEpisode())
	}
	t.Logf("LAUNCH LINE: %q", s.status)

	t.Logf("executing the play command — real mpv, capped at %s…", capped.cap)
	var played *playedMsg
	for _, m := range runLaunchBatch(t, launch) {
		if pm, ok := m.(playedMsg); ok {
			played = &pm
		}
	}
	if played == nil || played.err != nil {
		t.Fatalf("capped live play failed: %v", played)
	}
	// The runtime delivers every settled message; without the
	// playedMsg the screen stays in sessionStatePlaying (which eats
	// keys) and «⏭ След.» never reaches the menu.
	upd, _ := s.Update(*played)
	s = upd.(*sessionScreen)

	t.Logf("mpv titles played so far: %v", capped.seen)
	if len(capped.seen) != 1 {
		t.Fatalf("exactly one player launch expected, got %v", capped.seen)
	}

	// The library surface reads the saved row.
	view := libraryView(t, s)
	want := fmt.Sprintf("Серия %s/13", row.CurrentEpisode)
	if !contains(view, want) {
		t.Fatalf("library must render %q:\n%s", want, view)
	}
	t.Logf("LIBRARY ROW RENDERED: %q", firstListLine(view))

	// --- Defect 2: «⏭ След.» auto-launches the next episode.
	dub := s.videoDub
	s.list.Jump(sessionActionIndex(s, "next"))
	next, cmd := s.Update(enter())
	ss := next.(*sessionScreen)
	t.Logf("after «⏭ След.»: ep=%q state=%v status=%q", ss.currentEpisode(), ss.state, ss.status)
	if ss.currentEpisode() == row.CurrentEpisode || cmd == nil {
		t.Fatalf("next must advance AND carry the auto-launch resolve")
	}
	auto := cmd().(streamResolvedMsg)
	if auto.scope != dub {
		t.Fatalf("the remembered dub must scope the auto-resolve, got %q", auto.scope)
	}
	next, cmd = ss.Update(auto)
	ss = next.(*sessionScreen)
	if ss.state != sessionStatePlaying || cmd == nil {
		t.Fatalf("the settle must auto-launch, state=%v", ss.state)
	}
	t.Logf("AUTO-LAUNCH LINE: %q", ss.status)
	played = nil
	for _, m := range runLaunchBatch(t, cmd) {
		if pm, ok := m.(playedMsg); ok {
			played = &pm
		}
	}
	if played == nil || played.err != nil {
		t.Fatalf("the auto-launched play failed: %v", played)
	}
	next, _ = ss.Update(*played)
	ss = next.(*sessionScreen)
	if len(capped.seen) != 2 {
		t.Fatalf("the player must have launched twice, titles: %v", capped.seen)
	}
	t.Logf("AUTO-LAUNCHED MPV TITLE: %q", capped.seen[1])
	if !strings.Contains(capped.seen[1], "- "+ss.currentEpisode()) {
		t.Fatalf("the auto-launch must play ep %q, title %q",
			ss.currentEpisode(), capped.seen[1])
	}
	row2 := rowOf(t, s)
	t.Logf("ROW AFTER THE AUTO-LAUNCH: ep=%q", row2.CurrentEpisode)
	if row2.CurrentEpisode != ss.currentEpisode() {
		t.Fatalf("the auto-launch must record the next episode, got %q", row2.CurrentEpisode)
	}

	// --- Defect 2 fallback: the used source gone from the next
	// episode — typed note + the usual selection (in-memory mutation;
	// a live provider cannot be forced to drop a dub).
	ahead := s.order[s.currentIdx+1]
	ep := s.episodes[ahead]
	delete(ep.RawEmbeds, s.videoDub)
	s.episodes[ahead] = ep
	s.list.Jump(sessionActionIndex(s, "next"))
	next, _ = s.Update(enter())
	ss = next.(*sessionScreen)
	t.Logf("CHANGED-DUBS FALLBACK: ep=%q status=%q state=%v",
		ss.currentEpisode(), ss.status, ss.state)
	if !contains(ss.status, "Прошлые настройки недоступны") || ss.state != sessionStateQuality {
		t.Fatalf("the fallback must show the typed note + the stream selection, status=%q state=%v",
			ss.status, ss.state)
	}
}

// firstListLine extracts the first rendered list row from a menu view.
func firstListLine(view string) string {
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "Серия") {
			return "\n" + line
		}
	}
	return ""
}
