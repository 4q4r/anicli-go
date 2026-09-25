package providers

// AnimeTosho (PR38) — torrent search over the AnimeTosho newznab API,
// the third TorrentBase provider (after the PR36 origin and
// anilibria-torrent).
// Anonymous feed, no credentials, nothing to configure. Each RSS item
// carries the release title plus newznab/torznab attribute twins
// (infohash, size, seeders). PR66 .torrent-bytes ingestion (the
// tokyotosho PR53 pattern): the result link is the item's <enclosure>
// — a direct .torrent URL on AT's OWN storage (storage.animetosho.org,
// type application/x-bittorrent; live-verified) — and Search
// preflights every result's bytes (bounded ≤8, ~10s/URL), dropping the
// dead ones BEFORE they surface and handing the survivors' bytes to
// the engine (IngestMetaInfo). The reason: the live feed's magneturl
// is base32 (nekoBT mirror hashes, engine contract is 40-hex — not
// taken) and the synthesized magnet from the hex infohash attr is
// tracker-less, so its metadata arrived via DHT only and timed out
// under any real budget (the PR52 smoke FAIL `resolved 6/23` class);
// bytes ingestion needs no swarm round-trip at all. The engine's
// tracker pool attaches to the metainfo ingest as to every other
// (one mechanism, PR45). Magnets serve only as the no-enclosure
// fallback: a hex magneturl rides verbatim (its tr= announces aid peer
// discovery), otherwise a magnet is built from the infohash attr —
// kept unprobed, the engine owns magnets. Not a Python port: written
// against the live API, endpoints and field set captured by curl on
// 2026-09-17 (controller dossier, normative).

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
	"github.com/an0nx/anicli-go/internal/torrent"
)

// AnimeToshoFeedBase is the newznab API host. Domain-migration note
// (live-verified 2026-09-17): the project is moving to animetosho.xyz
// (announced 24/07/2026, already serving 200) and mirror.animetosho.org
// now 302s onto animetosho.org — but feed.animetosho.org still answers
// the API directly. Pin the feed host here; if search starts failing,
// check the .xyz announcement before touching the parser.
const AnimeToshoFeedBase = "https://feed.animetosho.org"

// AnimeToshoSearchLimit bounds one search page (newznab default would
// be 100): a bounded request keeps the response (and the TUI list)
// predictable.
const AnimeToshoSearchLimit = 30

// AnimeTosho search parameters (live-verified defaults): the newznab
// search over the anime category, first page.
const (
	atNewznabSearch = "search"
	atCategoryAnime = "5070"
)

// AnimeTosho is the animetosho.org torrent provider over the shared
// Base identity and TorrentBase engine plumbing.
type AnimeTosho struct {
	Base
	*TorrentBase

	// preflightTimeout overrides the per-URL preflight budget (tests);
	// 0 keeps torrentPreflightTimeout.
	preflightTimeout time.Duration
}

// newAnimeTosho builds the provider. The engine may be nil (fails loud
// on use): the registry injects the shared engine when [torrent] is
// enabled.
func newAnimeTosho(feedBase string, http *netclient.Client, engine *torrent.Engine) *AnimeTosho {
	return &AnimeTosho{
		Base: Base{
			id:          "animetosho",
			name:        "AnimeTosho",
			baseURL:     feedBase,
			sourceType:  contracts.SourceTypeBoth,
			contentLang: "ja",
			http:        http,
		},
		TorrentBase: NewTorrentBase(engine),
	}
}

// IsTorrent implements contracts.TorrentProvider.
func (p *AnimeTosho) IsTorrent() bool { return true }

// NamePreference implements contracts.NamePreferenceProvider (PR42):
// the animetosho feed indexes romaji/english release names only — the
// search fan-out must route it the latin variants, never the Cyrillic
// ones.
func (p *AnimeTosho) NamePreference() contracts.NamePreference { return contracts.NamePrefLatin }

// atAttr is one newznab/torznab attribute. The parse collects attrs
// from ANY namespace (the struct tag has no namespace): the feed emits
// every attribute twice — once per namespace — so a prefix or single-
// namespace rename upstream cannot break it. First occurrence wins.
type atAttr struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

// animeToshoRSS is the RSS 2.0 envelope of the newznab search feed.
type animeToshoRSS struct {
	Channel struct {
		Items []atItem `xml:"item"`
	} `xml:"channel"`
}

// atItem mirrors one newznab RSS <item>: the release name in <title>,
// the direct .torrent storage URL in <enclosure>, and the attribute
// set (infohash, size, seeders, …) under newznab:/torznab:.
type atItem struct {
	Title     string `xml:"title"`
	Link      string `xml:"link"`
	PubDate   string `xml:"pubDate"`
	Enclosure struct {
		URL string `xml:"url,attr"`
	} `xml:"enclosure"`
	Attrs []atAttr `xml:"attr"`
}

// attr returns the first attribute value recorded under name (the
// feed duplicates attrs across the newznab and torznab namespaces).
func (i atItem) attr(name string) string {
	for _, a := range i.Attrs {
		if a.Name == name {
			return a.Value
		}
	}
	return ""
}

