package providers

// Nyaa (nyaa.si) is the first torrent SEARCH provider (PR36): a
// public RSS feed over English-translated anime, no authentication,
// nothing to configure. Search results carry torrent links resolved
// by the embedded core engine (TorrentBase) — never the HTTP-embed
// pipeline. Not a Python port: written against the live site, the
// endpoint and field set were verified via curl on 2026-09-17
// (controller recon, normative).

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/url"
	"strings"

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

// nyaaRSS is the RSS 2.0 envelope of the search feed.
type nyaaRSS struct {
	Channel struct {
		Items []nyaaItem `xml:"item"`
	} `xml:"channel"`
}

// nyaaItem mirrors one RSS <item>: the release name in <title>, the
// .torrent URL in <link> and the nyaa: extension fields (matched by
// namespace URL, so a prefix rename upstream cannot break parsing).
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
// a magnet built from the RSS infoHash (the engine ingests the
// infohash directly — no .torrent download for ingestion) with the
// RSS <link> URL as the fallback when the feed omits the hash.
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
		results = append(results, contracts.SearchResult{
			Title:    title,
			URL:      nyaaResultLink(title, item.InfoHash, item.Link),
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
	return results, nil
}

// nyaaResultLink picks the torrent link of one RSS item: a magnet
// from a well-formed 40-hex infoHash, the .torrent URL otherwise.
func nyaaResultLink(title, infoHash, torrentURL string) string {
	hash := strings.ToLower(strings.TrimSpace(infoHash))
	if len(hash) == nyaaInfoHashHexLen && isHex(hash) {
		return "magnet:?xt=urn:btih:" + hash + "&dn=" + url.QueryEscape(title)
	}
	return torrentURL
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
	return p.TorrentBase.EpisodesWait(ctx, animeURL)
}

// ResolveStream resolves playback through the torrent core: the link
// rides the episode's RawEmbeds, the stream points at the loopback
// server.
func (p *Nyaa) ResolveStream(_ context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	return p.TorrentBase.ResolveStream(episode, dubID)
}
