package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/storage"
)

// fakeHistory implements HistoryService.
type fakeHistory struct {
	items   []storage.AnimeProgress
	saved   []int64
	binds   []int64
	byShiki map[int64]*storage.AnimeProgress
	rateIDs map[int64]int64 // animeID -> last SetRateID
}

func (f *fakeHistory) List(_ context.Context) ([]storage.AnimeProgress, error) {
	return f.items, nil
}

func (f *fakeHistory) SavePlayback(_ context.Context, rec storage.AnimeProgress, _, _, _ string) error {
	f.saved = append(f.saved, rec.ID)
	return nil
}

func (f *fakeHistory) BindSource(_ context.Context, id int64, _, _ string) error {
	f.binds = append(f.binds, id)
	return nil
}

func (f *fakeHistory) GetByShikimoriID(_ context.Context, shikimoriID int64) (*storage.AnimeProgress, error) {
	if rec, ok := f.byShiki[shikimoriID]; ok {
		return rec, nil
	}
	return nil, nil
}

func (f *fakeHistory) SetRateID(_ context.Context, animeID, rateID int64) error {
	if f.rateIDs == nil {
		f.rateIDs = map[int64]int64{}
	}
	f.rateIDs[animeID] = rateID
	return nil
}

var _ HistoryService = (*fakeHistory)(nil)

// fakeShiki implements ShikimoriService.
type fakeShiki struct {
	enabled bool
	updates []shikiUpdate
	// rateIDs records the rate id received per UpdateStatus call
	// (0 = the create/POST path).
	rateIDs []int64
	// queries records SearchIDs lookups; ids is their fixture.
	queries []string
	ids     map[string]int64
	// next is the rate id returned for the next create (PATCH echoes
	// the incoming id).
	next int64
}

type shikiUpdate struct {
	shikimoriID int64
	rateID      int64
	status      string
	score       *int
	rewatches   *int
}

func (f *fakeShiki) Enabled() bool { return f.enabled }

func (f *fakeShiki) UpdateStatus(_ context.Context, shikimoriID, rateID int64, status string, score, rewatches *int) (int64, error) {
	f.updates = append(f.updates, shikiUpdate{
		shikimoriID: shikimoriID, rateID: rateID, status: status,
		score: score, rewatches: rewatches,
	})
	f.rateIDs = append(f.rateIDs, rateID)
	if rateID > 0 {
		return rateID, nil
	}
	f.next++
	return f.next, nil
}

func (f *fakeShiki) SearchIDs(_ context.Context, query string) (map[string]int64, error) {
	f.queries = append(f.queries, query)
	return f.ids, nil
}

var _ ShikimoriService = (*fakeShiki)(nil)

func historyItems() []storage.AnimeProgress {
	watching := "watching"
	planned := "planned"
	poster := "p"
	return []storage.AnimeProgress{
		{ID: 1, Title: "Ванпанчмен", SourceID: "animego", SourceURL: "u1", CurrentEpisode: "3",
			ShikimoriStatus: watching, ShikimoriID: ptrTo(int64(11)), Poster: &poster},
		{ID: 2, Title: "Наруто", SourceID: "anilib", SourceURL: "u2", CurrentEpisode: "120",
			ShikimoriStatus: planned, ShikimoriID: ptrTo(int64(12))},
		{ID: 3, Title: "Bleach", SourceID: "animego", SourceURL: "u3", CurrentEpisode: "7",
			ShikimoriStatus: watching, NeedsCorrection: true},
	}
}

func ptrTo[T any](v T) *T { return &v }

// TestHistoryFilterFirst: the history flow asks the status filter
// FIRST, with Back and counts, including «Все».
func TestHistoryFilterFirst(t *testing.T) {
	deps := &Deps{History: &fakeHistory{items: historyItems()}}
	filter := NewHistoryFilter(deps)

	v := filter.View().Content
	for _, want := range []string{"Смотрю", "В планах", "Пересмотр", "Просмотрено", "Отложено", "Брошено", "Все"} {
		if !strings.Contains(v, want) {
			t.Fatalf("filter must offer %q:\n%s", want, v)
		}
	}
	if !strings.Contains(v, "Смотрю [2]") {
		t.Fatalf("filter must show per-status counts:\n%s", v)
	}
}

