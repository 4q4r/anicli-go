package tui

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/buffered"
	"github.com/an0nx/anicli-go/internal/contracts"
)

// realFileBuffered is a BufferedService fake. It writes a real
// on-disk file so the TUI state machine exercises the actual file
// lifecycle (buffer → play local path → delete), but it does NOT hop
// through loopback HTTP: the earlier real-GET variant raced httptest
// connection teardown in shared fd-number space (EBADF on
// write/close under -race, ~1/10 runs). Transport realism lives in
// internal/buffered's own httptest suite and the torrent E2E; the
// state machine under test here only needs the file lifecycle.
type realFileBuffered struct {
	mu    sync.Mutex
	dirs  []string
	wrote map[string][]byte
	// Payload is the file content every Buffer call writes.
	Payload []byte
}

func (b *realFileBuffered) Buffer(ctx context.Context, src buffered.Source, progress func(buffered.Progress)) (buffered.Handle, error) {
	// Cancellation before the download starts settles immediately
	// (the cancel-path contract the session must surface).
	if err := ctx.Err(); err != nil {
		return buffered.Handle{}, err
	}
	dir, err := os.MkdirTemp("", "anicli-test-buffer-")
	if err != nil {
		return buffered.Handle{}, err
	}
	b.mu.Lock()
	b.dirs = append(b.dirs, dir)
	b.mu.Unlock()
	path := filepath.Join(dir, "video.mp4")
	payload := append([]byte(nil), b.Payload...)
	if err := os.WriteFile(path, payload, 0o600); err != nil { //nolint:gosec // path is our MkdirTemp dir — no user-controlled inclusion
		_ = os.RemoveAll(dir)
		return buffered.Handle{}, err
	}
	b.mu.Lock()
	if b.wrote == nil {
		b.wrote = map[string][]byte{}
	}
	b.wrote[path] = payload
	b.mu.Unlock()
	if progress != nil {
		progress(buffered.Progress{Done: int64(len(payload)), Total: int64(len(payload))})
	}
	return buffered.NewHandle(path, func() { _ = os.RemoveAll(dir) }), nil
}

func (b *realFileBuffered) CleanupAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, dir := range b.dirs {
		_ = os.RemoveAll(dir)
	}
}

var _ BufferedService = (*realFileBuffered)(nil)

// newBufferedSession builds a one-episode session with real embeds and
// the real-file buffered fake: the resolved stream URL is inert (the
// fake writes its payload straight to disk), keeping the state-machine
// test free of loopback-HTTP fd churn.
func newBufferedSession(t *testing.T) (*sessionScreen, *realFileBuffered, *fakePlayback, string) {
	t.Helper()
	payload := "fake-mp4-bytes"

	deps := &Deps{
		Episode: &fakeEpisode{
			episodes: map[string][]contracts.Episode{
				"anidub": {{
					Num:       "1",
					RawID:     "1",
					RawEmbeds: map[string][]string{"AniDUB": {"embed"}},
				}},
			},
			streams: map[string]contracts.MediaStream{
				"[anidub] AniDUB": {Links: map[string]contracts.VideoSource{
					"720": {URL: "https://cdn.example/ep1.mp4"},
				}},
			},
		},
		Playback: &fakePlayback{},
		Buffered: &realFileBuffered{Payload: []byte(payload)},
		Log:      discardLogger(),
	}
	group := []contracts.SearchResult{{Title: "Тайтл", URL: "u", SourceID: "anidub"}}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()
	return s, deps.Buffered.(*realFileBuffered), deps.Playback.(*fakePlayback), payload
}

// TestWatchOpensFormatSelector pins the PR44 entry interaction:
// the action menu has NO «Формат:» toggle item, and «▶ Смотреть»
// opens a selector with EXACTLY two items («Потоковый», «Буферный»).
func TestWatchOpensFormatSelector(t *testing.T) {
	s, _, _, _ := newBufferedSession(t)

	if v := s.View().Content; strings.Contains(v, "Формат:") {
		t.Fatalf("menu view = %q, the format toggle item must be gone", v)
	}
	s.list.Jump(indexOfDayActionMenu(s, "watch"))
	if _, cmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil {
		t.Fatalf("watch must open the selector in place, got cmd %T", cmd)
	}
	if s.state != sessionStateFormat {
		t.Fatalf("state = %s, want the format selector", s.state)
	}
	items := s.formatList.Menu().Items
	if len(items) != 3 { // two formats + the pinned Back row
		t.Fatalf("selector rows = %d, want 3 (two formats + Back)", len(items))
	}
	if items[0].Label != "Потоковый" || items[1].Label != "Буферный" {
		t.Fatalf("selector labels = %q, %q; want «Потоковый», «Буферный»",
			items[0].Label, items[1].Label)
	}
}

