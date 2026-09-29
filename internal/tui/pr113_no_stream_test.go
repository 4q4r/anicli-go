package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// TestPR113AudioPickResolvesVideoFirst reproduces the owner's defect:
// «▶ Смотреть» on a resolved episode with the dubs remembered ends in
// «Ошибка воспроизведения: (no stream selected)».
//
// The hole (proceedWatch case 2): the VIDEO dub is remembered and
// present on the current episode, but the AUDIO dub's key vanished
// (dub titles rotate server-side) — the flow jumps STRAIGHT to the
// audio prompt without ever resolving the video. The audio pick then
// launches with a never-set pickedVideo — «(no stream selected)».
//
// The fix: launching from the audio prompt with an unpicked video
// first runs the scoped resolve of the remembered video dub; its
// settle auto-launches (the PR84 fast path) with the audio track
// resolved at play time.
func TestPR113AudioPickResolvesVideoFirst(t *testing.T) {
	pb := &fakePlayback{}
	deps := &Deps{
		Episode: &fakeEpisode{
			episodes: map[string][]contracts.Episode{
				"animevib": {
					{Num: "16", RawID: "av:16", RawEmbeds: map[string][]string{
						// The VIDEO dub is present on the current
						// episode (listing keys are plain dub names —
						// the merge prefixes them); the remembered
						// AUDIO dub is not — renamed/dropped.
						"Studio Band": {"https://embed/av/16"},
					}},
				},
			},
			streams: map[string]contracts.MediaStream{
				"[animevib] Studio Band": {DubName: "sb", Links: map[string]contracts.VideoSource{
					"1080": {URL: "https://cdn/16-1080.m3u8", Type: "m3u8"},
				}},
			},
		},
		Playback: pb,
	}
	group := []contracts.SearchResult{{Title: "Тайтл", URL: "u1", SourceID: "animevib"}}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()
	s.videoDub = "[animevib] Studio Band"
	s.audioDub = "[animevib] Studio Band & Wakanim" // the stale key
	s.setState(sessionStateMenu)
	s.buildActionMenu()

	// «▶ Смотреть» → the format selector → the stream pick lands in
	// proceedWatch: the video dub is remembered and present, the audio
	// dub's key is gone → the audio prompt opens directly.
	scr, _ := s.startWatch()
	ss, ok := scr.(*sessionScreen)
	if !ok {
		t.Fatalf("startWatch = %T", scr)
	}
	scr2, _ := ss.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // stream format
	ss, ok = scr2.(*sessionScreen)
	if !ok {
		t.Fatalf("format pick = %T", scr2)
	}
	if ss.state != sessionStateDubAudio {
		t.Fatalf("state = %v, want the audio prompt (sessionStateDubAudio)", ss.state)
	}
	if ss.pickedVideo.URL != "" {
		t.Fatalf("precondition: no video resolved yet, got %q", ss.pickedVideo.URL)
	}

	// The audio pick («⭐ Как видео» is the first row) must NOT launch
	// an unpicked video.
	_, cmd := ss.handleDubKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("the audio pick must continue the pipeline")
	}

	// Drive the pipeline to playback, following whatever the fix
	// schedules (a scoped resolve settle first, then the launch).
	var played *playedMsg
	for range 8 {
		msg := cmd()
		if m, ok := msg.(streamResolvedMsg); ok {
			next, nextCmd := ss.Update(m)
			ss, ok = next.(*sessionScreen)
			if !ok {
				t.Fatalf("resolve settle = %T", next)
			}
			cmd = nextCmd
			continue
		}
		if m, ok := msg.(playedMsg); ok {
			played = &m
			break
		}
		t.Fatalf("unexpected pipeline message %T: %+v", msg, msg)
	}
	if played == nil {
		t.Fatal("the pipeline never reached playback")
	}
	if played.err != nil {
		t.Fatalf("playback failed: %v (want the video resolved and played)", played.err)
	}
	if len(pb.played) != 1 {
		t.Fatalf("play calls = %d, want 1", len(pb.played))
	}
	if pb.played[0].URL != "https://cdn/16-1080.m3u8" {
		t.Fatalf("played URL = %q, want the resolved video stream", pb.played[0].URL)
	}
	// The dub labels carry the resolved video dub and the picked audio.
	if !strings.Contains(pb.played[0].Title, "Studio Band") {
		t.Fatalf("played title = %q", pb.played[0].Title)
	}
}

// TestPR113AudioPickStaleAudioFallsBackToTypedError: when the scoped
// resolve of the remembered video dub ALSO finds nothing (both keys
// rotated), the flow lands in the merged picker — never a silent
// empty launch.
func TestPR113AudioPickStaleAudioFallsBackToTypedError(t *testing.T) {
	pb := &fakePlayback{}
	deps := &Deps{
		Episode: &fakeEpisode{
			episodes: map[string][]contracts.Episode{
				"animevib": {
					{Num: "16", RawID: "av:16", RawEmbeds: map[string][]string{
						"Studio Band": {"https://embed/av/16"},
					}},
				},
			},
			// The scoped resolve errs for the remembered video dub
			// (the key is absent from the streams map — the provider
			// is down right now).
		},
		Playback: pb,
	}
	group := []contracts.SearchResult{{Title: "Тайтл", URL: "u1", SourceID: "animevib"}}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()
	s.videoDub = "[animevib] Studio Band"
	s.audioDub = "[animevib] Studio Band & Wakanim"
	s.setState(sessionStateMenu)
	s.buildActionMenu()

	scr, _ := s.startWatch()
	ss := scr.(*sessionScreen)
	scr2, _ := ss.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	ss = scr2.(*sessionScreen)
	if ss.state != sessionStateDubAudio {
		t.Fatalf("state = %v, want the audio prompt", ss.state)
	}

	_, cmd := ss.handleDubKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("the audio pick must continue the pipeline")
	}
	msg := cmd()
	m, ok := msg.(streamResolvedMsg)
	if !ok {
		t.Fatalf("want the scoped resolve settle, got %T", msg)
	}
	next, nextCmd := ss.Update(m)
	_ = next // the typed-error surface (menu or a fresh picker round)
	// The scoped resolve failed → the merged resolve of the rest ran
	// (a fresh round): no playback, no empty launch.
	var played *playedMsg
	if nextCmd != nil {
		if m2, ok := nextCmd().(playedMsg); ok {
			played = &m2
		}
	}
	if played != nil && played.err == nil {
		t.Fatal("a provider-down resolve must not reach playback")
	}
	if len(pb.played) != 0 {
		t.Fatalf("no play must happen, got %+v", pb.played)
	}
}
