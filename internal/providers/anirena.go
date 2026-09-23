package providers

// AniRena (anirena.com) — torrent search over the site's RSS feed, the
// fifth TorrentBase provider (nyaa PR36, anilibria-torrent PR37,
// animetosho/tokyotosho PR38 lineage; PR88). Anonymous public tracker,
// JA/multilingual releases, no credentials, nothing to configure (the
// documented JSON API gates uploads AND torrent search behind personal
// API keys — POST /api/v1/torrents/search without a bearer answers 401,
// live-verified 2026-09-23 — so the anonymous RSS is the provider
// route, exactly like nyaa).
//
// Live-verified 2026-09-23 (curl, the fixtures in testdata/ are real
// captures):
//   - GET /rss?q=<query> — RSS 2.0 feed of the site search (their guide
//     documents only /rss and ?category=; the q= parameter answers the
//     web search's query and is the anonymous search surface).
//   - The documented ?category= filter is IGNORED by the server (the
//     feed spans ALL categories even with category=anime), so the
//     Anime scope is enforced client-side on the item description's
//     "Category: …" field (Anime / Anime > Subcat kept).
//   - There is no usable server-side limit parameter (per_page/limit
//     ignored; a feed answers up to 250 items), so Search caps the
//     parsed items client-side (AniRenaSearchLimit) BEFORE the
//     preflight fan-out — one search can never spend unbounded fetches.
//   - Item titles carry a leading "[Category > Subcategory] " prefix —
//     stripped (anirenaTitle); release-group tags like [SubsPlease]
//     are not categories and survive verbatim.
//   - Each item's <enclosure> is the DIRECT .torrent URL on the site
//     itself: https://www.anirena.com/torrents/{uuid}.torrent (bencode
//     verified, announce-list carries the anirena tracker pool). The
//     /torrents/{uuid}/magnet sibling 302s to a tracker-rich magnet —
//     unused: the RSS never omits the enclosure, and items without one
//     are dropped (nothing the engine could ingest).
//   - The feed carries NO seed fields: unlike nyaa/animetosho there is
//     nothing for the PR44 seedless filter to key on, so every anime
//     item surfaces and the search-time preflight (the PR66 bytes
//     ingestion: bounded fetch, metainfo parse, IngestMetaInfo — the
//     resolve leg never re-fetches) is the only dead-result gate.
//
// Not a Python port: written against the live site (the anidub
// precedent for sources with no frozen original).

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

// AniRenaBase is the anirena.com site root: the RSS search endpoint is
// the root with query parameters.
const AniRenaBase = "https://www.anirena.com"

// AniRenaSearchLimit bounds one search page (the animetosho rule): the
// feed answers up to 250 items and has no server-side limit parameter,
// so the cap keeps the response, the preflight fan-out and the TUI
// list predictable.
const AniRenaSearchLimit = 30

// anirenaCategories are the site's torrent categories (their guide,
// live titles matched): a leading "[<category>…]" title group naming
// one of these is the feed's category prefix and is stripped from the
// release title.
var anirenaCategories = []string{
	"Anime", "Manga/Manhwa/Comic", "Audio", "Literature",
	"Live Action", "Pictures", "Software", "Hentai", "Other",
}

// AniRena is the anirena.com torrent provider over the shared Base
// identity and TorrentBase engine plumbing.
type AniRena struct {
	Base
	*TorrentBase

	// preflightTimeout overrides the per-URL preflight budget (tests);
	// 0 keeps torrentPreflightTimeout.
	preflightTimeout time.Duration
}

// newAniRena builds the provider. The engine may be nil (fails loud on
// use): the registry injects the shared engine when [torrent] is
// enabled.
func newAniRena(baseURL string, http *netclient.Client, engine *torrent.Engine) *AniRena {
	return &AniRena{
		Base: Base{
			id:          "anirena",
			name:        "AniRena",
			baseURL:     baseURL,
			sourceType:  contracts.SourceTypeBoth,
			contentLang: "ja",
			http:        http,
		},
		TorrentBase: NewTorrentBase(engine),
	}
}

