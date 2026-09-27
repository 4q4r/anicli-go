package sync

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/an0nx/anicli-go/internal/mal"
	"github.com/an0nx/anicli-go/internal/shikimori"
	"github.com/an0nx/anicli-go/internal/storage"
)

// --- fakes -----------------------------------------------------------

// fakeProvider is a scriptable SyncProvider.
type fakeProvider struct {
	key          string
	participates bool
	ready        bool
	err          error
	calls        int
	lastReq      PushRequest
}

func (f *fakeProvider) Key() string        { return f.key }
func (f *fakeProvider) Participates() bool { return f.participates }
func (f *fakeProvider) Ready() bool        { return f.ready }
func (f *fakeProvider) SyncEpisodeProgress(_ context.Context, req PushRequest) error {
	f.calls++
	f.lastReq = req
	return f.err
}

// fakeHistory is a scriptable History seam.
type fakeHistory struct {
	row    *storage.AnimeProgress
	getErr error
	rateID int64
}

func (f *fakeHistory) GetByShikimoriID(_ context.Context, _ int64) (*storage.AnimeProgress, error) {
	return f.row, f.getErr
}

func (f *fakeHistory) SetRateID(_ context.Context, _, rateID int64) error {
	f.rateID = rateID
	return nil
}

// fakePusher is a scriptable Shikimori tracker surface.
type fakePusher struct {
	enabled bool
	mode    string
	rate    int64
	err     error
	calls   int
	lastID  int64
	lastEp  int
	lastSt  string
}

func (f *fakePusher) Enabled() bool { return f.enabled }
func (f *fakePusher) Mode() string  { return f.mode }
func (f *fakePusher) UpdateEpisodes(_ context.Context, shikimoriID, _ int64, episodes int, status string) (int64, error) {
	f.calls++
	f.lastID, f.lastEp, f.lastSt = shikimoriID, episodes, status
	return f.rate, f.err
}

// fakeMAL is a scriptable MAL surface.
type fakeMAL struct {
	enabled bool
	authed  bool
	err     error
	calls   int
	lastID  int64
	lastIn  mal.ListInput
}

func (f *fakeMAL) Enabled() bool       { return f.enabled }
func (f *fakeMAL) Authenticated() bool { return f.authed }
func (f *fakeMAL) UpdateMyListStatus(_ context.Context, animeID int64, in mal.ListInput) error {
	f.calls++
	f.lastID, f.lastIn = animeID, in
	return f.err
}

// fakeCards removed — the card source is covered by fakeCardSource.

// --- dispatcher ------------------------------------------------------

func TestDispatcherBothSync(t *testing.T) {
	t.Parallel()

	shiki := &fakeProvider{key: "shikimori", participates: true, ready: true}
	malp := &fakeProvider{key: "myanimelist", participates: true, ready: true}
	d := NewDispatcher(&fakeHistory{}, nil, shiki, malp)

	rep := d.SyncEpisodeProgress(context.Background(), 21, 5)
	if !rep.Participated || rep.NoRollback {
		t.Fatalf("report = %+v", rep)
	}
	if len(rep.Verdicts) != 2 {
		t.Fatalf("verdicts = %d, want 2", len(rep.Verdicts))
	}
	if shiki.calls != 1 || malp.calls != 1 {
		t.Errorf("pushes: shiki=%d mal=%d, want 1/1", shiki.calls, malp.calls)
	}
	for i, v := range rep.Verdicts {
		if !v.Synced {
			t.Errorf("verdict[%d] = %+v, want synced", i, v)
		}
	}
}

func TestDispatcherShikimoriOnly(t *testing.T) {
	t.Parallel()

	shiki := &fakeProvider{key: "shikimori", participates: true, ready: true}
	malp := &fakeProvider{key: "myanimelist", participates: false, ready: false}
	d := NewDispatcher(&fakeHistory{}, nil, shiki, malp)

	rep := d.SyncEpisodeProgress(context.Background(), 21, 5)
	if !rep.Participated {
		t.Fatalf("report = %+v, want participated", rep)
	}
	if len(rep.Verdicts) != 1 || rep.Verdicts[0].Key != "shikimori" {
		t.Fatalf("verdicts = %+v, want shikimori only", rep.Verdicts)
	}
	if malp.calls != 0 {
		t.Error("disabled provider must not be called")
	}
}

