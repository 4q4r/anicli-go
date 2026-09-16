package providers

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/torrent"
)

func mustHash(t *testing.T, hex string) torrent.InfoHash {
	t.Helper()
	return torrent.InfoHash(metainfo.NewHashFromHex(hex))
}

func testTorrentConfig(t *testing.T) config.Torrent {
	t.Helper()
	return config.Torrent{Enabled: true, Dir: t.TempDir(), Port: 0, ReadaheadMB: 1}
}

const (
	linkA = "https://torrent.example.org/batch.torrent"
	linkB = "magnet:?xt=urn:btih:fedcba9876543210fedcba9876543210fedcba98"
)

func TestTorrentBaseEpisodesFromFiles(t *testing.T) {
	t.Parallel()

	ih := mustHash(t, "0123456789abcdef0123456789abcdef01234567")
	base := NewTorrentBase(nil)
	base.mu.Lock()
	base.links[linkA] = ih
	base.releases[ih] = torrent.Release{
		InfoHash:    ih,
		DisplayName: "[SubsPlease] Title (01-02) (1080p)",
		Status:      torrent.StatusReady,
		Files: []torrent.FileEntry{
			{Path: "Title - 01.mkv", Size: 100, Index: 0, Episodes: []int{1}},
			{Path: "Title - 02.mkv", Size: 100, Index: 1, Episodes: []int{2}},
		},
	}
	base.mu.Unlock()

	eps, err := base.Episodes(linkA)
	if err != nil {
		t.Fatalf("Episodes: %v", err)
	}
	if len(eps) != 2 {
		t.Fatalf("episodes = %d, want 2 (files as episodes)", len(eps))
	}
	if eps[0].Num != "1" || eps[1].Num != "2" {
		t.Errorf("episode numbers = [%s, %s], want [1, 2] (release-name parsing)", eps[0].Num, eps[1].Num)
	}
	if eps[0].RawID != "0" || eps[1].RawID != "1" {
		t.Errorf("RawIDs = [%s, %s], want file indices [0, 1]", eps[0].RawID, eps[1].RawID)
	}
	if _, ok := eps[0].RawEmbeds["Торрент"]; !ok {
		t.Errorf("episodes must carry the single «Торрент» dub, got %v", eps[0].RawEmbeds)
	}
	if eps[0].Title != "Title - 01.mkv" {
		t.Errorf("episode title = %q, want the file name", eps[0].Title)
	}
}

func TestTorrentBaseEpisodesUnknownLinkFailsLoud(t *testing.T) {
	t.Parallel()
	base := NewTorrentBase(nil)
	if _, err := base.Episodes("https://torrent.example.org/never-ingested"); err == nil {
		t.Fatal("episodes for an un-ingested link must fail loud")
	}
}

func TestTorrentBaseIngestRecordsLink(t *testing.T) {
	t.Parallel()
	base := NewTorrentBase(torrent.NewEngine(testTorrentConfig(t), nil, nil))
	t.Cleanup(func() { _ = base.Close() })

	ih, err := base.Ingest(context.Background(), linkB)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if ih != mustHash(t, "fedcba9876543210fedcba9876543210fedcba98") {
		t.Errorf("Ingest infohash = %s, want the magnet btih", ih.HexString())
	}
	again, err := base.Ingest(context.Background(), linkB)
	if err != nil || again != ih {
		t.Errorf("re-ingest = (%s, %v), want the same infohash (dedupe)", again.HexString(), err)
	}
}

func TestTorrentBaseStreamFailsLoudWithoutServer(t *testing.T) {
	t.Parallel()

	base := NewTorrentBase(torrent.NewEngine(testTorrentConfig(t), nil, nil))
	t.Cleanup(func() { _ = base.Close() })
	base.streamWait = 50 * time.Millisecond

	// Ingest for real (the engine registers the release, metadata
	// never arrives — no seeder), then stream: the bounded resolve
	// must fail loud instead of hanging or handing over an empty URL.
	if _, err := base.Ingest(context.Background(), linkB); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	_, err := base.Stream(linkB, contracts.Episode{Num: "1", RawID: "0"}, "Торрент")
	if err == nil {
		t.Fatal("stream without a running server must fail loud (empty URL is forbidden)")
	}
	if !strings.Contains(err.Error(), "resolve") {
		t.Errorf("error = %v, want the bounded resolve failure", err)
	}
}

func TestTorrentBaseNilEngineIngestFailsLoud(t *testing.T) {
	t.Parallel()
	base := NewTorrentBase(nil)
	if _, err := base.Ingest(context.Background(), linkB); err == nil {
		t.Fatal("ingest on a capability without an engine must fail loud")
	}
}

func TestRegistryFlagsTorrentProviders(t *testing.T) {
	t.Parallel()
	reg := NewEmptyRegistry()
	if len(reg.TorrentProviderIDs()) != 0 {
		t.Fatal("empty registry has no torrent providers")
	}

	stub := &torrentProviderStub{id: "torrent-stub"}
	if err := reg.Register(stub); err != nil {
		t.Fatalf("register: %v", err)
	}
	// The same provider wrapped like the registry wraps (capability
	// must survive the wrapper layers) under a distinct id.
	wrapped := SearchDelegator{Provider: &torrentProviderStub{id: "torrent-stub-wrapped"}}
	if err := reg.Register(wrapped); err != nil {
		t.Fatalf("register wrapped: %v", err)
	}
	got := reg.TorrentProviderIDs()
	if len(got) != 2 {
		t.Errorf("TorrentProviderIDs = %v, want both (capability must survive wrappers)", got)
	}
}

// torrentProviderStub is the minimal torrent-capable provider: it
// proves the capability detection peels the registry wrappers.
type torrentProviderStub struct {
	contracts.Provider
	id string
}

func (s *torrentProviderStub) ID() string   { return s.id }
func (s *torrentProviderStub) Name() string { return "Torrent Stub" }

func (s *torrentProviderStub) Search(_ context.Context, _ string) ([]contracts.SearchResult, error) {
	return nil, errors.New("not used")
}

func (s *torrentProviderStub) GetEpisodes(_ context.Context, _ string) ([]contracts.Episode, error) {
	return nil, errors.New("not used")
}

func (s *torrentProviderStub) ResolveStream(_ context.Context, _ contracts.Episode, _ string) (contracts.MediaStream, error) {
	return contracts.MediaStream{}, errors.New("not used")
}

func (s *torrentProviderStub) IsTorrent() bool { return true }
