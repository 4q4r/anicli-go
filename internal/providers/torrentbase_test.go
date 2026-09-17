package providers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
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

// seedReleaseFile writes one payload file with a release-style name and
// builds the metainfo for it (the PR35 E2E seed builder, providers-side:
// Info.BuildFromFilePath + bencode, no external programs).
func seedReleaseFile(t *testing.T, dir, name string, size int) ([]byte, *metainfo.MetaInfo, metainfo.Hash) {
	t.Helper()

	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 251)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	var info metainfo.Info
	info.PieceLength = 32 * 1024
	info.Name = name
	if err := info.BuildFromFilePath(path); err != nil {
		t.Fatalf("build metainfo info: %v", err)
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatalf("marshal info: %v", err)
	}
	mi := &metainfo.MetaInfo{InfoBytes: infoBytes}
	return data, mi, mi.HashInfoBytes()
}

// TestTorrentBaseEpisodesCarryIngestLink pins the provider-flow
// contract: the torrent link rides the episode in RawEmbeds under the
// «Торрент» dub, so ResolveStream can recover it (the session flow
// passes episodes, not links).
func TestTorrentBaseEpisodesCarryIngestLink(t *testing.T) {
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
		},
	}
	base.mu.Unlock()

	eps, err := base.Episodes(linkA)
	if err != nil {
		t.Fatalf("Episodes: %v", err)
	}
	link, ok := eps[0].RawEmbeds[torrentDubLabel]
	if !ok || len(link) != 1 || link[0] != linkA {
		t.Errorf("RawEmbeds[%q] = %v, want [%s] (the ingest link)", torrentDubLabel, eps[0].RawEmbeds[torrentDubLabel], linkA)
	}
}

// TestTorrentBaseResolveStreamReadsLinkFromEmbeds proves the base
// recovers the torrent link from the episode's RawEmbeds and reaches
// the engine resolve (which fails loud here — the magnet has no
// seeders, so metadata never arrives; a "link not added" error would
// mean the extraction is broken).
func TestTorrentBaseResolveStreamReadsLinkFromEmbeds(t *testing.T) {
	t.Parallel()

	base := NewTorrentBase(torrent.NewEngine(testTorrentConfig(t), nil, nil))
	t.Cleanup(func() { _ = base.Close() })
	base.streamWait = 50 * time.Millisecond

	// The real flow ingests before resolving (GetEpisodes →
	// ResolveStream); the engine registers the release, metadata never
	// arrives — no seeder.
	if _, err := base.Ingest(context.Background(), linkB); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	episode := contracts.Episode{
		Num:       "1",
		RawID:     "0",
		RawEmbeds: map[string][]string{torrentDubLabel: {linkB}},
	}
	_, err := base.ResolveStream(episode, torrentDubLabel)
	if err == nil {
		t.Fatal("resolve without metadata must fail loud")
	}
	if strings.Contains(err.Error(), "ссылка") && strings.Contains(err.Error(), "нет") {
		t.Fatalf("error %v suggests the link extraction failed; want the engine resolve path", err)
	}
	if !strings.Contains(err.Error(), "resolve") {
		t.Errorf("error = %v, want the bounded resolve failure", err)
	}
}

// TestTorrentBaseResolveStreamWithoutEmbedsFailsLoud: an episode
// without the «Торрент» embed must never reach the engine — typed
// loud failure instead of a guess.
func TestTorrentBaseResolveStreamWithoutEmbedsFailsLoud(t *testing.T) {
	t.Parallel()

	base := NewTorrentBase(torrent.NewEngine(testTorrentConfig(t), nil, nil))
	t.Cleanup(func() { _ = base.Close() })

	episode := contracts.Episode{Num: "1", RawID: "0"}
	if _, err := base.ResolveStream(episode, torrentDubLabel); err == nil {
		t.Fatal("ResolveStream without the torrent embed must fail loud")
	}
}