// TestHistoryFilterLogic: status filtering matches the chosen key;
// «Все» passes everything; empty filter result is legal (I3).
func TestHistoryFilterLogic(t *testing.T) {
	items := historyItems()

	t.Run("watching keeps 2", func(t *testing.T) {
		got := FilterHistory(items, "watching")
		if len(got) != 2 {
			t.Fatalf("want 2 watching items, got %d", len(got))
		}
	})

	t.Run("planned keeps 1", func(t *testing.T) {
		got := FilterHistory(items, "planned")
		if len(got) != 1 || got[0].Title != "Наруто" {
			t.Fatalf("want Naruto planned, got %+v", got)
		}
	})

	t.Run("all keeps everything", func(t *testing.T) {
		if got := FilterHistory(items, ""); len(got) != 3 {
			t.Fatalf("want all 3, got %d", len(got))
		}
	})

	t.Run("unknown status yields empty (legal)", func(t *testing.T) {
		if got := FilterHistory(items, "rewatching"); len(got) != 0 {
			t.Fatalf("want 0, got %d", len(got))
		}
	})
}

// TestHistoryListRendering: badges and episode info render per item.
func TestHistoryListRendering(t *testing.T) {
	deps := &Deps{History: &fakeHistory{items: historyItems()}}
	list := NewHistoryList(deps, "watching", FilterHistory(historyItems(), "watching"))
	v := list.View().Content
	if !strings.Contains(v, "[С] Ванпанчмен") {
		t.Fatalf("status badge [С] missing:\n%s", v)
	}
	if !strings.Contains(v, "(Серия 3)") {
		t.Fatalf("episode info missing:\n%s", v)
	}
	if !strings.Contains(v, "[⚠]") || !strings.Contains(v, "Bleach") {
		t.Fatalf("needs-correction badge missing:\n%s", v)
	}
}

// TestHistoryItemActions: selection opens item actions with resume
// and rebind.
func TestHistoryItemActions(t *testing.T) {
	deps := &Deps{History: &fakeHistory{items: historyItems()}}
	list := NewHistoryList(deps, "watching", FilterHistory(historyItems(), "watching"))
	list.list.Jump(0) // Ванпанчмен
	next, cmd := list.Update(enter())
	if cmd == nil {
		t.Fatalf("pick must navigate")
	}
	msg := cmd()
	pm, ok := msg.(pushMsg)
	if !ok {
		t.Fatalf("pick must push item actions, got %#v", msg)
	}
	actions := pm.screen
	if !strings.Contains(actions.View().Content, "Продолжить") {
		t.Fatalf("item actions must offer resume:\n%s", actions.View().Content)
	}
	_ = next
}

// TestHistoryNeedsCorrection: picking a needs_correction item routes
// to rebind, not to a session.
func TestHistoryNeedsCorrection(t *testing.T) {
	deps := &Deps{History: &fakeHistory{items: historyItems()}}
	list := NewHistoryList(deps, "watching", FilterHistory(historyItems(), "watching"))
	list.list.Jump(1) // Bleach (needs_correction)
	_, cmd := list.Update(enter())
	msg := cmd()
	pm, ok := msg.(pushMsg)
	if !ok {
		t.Fatalf("must push a screen, got %#v", msg)
	}
	if pm.screen.ID() != historyRebindID {
		t.Fatalf("needs_correction must route to rebind, got %q", pm.screen.ID())
	}
}

// TestHistoryRehydratePick: the rehydrate results screen applies
// strategy 1 (exact) then strategy 2 (similarity).
func TestHistoryRehydratePick(t *testing.T) {
	record := historyItems()[0] // animego u1
	groups := [][]contracts.SearchResult{
		{
			{Title: "Ванпанчмен", URL: "u1", SourceID: "animego"},
			{Title: "Ван Панч", URL: "u9", SourceID: "anilib"},
		},
		{{Title: "Совсем другое", URL: "u5", SourceID: "animego"}},
	}

	t.Run("exact wins", func(t *testing.T) {
		got := RehydrateGroup(groups, record)
		if len(got) != 2 || got[0].URL != "u1" {
			t.Fatalf("exact group must win, got %+v", got)
		}
	})

	t.Run("fallback to similarity when exact misses", func(t *testing.T) {
		moved := record
		moved.SourceURL = "u-changed"
		got := RehydrateGroup(groups, moved)
		if got == nil || got[0].URL != "u1" {
			t.Fatalf("similarity fallback must find the title group, got %+v", got)
		}
	})

	t.Run("nothing matches yields nil", func(t *testing.T) {
		moved := record
		moved.SourceURL = "u-changed"
		moved.Title = "Абсолютно другое название"
		moved.BoundTitle = nil
		if got := RehydrateGroup(groups, moved); got != nil {
			t.Fatalf("no match must yield nil, got %+v", got)
		}
	})
}

