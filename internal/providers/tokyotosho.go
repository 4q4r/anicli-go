package providers

// TokyoTosho (PR38) — torrent search over the Tokyo Toshokan search
// RSS, the fourth TorrentBase provider after nyaa, anilibria-torrent
// and animetosho. Anonymous feed, no credentials, nothing to
// configure. The search rides rss.php?terms={query}&type=1 — the very
// "RSS Feed of these results" link the site's own search page renders.
// Live-verified traps baked into this provider (curl 2026-09-17,
// controller dossier):
//   - rss.php?search= is a TRAP: the parameter is ignored and the feed
//     answers byte-identically to the plain latest-releases feed —
//     never fake, so it is not used. `terms=` is the real param.
//   - search.php?q= is a trap too (the form posts `terms`); the q=
//     request returns an empty listing table.
//   - The feed's type=1 filter is SOFT: a dandadan query still mixes
//     Raws/Manga/Hentai in, so the provider keeps only exact
//     <category>Anime</category> items.
//   - The description's magnet is base32 (nekoBT mirror hashes) and is
//     deliberately NOT ingested: the engine contract is a 40-hex btih.
//     Ingestion rides the <link> direct .torrent URL instead (often a
//     nyaa.si mirror of the release) — the engine downloads it itself.
//   - The RSS carries no seed counts; the size text ("1.66GB") rides
//     the description HTML blob and is parsed fail-soft. (The HTML
//     search table has S:/L: stats, but no seed data in the feed.)
//     The PR44 seedless filter therefore has nothing to key on here —
//     fail-soft: no field, no filter.

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
	"github.com/an0nx/anicli-go/internal/torrent"
)

// TokyoToshoBase is the site root (www host live-verified; the page
// footer names tokyotosho.info / tokyotosho.se as aliases).
const TokyoToshoBase = "https://www.tokyo-tosho.net"

// ttCategoryAnime is the exact <category> value kept by the search:
// the feed's own type=1 filter is soft, the category element is not.
const ttCategoryAnime = "Anime"

// ttSizeText matches the size line inside the description HTML blob
// ("Size: 1.66GB"); the feed owns the format, so it rides verbatim.
var ttSizeText = regexp.MustCompile(`Size:\s*([0-9.]+\s*[KMGTPE]?B)`)

// ttEmptyFeedFooter reports the live-verified TT zero-result shape
// (curl capture 2026-09-17, testdata/tokyotosho_empty.xml): the
// search RSS backend answers an unmatched query with HTTP 200 and the
// bare feed FOOTER — closing tags only, no <?xml/<rss/<channel
// opening, no items. A trimmed body that is empty or opens with a
// closing tag is definitionally not a feed: "not a feed at all"
// settles as zero results, never as a raw XML-syntax crash. A body
// that opens a real feed and breaks mid-stream fails this check and
// stays a typed decode error.
func ttEmptyFeedFooter(body []byte) bool {
	trimmed := bytes.TrimSpace(body)
	return len(trimmed) == 0 || bytes.HasPrefix(trimmed, []byte("</"))
}

// TokyoTosho is the tokyo-tosho.net torrent provider over the shared
// Base identity and TorrentBase engine plumbing.
//
// PR53 search-time dead-host preflight (owner standing rule: «мёртвь
// отфасовывается ещё до выдачи» — dead hosts are sorted out BEFORE they
// surface): the feed cross-posts third-party .torrent hosts (anidex.moe
// TLS-resets, parked domains, ad-trap redirect shells), and the PR52
// smoke showed the surfaced set held hostage to them (21 surfaced / 8
// resolved). Search now pre-fetches every result's .torrent bytes
// bounded-concurrent (≤8) with a short per-URL budget (~10s) through
// the provider's own netclient route; a result whose bytes cannot be
// fetched AND parsed as bencode metainfo is dropped with a logged
// typed reason. Survivors' bytes are handed straight to the engine
// (TorrentBase.IngestMetaInfo under the original link), so the later
// resolve leg never re-fetches what the preflight already carried.
//
// Latency budget (documented trade-off): N URLs × ≤10s ÷ 8 concurrent ≈
// ≤30s worst case for a 21-item surface, typically far less — the
// price of surfacing only results that really resolve. Results whose
// link is not http(s) (magnet-shaped <link> values) are kept
// unprobed — the preflight is an HTTP probe, the engine owns magnets.
type TokyoTosho struct {
	Base
	*TorrentBase

	// preflightTimeout overrides the per-URL preflight budget (tests);
	// 0 keeps ttPreflightTimeout.
	preflightTimeout time.Duration
	// log routes the preflight drop reasons; nil keeps slog.Default.
	log *slog.Logger
}

