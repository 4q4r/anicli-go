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

// TestSessionFormatToggle: the menu carries «Формат: [потоковый]» and
// Enter toggles it to буферный (per-session preference).
func TestSessionFormatToggle(t *testing.T) {
	s, _, _, _ := newBufferedSession(t)

	if v := s.View().Content; !strings.Contains(v, "Формат: [потоковый]") {
		t.Fatalf("menu view = %q, want the streaming format item", v)
	}
	s.list.Jump(indexOfDayActionMenu(s, "format"))
	if _, cmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil {
		t.Fatalf("toggle must stay in place, got cmd %T", cmd)
	}
	if v := s.View().Content; !strings.Contains(v, "Формат: [буферный]") {
		t.Fatalf("menu view = %q, want the buffered format item", v)
	}
	// buildActionMenu resets the cursor; aim at the toggle again.
	s.list.Jump(indexOfDayActionMenu(s, "format"))
	if _, cmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil {
		t.Fatalf("second toggle must stay in place, got cmd %T", cmd)
	}
	if v := s.View().Content; !strings.Contains(v, "Формат: [потоковый]") {
		t.Fatalf("menu view = %q, want the streaming format item back", v)
	}
}

// TestSessionBufferedWatchDownloadsPlaysCleans: in buffered mode the
// watch flow buffers the resolved stream to a LOCAL file, plays THAT
// path and deletes the file once the player exits.
func TestSessionBufferedWatchDownloadsPlaysCleans(t *testing.T) {
	s, bufSrv, playback, payload := newBufferedSession(t)
	s.buffered = true
	s.buildActionMenu()

	// Existence at play time is polled (transient fd churn must not
	// fail the check); content equality is asserted against the fake's
	// recorded copy — written BEFORE the handle was returned, so a
	// present file at play time holds exactly those bytes.
	var playStatErr error
	playback.playHook = func(req PlayRequest) error {
		playStatErr = statUntilPresent(req.URL, 2*time.Second)
		return nil
	}

	// ▶ Смотреть → dub pickers (embeds exist) → straight to quality.
	pickVideoDub(t, s, "[anidub] AniDUB")
	pickAudioDub(t, s)

	// Enter on «Авто» starts the buffered pipeline.
	s.qualityList.Jump(0)
	_, cmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("quality pick must start the buffered download")
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
	s.buffered = true
	s.buildActionMenu()
	pickVideoDub(t, s, "[anidub] AniDUB")
	pickAudioDub(t, s)
	s.qualityList.Jump(0)
	_, cmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("quality pick must start the buffered download")
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

// pickVideoDub drives the watch flow to the video dub pick.
func pickVideoDub(t *testing.T, s *sessionScreen, dub string) {
	t.Helper()
	if _, cmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil {
		t.Fatalf("watch cmd: %T", cmd)
	}
	if s.state != sessionStateDubVideo {
		t.Fatalf("state = %s, want dub video", s.state)
	}
	idx := -1
	for i, item := range s.dubList.Menu().Items {
		if item.Value == dub {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("dub %q not in picker", dub)
	}
	s.dubList.Jump(idx)
	if _, cmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil {
		t.Fatalf("video dub cmd: %T", cmd)
	}
}

// pickAudioDub confirms the audio dub pick («⭐ Как видео»); the pick
// resolves the stream synchronously, entering the quality picker.
func pickAudioDub(t *testing.T, s *sessionScreen) {
	t.Helper()
	if s.state != sessionStateDubAudio {
		t.Fatalf("state = %s, want dub audio", s.state)
	}
	s.dubList.Jump(0)
	_, cmd := s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if s.state != sessionStateQuality {
		t.Fatalf("state = %s, want quality", s.state)
	}
	if cmd != nil {
		// The stream resolve settles into the quality picker.
		s.Update(cmd())
	}
}
