package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// AnimePaheBase is the site root — the serving ORIGIN, deliberately not
// the canonical alias. Domain history (dated, verified 2026-09-18): the
// Python port targeted animepahe.ru (dead: 301s to a host the site
// disowns); animepahe.si — the Sep 2025 move (MALSync issue #3173) — is
// NXDOMAIN again since ~Apr 2026 (authoritative DNS). The site banner
// lists pw/com/org as its only domains; animepahe.com (MALSync's
// declared canonical) 301s to the serving origin animepahe.pw.
//
// Why the origin, not the .com alias: the Cloudflare clearance the [cf]
// ladder replays is host-scoped — live run 2026-09-18: with the alias
// base the solver solved (and captured cookies for) .pw, the jar scoped
// them to the .com request, and the redirected retry was re-challenged.
// Pointing at the origin keeps solve-and-replay same-host.
const AnimePaheBase = "https://animepahe.pw"

// animePaheDub is the fixed single dub resolve_stream would have used
// (animepahe.py:150 hardcodes dub_name="Original (Pahe)").
const animePaheDub = "Original (Pahe)"

// AnimePahe is the port of anicli-py anicli/providers/animepahe.py: a
// JSON /api face for search and the episode listing, and a scraped
// /play/<anime>/<episode> page whose #resolutionMenu buttons carry the
// kwik embed URLs (PR49: the site replaced the old quality anchors with
// server-rendered data-src buttons) that feed the extractor factory.
//
// PR71: the site operations ride the [cf] browser bridge when one is
// wired (the serving origin re-challenges non-browser fingerprints even
// with a replayed clearance — see animepahe_bridge.go); nil keeps the
// netclient + CF-ladder path.
type AnimePahe struct {
	Base
	// browser is the stealth-browser transport (nil = netclient).
	browser paheBrowser
}

// newAnimePahe builds the provider against baseURL.
//
// Divergence from Python (task ruling): the original defines the
// User-Agent+Referer header set (animepahe.py:24) but forgets to pass it
// to any request; the port actually sends the Referer (the UA is already
// applied by the netclient on every request).
func newAnimePahe(baseURL string, http *netclient.Client, browser paheBrowser) *AnimePahe {
	return &AnimePahe{Base: Base{
		id:          "animepahe",
		name:        "AnimePahe",
		baseURL:     baseURL,
		sourceType:  contracts.SourceTypeBoth,
		contentLang: "ja",
		headers:     map[string]string{"Referer": baseURL},
		http:        http,
	}, browser: browser}
}

// fetchPage GETs url through the provider's active transport: the
// in-page browser fetch (site cookies, real fingerprint) when the
// bridge is wired, the netclient + CF ladder otherwise. Failures are
// typed provider errors tagged with op.
func (p *AnimePahe) fetchPage(ctx context.Context, url string, op string) ([]byte, error) {
	if p.browser != nil {
		body, err := p.browser.PageFetch(ctx, url)
		if err != nil {
			return nil, contracts.WrapProvider(p.ID(), op, 0, err)
		}
		return body, nil
	}
	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     url,
		Headers: p.headers,
		Op:      op,
	})
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// animePaheSearch mirrors the fields consumed by animepahe.py:36-43.
// Shape re-verified live 2026-09-18 (animepahe.pw /api?m=search): the
// field names survived the domain moves; sessions became UUIDs.
type animePaheSearch struct {
	Data []struct {
		Title   string `json:"title"`
		Session string `json:"session"`
		Poster  string `json:"poster"`
	} `json:"data"`
}

// animePaheRelease mirrors the fields consumed by animepahe.py:62-87.
// Shape re-verified live 2026-09-18 (animepahe.pw /api?m=release).
type animePaheRelease struct {
	LastPage int `json:"last_page"`
	Data     []struct {
		Episode json.Number `json:"episode"`
		Session string      `json:"session"`
	} `json:"data"`
}

// Search queries the internal /api endpoint (anicli-py animepahe.py:26-46).
//
// PR49 divergence from Python: the original wraps http.get and json.loads
// in one except-block returning [] — transport and decode failures both
// surfaced as an empty result set. That quirk is retired: a silent empty
// set is what hid the domain death that killed this provider. Failures
// return typed provider errors (the gogoanime house pattern); only a
// genuinely empty result set returns no results.
func (p *AnimePahe) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	params := url.Values{}
	params.Set("m", "search")
	params.Set("q", query)

	body, err := p.fetchPage(ctx, p.baseURL+"/api?"+params.Encode(), contracts.OpSearch)
	if err != nil {
		return nil, err
	}

	var data animePaheSearch
	if jsonErr := json.Unmarshal(body, &data); jsonErr != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, 0,
			fmt.Errorf("decode search response: %w", jsonErr))
	}

	results := make([]contracts.SearchResult, 0, len(data.Data))
	for _, item := range data.Data {
		results = append(results, contracts.SearchResult{
			Title:    item.Title,
			URL:      item.Session, // session id doubles as the anime id
			SourceID: p.ID(),
			Poster:   item.Poster,
		})
	}
	return results, nil
}