// newTokyoTosho builds the provider. The engine may be nil (fails loud
// on use): the registry injects the shared engine when [torrent] is
// enabled.
func newTokyoTosho(baseURL string, http *netclient.Client, engine *torrent.Engine) *TokyoTosho {
	return &TokyoTosho{
		Base: Base{
			id:          "tokyotosho",
			name:        "TokyoTosho",
			baseURL:     baseURL,
			sourceType:  contracts.SourceTypeBoth,
			contentLang: "ja",
			http:        http,
		},
		TorrentBase: NewTorrentBase(engine),
	}
}

// IsTorrent implements contracts.TorrentProvider.
func (p *TokyoTosho) IsTorrent() bool { return true }

// NamePreference implements contracts.NamePreferenceProvider (PR42):
// TT indexes romaji/english release names only — the search fan-out
// must route it the latin variants, never the Cyrillic ones.
func (p *TokyoTosho) NamePreference() contracts.NamePreference { return contracts.NamePrefLatin }

// tokyoToshoRSS is the RSS 2.0 envelope of the search feed.
type tokyoToshoRSS struct {
	Channel struct {
		Items []ttItem `xml:"item"`
	} `xml:"channel"`
}

// ttItem mirrors one RSS <item>: the release name in <title>, the
// direct .torrent URL in <link>, the category name and the HTML
// description blob (size text, magnet — see the provider comment).
type ttItem struct {
	Category    string `xml:"category"`
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description string `xml:"description"`
	PubDate     string `xml:"pubDate"`
}

// Search queries the site's search RSS. The torrent link of a result
// is the <link> .torrent URL verbatim (the engine ingests it by URL —
// no separate download step here); Anime-category items only, linkless
// or titleless ones dropped instead of handed downstream as dead
// results (nyaa rule).
func (p *TokyoTosho) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	// Empty queries are a caller bug: reject before any network I/O.
	if strings.TrimSpace(query) == "" {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, 0,
			fmt.Errorf("%w: пустой поисковый запрос", contracts.ErrInvalidInput))
	}

	searchURL := p.baseURL + "/rss.php?" + url.Values{
		"terms": {query},
		"type":  {"1"},
	}.Encode()

	resp, err := p.http.Do(ctx, netclient.Request{
		Method: "GET",
		URL:    searchURL,
		Op:     contracts.OpSearch,
	})
	if err != nil {
		return nil, err
	}

	var feed tokyoToshoRSS
	if err := xml.Unmarshal(resp.Body, &feed); err != nil {
		// TT's own zero-result shape (ttEmptyFeedFooter): an empty
		// answer, not a malfunction — the row settles as "0 results".
		if ttEmptyFeedFooter(resp.Body) {
			return []contracts.SearchResult{}, nil
		}
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, resp.StatusCode,
			fmt.Errorf("decode rss: %w", err))
	}

	results := make([]contracts.SearchResult, 0, len(feed.Channel.Items))
	for _, item := range feed.Channel.Items {
		// The feed's type=1 filter is soft (live-verified): enforce
		// the anime category here, on the authoritative element.
		if strings.TrimSpace(item.Category) != ttCategoryAnime {
			continue
		}
		title := strings.TrimSpace(item.Title)
		if title == "" {
			continue
		}
		link := strings.TrimSpace(item.Link)
		if link == "" {
			// No .torrent URL: the item has nothing the engine
			// could ingest.
			continue
		}
		results = append(results, contracts.SearchResult{
			Title:    title,
			URL:      link,
			SourceID: p.ID(),
			Meta: map[string]any{
				// The size text rides the description blob
				// verbatim; absent on some items (fail-soft). The
				// feed carries no seed counts at all.
				SearchMetaSize: ttSize(item.Description),
				// The PR35 release parser rides the search
				// result too: the badge is known before ingest.
				SearchMetaQuality: torrent.ParseQuality(title).Badge(),
			},
		})
	}
	return p.preflight(ctx, results), nil
}

