package tui

// PR62 owner defects #2/#3: the provider binding must PERSIST when the
// provider checklist resolves (the «!» badge clears and the rebind
// search does not re-run on re-entry), and bound titles need a manual
// «Перепривязать» path.

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/storage"
)

// bindCall is one recorded HistoryService.BindSource call.
type bindCall struct {
	id         int64
	sourceID   string
	sourceURL  string
	boundTitle string
}

// settleTwoProviders settles the fan-out with one result per provider
// (animego then anilib; the sorted-first primary is animego).
func settleTwoProviders(t *testing.T, rp *rebindProgress) *rebindProgress {
	t.Helper()
	rp.searchProgress = settleSearch(rp.searchProgress,
		providerResultMsg{provider: ProviderMeta{ID: "animego"}, results: []contracts.SearchResult{
			{Title: "Наруто", URL: "u1", SourceID: "animego"},
		}},
		providerResultMsg{provider: ProviderMeta{ID: "anilib"}, results: []contracts.SearchResult{
			{Title: "Наруто", URL: "u2", SourceID: "anilib"},
		}})
	return rp
}

// TestRebindPickPersistsBinding (PR62 #2): resolving the checklist in
// the catalog flow persists the binding — the record moves onto the
// checked primary's (source_id, url) with the checked title — BEFORE
// the session opens. The owner's «выбрал провайдеры = привязал» intent.
func TestRebindPickPersistsBinding(t *testing.T) {
	_, deps := checklistTestDeps()
	hist := &fakeHistory{}
	deps.History = hist
	rec := rebindTestRecord() // ID 7, NeedsCorrection: true

	rp := settleTwoProviders(t, newRebindProgress(deps, rec))
	next, _ := rp.Update(tea.KeyPressMsg{Code: 'a'}) // check all
	rp = next.(*rebindProgress)
	_, cmd := rp.Update(enter())
	if _, ok := firstMsg(cmd).(replaceMsg); !ok {
		t.Fatalf("catalog pick must open the session, got %T", firstMsg(cmd))
	}
	if len(hist.bindCalls) != 1 {
		t.Fatalf("the pick must persist the binding via BindSource, got %+v", hist.bindCalls)
	}
	call := hist.bindCalls[0]
	// stablePrimary is SourceID-sorted-first: "anilib" < "animego".
	if call.id != rec.ID || call.sourceID != "anilib" || call.sourceURL != "u2" || call.boundTitle != "Наруто" {
		t.Fatalf("bind call = %+v, want {7 anilib u2 Наруто} (the sorted-first primary)", call)
	}
}

// TestFreshPickDoesNotBind (PR62 #2): the plain search flow has no
// record to bind — the checklist pick must not touch the history
// service (the record is created at playback save, python parity).
func TestFreshPickDoesNotBind(t *testing.T) {
	fs, deps := checklistTestDeps()
	hist := &fakeHistory{}
	deps.History = hist

	sp := NewSearchProgress(deps, "наруто")
	sp = settleSearch(sp,
		providerResultMsg{provider: fs.providers[0], results: []contracts.SearchResult{
			{Title: "Наруто", URL: "u1", SourceID: "animego"},
		}},
		providerResultMsg{provider: fs.providers[1], results: []contracts.SearchResult{
			{Title: "Наруто", URL: "u2", SourceID: "anilib"},
		}})
	next, _ := sp.Update(tea.KeyPressMsg{Code: 'a'})
	sp2 := next.(*searchProgress)
	sp2.Update(enter())
	if len(hist.bindCalls) != 0 {
		t.Fatalf("fresh flow must not bind (no record yet), got %+v", hist.bindCalls)
	}
}

// TestRealHistoryBindSourceClearsCorrectionWithBoundTitle (PR62 #2):
// the real store move carries the bound title, computes the similarity
// against the canonical shikimori title and clears needs_correction —
// the «!» badge's data.
func TestRealHistoryBindSourceClearsCorrectionWithBoundTitle(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	h := &realHistory{store: store}

	shiki := int64(21)
	canonical := "Магическая битва"
	rec := storage.AnimeProgress{
		Title:           canonical,
		SourceID:        "shikimori",
		SourceURL:       "21",
		ShikimoriID:     &shiki,
		ShikimoriTitle:  &canonical,
		NeedsCorrection: true,
		UpdatedAt:       nowUTC(),
	}
	if err := store.Progress.Upsert(ctx, &rec); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := h.BindSource(ctx, rec.ID, "anilib", "https://anilib/lib", "Магическая битва TV"); err != nil {
		t.Fatalf("BindSource: %v", err)
	}
	// The move re-keys the row (new (source_id, url) → new row id);
	// the shikimori binding finds it again.
	got, err := store.Progress.GetByShikimoriID(ctx, 21)
	if err != nil {
		t.Fatalf("reload by shikimori id: %v", err)
	}
	if got.NeedsCorrection {
		t.Fatalf("needs_correction must clear, got %+v", got)
	}
	if got.SourceID != "anilib" || got.SourceURL != "https://anilib/lib" {
		t.Fatalf("source keys must move, got %s/%s", got.SourceID, got.SourceURL)
	}
	if got.BoundTitle == nil || *got.BoundTitle != "Магическая битва TV" {
		t.Fatalf("bound title must persist, got %v", got.BoundTitle)
	}
	if got.BoundSimilarity == nil || *got.BoundSimilarity <= 0.6 {
		t.Fatalf("bound similarity must be computed above the binding threshold, got %v", got.BoundSimilarity)
	}
	if got.ShikimoriID == nil || *got.ShikimoriID != 21 {
		t.Fatalf("the shikimori binding must survive the move, got %v", got.ShikimoriID)
	}
}

