package tui

// PR63 owner defect #2: «⏭ След.» must advance the episode AND launch
// playback (python session_loop parity — the pick falls through into
// resolve_dubs_smart + extract_and_play):
//   - remembered dubs still available on the new episode → straight to
//     the player, no manual picks;
//   - the dub list changed (the audio key is gone) → the usual
//     selection prompt;
//   - the used source gone entirely → the typed «⚠ Прошлые настройки
//     недоступны» note + the usual selection.

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// nextSession builds a session with the watch pipeline pre-armed: dubs
// remembered from ep1 — the loop state right after a finished ep1
// playback (python's current_video_dub / current_audio_dub).
func nextSession(t *testing.T) (*sessionScreen, *fakePlayback) {
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
		Log:      discardLogger(),
	}
	group := []contracts.SearchResult{{Title: "Тайтл", URL: "u1", SourceID: "animego"}}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()
	s.videoDub = "[animego] Дубль 1"
	s.audioDub = "[animego] Дубль 1"
	return s, deps.Playback.(*fakePlayback)
}

// pressNext picks «⏭ След.» from the action menu.
func pressNext(t *testing.T, s *sessionScreen) (*sessionScreen, tea.Cmd) {
	t.Helper()
	s.list.Jump(sessionActionIndex(s, "next"))
	next, cmd := s.Update(enter())
	ss, ok := next.(*sessionScreen)
	if !ok {
		t.Fatalf("next must stay in the session, got %T", next)
	}
	return ss, cmd
}

// TestNextAutoLaunchesWhenDubsUnchanged (PR63 #2): with the remembered
// dubs still available on the new episode, «⏭ След.» runs the scoped
// fast path and LAUNCHES the player — no format selector, no stream
// list, no audio prompt.
func TestNextAutoLaunchesWhenDubsUnchanged(t *testing.T) {
	s, fp := nextSession(t)

	ss, cmd := pressNext(t, s)
	if ss.currentEpisode() != "2" {
		t.Fatalf("next must advance to ep 2, got %q", ss.currentEpisode())
	}
	if cmd == nil {
		t.Fatalf("next must launch playback (python fall-through), got no command")
	}
	sr, ok := cmd().(streamResolvedMsg)
	if !ok {
		t.Fatalf("scoped resolve expected, got %T", cmd())
	}
	if sr.scope != "[animego] Дубль 1" {
		t.Fatalf("the remembered dub must scope the resolve, got %q", sr.scope)
	}
	next, cmd := ss.Update(sr)
	ss = next.(*sessionScreen)
	if ss.state != sessionStatePlaying {
		t.Fatalf("the settle must auto-launch, state = %v", ss.state)
	}
	if cmd == nil {
		t.Fatalf("the auto-launch must carry the play command")
	}
	pm, ok := cmd().(playedMsg)
	if !ok || pm.err != nil {
		t.Fatalf("play must settle cleanly, got %#v", pm)
	}
	if len(fp.played) != 1 {
		t.Fatalf("exactly one player launch expected, got %d", len(fp.played))
	}
	if !strings.Contains(fp.played[0].Title, "Тайтл - 2") {
		t.Fatalf("the player must launch EPISODE 2, title %q", fp.played[0].Title)
	}
}

// TestNextDubChangedShowsSelection (PR63 #2): the remembered AUDIO key
// is gone from the new episode — no launch; the usual audio selection
// opens (python resolve_dubs_smart falls back to the interactive pick).
func TestNextDubChangedShowsSelection(t *testing.T) {
	s, fp := nextSession(t)
	s.audioDub = "[anilib] AniLib" // ep2 carries only the animego dub

	ss, cmd := pressNext(t, s)
	if ss.currentEpisode() != "2" {
		t.Fatalf("next must advance to ep 2, got %q", ss.currentEpisode())
	}
	if cmd != nil {
		t.Fatalf("a changed dub list must show the selection, not launch")
	}
	if ss.state != sessionStateDubAudio {
		t.Fatalf("the audio selection must open, state = %v", ss.state)
	}
	if len(fp.played) != 0 {
		t.Fatalf("nothing may play before the pick, got %+v", fp.played)
	}
	if v := ss.View().Content; !contains(v, "Выберите аудиопоток") {
		t.Fatalf("the audio prompt must render:\n%s", v)
	}
}

// TestNextSourceGoneShowsStatusAndSelection (PR63 #2): the remembered
// dub key is gone from the new episode entirely — the typed warning
// surfaces and the usual merged stream selection opens.
func TestNextSourceGoneShowsStatusAndSelection(t *testing.T) {
	s, fp := nextSession(t)
	s.videoDub = "[anilib] AniLib" // ep2 carries only the animego dub
	s.audioDub = "[anilib] AniLib"

	ss, cmd := pressNext(t, s)
	if ss.currentEpisode() != "2" {
		t.Fatalf("next must advance to ep 2, got %q", ss.currentEpisode())
	}
	if !contains(ss.status, "Прошлые настройки недоступны") {
		t.Fatalf("the typed warning must surface, status %q", ss.status)
	}
	if cmd == nil {
		t.Fatalf("the merged resolve must still run for the selection")
	}
	sr, ok := cmd().(streamResolvedMsg)
	if !ok {
		t.Fatalf("stream resolve expected, got %T", cmd())
	}
	if sr.scope != "" {
		t.Fatalf("the merged list must open (empty scope), got %q", sr.scope)
	}
	next, _ := ss.Update(sr)
	ss = next.(*sessionScreen)
	if ss.state != sessionStateQuality {
		t.Fatalf("the stream selection must show, state = %v", ss.state)
	}
	if len(fp.played) != 0 {
		t.Fatalf("nothing may play before the pick, got %+v", fp.played)
	}
}