func TestDispatcherMALOnly(t *testing.T) {
	t.Parallel()

	shiki := &fakeProvider{key: "shikimori", participates: false}
	malp := &fakeProvider{key: "myanimelist", participates: true, ready: true}
	d := NewDispatcher(&fakeHistory{}, nil, shiki, malp)

	rep := d.SyncEpisodeProgress(context.Background(), 21, 5)
	if len(rep.Verdicts) != 1 || rep.Verdicts[0].Key != "myanimelist" {
		t.Fatalf("verdicts = %+v, want myanimelist only", rep.Verdicts)
	}
	if shiki.calls != 0 {
		t.Error("disabled provider must not be called")
	}
}

func TestDispatcherNothingParticipates(t *testing.T) {
	t.Parallel()

	shiki := &fakeProvider{key: "shikimori", participates: false}
	malp := &fakeProvider{key: "myanimelist", participates: false}
	d := NewDispatcher(&fakeHistory{}, nil, shiki, malp)

	rep := d.SyncEpisodeProgress(context.Background(), 21, 5)
	if rep.Participated || len(rep.Verdicts) != 0 || rep.NoRollback {
		t.Fatalf("report = %+v, want silent skip", rep)
	}
	if shiki.calls != 0 || malp.calls != 0 {
		t.Error("no provider may be called")
	}
}

// TestDispatcherEnabledButUnauthenticated pins the typed auth note: an
// enabled provider without credentials participates with a skip verdict,
// it is never called.
func TestDispatcherEnabledButUnauthenticated(t *testing.T) {
	t.Parallel()

	shiki := &fakeProvider{key: "shikimori", participates: true, ready: true}
	malp := &fakeProvider{key: "myanimelist", participates: true, ready: false}
	d := NewDispatcher(&fakeHistory{}, nil, shiki, malp)

	rep := d.SyncEpisodeProgress(context.Background(), 21, 5)
	if len(rep.Verdicts) != 2 {
		t.Fatalf("verdicts = %+v", rep.Verdicts)
	}
	malv := rep.Verdicts[1]
	if malv.Synced || malv.Note != NoteAuthRequired || malv.Err != nil {
		t.Errorf("mal verdict = %+v, want typed auth skip", malv)
	}
	if malp.calls != 0 {
		t.Error("unauthenticated provider must not be called")
	}
}

// TestDispatcherMALMappingFailure pins the owner case: the title is not
// on MAL — the MAL push is skipped with a typed note while Shikimori
// still syncs.
func TestDispatcherMALMappingFailure(t *testing.T) {
	t.Parallel()

	shiki := &fakeProvider{key: "shikimori", participates: true, ready: true}
	malp := &fakeProvider{key: "myanimelist", participates: true, ready: true, err: ErrNotOnMAL}
	d := NewDispatcher(&fakeHistory{}, nil, shiki, malp)

	rep := d.SyncEpisodeProgress(context.Background(), 21, 5)
	if len(rep.Verdicts) != 2 {
		t.Fatalf("verdicts = %+v", rep.Verdicts)
	}
	if !rep.Verdicts[0].Synced {
		t.Errorf("shiki verdict = %+v, want synced", rep.Verdicts[0])
	}
	if rep.Verdicts[1].Synced || rep.Verdicts[1].Note != NoteNotOnMAL {
		t.Errorf("mal verdict = %+v, want typed not-on-MAL skip", rep.Verdicts[1])
	}
}

