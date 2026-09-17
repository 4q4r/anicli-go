package tui

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/buffered"
	"github.com/an0nx/anicli-go/internal/contracts"
) // realFileBuffered is a BufferedService fake that downloads for real
// (through stdlib http) so the file plumbing is exercised end to end.
type realFileBuffered struct {
	mu   sync.Mutex
	dirs []string
}

func (b *realFileBuffered) Buffer(ctx context.Context, src buffered.Source, progress func(buffered.Progress)) (buffered.Handle, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URL, nil)
	if err != nil {
		return buffered.Handle{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return buffered.Handle{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return buffered.Handle{}, &fakeStatusError{code: resp.StatusCode}
	}
	dir, err := os.MkdirTemp("", "anicli-test-buffer-")
	if err != nil {
		return buffered.Handle{}, err
	}
	b.mu.Lock()
	b.dirs = append(b.dirs, dir)
	b.mu.Unlock()
	path := filepath.Join(dir, "video.mp4")
	f, err := os.Create(path) //nolint:gosec // path is our MkdirTemp dir — no user-controlled inclusion
	if err != nil {
		_ = os.RemoveAll(dir)
		return buffered.Handle{}, err
	}
	// Copy with cancellation: a cancelled ctx stops the download and
	// removes the temp dir (mirroring the real downloader's contract).
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 1024)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := f.Write(buf[:n]); werr != nil {
					done <- werr
					return
				}
			}
			if rerr != nil {
				if errors.Is(rerr, io.EOF) {
					done <- nil
				} else {
					done <- rerr
				}
				return
			}
		}
	}()
	select {
	case rerr := <-done:
		if rerr != nil {
			_ = f.Close()
			_ = os.RemoveAll(dir)
			return buffered.Handle{}, rerr
		}
		_ = f.Close()
		return buffered.NewHandle(path, func() { _ = os.RemoveAll(dir) }), nil
	case <-ctx.Done():
		_ = resp.Body.Close()
		<-done
		_ = f.Close()
		_ = os.RemoveAll(dir)
		return buffered.Handle{}, ctx.Err()
	}
}

func (b *realFileBuffered) CleanupAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, dir := range b.dirs {
		_ = os.RemoveAll(dir)
	}
}

type fakeStatusError struct{ code int }

func (e *fakeStatusError) Error() string { return "status error" }

var _ BufferedService = (*realFileBuffered)(nil)

// newBufferedSession builds a one-episode session with real embeds, a
// live mp4 server as the resolved stream and the real-file buffered
// fake.
func newBufferedSession(t *testing.T) (*sessionScreen, *realFileBuffered, *fakePlayback, string) {
	t.Helper()
	payload := "fake-mp4-bytes"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)

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
					"720": {URL: srv.URL + "/ep1.mp4"},
				}},
			},
		},
		Playback: &fakePlayback{},
		Buffered: &realFileBuffered{},
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
	s, _, playback, payload := newBufferedSession(t)
	s.buffered = true
	s.buildActionMenu()

	// Capture the file state DURING the play call.
	var playStatErr, playReadErr error
	var playBody []byte
	playback.playHook = func(req PlayRequest) error {
		playStatErr = nil
		if _, err := os.Stat(req.URL); err != nil {
			playStatErr = err
			return nil
		}
		playBody, playReadErr = os.ReadFile(req.URL)
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
	if s.state != sessionStateBuffering {
		t.Fatalf("state = %s, want buffering", s.state)
	}
	ready, ok := runBufferedBatch(t, cmd).(bufferReadyMsg)
	if !ok {
		t.Fatalf("download settled %T, want bufferReadyMsg", ready)
	}
	if _, playCmd := s.Update(ready); playCmd == nil {
		t.Fatal("ready settle must schedule the local playback")
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
	if playReadErr != nil || string(playBody) != payload {
		t.Fatalf("buffered payload mismatch at play time (read err=%v)", playReadErr)
	}
	// After the playedMsg the temp file is gone (cleanup ran).
	if _, err := os.Stat(req.URL); !os.IsNotExist(err) {
		t.Fatalf("buffered file survived playback: %v", err)
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
// pump's progress/end messages are drained in the background.
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
	for range batch {
		select {
		case m := <-msgs:
			if ready, ok := m.(bufferReadyMsg); ok {
				return ready
			}
		case <-time.After(10 * time.Second):
			t.Fatal("buffered batch timed out")
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