// GetEpisodes pages through the m=release listing (anicli-py
// animepahe.py:48-91). animeURL is the search session id; the first
// response carries last_page and page one, remaining pages are fetched
// sequentially. PR49 divergence from Python: page failures are typed
// errors, not a silent empty list (see Search).
func (p *AnimePahe) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	episodes, lastPage, err := p.fetchReleasePage(ctx, animeURL, 1)
	if err != nil {
		return nil, err
	}

	for page := 2; page <= lastPage; page++ {
		more, _, err := p.fetchReleasePage(ctx, animeURL, page)
		if err != nil {
			return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
				fmt.Errorf("release page %d: %w", page, err))
		}
		episodes = append(episodes, more...)
	}
	return episodes, nil
}

// fetchReleasePage loads one m=release page, reporting its episodes and
// the listing's last_page.
func (p *AnimePahe) fetchReleasePage(ctx context.Context, animeURL string, page int) ([]contracts.Episode, int, error) {
	params := url.Values{}
	params.Set("m", "release")
	params.Set("id", animeURL)
	params.Set("sort", "episode_asc")
	params.Set("page", strconv.Itoa(page))

	body, err := p.fetchPage(ctx, p.baseURL+"/api?"+params.Encode(), contracts.OpGetEpisodes)
	if err != nil {
		return nil, 0, err
	}

	var data animePaheRelease
	if jsonErr := json.Unmarshal(body, &data); jsonErr != nil {
		return nil, 0, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
			fmt.Errorf("decode release page %d: %w", page, jsonErr))
	}

	episodes := make([]contracts.Episode, 0, len(data.Data))
	for _, item := range data.Data {
		// Python str(item.get("episode", 0)): the JSON wire literal is
		// kept ("2.5" stays "2.5"); a missing field yields "0".
		num := "0"
		if item.Episode != "" {
			num = pythonStr(item.Episode)
		}
		episodes = append(episodes, contracts.Episode{
			Num:   num,
			Title: "Episode " + num,
			RawID: animeURL + "|" + item.Session,
			RawEmbeds: map[string][]string{
				animePaheDub: {fmt.Sprintf("%s/play/%s/%s", p.baseURL, animeURL, item.Session)},
			},
		})
	}

	return episodes, data.LastPage, nil
}

// animePahePlayLinks parses a play page into quality → embed URL. The
// live page (2026-09-18 capture, testdata/animepahe_play.html)
// server-renders the quality menu:
//
//	<div class="dropdown-menu" id="resolutionMenu">
//	  <button type="button" data-src="https://kwik.cx/e/<id>" data-url="…"
//	    data-fansub="OZC" data-resolution="720" data-audio="jpn"
//	    data-av1="0" class="dropdown-item">OZC · 720p <span>BD</span></button>
//
// Buttons without a resolvable data-src or data-resolution are skipped;
// the page's OTHER .dropdown-item elements (episode links, provider
// tabs) must not leak in, hence the #resolutionMenu scoping. goquery,
// not a regex: the old attribute-order-sensitive anchor regex died with
// the old markup (PR49).
func animePahePlayLinks(body []byte) map[string]string {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil
	}

	links := map[string]string{}
	doc.Find("#resolutionMenu button[data-src]").Each(func(_ int, sel *goquery.Selection) {
		src, ok := sel.Attr("data-src")
		if !ok || src == "" {
			return
		}
		quality, ok := sel.Attr("data-resolution")
		if !ok || quality == "" {
			return
		}
		links[quality] = src // later buttons overwrite, like dict.update
	})
	return links
}