// TestDispatcherOneFailsOneSucceeds pins: both verdicts render even
// when only one push succeeded.
func TestDispatcherOneFailsOneSucceeds(t *testing.T) {
	t.Parallel()

	shiki := &fakeProvider{key: "shikimori", participates: true, ready: true}
	malp := &fakeProvider{key: "myanimelist", participates: true, ready: true,
		err: errors.New("network down")}
	d := NewDispatcher(&fakeHistory{}, nil, shiki, malp)

	rep := d.SyncEpisodeProgress(context.Background(), 21, 5)
	if len(rep.Verdicts) != 2 {
		t.Fatalf("verdicts = %+v, want both", rep.Verdicts)
	}
	if !rep.Verdicts[0].Synced {
		t.Errorf("shiki = %+v, want synced", rep.Verdicts[0])
	}
	if rep.Verdicts[1].Synced || !errors.Is(rep.Verdicts[1].Err, malp.err) {
		t.Errorf("mal = %+v, want the push error", rep.Verdicts[1])
	}
}

// TestDispatcherNoRollbackGuard pins the shared counter guard: one local
// row guard stops both providers (the counter never rolls back).
func TestDispatcherNoRollbackGuard(t *testing.T) {
	t.Parallel()

	shiki := &fakeProvider{key: "shikimori", participates: true, ready: true}
	malp := &fakeProvider{key: "myanimelist", participates: true, ready: true}
	d := NewDispatcher(&fakeHistory{row: &storage.AnimeProgress{
		ID: 7, CurrentEpisode: "9", ShikimoriRateID: &[]int64{55}[0],
		ShikimoriStatus: "watching",
	}}, nil, shiki, malp)

	rep := d.SyncEpisodeProgress(context.Background(), 21, 5)
	if !rep.NoRollback || !rep.Participated || len(rep.Verdicts) != 0 {
		t.Fatalf("report = %+v, want rollback guard with no verdicts", rep)
	}
	if shiki.calls != 0 || malp.calls != 0 {
		t.Error("guarded push must not reach any provider")
	}
}

// TestDispatcherResolvesRowContext pins: the dispatcher resolves the
// local row once (rate id, planned→watching on play) and shares it with
// both providers.
func TestDispatcherResolvesRowContext(t *testing.T) {
	t.Parallel()

	rateID := int64(55)
	shiki := &fakeProvider{key: "shikimori", participates: true, ready: true}
	malp := &fakeProvider{key: "myanimelist", participates: true, ready: true}
	d := NewDispatcher(&fakeHistory{row: &storage.AnimeProgress{
		ID: 7, CurrentEpisode: "3", ShikimoriRateID: &rateID,
		ShikimoriStatus: "planned",
	}}, nil, shiki, malp)

	rep := d.SyncEpisodeProgress(context.Background(), 21, 5)
	if len(rep.Verdicts) != 2 {
		t.Fatalf("verdicts = %+v", rep.Verdicts)
	}
	if shiki.lastReq.RateID != 55 || shiki.lastReq.AnimeID != 7 {
		t.Errorf("shiki request = %+v, want rate 55 anime 7", shiki.lastReq)
	}
	if shiki.lastReq.Status != "watching" {
		t.Errorf("status = %q, want watching (planned→watching on play)", shiki.lastReq.Status)
	}
	if malp.lastReq.Status != "watching" {
		t.Errorf("mal status = %q, want the same resolved status", malp.lastReq.Status)
	}
}

// --- ShikimoriSync ---------------------------------------------------

func TestShikimoriSyncPushAndPersist(t *testing.T) {
	t.Parallel()

	pusher := &fakePusher{enabled: true, mode: "bearer", rate: 99}
	hist := &fakeHistory{}
	p := &ShikimoriSync{Shiki: pusher, History: hist, Log: testLogger()}

	err := p.SyncEpisodeProgress(context.Background(), PushRequest{
		ShikimoriID: 21, Episode: 5, Status: "watching", AnimeID: 7,
	})
	if err != nil {
		t.Fatalf("SyncEpisodeProgress: %v", err)
	}
	if pusher.lastID != 21 || pusher.lastEp != 5 || pusher.lastSt != "watching" {
		t.Errorf("push = %+v", pusher)
	}
	if hist.rateID != 99 {
		t.Errorf("created rate id %d not persisted, want 99", hist.rateID)
	}
}

