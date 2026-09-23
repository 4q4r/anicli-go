package providers

// RuTor (PR87) — torrent search over the rutor.info public tracker,
// the fifth TorrentBase provider (nyaa/anilibria-torrent/animetosho/
// tokyotosho lineage). A RU general catalog (movies/TV/anime), fully
// anonymous: the search page and the .torrent downloads need no
// account — live-verified 2026-09-23 (search «dandadan»/«черная
// лагуна», download of release 1044920 whose bytes hash EXACTLY to
// the row magnet's btih). 4K/UHD releases are anonymous too (Принцесса
// Мононоке UHD BDRemux 2160p surfaced in the live check).
//
// Written against the live site with the mature Jackett definition
// (src/Jackett.Common/Definitions/rutor.yml) as the extraction recipe;
// every selector below was re-verified against the live HTML on
// 2026-09-23:
//   - Search: GET {base}/search/0/0/100/{sort}/{query}/ — the Jackett
//     path (100 = title search), sort 0 = created desc (Jackett
//     default). Queries ride the path percent-encoded; RU queries are
//     first-class (the index matches RU titles; е/ё treated alike).
//   - Rows: tr:has(td:has(a[href^="magnet:?xt="])) — the Jackett row
//     selector verbatim: it keys on the magnet anchor, so the header
//     row (tr.backgr) and the tracker-news block are excluded by
//     construction, and a zebra-class rename upstream cannot break
//     row detection.
//   - Title: td:nth-of-type(2) a[href^="/torrent/"] (Jackett).
//   - .torrent: a.downgif — on the wire a PROTOCOL-RELATIVE
//     //d.rutor.info/download/{id}; it is resolved against the site
//     root before surfacing. Anonymous (200, application/x-bittorrent).
//   - Magnet: a[href^="magnet:?xt="] — the fallback link when the row
//     has no download anchor; kept VERBATIM (it carries its own
//     tr= announces) and validated as a strict 40-hex btih (the
//     tokyotosho base32 lesson).
//   - Size: fished by content (`38.92&nbsp;GB` in its own td) —
//     Jackett fishes too, because the comments cell is OPTIONAL
//     upstream and positional td indexes lie.
//   - Seed/leech: td span.green / td span.red (Jackett), digits
//     extracted from the span text.
//
// The PR66 ingestion stands: the result link is the absolute .torrent
// URL; search preflights every result's bytes (bounded ≤8, ~10s/URL),
// drops the dead hosts BEFORE they surface and hands the survivors'
// bytes to the engine (IngestMetaInfo), so the resolve leg never
// re-fetches and metadata needs no swarm round-trip. Magnet-only rows
// are kept unprobed (the preflight is an HTTP probe; the engine owns
// magnets). The PR44 seedless filter stands (seed counts are on the
// page). Zero results (HTTP 200, «Результатов поиска 0», no data
// rows) settle as an empty surface — the row selector is the
// zero-detection, nothing to special-case.
//
// TRANSPORT (live-verified 2026-09-23, the animevost class): the
// main site tarpits the shared netclient's Chrome_150 uTLS
// fingerprint at the TLS layer — every rutor.info request stalls
// without response headers until the watchdog kills it (the smoke's
// "provider timeout" at 10s; still stalled at a raw 30s probe) —
// while non-Chrome fingerprints answer in 0.1–0.4s (Go stdlib,
// curl; headers irrelevant: an empty-UA stdlib request passes, so
// the discriminator is the ClientHello, not headers). The search
// page therefore rides a provider-scoped plain-Go client. The
// download host d.rutor.info has NO such tarpit (uTLS 200 in 0.6s,
// live) — the PR66 preflight keeps the provider's own netclient
// route for the .torrent bytes; should that ever change, the
// preflight's dead-host filter degrades honestly (drops) and the
// resolve leg retries by URL.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	nethttp "net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
	"github.com/an0nx/anicli-go/internal/torrent"
)

// RutorBase is the site root. The mirror family rotates (rutor.is /
// rutor.org / historical rutor.org blocks); rutor.info is the
// Jackett-canonical primary and answered fastest on the 2026-09-23
// live check (rutor.is flapped TLS-EOF on the same route).
const RutorBase = "https://rutor.info"

// rutor search parameters: the Jackett recipe path with the title
// bitmask (100) and the default sort (0 = created desc — fresh
// releases first; the seedless filter handles quality of the rest).
const (
	rutorSearchMask  = "100"
	rutorSortCreated = "0"
)

// rutorSizeText matches the size cell's full text ("38.92 GB" after
// the &nbsp; normalization); anchored, so the date/comments/title
// cells can never false-positive.
var rutorSizeText = regexp.MustCompile(`(?i)^[\d.,]+\s*[kmgtpe]?b$`)

// rutorPeerDigits extracts the count from a span.green/span.red text
// (the span renders an arrow img then "&nbsp;8" — Text() keeps only
// the tail).
var rutorPeerDigits = regexp.MustCompile(`\d+`)

