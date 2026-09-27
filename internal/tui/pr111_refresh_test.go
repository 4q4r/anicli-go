package tui

// PR111 refresh tests (owner defect 1): «Refresh sources» must
// re-run the SAME resolve path as the initial open — the fetch
// fan-out plus the current episode's hydration — so the refreshed
// entries REPLACE the cached ones and the verdict is honest (never
// the blanket «Sources not found» while sources are in hand).
// The shared rotation fixtures live in pr111_fresh_test.go.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// runCmdTree executes a command tree (BatchMsg included) and feeds every
// settle message back into Update, following chained commands until
// the pipeline settles (bounded — a screen never chains forever).
func runCmdTree(t *testing.T, s *sessionScreen, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	msgs := []tea.Msg{cmd()}
	for hops := 0; len(msgs) > 0 && hops < 64; hops++ {
		msg := msgs[0]
		msgs = msgs[1:]
		if msg == nil {
			continue
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				msgs = append(msgs, c())
			}
			continue
		}
		_, next := s.Update(msg)
		if next != nil {
			msgs = append(msgs, next())
		}
	}
}

type resolveRecord struct {
	provider string
	dub      string
	embeds   []string // the embed URLs the resolve was handed
}

type rotateFixture struct {
	mu       sync.Mutex
	fetches  int             // GetEpisodes calls (each rotates the embeds)
	hydrates int             // HydrateDubs calls (each rotates the embeds)
	resolves []resolveRecord // what ResolveStream was asked to extract from
}

// rotateProvider simulates the owner's rotation defect: every fetch
// or hydration round yields a NEW embed URL set, and ResolveStream
// echoes the first embed into the playable URL.
type rotateProvider struct {
	fakeEpisode
	fix *rotateFixture
}

func (r *rotateProvider) GetEpisodes(_ context.Context, providerID, _ string) ([]contracts.Episode, error) {
	r.fix.mu.Lock()
	r.fix.fetches++
	n := r.fix.fetches
	r.fix.mu.Unlock()
	return []contracts.Episode{{
		Num:   "1",
		RawID: "release-1",
		RawEmbeds: map[string][]string{
			"Kodik": {fmt.Sprintf("https://embed/f%d", n)},
		},
	}}, nil
}

func (r *rotateProvider) HydrateDubs(_ context.Context, _ string, episode contracts.Episode) (contracts.Episode, error) {
	r.fix.mu.Lock()
	r.fix.hydrates++
	n := r.fix.hydrates
	r.fix.mu.Unlock()
	if episode.RawEmbeds == nil {
		episode.RawEmbeds = map[string][]string{}
	}
	episode.RawEmbeds["Kodik"] = []string{fmt.Sprintf("https://embed/h%d", n)}
	return episode, nil
}

func (r *rotateProvider) ResolveStream(_ context.Context, providerID string, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	embeds := episode.RawEmbeds[dubID]
	r.fix.mu.Lock()
	r.fix.resolves = append(r.fix.resolves, resolveRecord{provider: providerID, dub: dubID, embeds: embeds})
	r.fix.mu.Unlock()
	if len(embeds) == 0 {
		return contracts.MediaStream{}, errors.New("no embeds")
	}
	return contracts.MediaStream{DubName: dubID, Links: map[string]contracts.VideoSource{
		"720": {URL: "play:" + embeds[0]},
	}}, nil
}

var _ EpisodeService = (*rotateProvider)(nil)

func (f *rotateFixture) lastResolve() resolveRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resolves[len(f.resolves)-1]
}

func (f *rotateFixture) resolveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.resolves)
}

// newRotatingSession builds a one-provider session whose fetch phase
// really ran; the provider's embeds rotate on every fetch/hydrate.
func newRotatingSession(t *testing.T) (*sessionScreen, *rotateFixture) {
	t.Helper()
	fix := &rotateFixture{}
	deps := &Deps{
		Episode:  &rotateProvider{fix: fix},
		Playback: &fakePlayback{},
		Log:      discardLogger(),
	}
	group := []contracts.SearchResult{{Title: "Тайтл", URL: "u", SourceID: "kodik"}}
	s := NewSessionScreen(deps, group[0], group)
	settleInit(t, s)
	return s, fix
}

