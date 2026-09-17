package providers

// TorrentBase is the shared machinery of torrent-type providers
// (PR35): it funnels torrent links into the embedded core engine and
// adapts the release's files onto the standard episode/stream
// interfaces, so a torrent provider flows through the SAME TUI path
// as any regular connector (search → list → episodes → play) with a
// single difference — the search result carries a torrent LINK, and
// playback rides the core's loopback server. Providers never touch
// the HTTP-embed pipeline: the capability (contracts.TorrentProvider)
// routes them here by construction. New torrent providers (rutracker,
// nyaa, anilibria-torrents, …) embed this base.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/torrent"
)

// torrentDubLabel is the single dub name torrent releases carry (the
// session flow needs one dub slot; a torrent file has no dubs).
const torrentDubLabel = "Торрент"

// filterSeedless drops search results whose feed-reported seeder count
// parses to 0 — a seedless torrent is a dead result, and surfacing it
// only produces dead ends downstream. Fail-soft by design: a result
// whose feed carries no (or an unparseable) seed field is kept — no
// field, no filter. Torrent search providers apply it to their Search
// output (PR44 owner ruling).
func filterSeedless(results []contracts.SearchResult) []contracts.SearchResult {
	out := results[:0:0]
	for _, r := range results {
		raw, ok := r.Meta[SearchMetaSeeders].(string)
		if !ok {
			out = append(out, r)
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || n > 0 {
			out = append(out, r)
		}
	}
	return out
}

// TorrentBase adapts one provider's links onto the torrent core. The
// engine handle may be nil (the provider then fails loud on use —
// kodik-parity: unconfigured providers never pretend to work).
type TorrentBase struct {
	engine *torrent.Engine

	mu       sync.Mutex
	links    map[string]torrent.InfoHash          // provider link → ingested release
	releases map[torrent.InfoHash]torrent.Release // last known release per infohash
	// streamWait overrides the metadata wait inside Stream (tests);
	// 0 keeps the default.
	streamWait time.Duration
}

// NewTorrentBase builds the adapter over the core engine (nil is
// accepted and fails loud on use — the engine is wired by the
// registry when [torrent] is enabled).
func NewTorrentBase(engine *torrent.Engine) *TorrentBase {
	return &TorrentBase{
		engine:   engine,
		links:    map[string]torrent.InfoHash{},
		releases: map[torrent.InfoHash]torrent.Release{},
	}
}

// SetEngine wires the core engine after construction (the registry
// injects it when the [torrent] subsystem is enabled).
func (b *TorrentBase) SetEngine(engine *torrent.Engine) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.engine = engine
}

// Ingest adds the link to the core engine (idempotent, deduped by
// infohash there) and records the link → release mapping. It returns
// immediately with the fetching/ready snapshot: metadata arrives in
// the engine's background.
//
// The link is passed VERBATIM (PR45-review collapse): the engine's
// addSpec attaches the tracker pool to every ingest — magnet, .torrent
// URL or metainfo alike — and health-prunes it, so PR41 tracker_lists
// and [torrent] trackers reach every torrent through ONE mechanism.
// Rewriting magnets here (the old enrichMagnet) double-attached the
// pool and would have needed per-shape tr= sniffing to avoid stripping
// pool trackers from feed magnets that carry their own announces.
func (b *TorrentBase) Ingest(ctx context.Context, link string) (torrent.InfoHash, error) {
	b.mu.Lock()
	eng := b.engine
	if ih, ok := b.links[link]; ok {
		b.mu.Unlock()
		return ih, nil
	}
	b.mu.Unlock()
	if eng == nil {
		return torrent.InfoHash{}, errors.New("torrent provider: engine is not wired ([torrent] disabled?)")
	}
	rel, err := eng.AddLink(ctx, link)
	if err != nil {
		return torrent.InfoHash{}, fmt.Errorf("torrent provider: ingest %s: %w", link, err)
	}
	b.mu.Lock()
	b.links[link] = rel.InfoHash
	b.releases[rel.InfoHash] = rel
	b.mu.Unlock()
	return rel.InfoHash, nil
}

// torrentEpisodePollInterval is the release-status poll cadence inside
// EpisodesWait: metadata arrives in the engine's background, the wait
// just observes it.
const torrentEpisodePollInterval = 500 * time.Millisecond