func TestShikimoriSyncSkipsPersistOnExistingRate(t *testing.T) {
	t.Parallel()

	pusher := &fakePusher{enabled: true, mode: "bearer", rate: 99}
	hist := &fakeHistory{}
	p := &ShikimoriSync{Shiki: pusher, History: hist, Log: testLogger()}

	err := p.SyncEpisodeProgress(context.Background(), PushRequest{
		ShikimoriID: 21, Episode: 5, Status: "watching", RateID: 55, AnimeID: 7,
	})
	if err != nil {
		t.Fatalf("SyncEpisodeProgress: %v", err)
	}
	if hist.rateID != 0 {
		t.Errorf("SetRateID called for an existing rate (rateID=%d)", hist.rateID)
	}
}

func TestShikimoriSyncErrorPassthrough(t *testing.T) {
	t.Parallel()

	want := errors.New("boom")
	p := &ShikimoriSync{Shiki: &fakePusher{enabled: true, mode: "bearer", err: want}, Log: testLogger()}
	if err := p.SyncEpisodeProgress(context.Background(), PushRequest{ShikimoriID: 1, Episode: 1, Status: "watching"}); !errors.Is(err, want) {
		t.Fatalf("error = %v, want the push error", err)
	}
}

// --- MALSync ---------------------------------------------------------

func TestMALSyncPushesMappedStatus(t *testing.T) {
	t.Parallel()

	pusher := &fakeMAL{enabled: true, authed: true}
	resolver := &fakeResolver{id: 30, calls: 0}
	p := &MALSync{MAL: pusher, Resolver: resolver, Log: testLogger()}

	err := p.SyncEpisodeProgress(context.Background(), PushRequest{
		ShikimoriID: 21, Episode: 5, Status: "planned",
	})
	_ = err
	if pusher.calls != 1 {
		t.Fatalf("calls = %d, want 1", pusher.calls)
	}
	if pusher.lastID != 30 {
		t.Errorf("anime id = %d, want the resolved 30", pusher.lastID)
	}
	if pusher.lastIn.Status != "plan_to_watch" {
		t.Errorf("status = %q, want plan_to_watch", pusher.lastIn.Status)
	}
	if pusher.lastIn.NumWatchedEpisodes != 5 {
		t.Errorf("episodes = %d, want 5", pusher.lastIn.NumWatchedEpisodes)
	}
}

func TestMALSyncRewatchingFlag(t *testing.T) {
	t.Parallel()

	pusher := &fakeMAL{enabled: true, authed: true}
	p := &MALSync{MAL: pusher, Resolver: &fakeResolver{id: 30}, Log: testLogger()}

	if err := p.SyncEpisodeProgress(context.Background(), PushRequest{
		ShikimoriID: 21, Episode: 5, Status: "rewatching",
	}); err != nil {
		t.Fatalf("SyncEpisodeProgress: %v", err)
	}
	if !pusher.lastIn.IsRewatching {
		t.Errorf("input = %+v, want the rewatching flag", pusher.lastIn)
	}
	if pusher.lastIn.Status != "watching" {
		t.Errorf("status = %q, want watching", pusher.lastIn.Status)
	}
}

func TestMALSyncUnknownStatusFallsBackWatching(t *testing.T) {
	t.Parallel()

	pusher := &fakeMAL{enabled: true, authed: true}
	p := &MALSync{MAL: pusher, Resolver: &fakeResolver{id: 30}, Log: testLogger()}

	if err := p.SyncEpisodeProgress(context.Background(), PushRequest{
		ShikimoriID: 21, Episode: 5, Status: "gibberish",
	}); err != nil {
		t.Fatalf("SyncEpisodeProgress: %v", err)
	}
	if pusher.lastIn.Status != "watching" {
		t.Errorf("status = %q, want the watching fallback", pusher.lastIn.Status)
	}
}

func TestMALSyncNotOnMALPassthrough(t *testing.T) {
	t.Parallel()

	p := &MALSync{MAL: &fakeMAL{enabled: true, authed: true},
		Resolver: &fakeResolver{err: ErrNotOnMAL}, Log: testLogger()}

	err := p.SyncEpisodeProgress(context.Background(), PushRequest{ShikimoriID: 21, Episode: 5, Status: "watching"})
	if !errors.Is(err, ErrNotOnMAL) {
		t.Fatalf("error = %v, want ErrNotOnMAL", err)
	}
}