// IsTorrent implements contracts.TorrentProvider.
func (p *AniRena) IsTorrent() bool { return true }

// NamePreference implements contracts.NamePreferenceProvider (PR42):
// anirena's index matches romaji/english release names only — the
// search fan-out must route it the latin variants, never the Cyrillic
// ones.
func (p *AniRena) NamePreference() contracts.NamePreference { return contracts.NamePrefLatin }

// anirenaRSS is the RSS 2.0 envelope of the search feed.
type anirenaRSS struct {
	Channel struct {
		Items []anirenaItem `xml:"item"`
	} `xml:"channel"`
}

// anirenaItem mirrors one RSS <item>: the release title (with a
// leading category prefix), the description CDATA ("Size: … | Uploader:
// … | Category: …") and the direct .torrent download URL in
// <enclosure>. Entity decoding applies to <title> (its "&gt;" becomes
// ">"), NOT inside the CDATA (the Category field keeps the literal
// "&gt;" — the Anime check prefixes and never sees the separator).
type anirenaItem struct {
	Title       string `xml:"title"`
	Description string `xml:"description"`
	Enclosure   struct {
		URL string `xml:"url,attr"`
	} `xml:"enclosure"`
}

// anirenaDescSize extracts the "Size: …" field from the description
// CDATA (up to the next " | " separator); "" when absent.
func anirenaDescSize(desc string) string {
	const key = "Size: "
	i := strings.Index(desc, key)
	if i < 0 {
		return ""
	}
	rest := desc[i+len(key):]
	if j := strings.Index(rest, " | "); j >= 0 {
		return strings.TrimSpace(rest[:j])
	}
	return strings.TrimSpace(rest)
}

// anirenaDescCategory extracts the "Category: …" field from the
// description CDATA (up to the next " | " separator); "" when absent.
func anirenaDescCategory(desc string) string {
	const key = "Category: "
	i := strings.Index(desc, key)
	if i < 0 {
		return ""
	}
	rest := desc[i+len(key):]
	if j := strings.Index(rest, " | "); j >= 0 {
		return strings.TrimSpace(rest[:j])
	}
	return strings.TrimSpace(rest)
}

// anirenaTitle strips a leading "[<category>[ > subcategory]] " group
// from an RSS item title: the feed prefixes EVERY title with the
// torrent's category (e.g. "[Anime > RAW] Show") and a release title
// must not carry it. A bracket group is treated as the category prefix
// only when it names one of the site categories — release-group tags
// like "[SubsPlease]" are part of the title and stay verbatim.
func anirenaTitle(raw string) string {
	title := strings.TrimSpace(raw)
	if !strings.HasPrefix(title, "[") {
		return title
	}
	end := strings.IndexByte(title, ']')
	if end < 0 {
		return title
	}
	inside := title[1:end]
	for _, cat := range anirenaCategories {
		if inside == cat || strings.HasPrefix(inside, cat+" ") {
			return strings.TrimSpace(title[end+1:])
		}
	}
	return title
}

// anirenaIsAnimeCategory reports whether the description's Category
// field scopes the item into the Anime category (exact "Anime" or an
// "Anime > …" subcategory). The live feed spans ALL site categories —
// the documented ?category= filter is ignored server-side — so this
// client-side scope check is what keeps manga PDFs and soundtracks out
// of an anime client's search. An item without a parseable category is
// out of scope too: catalog scope is a hard contract, not a dead-result
// filter, so the fail-soft "no field, no filter" convention does NOT
// apply here.
func anirenaIsAnimeCategory(desc string) bool {
	cat := anirenaDescCategory(desc)
	return cat == "Anime" || strings.HasPrefix(cat, "Anime ") || strings.HasPrefix(cat, "Anime>")
}