// rutorMagnetHash validates the magnet's btih: a strict 40-hex
// infohash (tokyotosho lesson: other encodings are rejected, the
// engine contract is 40-hex).
var rutorMagnetHash = regexp.MustCompile(`urn:btih:([0-9a-fA-F]{40})`)

// rutorHTTPClient is the provider-scoped DIRECT transport for the
// search page (the animevost pattern): rutor.info tarpits the shared
// netclient's Chrome_150 uTLS fingerprint host-wide — see the
// transport note in the provider comment. The site is RU-hosted, so
// the connection is DIRECT (no proxy); the 30s timeout mirrors
// netclient's RequestTimeout, and the UA/Accept-Language headers
// mirror browser-grade request shape.
var rutorHTTPClient = &nethttp.Client{Timeout: 30 * time.Second}

// rutorUserAgent and rutorAcceptLanguage mirror the netclient
// defaults (browser-grade UA, RU-leaning language list for the RU
// catalog).
const (
	rutorUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
	rutorAcceptLanguage = "ru-RU,ru;q=0.9,en-US;q=0.8,en;q=0.7"

	// rutorBodyLimit caps one search page; the largest observed live
	// body (a 20-result search) is ~30KB.
	rutorBodyLimit = 8 << 20
)

// RuTor is the rutor.info torrent provider over the shared Base
// identity and TorrentBase engine plumbing.
//
// The engine may be nil (fails loud on use): the registry injects the
// shared engine when [torrent] is enabled.
type RuTor struct {
	Base
	*TorrentBase

	// preflightTimeout overrides the per-URL preflight budget (tests);
	// 0 keeps torrentPreflightTimeout.
	preflightTimeout time.Duration
}

// newRutor builds the provider.
func newRutor(baseURL string, http *netclient.Client, engine *torrent.Engine) *RuTor {
	return &RuTor{
		Base: Base{
			id:          "rutor",
			name:        "RuTor",
			baseURL:     baseURL,
			sourceType:  contracts.SourceTypeBoth,
			contentLang: "ru",
			http:        http,
		},
		TorrentBase: NewTorrentBase(engine),
	}
}

// IsTorrent implements contracts.TorrentProvider.
func (p *RuTor) IsTorrent() bool { return true }

// The provider deliberately does NOT declare NamePreference: the
// contract's only alternative is NamePrefLatin (latin-only indexes —
// the foreign torrent feeds), and RuTor's index is RU-first (RU
// titles primary, RU queries verified live) — the anilibria-torrent
// RU-group convention. Default routing sends it the Cyrillic-first
// variants, which is exactly what its index matches.

// Search queries the public search page. The torrent link of a result
// is the row's absolute .torrent download URL (the PR66 preflight
// carries its bytes to the engine); the row magnet is the fallback
// when the download anchor is missing. Rows with neither are dropped
// instead of handed downstream as dead results (nyaa rule).
func (p *RuTor) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	// Empty queries are a caller bug: reject before any network I/O.
	if strings.TrimSpace(query) == "" {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, 0,
			fmt.Errorf("%w: пустой поисковый запрос", contracts.ErrInvalidInput))
	}

	searchURL := p.baseURL + "/search/0/0/" + rutorSearchMask + "/" + rutorSortCreated + "/" + url.PathEscape(query) + "/"
	body, err := rutorGet(ctx, searchURL)
	if err != nil {
		return nil, err
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, 0,
			fmt.Errorf("decode html: %w", err))
	}

	results := make([]contracts.SearchResult, 0, 16)
	// The Jackett row selector verbatim: rows carrying a magnet anchor.
	doc.Find(`tr:has(td:has(a[href^="magnet:?xt="]))`).Each(func(_ int, row *goquery.Selection) {
		title := strings.TrimSpace(row.Find("td").Eq(1).Find(`a[href^="/torrent/"]`).First().Text())
		if title == "" {
			return
		}
		link := rutorResultLink(row, p.baseURL)
		if link == "" {
			// No .torrent anchor and no usable magnet: the row has
			// nothing the engine could ingest.
			return
		}
		results = append(results, contracts.SearchResult{
			Title:    title,
			URL:      link,
			SourceID: p.ID(),
			Meta: map[string]any{
				SearchMetaSize:     rutorSize(row),
				SearchMetaSeeders:  rutorPeer(row, "span.green"),
				SearchMetaLeechers: rutorPeer(row, "span.red"),
				// The PR35 release parser rides the search result
				// too: the badge is known before ingest.
				SearchMetaQuality: torrent.ParseQuality(title).Badge(),
			},
		})
	})
	// Seedless items are dead results: drop them before they surface
	// (fail-soft — rows without a seed field survive), so the
	// preflight never spends a fetch on them. The preflight then
	// probes the survivors' .torrent bytes: dead hosts never surface,
	// and the surviving bytes feed the engine (no double fetch).
	return p.preflight(ctx, filterSeedless(results)), nil
}