// --- MALIDResolver ---------------------------------------------------

func TestCardResolverCacheHit(t *testing.T) {
	t.Parallel()

	store := &fakeStore{cached: 30, has: true}
	r := &CardMALIDResolver{Store: store, Log: testLogger()}

	id, err := r.ResolveMALID(context.Background(), 21)
	if err != nil || id != 30 {
		t.Fatalf("resolve = (%d,%v), want (30,nil)", id, err)
	}
	if store.sets != 0 {
		t.Error("cache hit must not re-persist")
	}
}

func TestCardResolverFetchesAndCaches(t *testing.T) {
	t.Parallel()

	store := &fakeStore{}
	r := &CardMALIDResolver{Cards: &fakeCardSource{payload: `{"id":21,"mal_id":30}`}, Store: store, Log: testLogger()}

	id, err := r.ResolveMALID(context.Background(), 21)
	if err != nil || id != 30 {
		t.Fatalf("resolve = (%d,%v), want (30,nil)", id, err)
	}
	if store.sets != 1 || store.lastID != 30 {
		t.Errorf("cache writes = %d lastID=%d, want one write of 30", store.sets, store.lastID)
	}
}

func TestCardResolverLinksFallback(t *testing.T) {
	t.Parallel()

	r := &CardMALIDResolver{Cards: &fakeCardSource{
		payload: `{"id":21,"links":[{"url":"https://myanimelist.net/anime/21/One_Piece","kind":"myanimelist"}]}`,
	}, Log: testLogger()}

	id, err := r.ResolveMALID(context.Background(), 21)
	if err != nil || id != 21 {
		t.Fatalf("resolve = (%d,%v), want (21,nil) from the links URL", id, err)
	}
}

func TestCardResolverNotOnMAL(t *testing.T) {
	t.Parallel()

	r := &CardMALIDResolver{Cards: &fakeCardSource{payload: `{"id":999}`}, Log: testLogger()}
	if _, err := r.ResolveMALID(context.Background(), 999); !errors.Is(err, ErrNotOnMAL) {
		t.Fatalf("error = %v, want ErrNotOnMAL", err)
	}
}

func TestCardResolverCardErrorIsNotSkip(t *testing.T) {
	t.Parallel()

	want := errors.New("network down")
	r := &CardMALIDResolver{Cards: &fakeCardSource{err: want}, Log: testLogger()}
	if _, err := r.ResolveMALID(context.Background(), 21); !errors.Is(err, want) || errors.Is(err, ErrNotOnMAL) {
		t.Fatalf("error = %v, want the card error (a transient failure is not a mapping miss)", err)
	}
}

// --- helpers ---------------------------------------------------------

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(&discardWriter{}, nil)) }

type discardWriter struct{}

func (*discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// fakeResolver is a scriptable MALIDResolver resolver seam.
type fakeResolver struct {
	id    int64
	err   error
	calls int
}

func (f *fakeResolver) ResolveMALID(_ context.Context, _ int64) (int64, error) {
	f.calls++
	return f.id, f.err
}

// fakeStore is a scriptable IDMapStore.
type fakeStore struct {
	cached int64
	has    bool
	sets   int
	lastID int64
}

func (f *fakeStore) Get(_ context.Context, _ int64) (int64, bool, error) {
	return f.cached, f.has, nil
}

func (f *fakeStore) Set(_ context.Context, _, malID int64) error {
	f.sets++
	f.lastID = malID
	return nil
}

// fakeCardSource answers GetAnime with a fixed payload.
type fakeCardSource struct {
	payload string
	err     error
}

func (f *fakeCardSource) GetAnime(_ context.Context, _ int64) (*shikimori.Anime, error) {
	if f.err != nil {
		return nil, f.err
	}
	anime := &shikimori.Anime{}
	if err := json.Unmarshal([]byte(f.payload), anime); err != nil {
		return nil, err
	}
	return anime, nil
}