// TestRefreshRerunsEpisodeFetch (owner defect 1): «🔄 Обновить
// источники» re-runs the SAME fetch path as the initial open, the
// refreshed entries REPLACE the cached ones, and the verdict is
// honest — never the blanket «Sources not found».
func TestRefreshRerunsEpisodeFetch(t *testing.T) {
	s, fix := newRotatingSession(t)
	cached := s.episodes["1"].RawEmbeds["[kodik] Kodik"]
	if len(cached) != 1 || cached[0] != "https://embed/f1" {
		t.Fatalf("pre-refresh cache = %v, want the fetch round f1", cached)
	}

	s.setState(sessionStateMenu)
	s.buildActionMenu()
	s.list.Jump(sessionActionIndex(s, "refresh"))
	_, cmd := s.handleMenuKey(enter())
	if cmd == nil {
		t.Fatal("refresh must schedule the fetch fan-out")
	}
	runCmdTree(t, s, cmd)

	fix.mu.Lock()
	fetches := fix.fetches
	fix.mu.Unlock()
	if fetches != 2 {
		t.Fatalf("GetEpisodes calls = %d, want 2 (the refresh re-runs the fetch phase)", fetches)
	}
	got := s.episodes["1"].RawEmbeds["[kodik] Kodik"]
	if len(got) != 1 || got[0] != "https://embed/f2" {
		t.Fatalf("post-refresh cache = %v, want the REFRESHED fetch round f2 replacing f1", got)
	}
	if s.state != sessionStateMenu {
		t.Fatalf("state = %s, want the rebuilt menu", s.state)
	}
	if s.status == "Sources not found" {
		t.Fatal("refresh reported «Sources not found» — the owner defect — the verdict must be honest")
	}
}

// TestRefreshFailSoftPerProvider: one provider failing the refresh
// fetch must not kill the others — the survivors' fresh entries land,
// the failure is recorded, and the menu rebuilds.
func TestRefreshFailSoftPerProvider(t *testing.T) {
	fix := &rotateFixture{}
	deps := &Deps{
		Episode: &failSecondRoundEpisode{rotateProvider: rotateProvider{fix: fix}},
		Log:     discardLogger(),
	}
	group := []contracts.SearchResult{
		{Title: "Тайтл", URL: "u1", SourceID: "kodik"},
		{Title: "Тайтл", URL: "u2", SourceID: "anilib"},
	}
	s := NewSessionScreen(deps, group[0], group)
	settleInit(t, s)
	if len(s.order) != 1 {
		t.Fatalf("order = %v, want the single merged episode", s.order)
	}

	s.setState(sessionStateMenu)
	s.buildActionMenu()
	s.list.Jump(sessionActionIndex(s, "refresh"))
	_, cmd := s.handleMenuKey(enter())
	runCmdTree(t, s, cmd)

	if got := s.episodes["1"].RawEmbeds["[kodik] Kodik"]; len(got) != 1 || got[0] != "https://embed/f2" {
		t.Fatalf("surviving provider cache = %v, want the fresh f2 round", got)
	}
	if err := s.sourceErrs["anilib"]; err == nil {
		t.Fatalf("sourceErrs = %v, want the failing provider recorded", s.sourceErrs)
	}
	if s.state != sessionStateMenu {
		t.Fatalf("state = %s, want the rebuilt menu after the failed round", s.state)
	}
}

// failSecondRoundEpisode fails ANILIB's fetch from the second round
// on (the refresh round); kodik keeps succeeding — the survivor whose
// fresh entries must land.
type failSecondRoundEpisode struct {
	rotateProvider
}

func (f *failSecondRoundEpisode) GetEpisodes(ctx context.Context, providerID, url string) ([]contracts.Episode, error) {
	if providerID == "anilib" {
		f.fix.mu.Lock()
		n := f.fix.fetches + 1
		f.fix.mu.Unlock()
		if n >= 2 {
			return nil, errors.New("provider down")
		}
	}
	return f.rotateProvider.GetEpisodes(ctx, providerID, url)
}

// TestHydrateZeroWorkHonestVerdict (owner defect 1, unit form): a
// hydration round with nothing to do (every provider already carries
// links — the eager-fetch reality) must NOT report «Источники не
// найдены» while the episode actually has sources.
func TestHydrateZeroWorkHonestVerdict(t *testing.T) {
	s, _ := newRotatingSession(t) // eager embeds from the fetch: f1

	s.hydrated = map[string]bool{"1": true}
	cmd := s.hydrateEpisode("1")
	if cmd == nil {
		t.Fatal("hydrate must schedule a round")
	}
	s.Update(cmd())
	if s.status == "Sources not found" {
		t.Fatalf("status = %q while the episode carries %d source tracks — the owner's false failure",
			s.status, len(s.episodes["1"].RawEmbeds))
	}
}