// rutorGet fetches one rutor.info page through the provider-scoped
// plain-Go client (the animevost postForm pattern): everything is
// wrapped with the provider id and operation tag, the body is capped,
// and non-2xx statuses settle as typed errors.
func rutorGet(ctx context.Context, url string) ([]byte, error) {
	const op = contracts.OpSearch
	req, err := nethttp.NewRequestWithContext(ctx, nethttp.MethodGet, url, nil)
	if err != nil {
		return nil, contracts.WrapProvider("rutor", op, 0, fmt.Errorf("build request: %w", err))
	}
	req.Header.Set("User-Agent", rutorUserAgent)
	req.Header.Set("Accept-Language", rutorAcceptLanguage)

	resp, err := rutorHTTPClient.Do(req)
	if err != nil {
		return nil, contracts.WrapProvider("rutor", op, 0, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	body, err := io.ReadAll(io.LimitReader(resp.Body, rutorBodyLimit+1))
	if err != nil {
		return nil, contracts.WrapProvider("rutor", op, 0, fmt.Errorf("read body: %w", err))
	}
	if int64(len(body)) > rutorBodyLimit {
		return nil, contracts.WrapProvider("rutor", op, 0, fmt.Errorf("response body exceeds limit: %w",
			netclient.ErrBodyLimit))
	}

	switch {
	case resp.StatusCode == nethttp.StatusForbidden:
		return nil, contracts.WrapProvider("rutor", op, resp.StatusCode, contracts.ErrProvider403)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, contracts.WrapProvider("rutor", op, resp.StatusCode,
			&netclient.StatusError{StatusCode: resp.StatusCode, Status: resp.Status})
	}
	return body, nil
}

// rutorResultLink picks the torrent link of one search row: the
// a.downgif .torrent URL resolved against the site root (the wire
// value is protocol-relative //d.rutor.info/download/{id}), the row
// magnet verbatim as the no-anchor fallback, "" when neither is
// usable (the caller drops such rows).
func rutorResultLink(row *goquery.Selection, baseURL string) string {
	if href, ok := row.Find("a.downgif").First().Attr("href"); ok {
		if u := rutorAbsolute(baseURL, strings.TrimSpace(href)); u != "" {
			return u
		}
	}
	if href, ok := row.Find(`a[href^="magnet:?xt="]`).First().Attr("href"); ok {
		href = strings.TrimSpace(href)
		if rutorMagnetHash.MatchString(href) {
			return href
		}
	}
	return ""
}

// rutorAbsolute resolves a (possibly protocol-relative) href against
// the site root: "//d.rutor.info/download/1" gains the site's scheme
// and surfaces as a preflightable https URL.
func rutorAbsolute(baseURL, href string) string {
	base, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	ref, err := url.Parse(href)
	if err != nil {
		return ""
	}
	return base.ResolveReference(ref).String()
}

// rutorSize fishes the size cell's text out of the row by content
// ("38.92 GB" after normalization) — the comments cell is optional
// upstream, so positional td indexes lie (the Jackett comment).
// "" when the row carries no size cell (fail-soft).
func rutorSize(row *goquery.Selection) string {
	size := ""
	row.Find("td").EachWithBreak(func(_ int, td *goquery.Selection) bool {
		text := strings.TrimSpace(strings.ReplaceAll(td.Text(), "\u00a0", " "))
		if rutorSizeText.MatchString(text) {
			size = text
			return false
		}
		return true
	})
	return size
}

// rutorPeer extracts the count from the row's seed/leech span
// (span.green / span.red); "" when the span is missing or carries no
// digits (fail-soft — the PR44 filter keeps no-field rows).
func rutorPeer(row *goquery.Selection, sel string) string {
	return rutorPeerDigits.FindString(row.Find(sel).First().Text())
}

// preflightBudget is the effective per-URL fetch budget (the shared
// torrentPreflightTimeout default, owner ruling: short, ~10s).
func (p *RuTor) preflightBudget() time.Duration {
	if p.preflightTimeout > 0 {
		return p.preflightTimeout
	}
	return torrentPreflightTimeout
}

// preflight drops search results whose .torrent bytes are not
// fetchable and parseable (dead hosts never surface), and hands the
// surviving bytes to the engine under the original link so ingestion
// never re-fetches (the shared TorrentBase.preflightResults).
func (p *RuTor) preflight(ctx context.Context, results []contracts.SearchResult) []contracts.SearchResult {
	return p.preflightResults(ctx, p.http, p.loggerOrDiscard(), p.ID(), p.preflightBudget(), results)
}

// GetEpisodes ingests the result's torrent link and waits — bounded
// by the caller's context — for the engine's metadata fetch, then
// maps the release's files onto standard episodes (the TorrentBase
// contract; single-episode releases yield one playable entry, batches
// one per file with the parsed episode numbers).
func (p *RuTor) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	return p.EpisodesWait(ctx, animeURL)
}

// ResolveStream resolves playback through the torrent core: the link
// rides the episode's RawEmbeds, the stream points at the loopback
// server.
func (p *RuTor) ResolveStream(_ context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	return p.TorrentBase.ResolveStream(episode, dubID)
}