// ttPreflightConcurrency bounds the dead-host preflight fan-out
// (owner ruling: ≤8).
const ttPreflightConcurrency = 8

// ttPreflightTimeout is the per-URL budget of one .torrent preflight
// fetch (owner ruling: short, ~10s).
const ttPreflightTimeout = 10 * time.Second

// logger returns the provider's drop-reason logger.
func (p *TokyoTosho) logger() *slog.Logger {
	if p.log != nil {
		return p.log
	}
	return slog.Default()
}

// preflightBudget is the effective per-URL fetch budget.
func (p *TokyoTosho) preflightBudget() time.Duration {
	if p.preflightTimeout > 0 {
		return p.preflightTimeout
	}
	return ttPreflightTimeout
}

// preflight drops search results whose .torrent bytes are not fetchable
// and parseable (dead hosts never surface), and hands the surviving
// bytes to the engine under the original link so ingestion never
// re-fetches. Fail-soft at the edges: a link that is not an http(s)
// URL cannot be probed and is kept; an engine handoff failure keeps
// the result (the resolve leg falls back to the URL ingest).
func (p *TokyoTosho) preflight(ctx context.Context, results []contracts.SearchResult) []contracts.SearchResult {
	if p.engineSnapshot() == nil {
		// No engine wired: the registry never registers the provider
		// this way ([torrent] disabled) — hand-built unit tests do.
		// Nothing to preflight, nothing to feed; keep the surface.
		return results
	}

	type indexed struct {
		i int
		r contracts.SearchResult
	}
	items := make([]indexed, len(results))
	for i, r := range results {
		items[i] = indexed{i: i, r: r}
	}
	alive := make([]bool, len(results))
	_ = netclient.Parallel(ctx, items, ttPreflightConcurrency, func(ctx context.Context, it indexed) error {
		// The engine handoff rides the CALLER's context (no point
		// ingesting into a cancelled session); only the fetch carries
		// the short per-URL budget.
		if !strings.HasPrefix(it.r.URL, "http://") && !strings.HasPrefix(it.r.URL, "https://") {
			alive[it.i] = true
			return nil
		}
		probeCtx, cancel := context.WithTimeout(ctx, p.preflightBudget())
		defer cancel()
		resp, err := p.http.Get(probeCtx, it.r.URL, nil)
		if err != nil {
			p.logger().Info("tokyotosho: preflight dropped dead .torrent host", "url", it.r.URL, "reason", err)
			return nil // never aborts the group
		}
		mi, err := metainfo.Load(bytes.NewReader(resp.Body))
		if err != nil {
			p.logger().Info("tokyotosho: preflight dropped .torrent host",
				"url", it.r.URL,
				"reason", fmt.Errorf("%w: %w", torrent.ErrNotMetainfo, err))
			return nil
		}
		alive[it.i] = true
		if _, err := p.IngestMetaInfo(ctx, it.r.URL, mi); err != nil {
			p.logger().Info("tokyotosho: preflight ingest failed (resolve leg will retry by URL)", "url", it.r.URL, "reason", err)
		}
		return nil
	})

	kept := make([]contracts.SearchResult, 0, len(results))
	for i, r := range results {
		if alive[i] {
			kept = append(kept, r)
		}
	}
	return kept
}

// ttSize extracts the size text ("1.66GB") from the description HTML
// blob; "" when the feed omits the line (fail-soft).
func ttSize(description string) string {
	m := ttSizeText.FindStringSubmatch(description)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// GetEpisodes ingests the result's torrent link and waits — bounded
// by the caller's context — for the engine's metadata fetch, then
// maps the release's files onto standard episodes (the TorrentBase
// contract; single-episode releases yield one playable entry, batches
// one per file with the parsed episode numbers).
func (p *TokyoTosho) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	return p.EpisodesWait(ctx, animeURL)
}

// ResolveStream resolves playback through the torrent core: the link
// rides the episode's RawEmbeds, the stream points at the loopback
// server.
func (p *TokyoTosho) ResolveStream(_ context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	return p.TorrentBase.ResolveStream(episode, dubID)
}