// Search queries the public RSS feed with the raw query (the same
// operators the web search takes). The torrent link of a result is the
// <enclosure> .torrent URL on the site itself (the PR66 ingestion: the
// preflight carries its bytes to the engine); items without an
// enclosure — or outside the Anime category — are dropped instead of
// handed downstream as dead results (the nyaa rule). The feed carries
// no seed fields, so the PR44 seedless filter has nothing to drop here:
// the search-time preflight is the only dead-result gate.
func (p *AniRena) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	// Empty queries are a caller bug: reject before any network I/O.
	if strings.TrimSpace(query) == "" {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, 0,
			fmt.Errorf("%w: пустой поисковый запрос", contracts.ErrInvalidInput))
	}

	searchURL := p.baseURL + "/rss?" + url.Values{
		"q": {query},
	}.Encode()

	resp, err := p.http.Do(ctx, netclient.Request{
		Method: "GET",
		URL:    searchURL,
		Op:     contracts.OpSearch,
	})
	if err != nil {
		return nil, err
	}

	var feed anirenaRSS
	if err := xml.Unmarshal(resp.Body, &feed); err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, resp.StatusCode,
			fmt.Errorf("decode rss: %w", err))
	}

	// The bounded surface: cap BEFORE building results so neither the
	// result slice nor the preflight fan-out can grow with the feed.
	items := feed.Channel.Items
	if len(items) > AniRenaSearchLimit {
		items = items[:AniRenaSearchLimit]
	}

	results := make([]contracts.SearchResult, 0, len(items))
	for _, item := range items {
		title := anirenaTitle(item.Title)
		if title == "" {
			continue
		}
		if !anirenaIsAnimeCategory(item.Description) {
			continue
		}
		link := strings.TrimSpace(item.Enclosure.URL)
		if link == "" {
			// No <enclosure>: the item has nothing the engine
			// could ingest (the live feed always carries it).
			continue
		}
		results = append(results, contracts.SearchResult{
			Title:    title,
			URL:      link,
			SourceID: p.ID(),
			Meta: map[string]any{
				// The human release size rides the description
				// ("7.7 GB"); the feed has no seed fields (live-
				// verified) — the seed meta keys stay absent so
				// filterSeedless keeps the item (no field, no
				// filter).
				SearchMetaSize: anirenaDescSize(item.Description),
				// The PR35 release parser rides the search
				// result too: the badge is known before ingest.
				SearchMetaQuality: torrent.ParseQuality(title).Badge(),
			},
		})
	}
	// The PR66 preflight probes the survivors' .torrent bytes: dead
	// hosts never surface, and the surviving bytes feed the engine (no
	// double fetch).
	return p.preflight(ctx, results), nil
}

// preflightBudget is the effective per-URL fetch budget.
func (p *AniRena) preflightBudget() time.Duration {
	if p.preflightTimeout > 0 {
		return p.preflightTimeout
	}
	return torrentPreflightTimeout
}

// preflight drops search results whose .torrent bytes are not
// fetchable and parseable (the shared TorrentBase preflight; the
// tokyotosho PR53 mechanism).
func (p *AniRena) preflight(ctx context.Context, results []contracts.SearchResult) []contracts.SearchResult {
	return p.preflightResults(ctx, p.http, p.loggerOrDiscard(), p.ID(), p.preflightBudget(), results)
}

// GetEpisodes ingests the result's torrent link and waits — bounded
// by the caller's context — for the engine's metadata fetch, then
// maps the release's files onto standard episodes (single-episode
// releases yield one playable entry, batches one per file with the
// parsed episode numbers).
func (p *AniRena) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	return p.EpisodesWait(ctx, animeURL)
}

// ResolveStream resolves playback through the torrent core: the link
// rides the episode's RawEmbeds, the stream points at the loopback
// server.
func (p *AniRena) ResolveStream(_ context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	return p.TorrentBase.ResolveStream(episode, dubID)
}