// TestFormatSelectorStreamPickProceeds: Enter on «Потоковый» starts
// the watch pipeline in streaming mode — the merged stream resolve
// (PR61) schedules immediately for a fresh session.
func TestFormatSelectorStreamPickProceeds(t *testing.T) {
	s, _, _, _ := newBufferedSession(t)

	s.list.Jump(indexOfDayActionMenu(s, "watch"))
	_, _ = s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	s.formatList.Jump(0) // «Потоковый»
	_, cmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("stream pick must schedule the stream resolve")
	}
	if s.buffered {
		t.Fatal("stream pick must clear the buffered mode")
	}
	if s.state != sessionStateQuality {
		t.Fatalf("state = %s, want the merged stream list", s.state)
	}
	// The resolve settles into the merged entries (PR61).
	msg := cmd()
	sr, ok := msg.(streamResolvedMsg)
	if !ok {
		t.Fatalf("stream resolve expected, got %T", msg)
	}
	s.Update(sr)
	if len(s.streamEntries) != 1 {
		t.Fatalf("merged entries = %d, want 1", len(s.streamEntries))
	}
}

// TestFormatSelectorBufferedPickProceeds: Enter on «Буферный» arms the
// buffered mode and starts the same merged resolve (PR61).
func TestFormatSelectorBufferedPickProceeds(t *testing.T) {
	s, _, _, _ := newBufferedSession(t)

	s.list.Jump(indexOfDayActionMenu(s, "watch"))
	_, _ = s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	s.formatList.Jump(1) // «Буферный»
	_, cmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("buffered pick must schedule the stream resolve")
	}
	if !s.buffered {
		t.Fatal("buffered pick must arm the buffered mode")
	}
	if s.state != sessionStateQuality {
		t.Fatalf("state = %s, want the merged stream list", s.state)
	}
}

// TestFormatSelectorBufferedUnavailable: without a BufferedService the
// «Буферный» pick explains honestly and the selector stays open so
// «Потоковый» remains pickable.
func TestFormatSelectorBufferedUnavailable(t *testing.T) {
	s, _, _, _ := newBufferedSession(t)
	s.deps.Buffered = nil

	s.list.Jump(indexOfDayActionMenu(s, "watch"))
	_, _ = s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	s.formatList.Jump(1) // «Буферный»
	if _, cmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil {
		t.Fatalf("unavailable pick must stay in place, got cmd %T", cmd)
	}
	if s.buffered {
		t.Fatal("buffered mode must not arm without the service")
	}
	if s.state != sessionStateFormat {
		t.Fatalf("state = %s, want the selector to stay open", s.state)
	}
	if !strings.Contains(s.status, "Буферный режим недоступен") {
		t.Fatalf("status = %q, want the honest unavailability note", s.status)
	}
}

// TestFormatSelectorBackReturnsToMenu: Esc/Back from the selector
// returns to the episode menu WITHOUT playing anything.
func TestFormatSelectorBackReturnsToMenu(t *testing.T) {
	s, bufSrv, playback, _ := newBufferedSession(t)

	s.list.Jump(indexOfDayActionMenu(s, "watch"))
	_, _ = s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if _, cmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEsc}); cmd != nil {
		t.Fatalf("esc must stay in place, got cmd %T", cmd)
	}
	if s.state != sessionStateMenu {
		t.Fatalf("state = %s, want the menu after Esc", s.state)
	}
	// The pinned Back row behaves the same.
	s.list.Jump(indexOfDayActionMenu(s, "watch"))
	_, _ = s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	s.formatList.Jump(indexOfDayFormatList(s, "buffer"))
	s.formatList.Jump(len(s.formatList.Menu().Items) - 1) // Back row
	if _, cmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil {
		t.Fatalf("back pick cmd: %T", cmd)
	}
	if s.state != sessionStateMenu {
		t.Fatalf("state = %s, want the menu after Back", s.state)
	}
	if len(playback.played) != 0 || bufSrv.wrote != nil {
		t.Fatal("cancelling the selector must not play or buffer")
	}
}

// indexOfDayFormatList returns the cursor index of a format selector
// item id.
func indexOfDayFormatList(s *sessionScreen, id string) int {
	for i, item := range s.formatList.Menu().Items {
		if item.ID == id {
			return i
		}
	}
	return 0
}

