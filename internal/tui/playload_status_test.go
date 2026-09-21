package tui

// PR84: the remembered-dub auto-launch (proceedWatch's scoped resolve)
// rendered the SELECTION surface (the quality picker's title with the
// «Ищу потоки…» row) while the streams were still resolving — the
// owner read it as «pick again». The fix: a DISTINCT minimal loading
// surface («Загрузка потоков…» + ep/dub context) for every armed
// auto-launch (▶ Смотреть, ⏭ След., ⏮ Пред.); the picker appears
// only when a choice is genuinely needed (the remembered dub
// vanished) with its typed warning. These tests pin the surface
// sequence frame by frame.

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// playloadSession builds the remembered-dubs session of the PR61
// regression (both dubs remembered, scoped streams available) with
// the episode cursor moved to num.
func playloadSession(t *testing.T, num string, buffered bool) *sessionScreen {
	t.Helper()
	pb := &fakePlayback{}
	deps := &Deps{
		Episode: &fakeEpisode{
			episodes: testEpisodeSet(),
			streams: map[string]contracts.MediaStream{
				"[animego] Дубль 1": {DubName: "d1", Links: map[string]contracts.VideoSource{
					"1080": {URL: "v1080"},
					"720":  {URL: "v720"},
				}},
			},
		},
		Playback: pb,
	}
	if buffered {
		deps.Buffered = &realFileBuffered{Payload: []byte("x")}
	}
	group := []contracts.SearchResult{{Title: "Тайтл", URL: "u1", SourceID: "animego"}}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()
	s.videoDub, s.audioDub = "[animego] Дубль 1", "[animego] Дубль 1"
	s.buffered = buffered
	for i, candidate := range s.order {
		if candidate == num {
			s.currentIdx = i
			break
		}
	}
	if got := s.currentEpisode(); got != num {
		t.Fatalf("cursor at %q, want %q", got, num)
	}
	return s
}

// TestAutoWatchNextArmedShowsResolveLoadingNotPicker: ⏭ След. on a
// remembered-dubs episode opens the DISTINCT loading surface — the
// picker's title and its «Ищу потоки…» row must never flash.
func TestAutoWatchNextArmedShowsResolveLoadingNotPicker(t *testing.T) {
	s := playloadSession(t, "2", false)

	scr, cmd := s.autoWatchNext()
	ss, ok := scr.(*sessionScreen)
	if !ok {
		t.Fatalf("scr = %T, want the session screen", scr)
	}
	if ss.state != sessionStateResolveLoading {
		t.Fatalf("state = %v, want sessionStateResolveLoading", ss.state)
	}
	if cmd == nil {
		t.Fatal("the scoped resolve command is missing")
	}
	view := ss.View().Content
	for _, want := range []string{"Загрузка потоков…", "Эп. 2", "Дубль 1"} {
		if !strings.Contains(view, want) {
			t.Errorf("loading surface missing %q:\n%s", want, view)
		}
	}
	for _, banned := range []string{"Выберите поток", "Ищу потоки…"} {
		if strings.Contains(view, banned) {
			t.Errorf("the picker surface flashed during the resolve:\n%s", view)
		}
	}
}

// TestResolveLoadingSettleLaunchesDirectly: the scoped settle jumps
// straight to playback — the picker is never rendered on the way.
func TestResolveLoadingSettleLaunchesDirectly(t *testing.T) {
	pb := &fakePlayback{}
	deps := &Deps{
		Episode: &fakeEpisode{
			episodes: testEpisodeSet(),
			streams: map[string]contracts.MediaStream{
				"[animego] Дубль 1": {DubName: "d1", Links: map[string]contracts.VideoSource{
					"1080": {URL: "v1080"},
					"720":  {URL: "v720"},
				}},
			},
		},
		Playback: pb,
	}
	group := []contracts.SearchResult{{Title: "Тайтл", URL: "u1", SourceID: "animego"}}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()
	s.videoDub, s.audioDub = "[animego] Дубль 1", "[animego] Дубль 1"
	for i, candidate := range s.order {
		if candidate == "2" {
			s.currentIdx = i
			break
		}
	}
	scr, cmd := s.autoWatchNext()
	ss := scr.(*sessionScreen)
	if cmd == nil {
		t.Fatal("missing resolve command")
	}
	sr, ok := cmd().(streamResolvedMsg)
	if !ok {
		t.Fatalf("scoped resolve expected, got %T", cmd())
	}
	seen := ss.View().Content
	next, play := ss.Update(sr)
	_ = next
	if play == nil {
		t.Fatal("the scoped settle must launch playback")
	}
	if ss.state != sessionStatePlaying {
		t.Fatalf("state after settle = %v, want sessionStatePlaying", ss.state)
	}
	pm, ok := play().(playedMsg)
	if !ok {
		t.Fatalf("play must settle into playedMsg, got %#v", play())
	}
	if pm.err != nil {
		t.Fatalf("playback must succeed, got %v", pm.err)
	}
	if len(pb.played) != 1 {
		t.Fatalf("exactly one playback must run, got %d", len(pb.played))
	}
	if strings.Contains(seen, "Выберите поток") {
		t.Errorf("the picker flashed before the launch:\n%s", seen)
	}
}

