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
	"net/url"
	"regexp"
	"strings"

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
type TokyoTosho struct {
	Base
	*TorrentBase
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
	return results, nil
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