// TestSessionBufferedWatchDownloadsPlaysCleans: in buffered mode the
// watch flow buffers the resolved stream to a LOCAL file, plays THAT
// path and deletes the file once the player exits.
func TestSessionBufferedWatchDownloadsPlaysCleans(t *testing.T) {
	s, bufSrv, playback, payload := newBufferedSession(t)

	// Existence at play time is polled (transient fd churn must not
	// fail the check); content equality is asserted against the fake's
	// recorded copy — written BEFORE the handle was returned, so a
	// present file at play time holds exactly those bytes.
	var playStatErr error
	playback.playHook = func(req PlayRequest) error {
		playStatErr = statUntilPresent(req.URL, 2*time.Second)
		return nil
	}

	// ▶ Смотреть → format selector («Буферный») → merged stream pick →
	// «⭐ Как видео».
	s.list.Jump(indexOfDayActionMenu(s, "watch"))
	_, _ = s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	s.formatList.Jump(indexOfDayFormatList(s, "buffer"))
	_, formatCmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	pickMergedStream(t, s, formatCmd, 0)
	cmd := confirmAudioStar(t, s)
	if cmd == nil {
		t.Fatal("audio pick must start the buffered download")
	}
	if s.state != sessionStateBuffering {
		t.Fatalf("state = %s, want buffering", s.state)
	}
	// The quality pick arms a batch: the download cmd plus the
	// progress pump. Run the batch and take the bufferReadyMsg.
	ready, ok := runBufferedBatch(t, cmd).(bufferReadyMsg)
	if !ok {
		t.Fatalf("download settled %T (%v), want bufferReadyMsg", ready, ready)
	}
	// A settle that carries an error (or a stale generation) returns
	// no playback command — name its cause in the failure instead of
	// an uninformative fatal.
	if _, playCmd := s.Update(ready); playCmd == nil {
		t.Fatalf("ready settle must schedule the local playback: settle err=%v, gen=%d/%d, state=%s, status=%q",
			ready.err, ready.gen, s.bufferGen, s.state, s.status)
	} else {
		if s.state != sessionStatePlaying {
			t.Fatalf("state = %s, want playing", s.state)
		}
		// The playback cmd blocks through the fake player, then cleans
		// up.
		pm, ok := playBuffered(s, playCmd).(playedMsg)
		if !ok {
			t.Fatalf("playback settled %T, want playedMsg", pm)
		}
		s.Update(pm)
	}
	if len(playback.played) != 1 {
		t.Fatalf("player calls = %d, want 1", len(playback.played))
	}
	req := playback.played[0]
	if !strings.HasPrefix(req.URL, string(os.PathSeparator)) || !strings.HasSuffix(req.URL, ".mp4") {
		t.Fatalf("player URL = %q, want a local .mp4 path", req.URL)
	}
	if playStatErr != nil {
		t.Fatalf("buffered file missing at play time: %v", playStatErr)
	}
	bufSrv.mu.Lock()
	written := append([]byte(nil), bufSrv.wrote[req.URL]...)
	bufSrv.mu.Unlock()
	if !bytes.Equal(written, []byte(payload)) {
		t.Fatalf("buffered payload mismatch: the fake wrote %d bytes, want %d", len(written), len(payload))
	}
	// After the playedMsg the temp file is gone (cleanup ran).
	if err := statUntilGone(req.URL, 2*time.Second); err != nil {
		t.Fatalf("buffered file survived playback: %v", err)
	}
}