// TestDBMenuConfirmFlow: clears demand an explicit confirmation.
func TestDBMenuConfirmFlow(t *testing.T) {
	db := &fakeDatabase{}
	deps := &Deps{Database: db}
	menu := NewDBMenu(deps)

	v := menu.View().Content
	for _, want := range []string{
		"Очистить предсказания таймкодов",
		"Очистить всю историю просмотров",
		"Полная очистка БД",
	} {
		if !strings.Contains(v, want) {
			t.Fatalf("db menu must offer %q:\n%s", want, v)
		}
	}

	t.Run("back on the confirm cancels", func(t *testing.T) {
		m := NewDBMenu(deps)
		idx := dbActionIndex(m, "clear_skips")
		m.list.Jump(idx)
		_, cmd := m.Update(enter())
		pm := cmd().(pushMsg)
		conf := pm.screen
		if !strings.Contains(conf.View().Content, "Вы уверены") {
			t.Fatalf("confirm must warn:\n%s", conf.View().Content)
		}
		_, cmd = conf.Update(esc())
		if _, ok := cmd().(popMsg); !ok {
			t.Fatalf("esc on confirm cancels, got %#v", cmd())
		}
		if len(db.cleared) != 0 {
			t.Fatalf("cancel must not clear anything, got %v", db.cleared)
		}
	})

	t.Run("confirm yes clears once", func(t *testing.T) {
		m := NewDBMenu(deps)
		idx := dbActionIndex(m, "clear_history")
		m.list.Jump(idx)
		_, cmd := m.Update(enter())
		conf := cmd().(pushMsg).screen
		_, cmd = conf.Update(enter()) // Да
		_ = cmd()                     // run the clear command
		if len(db.cleared) != 1 || db.cleared[0] != "history" {
			t.Fatalf("confirm must run the clear, got %v", db.cleared)
		}
	})

	t.Run("cleared feedback lands and enter leaves (I7)", func(t *testing.T) {
		db2 := &fakeDatabase{}
		deps2 := &Deps{Database: db2, Log: testLogger()}
		m := NewDBMenu(deps2)
		idx := dbActionIndex(m, "clear_history")
		m.list.Jump(idx)
		_, cmd := m.Update(enter())
		conf := cmd().(pushMsg).screen
		next, cmd := conf.Update(enter()) // Да
		cleared := cmd()
		cm, ok := cleared.(dbClearedMsg)
		if !ok {
			t.Fatalf("clear must settle into dbClearedMsg, got %T", cleared)
		}
		// The screen must CONSUME dbClearedMsg: counts on the status
		// line, no silent drop.
		next, _ = next.Update(cm)
		if !contains(next.View().Content, "Удалено 7") {
			t.Fatalf("cleared counts must render:\n%s", next.View().Content)
		}
		// Enter after clearing pops back instead of re-clearing.
		_, cmd = next.Update(enter())
		if cmd == nil {
			t.Fatalf("enter after clear must leave the screen")
		}
		if _, ok := cmd().(popMsg); !ok {
			t.Fatalf("enter after clear must pop, got %#v", cmd())
		}
		if len(db2.cleared) != 1 {
			t.Fatalf("no second clear may run, got %v", db2.cleared)
		}
	})
}

// TestHistoryResumeAutoEntersSession (I6): resume with matching
// results skips manual grouping and enters the session restored to
// the saved episode and dubs, carrying the shikimori binding.
func TestHistoryResumeAutoEntersSession(t *testing.T) {
	fs := &fakeSearch{providers: []ProviderMeta{{ID: "animego", Name: "AnimeGO"}},
		results: map[string][]contracts.SearchResult{
			"animego": {{Title: "Ванпанчмен", URL: "u1", SourceID: "animego"}},
		},
		queries: map[string]string{}}
	ep := &fakeEpisode{
		episodes: testEpisodeSet(),
		streams: map[string]contracts.MediaStream{
			"[animego] Дубль 1": {Links: map[string]contracts.VideoSource{"1080": {URL: "v1080"}}},
		},
	}
	deps := &Deps{Search: fs, Episode: ep, Playback: &fakePlayback{}, Log: testLogger()}
	rec := historyItems()[0] // Ванпанчмен animego u1, shikimori 11
	rec.CurrentEpisode = "2"
	rec.VideoDub = ptrTo("[animego] Дубль 1")
	rec.AudioDub = ptrTo("[animego] Дубль 1")

	app := NewApp(NewRootScreen(deps), deps, testLogger())
	model := drive(app, pushMsg{screen: newHistoryResume(deps, &rec)})

	// Enter on the settled fan-out: auto-match replaces the screen
	// with the resumed session.
	top := topOf(model)
	_, cmd := top.Update(enter())
	if cmd == nil {
		t.Fatalf("enter on settled resume must advance")
	}
	rm, ok := cmd().(replaceMsg)
	if !ok {
		t.Fatalf("resume must replace with the session, got %T", cmd())
	}
	if _, ok := rm.screen.(*sessionScreen); !ok {
		t.Fatalf("resume must enter a session, got %T", rm.screen)
	}

	// Drive the session's own Init (episode fan-out) through the app.
	model = drive(model, replaceMsg{screen: rm.screen})
	sess := topOf(model).(*sessionScreen)
	if sess.state != sessionStateMenu {
		t.Fatalf("session must land on the menu, got %v", sess.state)
	}
	if sess.currentEpisode() != "2" {
		t.Fatalf("session must resume at the saved episode, got %q", sess.currentEpisode())
	}
	if sess.videoDub != "[animego] Дубль 1" || sess.audioDub != "[animego] Дубль 1" {
		t.Fatalf("session must restore dubs, got %q/%q", sess.videoDub, sess.audioDub)
	}
	if sess.shikimoriID() != 11 {
		t.Fatalf("session must carry the record's shikimori binding, got %d", sess.shikimoriID())
	}
}