// TestHistoryBadgeClearsAfterBind (PR62 #2): a bound record renders
// the status badge, not [⚠] — the owner-visible «!» clear.
func TestHistoryBadgeClearsAfterBind(t *testing.T) {
	bound := storage.AnimeProgress{ShikimoriStatus: "watching", NeedsCorrection: false}
	if got := historyBadge(bound); got == "[⚠]" {
		t.Fatalf("bound record must not render the [⚠] badge, got %q", got)
	}
}

// sessionMenuItem finds a menu item by id.
func sessionMenuItem(s *sessionScreen, id string) *Choice {
	for i := range s.list.Menu().Items {
		if s.list.Menu().Items[i].ID == id {
			return &s.list.Menu().Items[i]
		}
	}
	return nil
}

// rebindSession builds a resumed session (record ID 7) with episodes
// loaded and the action menu open.
func rebindSession(t *testing.T) *sessionScreen {
	t.Helper()
	deps := &Deps{
		Episode: &fakeEpisode{episodes: testEpisodeSet()},
		History: &fakeHistory{},
	}
	primary := contracts.SearchResult{Title: "Наруто", URL: "u2", SourceID: "anilib"}
	group := []contracts.SearchResult{primary, {Title: "Наруто", URL: "u1", SourceID: "animego"}}
	s := newResumedSession(deps, primary, group, storage.AnimeProgress{
		ID: 7, Title: "Наруто", SourceID: "anilib", SourceURL: "u2", CurrentEpisode: "2",
	})
	s.loadEpisodesSync()
	return s
}

// TestSessionRebindMenuItemPresentForResumedRecord (PR62 #3): a
// session backed by a stored record offers «🔄 Перепривязать» — the
// manual re-binding path (e.g. a new source appeared).
func TestSessionRebindMenuItemPresentForResumedRecord(t *testing.T) {
	s := rebindSession(t)
	item := sessionMenuItem(s, "rebind")
	if item == nil {
		t.Fatalf("resumed session must offer «Перепривязать», got %v", s.list.Menu().Items)
	}
	if item.Label != "🔗 Rebind" {
		t.Fatalf("label = %q, want «🔗 Rebind»", item.Label)
	}
}

// TestSessionRebindMenuItemAbsentForFreshSession (PR62 #3): a fresh
// session has no stored binding to re-run — no item.
func TestSessionRebindMenuItemAbsentForFreshSession(t *testing.T) {
	deps := &Deps{
		Episode: &fakeEpisode{episodes: testEpisodeSet()},
		History: &fakeHistory{},
	}
	s := NewSessionScreen(deps,
		contracts.SearchResult{Title: "Наруто", URL: "u1", SourceID: "animego"},
		[]contracts.SearchResult{{Title: "Наруто", URL: "u1", SourceID: "animego"}})
	s.loadEpisodesSync()
	if item := sessionMenuItem(s, "rebind"); item != nil {
		t.Fatalf("fresh session must not offer «Перепривязать», got %+v", item)
	}
}

// TestSessionRebindRerunsSearchForRecord (PR62 #3): picking
// «Перепривязать» replaces the session with the catalog fan-out over
// the SAME record; the subsequent pick re-persists the binding (the
// old source row is deleted by the move), closing the loop.
func TestSessionRebindRerunsSearchForRecord(t *testing.T) {
	s := rebindSession(t)
	item := sessionMenuItem(s, "rebind")
	if item == nil {
		t.Fatalf("resumed session must offer «Перепривязать»")
	}
	for i := range s.list.Menu().Items {
		if s.list.Menu().Items[i].ID == "rebind" {
			s.list.Jump(i)
			break
		}
	}
	_, cmd := s.Update(enter())
	if cmd == nil {
		t.Fatalf("the pick must navigate")
	}
	msg := cmd()
	rm, ok := msg.(replaceMsg)
	if !ok {
		t.Fatalf("rebind must replace the session, got %#v", msg)
	}
	rp, ok := rm.screen.(*rebindProgress)
	if !ok {
		t.Fatalf("rebind must open the catalog fan-out, got %T", rm.screen)
	}
	if rp.rec == nil || rp.rec.ID != 7 {
		t.Fatalf("the fan-out must carry the same record, got %+v", rp.rec)
	}
}
