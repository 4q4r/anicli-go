package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// GogoAnimeBase is the site root [LIVE-VERIFIED 2026-09-13]. gogoanime.by
// replaced the legacy gogoanime3.co platform (Cloudflare-blocked) with a
// WordPress/dramastream site: search, episode lists and server links are
// all server-rendered WordPress pages now — the anicli-py port's HTML
// shapes and the ajax.gogo-load.com episode endpoint are obsolete.
const GogoAnimeBase = "https://gogoanime.by"

// gogoFileRe captures the inline jwplayer media URL of megaplay-style
// embeds [LIVE-VERIFIED 2026-09-13: megaplay.su/embed.php setups carry
// file: "<url>" — mirrors the sovetromantica inline-player regex shape].
var gogoFileRe = regexp.MustCompile(`file:\s*"([^"]+)"`)

// GogoAnime serves the gogoanime.by WordPress platform: a `/?s=` search,
// episode lists rendered on each /series/ page, per-episode server links
// (#w-servers) behind a referer-gated same-origin /player/ proxy, and two
// live embed families (megavid #player-payload JSON, megaplay inline
// jwplayer). All shapes verified live 2026-09-13.
type GogoAnime struct {
	Base
}

// newGogoAnime builds the provider against baseURL. The netclient already
// sends a user agent on every request; the Referer is a PR5 task ruling
// kept for the embed-heavy site.
func newGogoAnime(baseURL string, http *netclient.Client) *GogoAnime {
	return &GogoAnime{
		Base: Base{
			id:         "gogoanime",
			name:       "GogoAnime",
			baseURL:    baseURL,
			sourceType: contracts.SourceTypeVideo,
			headers:    map[string]string{"Referer": baseURL},
			http:       http,
		},
	}
}

// gogoPlayerPayload mirrors the #player-payload JSON of megavid embeds
// [LIVE-VERIFIED 2026-09-13].
type gogoPlayerPayload struct {
	SourceURL string `json:"sourceUrl"`
}

// gogoPlayerSource mirrors the megavid source-endpoint response
// [LIVE-VERIFIED 2026-09-13: {"status":"ok","source":"https://…/vid/…",
// "type":"hls"}].
type gogoPlayerSource struct {
	Status string `json:"status"`
	Source string `json:"source"`
	Type   string `json:"type"`
}

// Search scrapes the WordPress search `/?s=<query>`
// [LIVE-VERIFIED 2026-09-13]. Results are the a.tip cards of the FIRST
// .listupd — the search-results section under the `Search '…'` heading.
// The second .listupd (an external-site aggregator section) and the
// sidebar .leftseries cards must not leak in. Result URLs stay the
// absolute /series/ hrefs the site emits, verbatim.
func (p *GogoAnime) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	params := url.Values{}
	params.Set("s", query)

	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     p.baseURL + "/?" + params.Encode(),
		Headers: p.headers,
		Op:      contracts.OpSearch,
	})
	if err != nil {
		return nil, err
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(resp.Body))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, resp.StatusCode,
			fmt.Errorf("parse search page: %w", err))
	}

	var results []contracts.SearchResult
	doc.Find(".listupd").First().Find("a.tip").Each(func(_ int, item *goquery.Selection) {
		href, exists := item.Attr("href")
		if !exists || href == "" {
			return
		}
		title, _ := item.Attr("title")

		var poster string
		if img := item.Find(".limit img").First(); img.Length() > 0 {
			poster, _ = img.Attr("src")
		}

		results = append(results, contracts.SearchResult{
			Title:    title,
			URL:      href,
			SourceID: p.ID(),
			Poster:   poster,
		})
	})
	return results, nil
}

// GetEpisodes lists the server-rendered episode grid of a series page
// [LIVE-VERIFIED 2026-09-13: .episodes-container renders ALL
// .episode-item entries — the live Naruto Shippuuden page carries 499,
// One Piece Dubbed 1123 — newest-first; the page "select" pagination is
// display-only]. The provider reverses to ascending, the order the legacy
// ajax path produced. RawID is the absolute episode URL.
func (p *GogoAnime) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	if !strings.HasPrefix(animeURL, "http") {
		animeURL = p.baseURL + animeURL
	}

	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     animeURL,
		Headers: p.headers,
		Op:      contracts.OpGetEpisodes,
	})
	if err != nil {
		return nil, err
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(resp.Body))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("parse series page: %w", err))
	}

	var episodes []contracts.Episode
	doc.Find(".episodes-container .episode-item").Each(func(_ int, item *goquery.Selection) {
		linkNode := item.Find("a").First()
		if linkNode.Length() == 0 {
			return
		}
		href, _ := linkNode.Attr("href")
		href = strings.TrimSpace(href)
		if href == "" {
			return
		}

		// Episode number: the structured data attribute, with the
		// "Episode N" anchor text as fallback.
		num, _ := item.Attr("data-episode-number")
		if num == "" {
			num = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(linkNode.Text()), "Episode "))
		}
		if num == "" {
			num = "0"
		}

		title := strings.TrimSpace(linkNode.Text())
		if title == "" {
			title = "Episode " + num
		}

		episodes = append(episodes, contracts.Episode{
			Num:       num,
			Title:     title,
			RawID:     href,
			RawEmbeds: map[string][]string{},
		})
	})

	// The grid renders newest-first; flip to ascending.
	for i, j := 0, len(episodes)-1; i < j; i, j = i+1, j-1 {
		episodes[i], episodes[j] = episodes[j], episodes[i]
	}
	return episodes, nil
}