// ResolveStream resolves an episode through the two-attempt chain (PR71):
//
//	(a) the #resolutionMenu kwik embeds through the extractor factory —
//	    unchanged from PR49 and first in line so unblocked networks keep
//	    the player flow;
//	(b) when the bridge is wired and (a) left qualities unresolved (kwik
//	    /e/ is hard WAF-blocked on many networks; the failure is a plain
//	    403 deny, not a challenge), the #pickDownload interstitial
//	    chain: pahe.win short link → kwik /f/ file page → captured
//	    download URL. See animepahe_bridge.go for the full mechanism.
//
// A play page with neither menu is a typed provider error — both menus
// are server-rendered, so an empty parse means the shape drifted or a
// challenge page got through, and neither must masquerade as "no
// streams".
func (p *AnimePahe) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	stream := contracts.MediaStream{
		// Python hardcodes the dub name, ignoring dub_id (animepahe.py:150).
		DubName: animePaheDub,
		Links:   map[string]contracts.VideoSource{},
	}

	embeds := episode.RawEmbeds[dubID]
	if len(embeds) == 0 {
		return stream, nil
	}

	body, err := p.fetchPage(ctx, embeds[0], contracts.OpResolveStream)
	if err != nil {
		return stream, err
	}

	playLinks := animePahePlayLinks(body)

	// Attempt (a): the embed extraction, per quality, in sorted order.
	// Python ignores per-link extraction failures (an empty extractor
	// result updates nothing); a failing extractor must not shadow
	// links that do resolve. PR71: the first failure is remembered but
	// no longer short-circuits — attempt (b) gets its chance first.
	qualities := make([]string, 0, len(playLinks))
	for quality := range playLinks {
		qualities = append(qualities, quality)
	}
	sort.Strings(qualities)

	var embedErr error
	for _, quality := range qualities {
		sources, extractErr := resolveEmbeds(ctx, p.http, []string{playLinks[quality]})
		if extractErr != nil {
			if embedErr == nil {
				embedErr = extractErr
			}
			continue
		}
		for srcQuality, src := range sources {
			stream.Links[srcQuality] = src
		}
	}

	// Attempt (b): the interstitial chain fills whatever (a) missed.
	if p.browser != nil {
		if interErr := p.resolveViaInterstitials(ctx, body, &stream); interErr != nil {
			if embedErr == nil {
				embedErr = interErr
			} else {
				// Both attempts left evidence: the caller needs both —
				// the embed failure alone would mask an interstitial
				// wall (and vice versa).
				embedErr = fmt.Errorf("%w; interstitial chain: %w", embedErr, interErr)
			}
		}
	}

	if len(stream.Links) == 0 {
		if embedErr == nil {
			return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
				fmt.Errorf("play page carries no resolution buttons or download links (%s)", embeds[0]))
		}
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0, embedErr)
	}
	return stream, nil
}

// resolveViaInterstitials runs attempt (b): for every play-page
// quality the embed extraction missed, walk pahe.win → kwik /f/ →
// captured download URL through the bridge. Qualities that already
// resolved stay untouched; per-quality failures are remembered (the
// first one surfaces only when nothing resolved at all — the caller's
// convention) and never shadow qualities that do resolve.
func (p *AnimePahe) resolveViaInterstitials(ctx context.Context, playBody []byte, stream *contracts.MediaStream) error {
	downloads := animePaheDownloadLinks(playBody)
	if len(downloads) == 0 {
		return fmt.Errorf("play page carries no download menu for the interstitial chain")
	}

	qualities := make([]string, 0, len(downloads))
	for quality := range downloads {
		qualities = append(qualities, quality)
	}
	sort.Strings(qualities)

	var failures []string
	for _, quality := range qualities {
		if _, resolved := stream.Links[quality]; resolved {
			continue
		}
		media, err := p.resolveInterstitial(ctx, downloads[quality])
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s (%s): %s", quality, downloads[quality], err))
			continue
		}
		mediaType := "mp4"
		if strings.HasSuffix(media, ".m3u8") {
			mediaType = "m3u8"
		}
		stream.Links[quality] = contracts.VideoSource{
			URL:     media,
			Quality: quality,
			Type:    mediaType,
			// The kwik extractor's Referer convention: the media edge
			// sits behind the kwik estate.
			Headers: map[string]string{"Referer": "https://kwik.cx/"},
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("interstitial chain: %s", strings.Join(failures, "; "))
	}
	return nil
}

// resolveInterstitial walks ONE pahe.win short link to its direct
// media URL: interstitial page → kwik /f/ file page → token form →
// in-page submit under the driver's download capture.
func (p *AnimePahe) resolveInterstitial(ctx context.Context, interstitial string) (string, error) {
	pageBody, err := p.browser.PageHTML(ctx, interstitial)
	if err != nil {
		return "", fmt.Errorf("interstitial %s: %w", interstitial, err)
	}
	target := animePaheInterstitialTarget(pageBody)
	if target == "" {
		return "", fmt.Errorf("interstitial %s exposes no kwik target", interstitial)
	}

	fileBody, err := p.browser.PageHTML(ctx, target)
	if err != nil {
		return "", fmt.Errorf("kwik file page %s: %w", target, err)
	}
	action, _, ok := animePaheKwikForm(fileBody)
	if !ok {
		return "", fmt.Errorf("kwik file page %s carries no download form", target)
	}

	media, err := p.browser.SubmitDownload(ctx, target, action)
	if err != nil {
		return "", fmt.Errorf("download submit %s: %w", action, err)
	}
	return media, nil
}