// TestTorrentBaseEpisodesWaitTimesOutFailsLoud: EpisodesWait bounds on
// the caller context — an unreachable magnet produces a loud error,
// never a silent empty list.
func TestTorrentBaseEpisodesWaitTimesOutFailsLoud(t *testing.T) {
	t.Parallel()

	base := NewTorrentBase(torrent.NewEngine(testTorrentConfig(t), nil, nil))
	t.Cleanup(func() { _ = base.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	eps, err := base.EpisodesWait(ctx, linkB)
	if err == nil {
		t.Fatal("EpisodesWait without metadata must fail loud on ctx deadline")
	}
	if len(eps) != 0 {
		t.Errorf("episodes = %v, want none", eps)
	}
}

// TestTorrentBaseEpisodesWaitAfterMetadataArrives is the
// providers-side load-bearing proof (PR35 E2E trick): a seed client in
// this process serves the metadata, the base ingests the magnet with
// x.pe, EpisodesWait returns once the release turns ready — proving
// the stale-snapshot refresh actually observes the engine state.
func TestTorrentBaseEpisodesWaitAfterMetadataArrives(t *testing.T) {
	if testing.Short() {
		t.Skip("engine E2E in short mode")
	}
	t.Parallel()

	// The payload MUST live inside the seeder engine's storage dir:
	// anacrolix v1.61 serves metadata only while the torrent's storage
	// is usable (proven by the scratch probe — payload outside the dir
	// blocks even the metadata exchange).
	dirSeed := t.TempDir()
	_, mi, ih := seedReleaseFile(t, dirSeed, "Test Show - 01.mkv", 256*1024)

	engSeed := torrent.NewEngine(config.Torrent{
		Enabled: true, Dir: dirSeed, Port: 0, ReadaheadMB: 1,
	}, nil, nil)
	t.Cleanup(func() { _ = engSeed.Close() })
	if _, err := engSeed.AddMetaInfo(mi); err != nil {
		t.Fatalf("seeder AddMetaInfo: %v", err)
	}
	port, ok := engSeed.ListenPort()
	if !ok {
		t.Fatal("seeder client has no listen port")
	}

	base := NewTorrentBase(torrent.NewEngine(testTorrentConfig(t), nil, nil))
	t.Cleanup(func() { _ = base.Close() })

	magnet := fmt.Sprintf("magnet:?xt=urn:btih:%s&dn=Test%%20Show&x.pe=127.0.0.1:%d", ih.HexString(), port)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	eps, err := base.EpisodesWait(ctx, magnet)
	if err != nil {
		t.Fatalf("EpisodesWait: %v", err)
	}
	if len(eps) != 1 {
		t.Fatalf("episodes = %d, want the single seeded file", len(eps))
	}
	if eps[0].Num != "1" {
		t.Errorf("episode Num = %q, want %q (release-name parsing)", eps[0].Num, "1")
	}
	if got := eps[0].RawEmbeds[torrentDubLabel]; len(got) != 1 || got[0] != magnet {
		t.Errorf("RawEmbeds[%q] = %v, want the ingested magnet", torrentDubLabel, got)
	}
}

// TestTorrentBaseRefreshesReleaseFromEngine pins the stale-snapshot
// fix: releaseForLink consults the live engine first, so the base
// observes metadata that arrived after Ingest cached the fetching
// snapshot.
func TestTorrentBaseRefreshesReleaseFromEngine(t *testing.T) {
	t.Parallel()

	base := NewTorrentBase(torrent.NewEngine(testTorrentConfig(t), nil, nil))
	t.Cleanup(func() { _ = base.Close() })

	ih, err := base.Ingest(context.Background(), linkB)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	// Poison the cache with a stale snapshot (what Ingest saw); the
	// engine's live snapshot carries the dn= name instead.
	base.mu.Lock()
	base.releases[ih] = torrent.Release{InfoHash: ih, DisplayName: "stale", Status: torrent.StatusFetching}
	base.mu.Unlock()

	rel, err := base.releaseForLink(linkB)
	if err != nil {
		t.Fatalf("releaseForLink: %v", err)
	}
	if rel.DisplayName == "stale" {
		t.Fatal("releaseForLink returned the cached snapshot; want the engine's live one")
	}
}

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

// TestRegistryWiresSharedTorrentEngine pins the PR36 wiring: with
// [torrent] enabled the registry builds ONE lazy engine, injects it
// into every torrent provider and exposes it (the TUI reuses the same
// engine instead of booting a second client). With [torrent] disabled
// nyaa never registers (disabled-table) and no engine exists.
func TestRegistryWiresSharedTorrentEngine(t *testing.T) {
	t.Parallel()

	t.Run("enabled", func(t *testing.T) {
		t.Parallel()
		cfg := config.Default()
		cfg.Network.ProxyURL = ""
		cfg.Providers.Kodik.Token = "test-token"
		cfg.Providers.Yanima.DDoSP1 = "test-p1"
		cfg.Providers.Yanima.DDoSP2 = "test-p2"

		reg, err := NewRegistry(cfg, nil)
		if err != nil {
			t.Fatalf("NewRegistry: %v", err)
		}
		t.Cleanup(func() { _ = reg.Close() })

		eng := reg.TorrentEngine()
		if eng == nil {
			t.Fatal("TorrentEngine() = nil with [torrent] enabled")
		}
		p, ok := reg.Get("nyaa")
		if !ok {
			t.Fatal("nyaa not registered")
		}
		ny, ok := bareProvider(p).(*Nyaa)
		if !ok {
			t.Fatalf("nyaa entry is %T, want *Nyaa", bareProvider(p))
		}
		ny.mu.Lock()
		wired := ny.engine
		ny.mu.Unlock()
		if wired == nil {
			t.Fatal("the shared engine was not injected into the nyaa provider")
		}
	})

	t.Run("disabled", func(t *testing.T) {
		t.Parallel()
		cfg := config.Default()
		cfg.Network.ProxyURL = ""
		cfg.Providers.Kodik.Token = "test-token"
		cfg.Providers.Yanima.DDoSP1 = "test-p1"
		cfg.Providers.Yanima.DDoSP2 = "test-p2"
		cfg.Torrent.Enabled = false

		reg, err := NewRegistry(cfg, nil)
		if err != nil {
			t.Fatalf("NewRegistry: %v", err)
		}
		t.Cleanup(func() { _ = reg.Close() })

		if eng := reg.TorrentEngine(); eng != nil {
			t.Error("TorrentEngine() must be nil with [torrent] disabled")
		}
		if _, ok := reg.Get("nyaa"); ok {
			t.Error("nyaa must not register when the torrent subsystem is off")
		}
	})
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