// FetchDubs hydrates episode.RawEmbeds from the episode page's #w-servers
// server list [LIVE-VERIFIED 2026-09-13: #w-servers .servers
// li.player-type-link entries carry the server name as text and the
// same-origin /player/ proxy URL in data-src; entries without data-src
// are skipped].
func (p *GogoAnime) FetchDubs(ctx context.Context, episode *contracts.Episode) (*contracts.Episode, error) {
	episodeURL := episode.RawID
	if !strings.HasPrefix(episodeURL, "http") {
		episodeURL = p.baseURL + episodeURL
	}

	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     episodeURL,
		Headers: p.headers,
		Op:      contracts.OpGetEpisodes,
	})
	if err != nil {
		return nil, err
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(resp.Body))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("parse episode page: %w", err))
	}

	embeds := map[string][]string{}
	doc.Find("#w-servers li.player-type-link").Each(func(_ int, item *goquery.Selection) {
		videoURL, _ := item.Attr("data-src")
		if videoURL == "" {
			return
		}
		serverName := strings.TrimSpace(item.Text())
		if serverName == "" {
			serverName = "Unknown"
		}
		embeds[serverName] = append(embeds[serverName], videoURL)
	})

	episode.RawEmbeds = embeds
	return episode, nil
}

// ResolveStream resolves the chosen server through the referer-gated
// /player/ proxy onto the real embed and from there to media URLs
// [LIVE-VERIFIED 2026-09-13]:
//
//  1. GET the /player/ data-src with the episode-page Referer (without it
//     the proxy redirects to the site root) — it wraps the real embed in
//     an iframe.player-iframe;
//  2. GET the embed page with the /player/ Referer:
//     megavid-family pages carry #player-payload JSON whose sourceUrl
//     answers {status,source,type} (Accept: application/json), megaplay
//     pages carry an inline jwplayer `file: "<url>"`;
//     other embed hosts run through the extractor factory.
//
// Like the Python original (gogoanime.py:122-134) the dub list is fetched
// lazily when the episode carries none, and links merge with
// dict.update semantics; an extraction failure surfaces only when no
// link resolved anything.
func (p *GogoAnime) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	if len(episode.RawEmbeds) == 0 {
		hydrated, err := p.FetchDubs(ctx, &episode)
		if err != nil {
			return contracts.MediaStream{DubName: dubID, Links: map[string]contracts.VideoSource{}}, err
		}
		episode = *hydrated
	}

	stream := contracts.MediaStream{
		DubName: dubID,
		Links:   map[string]contracts.VideoSource{},
	}

	episodeURL := episode.RawID
	if !strings.HasPrefix(episodeURL, "http") {
		episodeURL = p.baseURL + episodeURL
	}

	var firstErr error
	for _, link := range episode.RawEmbeds[dubID] {
		sources, err := p.resolveEmbed(ctx, link, episodeURL)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for quality, src := range sources {
			stream.Links[quality] = src // dict.update: later links overwrite
		}
	}
	if len(stream.Links) == 0 && firstErr != nil {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0, firstErr)
	}
	return stream, nil
}

// resolveEmbed walks the /player/ proxy chain for one server link.
func (p *GogoAnime) resolveEmbed(ctx context.Context, playerURL, episodeURL string) (map[string]contracts.VideoSource, error) {
	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     playerURL,
		Headers: map[string]string{"Referer": episodeURL},
		Op:      contracts.OpResolveStream,
	})
	if err != nil {
		return nil, err
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(resp.Body))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, resp.StatusCode,
			fmt.Errorf("parse player page: %w", err))
	}

	embedURL, _ := doc.Find("iframe.player-iframe").First().Attr("src")
	embedURL = strings.TrimSpace(embedURL)
	if embedURL == "" {
		// No proxy wrapper: the link may be a direct embed — run it
		// through the factory. A link that yields nothing anywhere is a
		// dead end, reported loudly rather than as an empty stream.
		return p.factorySources(ctx, playerURL)
	}

	return p.resolvePlayerEmbed(ctx, embedURL, playerURL)
}