// Search queries the newznab search API. The torrent link of a result
// is the <enclosure> .torrent URL on AT's own storage (the PR66
// ingestion: the preflight carries its bytes to the engine); a hex
// magneturl verbatim or an infohash-built magnet is the fallback when
// the item carries no usable enclosure. Items with neither, or without
// a title, are dropped instead of handed downstream as dead results
// (the TorrentBase rule).
func (p *AnimeTosho) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	// Empty queries are a caller bug: reject before any network I/O.
	if strings.TrimSpace(query) == "" {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, 0,
			fmt.Errorf("%w: пустой поисковый запрос", contracts.ErrInvalidInput))
	}

	searchURL := p.baseURL + "/api?" + url.Values{
		"t":      {atNewznabSearch},
		"q":      {query},
		"cat":    {atCategoryAnime},
		"limit":  {fmt.Sprintf("%d", AnimeToshoSearchLimit)},
		"offset": {"0"},
	}.Encode()

	resp, err := p.http.Do(ctx, netclient.Request{
		Method: "GET",
		URL:    searchURL,
		Op:     contracts.OpSearch,
	})
	if err != nil {
		return nil, err
	}

	var feed animeToshoRSS
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
		link := animeToshoResultLink(title, item.attr("magneturl"), item.attr("infohash"), item.Enclosure.URL)
		if link == "" {
			// No enclosure and no usable hash: the item has
			// nothing the engine could ingest.
			continue
		}
		results = append(results, contracts.SearchResult{
			Title:    title,
			URL:      link,
			SourceID: p.ID(),
			Meta: map[string]any{
				// Size rides as a raw byte count attr; the seed
				// counts are absent on some items (fail-soft, the
				// TUI shows what the source gives).
				SearchMetaSize:     humanBytesAttr(item.attr("size")),
				SearchMetaSeeders:  strings.TrimSpace(item.attr("seeders")),
				SearchMetaLeechers: strings.TrimSpace(item.attr("leechers")),
				// The PR35 release parser rides the search
				// result too: the badge is known before ingest.
				SearchMetaQuality: torrent.ParseQuality(title).Badge(),
			},
		})
	}
	// Seedless entries are dead results: drop them before they surface
	// (fail-soft — items without a seed attr survive), so the
	// preflight never spends a fetch on them. The PR66 preflight then
	// probes the survivors' .torrent bytes: dead hosts never surface,
	// and the surviving bytes feed the engine (no double fetch).
	return p.preflight(ctx, filterSeedless(results)), nil
}

// preflightBudget is the effective per-URL fetch budget.
func (p *AnimeTosho) preflightBudget() time.Duration {
	if p.preflightTimeout > 0 {
		return p.preflightTimeout
	}
	return torrentPreflightTimeout
}

// preflight drops search results whose .torrent bytes are not
// fetchable and parseable (the shared TorrentBase preflight; the
// tokyotosho PR53 mechanism).
func (p *AnimeTosho) preflight(ctx context.Context, results []contracts.SearchResult) []contracts.SearchResult {
	return p.preflightResults(ctx, p.http, p.loggerOrDiscard(), p.ID(), p.preflightBudget(), results)
}

// animeToshoResultLink picks the torrent link of one RSS item: the
// <enclosure> .torrent URL on AT's own storage verbatim (PR66 — the
// bytes-ingestible, preflightable link), a hex magneturl verbatim (its
// tr= announces ride), a magnet built from a well-formed 40-hex
// infohash attr, "" when none is usable (the caller drops such items).
func animeToshoResultLink(title, magnetURL, infoHash, enclosureURL string) string {
	if u := strings.TrimSpace(enclosureURL); u != "" {
		return u
	}
	if magnetURI(magnetURL) {
		return magnetURL
	}
	hash := strings.ToLower(strings.TrimSpace(infoHash))
	if len(hash) == infoHashHexLen && isHex(hash) {
		return "magnet:?xt=urn:btih:" + hash + "&dn=" + url.QueryEscape(title)
	}
	return ""
}

// humanBytesAttr renders a byte-count attribute ("23175675801") in the
// TUI torrent-suffix convention; non-numeric input stays verbatim
// (fail-soft: the feed owns the format). The parse is strict — a
// numeric-prefixed value like "123abc" is garbage, not 123.
func humanBytesAttr(raw string) string {
	trimmed := strings.TrimSpace(raw)
	n, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return trimmed
	}
	return humanBytes(n)
}

// GetEpisodes ingests the result's torrent link and waits — bounded
// by the caller's context — for the engine's metadata fetch, then
// maps the release's files onto standard episodes (the TorrentBase
// contract; single-episode releases yield one playable entry, batches
// one per file with the parsed episode numbers).
func (p *AnimeTosho) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	return p.EpisodesWait(ctx, animeURL)
}

// ResolveStream resolves playback through the torrent core: the link
// rides the episode's RawEmbeds, the stream points at the loopback
// server.
func (p *AnimeTosho) ResolveStream(_ context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	return p.TorrentBase.ResolveStream(episode, dubID)
}
