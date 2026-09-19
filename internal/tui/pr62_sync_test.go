package tui

// PR62 owner defect #1: the watch-progress push must survive the whole
// chain — binding persistence, BOTH watch formats (streaming and
// buffered), and repeat watches (the local row must keep its shikimori
// columns so the push PATCHes instead of re-creating).

import (
	"context"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/storage"
)

// shikiResumeSession builds the owner-scenario session: a resumed
// record (the rebind flow's resume) bound to shikimori 21 with a known
// rate id 55, over REAL storage — the whole chain runs for real.
func shikiResumeSession(t *testing.T, store *storage.Store, shiki *fakeShiki) *sessionScreen {
	t.Helper()
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
		Shiki:    shiki,
		History:  &realHistory{store: store},
		Log:      discardLogger(),
	}
	primary := contracts.SearchResult{Title: "Тайтл", URL: "u1", SourceID: "animego"}
	group := []contracts.SearchResult{primary}
	rate := int64(55)
	rec := storage.AnimeProgress{
		ID: 7, Title: "Тайтл", SourceID: "animego", SourceURL: "u1",
		CurrentEpisode: "0", ShikimoriID: ptrTo(int64(21)),
		ShikimoriRateID: &rate, ShikimoriStatus: "planned", ShikimoriTitle: ptrTo("Тайтл"),
	}
	s := newResumedSession(deps, primary, group, rec)
	s.loadEpisodesSync()
	return s
}

// runLaunchBatch runs the launch batch and returns every settled
// message (the sync verdict rides alongside the play).
func runLaunchBatch(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return []tea.Msg{msg}
	}
	msgs := make(chan tea.Msg, len(batch))
	for _, c := range batch {
		go func(c tea.Cmd) { msgs <- c() }(c)
	}
	var out []tea.Msg
	deadline := time.After(10 * time.Second)
	for range batch {
		select {
		case m := <-msgs:
			out = append(out, m)
		case <-deadline:
			t.Fatalf("launch batch timed out; settled: %v", out)
		}
	}
	return out
}

