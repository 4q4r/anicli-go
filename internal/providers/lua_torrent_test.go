package providers

// The luaTorrent adapter (PR143): the Go torrent plumbing wrapped
// around a Lua-served torrent roster slot. The bundled script serves
// the search surface; the adapter owns everything the sandbox cannot
// carry — the PR66 .torrent preflight, the engine ingestion, the
// EpisodesWait resolve and the loopback stream — byte-identical to
// the compiled torrent providers' TorrentBase behavior. These unit
// pins run against a fake inner provider so the adapter contract is
// proven in isolation from any script (the anirena×adapter
// integration pins live in anirena_test.go).

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
	"github.com/an0nx/anicli-go/internal/torrent"
)

// fakeInnerProvider is a minimal contracts.Provider double with the
// optional capability surfaces the adapter must forward.
type fakeInnerProvider struct {
	searchCalls int
	results     []contracts.SearchResult
	err         error

	client *netclient.Client
	logged *slog.Logger
	lang   string
	pref   contracts.NamePreference
	smoke  string
}

var _ contracts.Provider = (*fakeInnerProvider)(nil)

func (f *fakeInnerProvider) ID() string                               { return "fakeprov" }
func (f *fakeInnerProvider) Name() string                             { return "FakeProv" }
func (f *fakeInnerProvider) BaseURL() string                          { return "https://fake.example" }
func (f *fakeInnerProvider) SourceType() contracts.SourceType         { return contracts.SourceTypeBoth }
func (f *fakeInnerProvider) ContentLanguage() string                  { return f.lang }
func (f *fakeInnerProvider) NamePreference() contracts.NamePreference { return f.pref }
func (f *fakeInnerProvider) SmokeQuery() string                       { return f.smoke }

func (f *fakeInnerProvider) SetLogger(log *slog.Logger) { f.logged = log }

// HTTPClient is the transport seam the adapter probes through (the
// lua.Provider surface it wraps in production).
func (f *fakeInnerProvider) HTTPClient() *netclient.Client { return f.client }

func (f *fakeInnerProvider) Search(_ context.Context, _ string) ([]contracts.SearchResult, error) {
	f.searchCalls++
	if f.err != nil {
		return nil, f.err
	}
	return f.results, nil
}

func (f *fakeInnerProvider) GetEpisodes(_ context.Context, _ string) ([]contracts.Episode, error) {
	return nil, errors.New("fakeprov: episodes must never reach the inner provider")
}

func (f *fakeInnerProvider) ResolveStream(_ context.Context, _ contracts.Episode, _ string) (contracts.MediaStream, error) {
	return contracts.MediaStream{}, errors.New("fakeprov: streams must never reach the inner provider")
}

// TestLuaTorrentIsTorrent pins the capability marker: the adapter
// carries contracts.TorrentProvider so the registry's TorrentProviderIDs
// and the parity smoke's torrent leg route the slot through the
// metadata+files resolve.
func TestLuaTorrentIsTorrent(t *testing.T) {
	t.Parallel()

	a := newLuaTorrent(&fakeInnerProvider{}, nil, nil)
	var tp contracts.TorrentProvider = a
	if !tp.IsTorrent() {
		t.Fatal("the adapter must carry the torrent capability")
	}
}

// TestLuaTorrentSearchPreflightsAndFeedsEngine pins the core delta:
// the adapter runs the PR66 preflight over the inner search's results
// (dead .torrent hosts dropped) and the survivors feed the engine —
// the exact TorrentBase behavior the compiled providers get from
// their Search.
func TestLuaTorrentSearchPreflightsAndFeedsEngine(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, mi, _ := seedReleaseFile(t, dir, "Test Show - 01.mkv", 256*1024)
	var torrentBytes bytes.Buffer
	if err := mi.Write(&torrentBytes); err != nil {
		t.Fatalf("serialize metainfo: %v", err)
	}

	var fetches atomic.Int64
	live := torrentFixtureServer(t, torrentBytes.Bytes(), &fetches)
	dead := newDeadListener(t)

	inner := &fakeInnerProvider{
		client: testClient(t, "fakeprov"),
		results: []contracts.SearchResult{
			{Title: "live", URL: live.URL + "/download/1.torrent"},
			{Title: "dead", URL: "http://" + dead.Addr().String() + "/download/2.torrent"},
		},
	}
	a := newLuaTorrent(inner, nil, nil)
	a.SetEngine(newOfflineTestEngine(t))

	results, err := a.Search(context.Background(), "show")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].Title != "live" {
		t.Fatalf("results = %v, want only the live result (the dead host dropped)", results)
	}
	if fetches.Load() != 1 {
		t.Errorf("preflight fetches = %d, want 1", fetches.Load())
	}
}

