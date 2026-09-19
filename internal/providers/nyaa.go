package providers

// Nyaa (nyaa.si) is the first torrent SEARCH provider (PR36): a
// public RSS feed over English-translated anime, no authentication,
// nothing to configure. Search results carry torrent links resolved
// by the embedded core engine (TorrentBase) — never the HTTP-embed
// pipeline. Not a Python port: written against the live site, the
// endpoint and field set were verified via curl on 2026-09-17
// (controller recon, normative).
//
// PR66 .torrent-bytes ingestion (the tokyotosho PR53 pattern): the
// result link is the RSS <link> — the direct
// https://nyaa.si/download/{id}.torrent — not the magnet synthesized
// from nyaa:infoHash. The synthesized magnet is tracker-less, so its
// metadata arrives via DHT only and times out under any real budget
// (the PR52 smoke FAIL `resolved 2/35` class). Search therefore
// preflights every result's .torrent bytes (bounded ≤8, ~10s/URL) and
// drops the dead ones BEFORE they surface; survivors' bytes are handed
// to the engine (IngestMetaInfo), so the resolve leg never re-fetches
// and metadata needs no swarm round-trip at all. nyaa.si route
// flakiness (RST direct / 504 flaps via proxy) is absorbed by the same
// preflight: unreachable-host results are dropped that instant. The
// magnet from nyaa:infoHash is only the no-<link> fallback (kept
// unprobed — the engine owns magnets), and the PR44 seeders>0 filter
// stands.

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
	"github.com/an0nx/anicli-go/internal/torrent"
)

// NyaaBase is the nyaa.si site root: the RSS search endpoint is the
// root with query parameters.
const NyaaBase = "https://nyaa.si"

// SearchMeta keys are the cross-package contract between torrent
// search providers and the TUI result lists: the provider stashes
// them in SearchResult.Meta, the TUI renders the torrent suffix.
const (
	// SearchMetaSize is the human release size ("1.2 GiB").
	SearchMetaSize = "size"
	// SearchMetaSeeders is the seeder count as reported ("421").
	SearchMetaSeeders = "seeders"
	// SearchMetaLeechers is the leecher count as reported ("33").
	SearchMetaLeechers = "leechers"
	// SearchMetaQuality is the PR35 quality badge ("1080p").
	SearchMetaQuality = "quality"
)

// nyaa search parameters (controller-verified defaults): the RSS
// endpoint over the English-translated anime category, no filter,
// seeded-first ordering.
const (
	nyaaPageRSS        = "rss"
	nyaaCategoryAnime  = "1_2" // Anime - English Translated
	nyaaFilterNone     = "0"
	nyaaSortSeeders    = "seeders"
	nyaaSortOrder      = "desc"
	nyaaInfoHashHexLen = 40
)

// Nyaa is the nyaa.si torrent provider over the shared Base identity
// and TorrentBase engine plumbing.
type Nyaa struct {
	Base
	*TorrentBase

	// preflightTimeout overrides the per-URL preflight budget (tests);
	// 0 keeps torrentPreflightTimeout.
	preflightTimeout time.Duration
}

// newNyaa builds the provider. The engine may be nil (fails loud on
// use): the registry injects the shared engine when [torrent] is
// enabled.
func newNyaa(baseURL string, http *netclient.Client, engine *torrent.Engine) *Nyaa {
	return &Nyaa{
		Base: Base{
			id:          "nyaa",
			name:        "Nyaa",
			baseURL:     baseURL,
			sourceType:  contracts.SourceTypeBoth,
			contentLang: "ja",
			http:        http,
		},
		TorrentBase: NewTorrentBase(engine),
	}
}

// IsTorrent implements contracts.TorrentProvider.
func (p *Nyaa) IsTorrent() bool { return true }

// NamePreference implements contracts.NamePreferenceProvider (PR42):
// nyaa's index matches romaji/english release names only — the search
// fan-out must route it the latin variants, never the Cyrillic ones.
func (p *Nyaa) NamePreference() contracts.NamePreference { return contracts.NamePrefLatin }

// nyaaRSS is the RSS 2.0 envelope of the search feed.
type nyaaRSS struct {
	Channel struct {
		Items []nyaaItem `xml:"item"`
	} `xml:"channel"`
}

// nyaaItem mirrors one RSS <item>: the release name in <title>, the
// direct .torrent download URL in <link> and the nyaa: extension
// fields (matched by namespace URL, so a prefix rename upstream cannot
// break parsing).
type nyaaItem struct {
	Title    string `xml:"title"`
	Link     string `xml:"link"`
	PubDate  string `xml:"pubDate"`
	Seeders  string `xml:"https://nyaa.si/xmlns/nyaa seeders"`
	Leechers string `xml:"https://nyaa.si/xmlns/nyaa leechers"`
	InfoHash string `xml:"https://nyaa.si/xmlns/nyaa infoHash"`
	Size     string `xml:"https://nyaa.si/xmlns/nyaa size"`
}