// TestRebindFlowWatchPushesPatchWithStoredRate (PR62 #1, the owner
// scenario): watching ep1 of a resumed (rebound) record pushes
// {episodes: 1, status: watching} ONTO THE STORED RATE — the PATCH
// dispatch, not a duplicate-rate CREATE.
func TestRebindFlowWatchPushesPatchWithStoredRate(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Seed the rebound row exactly as the #2 pick leaves it: real
	// source key, needs_correction clear, rate id + status preserved.
	seed := storage.AnimeProgress{
		Title: "Тайтл", SourceID: "animego", SourceURL: "u1",
		CurrentEpisode: "0", ShikimoriID: ptrTo(int64(21)),
		ShikimoriRateID: ptrTo(int64(55)), ShikimoriStatus: "planned",
		ShikimoriTitle: ptrTo("Тайтл"), UpdatedAt: nowUTC(),
	}
	if err := store.Progress.Upsert(ctx, &seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	shiki := &fakeShiki{enabled: true}
	s := shikiResumeSession(t, store, shiki)
	msgs := runLaunchBatch(t, shikiPlayToLaunchCmd(t, s))

	synced := false
	for _, m := range msgs {
		if sm, ok := m.(shikiSyncedMsg); ok {
			synced = true
			if sm.err != nil {
				t.Fatalf("sync must succeed, got %v", sm.err)
			}
		}
	}
	if !synced {
		t.Fatalf("the launch batch must carry shikiSyncedMsg, got %v", msgs)
	}
	if len(shiki.epPushes) != 1 {
		t.Fatalf("exactly one push expected, got %+v", shiki.epPushes)
	}
	push := shiki.epPushes[0]
	if push.shikimoriID != 21 || push.rateID != 55 || push.episodes != 1 || push.status != "watching" {
		t.Fatalf("push = %+v, want PATCH {21 rate 55 episodes 1 watching}", push)
	}
}

// shikiPlayToLaunchCmd drives the fresh watch through the merged list
// and the ⭐ audio pick, returning the LAUNCH command (not executed).
func shikiPlayToLaunchCmd(t *testing.T, s *sessionScreen) tea.Cmd {
	t.Helper()
	s.list.Jump(sessionActionIndex(s, "watch"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	ss.formatList.Jump(indexOfDayFormatList(ss, "stream"))
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

// TestSessionBufferedWatchSyncsShikiProgress (PR62 #1): the BUFFERED
// watch path pushes the progress too — it used to return before the
// sync was ever scheduled.
func TestSessionBufferedWatchSyncsShikiProgress(t *testing.T) {
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
		Shiki:    &fakeShiki{enabled: true},
		History:  &fakeHistory{},
		Log:      discardLogger(),
	}
	group := []contracts.SearchResult{{Title: "Тайтл", URL: "u1", SourceID: "animego",
		Meta: map[string]any{"shikimori_id": int64(21)}}}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()

	s.list.Jump(sessionActionIndex(s, "watch"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	ss.formatList.Jump(indexOfDayFormatList(ss, "buffer"))
	_, formatCmd := ss.Update(enter())
	sr := formatCmd().(streamResolvedMsg)
	next, _ = ss.Update(sr)
	ss = next.(*sessionScreen)
	ss.qualityList.Jump(0)
	next, _ = ss.Update(enter())
	ss = next.(*sessionScreen)
	ss.dubList.Jump(0)
	_, launch := ss.Update(enter())
	if launch == nil {
		t.Fatalf("the audio pick must start the buffered download")
	}

	shiki := deps.Shiki.(*fakeShiki)
	for _, m := range runLaunchBatch(t, launch) {
		if _, ok := m.(bufferReadyMsg); ok {
			continue
		}
		if _, ok := m.(shikiSyncedMsg); ok {
			if len(shiki.epPushes) != 1 || shiki.epPushes[0].shikimoriID != 21 {
				t.Fatalf("buffered watch must push the progress, got %+v", shiki.epPushes)
			}
			return
		}
	}
	t.Fatalf("the buffered launch batch must carry shikiSyncedMsg")
}

// TestSavePlaybackPreservesShikiColumns (PR62 #1): recording playback
// over an existing row must not wipe its shikimori columns — the
// upsert template does not carry them, and a wiped rate id turns every
// later push into a duplicate-rate CREATE.
func TestSavePlaybackPreservesShikiColumns(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	h := &realHistory{store: store}

	rate := int64(55)
	seed := storage.AnimeProgress{
		Title: "Тайтл", SourceID: "animego", SourceURL: "u1",
		CurrentEpisode: "1", ShikimoriID: ptrTo(int64(21)),
		ShikimoriRateID: &rate, ShikimoriStatus: "watching",
		ShikimoriTitle: ptrTo("Тайтл"), Score: 8, TotalEpisodes: 13,
		UpdatedAt: nowUTC(),
	}
	if err := store.Progress.Upsert(ctx, &seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The session's saveProgress template: source keys + episode +
	// shikimori id, nothing else.
	rec := storage.AnimeProgress{
		Title: "Тайтл", SourceID: "animego", SourceURL: "u1",
		ShikimoriID: ptrTo(int64(21)), UpdatedAt: nowUTC(),
	}
	if err := h.SavePlayback(ctx, rec, "2", "dub", "aud"); err != nil {
		t.Fatalf("SavePlayback: %v", err)
	}

	rows, err := store.Progress.ListHistory(ctx, "", 0, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("exactly one row expected, got %d", len(rows))
	}
	got := rows[0]
	if got.ShikimoriRateID == nil || *got.ShikimoriRateID != 55 {
		t.Fatalf("rate id must survive the playback save, got %+v", got.ShikimoriRateID)
	}
	if got.ShikimoriStatus != "watching" || got.ShikimoriTitle == nil || got.Score != 8 || got.TotalEpisodes != 13 {
		t.Fatalf("shikimori columns must survive, got %+v", got)
	}
	if got.CurrentEpisode != "2" || got.VideoDub == nil || *got.VideoDub != "dub" {
		t.Fatalf("playback fields must update, got %+v", got)
	}
}

// TestSavePlaybackMovesShikiBoundRowOntoNewSource (PR62 #1, python
// save_progress parity): a shikimori-bound template missing on the
// (source_id, url) key adopts the existing bound row — the placeholder
// MOVES onto the real source instead of spawning a duplicate whose
// empty rate id breaks the next push.
func TestSavePlaybackMovesShikiBoundRowOntoNewSource(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	h := &realHistory{store: store}

	rate := int64(55)
	seed := storage.AnimeProgress{
		Title: "Ателье колдовских колпаков", SourceID: "shikimori", SourceURL: "21",
		CurrentEpisode: "0", ShikimoriID: ptrTo(int64(21)),
		ShikimoriRateID: &rate, ShikimoriStatus: "planned",
		NeedsCorrection: true, UpdatedAt: nowUTC(),
	}
	if err := store.Progress.Upsert(ctx, &seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := storage.AnimeProgress{
		Title: "Ателье колдовских колпаков", SourceID: "anilib", SourceURL: "u2",
		ShikimoriID: ptrTo(int64(21)), UpdatedAt: nowUTC(),
	}
	if err := h.SavePlayback(ctx, rec, "1", "dub", "aud"); err != nil {
		t.Fatalf("SavePlayback: %v", err)
	}

	rows, err := store.Progress.ListHistory(ctx, "", 0, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the bound row must MOVE, not duplicate: %d rows", len(rows))
	}
	got := rows[0]
	if got.SourceID != "anilib" || got.SourceURL != "u2" {
		t.Fatalf("row must sit on the watched source, got %s/%s", got.SourceID, got.SourceURL)
	}
	if got.ShikimoriRateID == nil || *got.ShikimoriRateID != 55 {
		t.Fatalf("rate id must survive the move, got %+v", got.ShikimoriRateID)
	}
	if got.NeedsCorrection {
		t.Fatalf("watching a title clears the placeholder flag, got %+v", got)
	}

	// The next push finds the row and PATCHes the stored rate.
	looked, err := h.GetByShikimoriID(ctx, 21)
	if err != nil || looked == nil {
		t.Fatalf("GetByShikimoriID after the move: %v, %+v", err, looked)
	}
	if looked.ShikimoriRateID == nil || *looked.ShikimoriRateID != 55 {
		t.Fatalf("the push lookup must see the rate id, got %+v", looked.ShikimoriRateID)
	}
}
