package tui

// PR63 owner defect #1: local progress must be recorded at LAUNCH time
// (python extract_and_play calls db_service.save_progress BEFORE the
// player starts), not on a clean player exit. The owner watched an
// episode streaming and the library row stayed «0/13»: every early
// exit path — Esc popping the session mid-play (the late playedMsg is
// delivered to the screen above the session), Ctrl+C while the
// process-group-detached mpv outlives the TUI, a player error — drops
// the settle message and the save with it.

import (
	"context"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/storage"
)

// progressSession builds the owner-scenario session over REAL storage:
// a resumed row bound to (animego, u1) whose shikimori import left it
// at «0/13» — the exact row the owner saw stuck.
func progressSession(t *testing.T) (*sessionScreen, *storage.Store) {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seed := storage.AnimeProgress{
		Title: "Тайтл", SourceID: "animego", SourceURL: "u1",
		CurrentEpisode: "0", TotalEpisodes: 13, ShikimoriID: ptrTo(int64(21)),
		ShikimoriStatus: "watching", UpdatedAt: time.Now().UTC().Add(-time.Hour),
	}
	if err := store.Progress.Upsert(ctx, &seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	deps := &Deps{
		Episode: &fakeEpisode{
			episodes: testEpisodeSet(),
			streams: map[string]contracts.MediaStream{
				"[animego] Дубль 1": {Links: map[string]contracts.VideoSource{
					"1080": {URL: "v1080"},
				}},
			},
		},
		Playback: &fakePlayback{},
		Buffered: &realFileBuffered{Payload: []byte("x")},
		History:  &realHistory{store: store},
		Log:      discardLogger(),
	}
	primary := contracts.SearchResult{Title: "Тайтл", URL: "u1", SourceID: "animego"}
	s := newResumedSession(deps, primary, []contracts.SearchResult{primary}, seed)
	s.loadEpisodesSync()
	return s, store
}

// watchToLaunch drives the fresh watch to the launch command: watch →
// format pick → resolve settle → stream pick → ⭐ audio pick.
func watchToLaunch(t *testing.T, s *sessionScreen, format string) tea.Cmd {
	t.Helper()
	s.list.Jump(sessionActionIndex(s, "watch"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	ss.formatList.Jump(indexOfDayFormatList(ss, format))
	_, cmd := ss.Update(enter())
	sr, ok := cmd().(streamResolvedMsg)
	if !ok {
		t.Fatalf("stream resolve expected, got %T", cmd())
	}
	next, _ = ss.Update(sr)
	ss = next.(*sessionScreen)
	ss.qualityList.Jump(0)
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)
	ss.dubList.Jump(0)
	_, launch := ss.Update(enter())
	if launch == nil {
		t.Fatalf("the audio pick must launch playback")
	}
	return launch
}

// savedRow loads the (animego, u1) row the library display reads.
func savedRow(t *testing.T, store *storage.Store) *storage.AnimeProgress {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), statWriteTimeout)
	defer cancel()
	row, err := store.Progress.GetBySource(ctx, "animego", "u1")
	if err != nil {
		t.Fatalf("row must exist after the launch: %v", err)
	}
	return row
}

// TestStreamingWatchSavesProgressAtLaunch (PR63 #1): a streaming watch
// records the episode locally at LAUNCH — before the player runs and
// before any playedMsg settle. The owner's exit path (player closed
// mid-play, the session popped, the TUI quit) must not lose the save.
func TestStreamingWatchSavesProgressAtLaunch(t *testing.T) {
	s, store := progressSession(t)
	seedUpdatedAt := savedRow(t, store).UpdatedAt

	_ = watchToLaunch(t, s, "stream")

	row := savedRow(t, store)
	if row.CurrentEpisode != "1" {
		t.Fatalf("launch must record episode 1 locally, got %q", row.CurrentEpisode)
	}
	if row.VideoDub == nil || *row.VideoDub == "" {
		t.Fatalf("launch must record the dub preference, got %+v", row.VideoDub)
	}
	if !row.UpdatedAt.After(seedUpdatedAt) {
		t.Fatalf("launch must stamp a fresh updated_at, got %s (seed %s)",
			row.UpdatedAt, seedUpdatedAt)
	}
}

// TestHistoryDisplayReadsSavedRow (PR63 #1): the «Списки» surface
// renders the recorded progress — «Серия 1/13», not the stale «0/13».
func TestHistoryDisplayReadsSavedRow(t *testing.T) {
	s, _ := progressSession(t)
	_ = watchToLaunch(t, s, "stream")

	items, err := loadHistory(s.deps)
	if err != nil {
		t.Fatalf("load history: %v", err)
	}
	view := newHistoryListFromFiltered(s.deps, "", items).View().Content
	if !contains(view, "Серия 1/13") {
		t.Fatalf("the library row must show the saved progress:\n%s", view)
	}
}

// TestBufferedWatchSavesProgressAtLaunch (PR63 #1): the buffered path
// records progress at launch too — the download need not finish first.
func TestBufferedWatchSavesProgressAtLaunch(t *testing.T) {
	s, store := progressSession(t)

	_ = watchToLaunch(t, s, "buffer")

	if row := savedRow(t, store); row.CurrentEpisode != "1" {
		t.Fatalf("buffered launch must record episode 1 locally, got %q",
			row.CurrentEpisode)
	}
}