// EpisodesWait ingests the link (idempotent) and waits — bounded by
// the caller's context — for the engine's background metadata fetch,
// then lists the release's files as standard episodes. The provider
// GetEpisodes path: search results resolve hours after the RSS was
// fetched, so the one-shot Episodes snapshot is not enough here.
func (b *TorrentBase) EpisodesWait(ctx context.Context, link string) ([]contracts.Episode, error) {
	if _, err := b.Ingest(ctx, link); err != nil {
		return nil, err
	}
	ticker := time.NewTicker(torrentEpisodePollInterval)
	defer ticker.Stop()
	for {
		rel, err := b.releaseForLink(link)
		if err != nil {
			return nil, err
		}
		switch rel.Status {
		case torrent.StatusReady:
			return episodesFromRelease(rel, link), nil
		case torrent.StatusError:
			return nil, fmt.Errorf("торренты: метаданные не получены: %s", rel.Err)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("торренты: метаданные не готовы (ожидание прервано): %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// ResolveStream is the provider-facing shape of Stream: the torrent
// link rides the episode's RawEmbeds (the Episodes contract), the dub
// slot is the single «Торрент» label.
func (b *TorrentBase) ResolveStream(episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	embeds, ok := episode.RawEmbeds[torrentDubLabel]
	if !ok || len(embeds) == 0 || embeds[0] == "" {
		return contracts.MediaStream{}, fmt.Errorf("торренты: в эпизоде нет ссылки «%s»", torrentDubLabel)
	}
	return b.Stream(embeds[0], episode, dubID)
}

// Episodes lists the release's files as standard episodes: Num is the
// parsed episode number when available (file index otherwise), RawID
// is the file index the stream resolves, and the single «Торрент» dub
// slot keeps the session flow unchanged. The torrent link itself rides
// RawEmbeds so ResolveStream can recover it.
func (b *TorrentBase) Episodes(link string) ([]contracts.Episode, error) {
	rel, err := b.releaseForLink(link)
	if err != nil {
		return nil, err
	}
	if rel.Status != torrent.StatusReady {
		return nil, fmt.Errorf("торренты: метаданные ещё не готовы (%s)", rel.Status)
	}
	return episodesFromRelease(rel, link), nil
}

// episodesFromRelease maps the release's files onto standard episodes
// (shared by the one-shot Episodes and the waiting EpisodesWait).
func episodesFromRelease(rel torrent.Release, link string) []contracts.Episode {
	out := make([]contracts.Episode, 0, len(rel.Files))
	for _, f := range rel.Files {
		num := strconv.Itoa(f.Index + 1)
		if len(f.Episodes) == 1 {
			num = strconv.Itoa(f.Episodes[0])
		}
		name := f.Path
		if slash := strings.LastIndexByte(name, '/'); slash >= 0 {
			name = name[slash+1:]
		}
		out = append(out, contracts.Episode{
			Num:       num,
			Title:     name,
			RawID:     strconv.Itoa(f.Index),
			RawEmbeds: map[string][]string{torrentDubLabel: {link}},
		})
	}
	return out
}

// torrentStreamWaitTimeout bounds the metadata wait inside Stream:
// the episode listing already ensured ready state, so a stream this
// deep into the flow only ever waits for stragglers — never for a
// full metadata fetch (the UI never blocks on the network).
const torrentStreamWaitTimeout = 15 * time.Second

// Stream resolves one file for playback: it prioritizes the file's
// pieces in the engine and returns the standard MediaStream whose
// single «local» link points at the core's loopback server (the
// player plumbing is the providers' own mpv path).
func (b *TorrentBase) Stream(link string, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	rel, err := b.releaseForLink(link)
	if err != nil {
		return contracts.MediaStream{}, err
	}
	index, err := strconv.Atoi(episode.RawID)
	if err != nil {
		return contracts.MediaStream{}, fmt.Errorf("торренты: file index %q: %w", episode.RawID, err)
	}
	b.mu.Lock()
	eng := b.engine
	wait := b.streamWait
	if wait <= 0 {
		wait = torrentStreamWaitTimeout
	}
	b.mu.Unlock()
	if eng == nil {
		return contracts.MediaStream{}, errors.New("torrent provider: engine is not wired ([torrent] disabled?)")
	}
	// Resolve sets the per-file priorities (the chosen file over
	// everything else); its reader is closed immediately — the player
	// streams through the loopback server, which resolves again.
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	handle, err := eng.Resolve(ctx, rel.InfoHash, index)
	if err != nil {
		return contracts.MediaStream{}, fmt.Errorf("торренты: resolve: %w", err)
	}
	_ = handle.Reader.Close()
	url := eng.StreamURL(rel.InfoHash, index)
	if url == "" {
		return contracts.MediaStream{}, errors.New("торренты: локальный стрим-сервер не запущен")
	}
	return contracts.MediaStream{
		DubName: dubID,
		Links: map[string]contracts.VideoSource{
			"local": {
				URL:     url,
				Quality: rel.Quality.Resolution,
			},
		},
	}, nil
}

// releaseForLink resolves the last known release for a link. The
// flow contract: Ingest runs first (the provider's GetEpisodes calls
// it), so a missing mapping fails loud instead of guessing. The LIVE
// engine snapshot wins over the cached one: metadata arrives in the
// engine's background after Ingest, so the cache holds the fetching
// snapshot forever unless refreshed here.
func (b *TorrentBase) releaseForLink(link string) (torrent.Release, error) {
	b.mu.Lock()
	ih, ok := b.links[link]
	eng := b.engine
	b.mu.Unlock()
	if !ok {
		return torrent.Release{}, fmt.Errorf("торренты: ссылка не добавлена: %s", link)
	}
	if eng != nil {
		if rel, ok := eng.Release(ih); ok {
			return rel, nil
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rel, ok := b.releases[ih]
	if !ok {
		return torrent.Release{}, fmt.Errorf("торренты: релиз не найден: %s", ih.HexString())
	}
	return rel, nil
}

// Close tears down the engine when the base owns one.
func (b *TorrentBase) Close() error {
	if b.engine != nil {
		return b.engine.Close()
	}
	return nil
}