// TestResolveLoadingVanishedDubOpensPickerWithWarning: the remembered
// dub gone from the next episode → the REAL picker with the typed
// warning (the choice is genuinely needed — this surface stays).
func TestResolveLoadingVanishedDubOpensPickerWithWarning(t *testing.T) {
	// Episode 2 carries ONLY the anilib dub — the remembered animego
	// pair has vanished from it (the PR78-flow vanishing shape).
	pb := &fakePlayback{}
	deps := &Deps{
		Episode: &fakeEpisode{
			episodes: map[string][]contracts.Episode{
				"animego": {{Num: "1", RawID: "a1", RawEmbeds: map[string][]string{"Дубль 1": {"u1v"}}}},
				"anilib":  {{Num: "1", RawID: "b1", RawEmbeds: map[string][]string{"AniLib": {"u1b"}}}, {Num: "2", RawID: "b2", RawEmbeds: map[string][]string{"AniLib": {"u2b"}}}},
			},
		},
		Playback: pb,
	}
	group := []contracts.SearchResult{
		{Title: "Тайтл", URL: "u1", SourceID: "animego"},
		{Title: "Тайтл", URL: "u2", SourceID: "anilib"},
	}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()
	s.videoDub, s.audioDub = "[animego] Дубль 1", "[animego] Дубль 1"
	for i, candidate := range s.order {
		if candidate == "2" {
			s.currentIdx = i
			break
		}
	}

	scr, _ := s.autoWatchNext()
	ss := scr.(*sessionScreen)
	if ss.state != sessionStateQuality {
		t.Fatalf("state = %v, want sessionStateQuality (the picker path)", ss.state)
	}
	view := ss.View().Content
	for _, want := range []string{"Выберите поток", "Ищу потоки…"} {
		if !strings.Contains(view, want) {
			t.Errorf("picker loading surface missing %q:\n%s", want, view)
		}
	}
	if !strings.Contains(ss.status, "⚠ Прошлые настройки недоступны") {
		t.Errorf("status = %q, want the typed vanished-dub warning", ss.status)
	}
}

// TestResolveLoadingEscCancelsToOrigin: Esc on the loading surface
// cancels the resolve (the round is superseded, its cleanup runs) and
// returns to the screen the launch came from.
func TestResolveLoadingEscCancelsToOrigin(t *testing.T) {
	s := playloadSession(t, "2", false)
	// The real ⏭ След. is picked from the episode list — the origin
	// the loading surface's Esc returns to.
	s.setState(sessionStateEpisodeList)
	if _, cmd := s.autoWatchNext(); cmd == nil {
		t.Fatal("missing resolve command")
	}
	if s.state != sessionStateResolveLoading {
		t.Fatalf("state = %v, want the loading surface", s.state)
	}
	gen := s.resolveGen

	cleaned := false
	late := streamResolvedMsg{
		gen:         gen, // the cancelled round
		scope:       "[animego] Дубль 1",
		entries:     []streamEntry{{Quality: "720", DubKey: "[animego] Дубль 1", Source: contracts.VideoSource{URL: "v720"}}},
		skipCleanup: func() { cleaned = true },
	}
	next2, _ := s.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	ss, ok := next2.(*sessionScreen)
	if !ok {
		t.Fatalf("Esc scr = %T", next2)
	}
	if ss.state != sessionStateEpisodeList {
		t.Fatalf("state after Esc = %v, want sessionStateEpisodeList (the origin)", ss.state)
	}
	if ss.resolveGen <= gen {
		t.Fatal("Esc must supersede the in-flight round (gen bump)")
	}
	// The late settle of the cancelled round is dropped, its chapters
	// cleanup still runs.
	if _, cmd := ss.Update(late); cmd != nil {
		t.Fatal("a cancelled round must not schedule playback")
	}
	if !cleaned {
		t.Fatal("the cancelled round's cleanup must run")
	}
}

// TestWatchFromMenuArmedShowsResolveLoading: the zero-prompt ▶
// Смотреть entry uses the same loading surface (one mechanism, three
// entry points).
func TestWatchFromMenuArmedShowsResolveLoading(t *testing.T) {
	s := playloadSession(t, "1", false)
	s.setState(sessionStateMenu)

	s.list.Jump(sessionActionIndex(s, "watch"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	if ss.state != sessionStateFormat {
		t.Fatalf("watch must open the format selector, got %v", ss.state)
	}
	ss.formatList.Jump(indexOfDayFormatList(ss, "stream"))
	next2, cmd := ss.Update(enter())
	ss2 := next2.(*sessionScreen)
	if ss2.state != sessionStateResolveLoading {
		t.Fatalf("state = %v, want sessionStateResolveLoading", ss2.state)
	}
	if view := ss2.View().Content; strings.Contains(view, "Выберите поток") {
		t.Errorf("the picker flashed on the ▶ Смотреть path:\n%s", view)
	}
	if cmd == nil {
		t.Fatal("missing resolve command")
	}
}

// TestResolveLoadingBufferedComposition: buffered mode keeps its own
// progress surface — the loading surface is replaced by the buffering
// state at launch, never mixed with it.
func TestResolveLoadingBufferedComposition(t *testing.T) {
	s := playloadSession(t, "2", true)

	scr, cmd := s.autoWatchNext()
	ss := scr.(*sessionScreen)
	if ss.state != sessionStateResolveLoading {
		t.Fatalf("state = %v, want the loading surface", ss.state)
	}
	if view := ss.View().Content; strings.Contains(view, "Буферизация") {
		t.Errorf("buffered progress must not bleed into the loading surface:\n%s", view)
	}
	sr, ok := cmd().(streamResolvedMsg)
	if !ok {
		t.Fatalf("scoped resolve expected, got %T", cmd())
	}
	if _, play := ss.Update(sr); play == nil {
		t.Fatal("the buffered settle must start buffering")
	}
	if ss.state != sessionStateBuffering {
		t.Fatalf("state after buffered settle = %v, want sessionStateBuffering", ss.state)
	}
	if view := ss.View().Content; strings.Contains(view, "Загрузка потоков…") {
		t.Errorf("the loading surface must be gone at buffering:\n%s", view)
	}
}