// statUntilPresent polls until the path stats OK (or the deadline);
// transient fd churn (EBADF) in a loaded test process must not fail
// the existence check.
func statUntilPresent(path string, deadline time.Duration) error {
	var lastErr error
	lim := time.Now().Add(deadline)
	for {
		_, lastErr = os.Stat(path)
		if lastErr == nil {
			return nil
		}
		if os.IsNotExist(lastErr) || time.Now().After(lim) {
			return lastErr
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// statUntilGone polls until the path is gone (or the deadline).
func statUntilGone(path string, deadline time.Duration) error {
	lim := time.Now().Add(deadline)
	for {
		_, err := os.Stat(path)
		if os.IsNotExist(err) {
			return nil
		}
		if time.Now().After(lim) {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestFormatBufferedProgress: the minimal progress line covers the
// byte path (percent + speed) and the HLS segment path.
func TestFormatBufferedProgress(t *testing.T) {
	byBytes := formatBufferedProgress(buffered.Progress{Done: 500 << 20, Total: 1000 << 20, SpeedBPS: 3 << 20})
	if !strings.Contains(byBytes, "50%") || !strings.Contains(byBytes, "3.0 МБ/с") {
		t.Fatalf("byte progress = %q, want 50%% + speed", byBytes)
	}
	bySegments := formatBufferedProgress(buffered.Progress{SegmentsDone: 5, SegmentsTotal: 20, SpeedBPS: 1 << 20})
	if !strings.Contains(bySegments, "25%") || !strings.Contains(bySegments, "сегмент 5/20") {
		t.Fatalf("segment progress = %q, want 25%% + segment counts", bySegments)
	}
	// Segment-based samples carry no byte counts, so the byte speed is
	// unknown — a bogus «0.0 МБ/с» must be suppressed (PR43 review).
	bySegmentsZero := formatBufferedProgress(buffered.Progress{SegmentsDone: 1, SegmentsTotal: 20})
	if strings.Contains(bySegmentsZero, "МБ/с") {
		t.Fatalf("segment progress with zero speed = %q, want no speed segment", bySegmentsZero)
	}
	unknown := formatBufferedProgress(buffered.Progress{Done: 512, SpeedBPS: 0})
	if !strings.Contains(unknown, "512 Б") {
		t.Fatalf("unknown-total progress = %q, want the byte count", unknown)
	}
}

// TestSessionBufferedCancelCleans: Esc during buffering cancels the
// download, returns to the menu with the verdict and leaves no file.
func TestSessionBufferedCancelCleans(t *testing.T) {
	s, _, playback, _ := newBufferedSession(t)
	s.list.Jump(indexOfDayActionMenu(s, "watch"))
	_, _ = s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	s.formatList.Jump(indexOfDayFormatList(s, "buffer"))
	_, formatCmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	pickMergedStream(t, s, formatCmd, 0)
	cmd := confirmAudioStar(t, s)
	if cmd == nil {
		t.Fatal("audio pick must start the buffered download")
	}
	// Esc cancels while the download runs.
	if _, cancelCmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEsc}); cancelCmd != nil {
		t.Fatalf("cancel must stay in place, got cmd %T", cancelCmd)
	}
	if s.state != sessionStateMenu {
		t.Fatalf("state = %s, want menu after cancel", s.state)
	}
	if !strings.Contains(s.status, "Буферизация отменена") {
		t.Fatalf("status = %q, want the cancel verdict", s.status)
	}
	// The download goroutine settles (cancelled) — its message is
	// stale and must not disturb the menu.
	if ready, ok := runBufferedBatch(t, cmd).(bufferReadyMsg); ok {
		s.Update(ready)
		if s.state != sessionStateMenu || len(playback.played) != 0 {
			t.Fatal("stale buffer settle must not start playback")
		}
	}
}

// playBuffered runs the playback command and returns its settle
// message.
func playBuffered(s *sessionScreen, cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	return cmd()
}

// runBufferedBatch executes the buffering command batch (download +
// progress pump) concurrently and returns the bufferReadyMsg — the
// pump's progress/end messages are drained in the background. The wait
// is a generous condition poll (10 s deadline); on timeout it reports
// which messages DID arrive, so a partial settle names its cause.
func runBufferedBatch(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return msg
	}
	msgs := make(chan tea.Msg, len(batch))
	for _, c := range batch {
		go func(c tea.Cmd) { msgs <- c() }(c)
	}
	deadline := time.After(10 * time.Second)
	var arrived []string
	for range batch {
		select {
		case m := <-msgs:
			if ready, ok := m.(bufferReadyMsg); ok {
				return ready
			}
			arrived = append(arrived, fmt.Sprintf("%T", m))
		case <-deadline:
			t.Fatalf("buffered batch timed out; settled so far: %v", arrived)
		}
	}
	return nil
}

// pickFormat drives the watch entry through the PR44 format selector:
// Enter on «Смотреть», then the chosen mode («Потоковый» unless
// buffered).
func pickFormat(t *testing.T, s *sessionScreen, buffered bool) {
	t.Helper()
	if _, cmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil {
		t.Fatalf("watch cmd: %T", cmd)
	}
	if s.state != sessionStateFormat {
		t.Fatalf("state = %s, want the format selector", s.state)
	}
	idx := 0
	if buffered {
		idx = indexOfDayFormatList(s, "buffer")
	}
	s.formatList.Jump(idx)
	if _, cmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil {
		t.Fatalf("format pick cmd: %T", cmd)
	}
}

// pickMergedStream settles the post-format merged resolve (PR61) and
// picks the entry at idx, landing on the audio prompt.
func pickMergedStream(t *testing.T, s *sessionScreen, cmd tea.Cmd, idx int) {
	t.Helper()
	if cmd == nil {
		t.Fatal("format pick must schedule the stream resolve")
	}
	sr, ok := cmd().(streamResolvedMsg)
	if !ok {
		t.Fatalf("stream resolve expected, got %T", cmd())
	}
	s.Update(sr)
	if s.state != sessionStateQuality {
		t.Fatalf("state = %s, want the merged stream list", s.state)
	}
	s.qualityList.Jump(idx)
	s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if s.state != sessionStateDubAudio {
		t.Fatalf("state = %s, want the audio prompt", s.state)
	}
}

// confirmAudioStar confirms the «⭐ Как видео» row and returns the
// launched command (the buffered batch or the play cmd).
func confirmAudioStar(t *testing.T, s *sessionScreen) tea.Cmd {
	t.Helper()
	if s.state != sessionStateDubAudio {
		t.Fatalf("state = %s, want the audio prompt", s.state)
	}
	s.dubList.Jump(0)
	_, cmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	return cmd
}