// TestLuaTorrentSearchNoEngineSkipsPreflight pins the nil-engine rule
// (the production shape of All() without NewRegistry: the transport
// is wired, the engine is not): nothing is preflighted or fed — the
// surface passes through untouched.
func TestLuaTorrentSearchNoEngineSkipsPreflight(t *testing.T) {
	t.Parallel()

	hits := 0
	srv := anirenaServerCounted(t, anirenaFeed(
		anirenaItemXML("[Anime > RAW] Show - 01", "Size: 1.0 GB | Uploader: u | Category: Anime &gt; RAW", "https://www.anirena.com/torrents/x1.torrent"),
	), &hits)
	inner := &fakeInnerProvider{
		client: testClient(t, "fakeprov"),
		results: []contracts.SearchResult{
			{Title: "r", URL: srv + "/download/1.torrent"},
		},
	}
	a := newLuaTorrent(inner, nil, nil)

	results, err := a.Search(context.Background(), "q")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %v, want the untouched surface", results)
	}
	if hits != 0 {
		t.Errorf("fixture hits = %d, want 0 (no preflight without an engine)", hits)
	}
}

// TestLuaTorrentGetEpisodesDelegatesToTorrentBase pins the plumbing
// delegation: GetEpisodes rides TorrentBase.EpisodesWait — with no
// engine it fails loud with the base's not-wired error, never
// reaching the inner provider.
func TestLuaTorrentGetEpisodesDelegatesToTorrentBase(t *testing.T) {
	t.Parallel()

	inner := &fakeInnerProvider{}
	a := newLuaTorrent(inner, nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := a.GetEpisodes(ctx, linkA)
	if err == nil {
		t.Fatal("GetEpisodes without an engine must fail loud")
	}
	if !strings.Contains(err.Error(), "engine is not wired") {
		t.Errorf("error = %v, want the TorrentBase not-wired wall", err)
	}
}

// TestLuaTorrentSetEngineWiresTorrentBase: the registry's duck-type
// injection must reach the base the adapter delegates to.
func TestLuaTorrentSetEngineWiresTorrentBase(t *testing.T) {
	t.Parallel()

	a := newLuaTorrent(&fakeInnerProvider{}, nil, nil)
	var se interface{ SetEngine(*torrent.Engine) } = a
	se.SetEngine(newOfflineTestEngine(t))

	if a.engineSnapshot() == nil {
		t.Fatal("SetEngine must wire the TorrentBase engine handle")
	}
}

// TestLuaTorrentForwardsCapabilities pins the composite-forwarding
// rule (the caps.go lesson: interface embedding hides the inner
// provider's concrete capability methods — the adapter must forward
// them explicitly or the registry and the parity smoke lose the
// surfaces).
func TestLuaTorrentForwardsCapabilities(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
	inner := &fakeInnerProvider{lang: "xx", pref: contracts.NamePrefLatin, smoke: "sq"}
	a := newLuaTorrent(inner, nil, nil)

	a.SetLogger(log)
	if inner.logged != log {
		t.Error("SetLogger must reach the inner provider")
	}
	if got := a.ContentLanguage(); got != "xx" {
		t.Errorf("ContentLanguage = %q, want the inner value", got)
	}
	if got := a.NamePreference(); got != contracts.NamePrefLatin {
		t.Errorf("NamePreference = %v, want the inner value", got)
	}
	if got := a.SmokeQuery(); got != "sq" {
		t.Errorf("SmokeQuery = %q, want the inner value", got)
	}
	if got := a.ID(); got != "fakeprov" {
		t.Errorf("ID = %q, want the inner value", got)
	}
}

// TestLuaTorrentSearchErrorPropagates: an inner search failure is the
// caller's error, untouched — the adapter never converts failures
// into empty successes.
func TestLuaTorrentSearchErrorPropagates(t *testing.T) {
	t.Parallel()

	wall := errors.New("inner wall")
	a := newLuaTorrent(&fakeInnerProvider{err: wall}, nil, nil)
	a.SetEngine(newOfflineTestEngine(t))

	_, err := a.Search(context.Background(), "q")
	if !errors.Is(err, wall) {
		t.Fatalf("error = %v, want the inner failure verbatim", err)
	}
}