// Search queries the public RSS feed. The torrent link of a result is
// the RSS <link> .torrent download URL (the PR66 ingestion: the
// preflight carries its bytes to the engine); the infoHash-built
// magnet is the fallback when the feed omits the <link>.
func (p *Nyaa) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	// Empty queries are a caller bug: reject before any network I/O.
	if strings.TrimSpace(query) == "" {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, 0,
			fmt.Errorf("%w: пустой поисковый запрос", contracts.ErrInvalidInput))
	}

	searchURL := p.baseURL + "/?" + url.Values{
		"page": {nyaaPageRSS},
		"q":    {query},
		"c":    {nyaaCategoryAnime},
		"f":    {nyaaFilterNone},
		"s":    {nyaaSortSeeders},
		"o":    {nyaaSortOrder},
	}.Encode()

	resp, err := p.http.Do(ctx, netclient.Request{
		Method: "GET",
		URL:    searchURL,
		Op:     contracts.OpSearch,
	})
	if err != nil {
		return nil, err
	}

	var feed nyaaRSS
	if err := xml.Unmarshal(resp.Body, &feed); err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, resp.StatusCode,
			fmt.Errorf("decode rss: %w", err))
	}

	results := make([]contracts.SearchResult, 0, len(feed.Channel.Items))
	for _, item := range feed.Channel.Items {
		title := strings.TrimSpace(item.Title)
		if title == "" {
			continue
		}
		link := nyaaResultLink(title, item.InfoHash, item.Link)
		if link == "" {
			// No <link> and no usable infohash: the item has
			// nothing the engine could ingest — drop it like an
			// empty title instead of handing downstream a dead
			// result.
			continue
		}
		results = append(results, contracts.SearchResult{
			Title:    title,
			URL:      link,
			SourceID: p.ID(),
			Meta: map[string]any{
				SearchMetaSize:     strings.TrimSpace(item.Size),
				SearchMetaSeeders:  strings.TrimSpace(item.Seeders),
				SearchMetaLeechers: strings.TrimSpace(item.Leechers),
				// The PR35 release parser rides the search
				// result too: the badge is known before ingest.
				SearchMetaQuality: torrent.ParseQuality(title).Badge(),
			},
		})
	}
	// Seedless items are dead results: drop them before they surface
	// (fail-soft — items without a seed field survive), so the
	// preflight never spends a fetch on them. The PR66 preflight then
	// probes the survivors' .torrent bytes: dead hosts never surface,
	// and the surviving bytes feed the engine (no double fetch).
	return p.preflight(ctx, filterSeedless(results)), nil
}

// preflightBudget is the effective per-URL fetch budget.
func (p *Nyaa) preflightBudget() time.Duration {
	if p.preflightTimeout > 0 {
		return p.preflightTimeout
	}
	return torrentPreflightTimeout
}

// preflight drops search results whose .torrent bytes are not
// fetchable and parseable (the shared TorrentBase preflight; the
// tokyotosho PR53 mechanism).
func (p *Nyaa) preflight(ctx context.Context, results []contracts.SearchResult) []contracts.SearchResult {
	return p.preflightResults(ctx, p.http, p.loggerOrDiscard(), p.ID(), p.preflightBudget(), results)
}

// nyaaResultLink picks the torrent link of one RSS item: the <link>
// .torrent download URL verbatim (PR66 — the bytes-ingestible,
// preflightable link), a magnet from a well-formed 40-hex infoHash as
// the no-<link> fallback, "" when neither is usable (the caller drops
// such items).
func nyaaResultLink(title, infoHash, torrentURL string) string {
	if u := strings.TrimSpace(torrentURL); u != "" {
		return u
	}
	hash := strings.ToLower(strings.TrimSpace(infoHash))
	if len(hash) == nyaaInfoHashHexLen && isHex(hash) {
		return "magnet:?xt=urn:btih:" + hash + "&dn=" + url.QueryEscape(title)
	}
	return ""
}

// isHex reports whether s is non-empty lowercase hexadecimal.
func isHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return s != ""
}

// GetEpisodes ingests the result's torrent link and waits — bounded
// by the caller's context — for the engine's metadata fetch, then
// maps the release's files onto standard episodes (single-episode
// releases yield one playable entry, batches one per file with the
// parsed episode numbers).
func (p *Nyaa) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	return p.EpisodesWait(ctx, animeURL)
}

// ResolveStream resolves playback through the torrent core: the link
// rides the episode's RawEmbeds, the stream points at the loopback
// server.
func (p *Nyaa) ResolveStream(_ context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	return p.TorrentBase.ResolveStream(episode, dubID)
}