// factorySources runs URLs through the extractor factory and converts a
// nothing-resolved outcome into a typed error (the factory itself mirrors
// the Python {} for unmatched hosts).
func (p *GogoAnime) factorySources(ctx context.Context, links ...string) (map[string]contracts.VideoSource, error) {
	sources, err := resolveEmbeds(ctx, p.http, links)
	if err != nil {
		return nil, err
	}
	if len(sources) == 0 {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
			fmt.Errorf("no sources extracted from %s", strings.Join(links, ", ")))
	}
	return sources, nil
}

// resolvePlayerEmbed extracts media sources off the real embed page.
func (p *GogoAnime) resolvePlayerEmbed(ctx context.Context, embedURL, refererURL string) (map[string]contracts.VideoSource, error) {
	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     embedURL,
		Headers: map[string]string{"Referer": refererURL},
		Op:      contracts.OpResolveStream,
	})
	if err != nil {
		return nil, err
	}

	// megavid family: #player-payload JSON → sourceUrl endpoint.
	if payload := strings.TrimSpace(docText(resp.Body, "#player-payload")); payload != "" {
		var data gogoPlayerPayload
		if err := json.Unmarshal([]byte(payload), &data); err != nil {
			return nil, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, resp.StatusCode,
				fmt.Errorf("decode player payload %s: %w", embedURL, err))
		}
		if data.SourceURL == "" {
			return nil, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, resp.StatusCode,
				fmt.Errorf("player payload %s carries no sourceUrl", embedURL))
		}
		return p.resolvePayloadSource(ctx, data, embedURL)
	}

	// megaplay family: inline jwplayer file URL.
	if match := gogoFileRe.FindSubmatch(resp.Body); match != nil {
		src := string(match[1])
		return map[string]contracts.VideoSource{
			"720": {
				URL:     src,
				Quality: "720",
				Type:    mediaType(src),
				Headers: map[string]string{"Referer": embedURL},
			},
		}, nil
	}

	// Unknown embed host: the extractor factory (kwik, dood,
	// streamtape, …); nothing-resolving is a dead end and errors loudly.
	return p.factorySources(ctx, embedURL)
}

// resolvePayloadSource fetches the payload's sourceUrl endpoint and maps
// its {status,source,type} answer to a VideoSource [LIVE-VERIFIED
// 2026-09-13: the megavid bootstrap sends Accept: application/json and
// the episode-page Referer].
func (p *GogoAnime) resolvePayloadSource(ctx context.Context, data gogoPlayerPayload, embedURL string) (map[string]contracts.VideoSource, error) {
	sourceURL := data.SourceURL
	if !strings.HasPrefix(sourceURL, "http") {
		base, err := url.Parse(embedURL)
		if err != nil {
			return nil, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
				fmt.Errorf("parse embed url %s: %w", embedURL, err))
		}
		ref, err := url.Parse(sourceURL)
		if err != nil {
			return nil, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
				fmt.Errorf("parse source url %s: %w", sourceURL, err))
		}
		sourceURL = base.ResolveReference(ref).String()
	}

	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     sourceURL,
		Headers: map[string]string{"Referer": embedURL, "Accept": "application/json"},
		Op:      contracts.OpResolveStream,
	})
	if err != nil {
		return nil, err
	}

	var source gogoPlayerSource
	if err := json.Unmarshal(resp.Body, &source); err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, resp.StatusCode,
			fmt.Errorf("decode source %s: %w", sourceURL, err))
	}
	if source.Status != "ok" || source.Source == "" {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, resp.StatusCode,
			fmt.Errorf("source %s status %q carries no media", sourceURL, source.Status))
	}

	kind := source.Type
	if kind == "hls" {
		kind = "m3u8"
	}
	return map[string]contracts.VideoSource{
		"720": {
			URL:     source.Source,
			Quality: "720",
			Type:    kind,
			Headers: map[string]string{"Referer": embedURL},
		},
	}, nil
}

// docText extracts the trimmed text of the first selector match ("" when
// absent or the body is not HTML).
func docText(body []byte, selector string) string {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(doc.Find(selector).First().Text())
}

// mediaType maps a media URL onto the container-ish type tag the
// VideoSource contract uses.
func mediaType(src string) string {
	if strings.Contains(src, ".m3u8") {
		return "m3u8"
	}
	if strings.Contains(src, ".mp4") {
		return "mp4"
	}
	return ""
}