// TestHistoryResumeNoMatchFallsToManualGrouping (I6): without a
// confident match, resume falls through to the manual grouping screen
// with a note.
func TestHistoryResumeNoMatchFallsToManualGrouping(t *testing.T) {
	fs := &fakeSearch{providers: []ProviderMeta{{ID: "animego", Name: "AnimeGO"}},
		results: map[string][]contracts.SearchResult{
			"animego": {{Title: "Совсем Другое Аниме", URL: "u9", SourceID: "animego"}},
		},
		queries: map[string]string{}}
	deps := &Deps{Search: fs, Episode: &fakeEpisode{episodes: testEpisodeSet()}, Log: testLogger()}
	rec := historyItems()[0]
	rec.SourceURL = "u-changed"

	app := NewApp(NewRootScreen(deps), deps, testLogger())
	model := drive(app, pushMsg{screen: newHistoryResume(deps, &rec)})

	top := topOf(model)
	_, cmd := top.Update(enter())
	if cmd == nil {
		t.Fatalf("enter on settled resume must advance")
	}
	rm, ok := cmd().(replaceMsg)
	if !ok {
		t.Fatalf("no-match resume must fall through to grouping, got %T", cmd())
	}
	group, ok := rm.screen.(*searchGroup)
	if !ok {
		t.Fatalf("expected the manual grouping screen, got %T", rm.screen)
	}
	if !contains(group.View().Content, "сгруппируйте вручную") {
		t.Fatalf("fall-through must carry a note:\n%s", group.View().Content)
	}
}

func dbActionIndex(m *MenuScreen, id string) int {
	for i, c := range m.list.Menu().Items {
		if c.ID == id {
			return i
		}
	}
	return -1
}

// fakeDatabase implements DatabaseService.
type fakeDatabase struct {
	cleared []string
}

func (f *fakeDatabase) ClearSkips(_ context.Context) (int64, error) {
	f.cleared = append(f.cleared, "skips")
	return 3, nil
}

func (f *fakeDatabase) ClearHistory(_ context.Context) (int64, error) {
	f.cleared = append(f.cleared, "history")
	return 7, nil
}

func (f *fakeDatabase) ClearAll(_ context.Context) (int64, int64, error) {
	f.cleared = append(f.cleared, "all")
	return 7, 3, nil
}

var _ DatabaseService = (*fakeDatabase)(nil)

// TestHealthScreen: providers check in parallel with live OK/Error
// rows; a panicking provider shows Error, not a crash.
func TestHealthScreen(t *testing.T) {
	hc := &fakeHealth{errFor: map[string]error{"broken": errors.New("connection refused")}}
	deps := &Deps{
		Search: &fakeSearch{providers: []ProviderMeta{
			{ID: "animego", Name: "AnimeGO"},
			{ID: "broken", Name: "Broken"},
		}},
		Health: hc,
	}
	h := NewHealthScreen(deps)
	v := h.View().Content
	if !strings.Contains(v, "AnimeGO") || !strings.Contains(v, "Broken") {
		t.Fatalf("health table must list providers:\n%s", v)
	}

	// Init schedules the checks; drive them through the model the way
	// the runtime would after calling Init.
	app := NewApp(h, deps, testLogger())
	model := drive(app, tea.WindowSizeMsg{Width: 100, Height: 30})
	model = drain(model, h.Init())
	h2 := topOf(model)
	v = h2.View().Content
	if !strings.Contains(v, "OK") {
		t.Fatalf("healthy provider must show OK:\n%s", v)
	}
	if !strings.Contains(v, "Error") {
		t.Fatalf("failed provider must show Error:\n%s", v)
	}
	if hc.checked["animego"] != "test" {
		t.Fatalf("health check must use the test query, got %q", hc.checked["animego"])
	}
}

// fakeHealth implements HealthService.
type fakeHealth struct {
	errFor  map[string]error
	checked map[string]string
}

func (f *fakeHealth) Check(_ context.Context, providerID string) error {
	if f.checked == nil {
		f.checked = map[string]string{}
	}
	f.checked[providerID] = "test"
	return f.errFor[providerID]
}

var _ HealthService = (*fakeHealth)(nil)
