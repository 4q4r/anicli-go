package tui

// PR95: «🎨 Change dub» открывает меню выбора озвучки из слитых
// записей эпизода (разные дабы, подпись дубляж · провайдер · качества);
// выбор перенацеливает воспроизведение на этот дубль (пара
// запоминается — «След.» продолжает с ним); Esc возвращает без сброса
// и перезапуска. Прецедент паттерна: цикл change_quality в ani-cli
// (плоское меню выбора, выбор сохраняется для последующих серий).

import (
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// redubSession загружает сессию на серии 1 с уже решенными двумя
// разными озвучками в общих записях.
func redubSession(t *testing.T) *sessionScreen {
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
				"[anilib] AniLib": {DubName: "d2", Links: map[string]contracts.VideoSource{
					"1080": {URL: "a1080"},
				}},
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
	s.setState(sessionStateMenu)
	s.buildActionMenu()
	return s
}

// settleRedubResolve runs the fresh unscoped resolve the redub menu
// item schedules (PR111: the menu is built from a FRESH round, never
// from the entries cached by an earlier one).
func settleRedubResolve(t *testing.T, s *sessionScreen) *sessionScreen {
	t.Helper()
	_, cmd := s.handleMenuKey(enter())
	if cmd == nil {
		t.Fatal("«Сменить озвучку» must schedule a fresh resolve")
	}
	msg := cmd()
	_scr, _ := s.Update(msg)
	ss := _scr.(*sessionScreen)
	if ss.state != sessionStateRedub {
		t.Fatalf("state after settle = %v, want the redub menu", ss.state)
	}
	return ss
}

// TestRedubOpensDubMenu (дефект владельца): «Сменить озвучку»
// открывает меню выбора озвучки — оно НЕ должно сбрасывать озвучку.
func TestRedubOpensDubMenu(t *testing.T) {
	s := redubSession(t)

	s.list.Jump(sessionActionIndex(s, "redub"))
	ss := settleRedubResolve(t, s)
	if ss.videoDub != "[animego] Дубль 1" || ss.audioDub != "[animego] Дубль 1" {
		t.Fatalf("redub reset the dubs: %q/%q", ss.videoDub, ss.audioDub)
	}
	if got := len(ss.redubList.Menu().Items); got != 3 {
		t.Fatalf("redub rows = %d (%v), want 2 dubs + Back",
			got, labelsOf(ss.redubList.Menu().Items))
	}
	var anilibRow string
	for _, c := range ss.redubList.Menu().Items {
		if strings.Contains(c.Label, "AniLib") {
			anilibRow = c.Label
		}
	}
	for _, want := range []string{"AniLib", "anilib", "1080p"} {
		if !strings.Contains(anilibRow, want) {
			t.Errorf("AniLib row %q missing %q", anilibRow, want)
		}
	}
	if v := ss.View().Content; !strings.Contains(v, "Pick a dub") {
		t.Errorf("view missing the redub title:\n%s", v)
	}
}

// TestRedubPickRetargetsAndLaunches: выбор озвучки перенацеливает
// запомненную пару и немедленно запускает ее поток.
func TestRedubPickRetargetsAndLaunches(t *testing.T) {
	s := redubSession(t)
	s.list.Jump(sessionActionIndex(s, "redub"))
	ss := settleRedubResolve(t, s)

	for i, c := range ss.redubList.Menu().Items {
		if c.ID == "[anilib] AniLib" {
			ss.redubList.Jump(i)
			break
		}
	}
	next, resolve := ss.Update(enter())
	ss2 := next.(*sessionScreen)
	// PR111: the pick re-resolves the dub FRESH (no cached launch);
	// its settle auto-launches via the fast path.
	if resolve == nil {
		t.Fatal("the pick must schedule a fresh scoped resolve")
	}
	next3, play := ss2.Update(resolve())
	ss3 := next3.(*sessionScreen)

	if ss3.videoDub != "[anilib] AniLib" || ss3.audioDub != "[anilib] AniLib" {
		t.Fatalf("remembered pair = %q/%q, want the picked dub", ss3.videoDub, ss3.audioDub)
	}
	if ss3.state != sessionStatePlaying {
		t.Fatalf("state = %v, want sessionStatePlaying (immediate launch)", ss3.state)
	}
	if play == nil {
		t.Fatal("missing playedMsg command")
	}
	pm, ok := play().(playedMsg)
	if !ok {
		t.Fatalf("play settled into %T", play)
	}
	if pm.err != nil {
		t.Fatalf("playback must succeed, got %v", pm.err)
	}
	got := s.deps.Playback.(*fakePlayback).played
	if len(got) != 1 || got[0].URL != "a1080" {
		t.Fatalf("played = %+v, want the picked dub's stream", got)
	}
}

// TestRedubEscReturnsUnchanged: Esc возвращает в меню действий —
// без сброса и перезапуска.
func TestRedubEscReturnsUnchanged(t *testing.T) {
	s := redubSession(t)
	s.list.Jump(sessionActionIndex(s, "redub"))
	ss := settleRedubResolve(t, s)

	next2, cmd := ss.Update(esc())
	ss2 := next2.(*sessionScreen)
	if ss2.state != sessionStateMenu {
		t.Fatalf("state after Esc = %v, want sessionStateMenu", ss2.state)
	}
	if cmd != nil {
		t.Fatal("Esc must not schedule playback")
	}
	if ss2.videoDub != "[animego] Дубль 1" || ss2.audioDub != "[animego] Дубль 1" {
		t.Fatalf("Esc must not touch the dubs, got %q/%q", ss2.videoDub, ss2.audioDub)
	}
	if got := len(s.deps.Playback.(*fakePlayback).played); got != 0 {
		t.Fatalf("Esc must not launch, played %d", got)
	}
}

// TestRedubPendingResolveOpensMenuAfterSettle: без кэшированных
// записей redub запускает unscoped-resolve; его завершение
// открывает меню озвучки (без автозапуска).
func TestRedubPendingResolveOpensMenuAfterSettle(t *testing.T) {
	pb := &fakePlayback{}
	deps := &Deps{
		Episode: &fakeEpisode{
			episodes: testEpisodeSet(),
			streams: map[string]contracts.MediaStream{
				"[animego] Дубль 1": {DubName: "d1", Links: map[string]contracts.VideoSource{
					"1080": {URL: "v1080"},
				}},
			},
		},
		Playback: pb,
	}
	group := []contracts.SearchResult{{Title: "Тайтл", URL: "u1", SourceID: "animego"}}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()
	s.videoDub, s.audioDub = "[animego] Дубль 1", "[animego] Дубль 1"
	s.list.Jump(sessionActionIndex(s, "redub"))

	scr, cmd := s.handleMenuKey(enter())
	ss := scr.(*sessionScreen)
	if cmd == nil {
		t.Fatal("no cached entries — the redub must start the unscoped resolve")
	}
	if ss.state != sessionStateQuality {
		t.Fatalf("while resolving the state = %v, want the quality surface", ss.state)
	}

	msg := cmd()
	next, _ := ss.Update(msg)
	ss2 := next.(*sessionScreen)
	if ss2.state != sessionStateRedub {
		t.Fatalf("state after settle = %v, want the redub menu", ss2.state)
	}
	if got := len(ss2.redubList.Menu().Items); got != 2 {
		t.Fatalf("redub rows = %d (%v), want the animego dub + Back",
			got, labelsOf(ss2.redubList.Menu().Items))
	}
	if len(pb.played) != 0 {
		t.Fatalf("the redub resolve must not auto-launch, played %d", len(pb.played))
	}
}
