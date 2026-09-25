package providers

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// AniKotoBase is the site root [LIVE-VERIFIED 2026-09-25: direct HTTP
// 200 on every leg, no JS challenge, anonymous viewing].
const AniKotoBase = "https://anikototv.to"

// The site's own audio axis (the server list groups every embed under
// data-type="sub" or "dub"): SUB = original audio with subtitles, DUB
// = English dub. The names stay the site's verbatim labels — the same
// axis the reference implementations expose as --audio {sub,dub}.
const (
	akDubSub = "SUB"
	akDubDub = "DUB"
)

// akWatchSuffixRe strips the /ep-N tail of the search-card links so
// every result URL is the canonical /watch/{slug} page (both forms
// carry the watch-page data-id [LIVE-VERIFIED 2026-09-25]).
var akWatchSuffixRe = regexp.MustCompile(`/ep-[^/]+$`)

// akDataIDRe extracts the anime id from the watch page (the page also
// embeds it as /anime/getinfo/{id}; the data-id attribute is the first
// occurrence in both captures).
var akDataIDRe = regexp.MustCompile(`data-id="(\d+)"`)

// akPlayerParamRe reads the public AES key/IV, CDN signing secret and
// token TTL out of the (possibly unpacked) player bundle: four const
// declarations — three string literals (captured) and one integer —
// immediately followed by a function, with the crypto string table
// within reach (the akPlayerParameters nearby-check disambiguates
// multiple matches).
var akPlayerParamRe = regexp.MustCompile(
	`\bconst\s+[\w$]+\s*=\s*"(` + akJSStrBody + `)"` +
		`\s*(?:,|;\s*const\s+)\s*[\w$]+\s*=\s*"(` + akJSStrBody + `)"` +
		`\s*(?:,|;\s*const\s+)\s*[\w$]+\s*=\s*"(` + akJSStrBody + `)"` +
		`\s*(?:,|;\s*const\s+)\s*[\w$]+\s*=\s*(\d+)\s*;\s*function\s+[\w$]+\(`)

// akJSStrBody matches the CONTENT of a double-quoted JS string literal
// (escapes allowed, quotes excluded).
const akJSStrBody = `(?:\\.|[^"\\])*`

// akReturnEvalRe locates the XOR wrapper's return eval("…") call.
var akReturnEvalRe = regexp.MustCompile(`\breturn\s+eval\("(` + akJSStrBody + `)"\)`)

// akPayloadTailRe locates the wrapper payload argument: `})( "…" )` at
// the very end of the wrapper.
var akPayloadTailRe = regexp.MustCompile(`\}\)\("(` + akJSStrBody + `)"\)\s*$`)

// akObjectDeclRe reads the wrapper's obfuscation object declaration
// (`let X;`) at the start of the bundle.
var akObjectDeclRe = regexp.MustCompile(`^\s*let\s+([A-Za-z_$][\w$]*)\s*;`)

// akStringArrayRe finds the decoded payload's string-table arrays
// (each element a full quoted literal).
var akStringArrayRe = regexp.MustCompile(`(?:let|const|var)\s+[\w$]+\s*=\s*(\[\s*(?:"` + akJSStrBody + `"\s*,?\s*)*\])`)

// akJSLiteralRe scans a raw array body for its string literals.
var akJSLiteralRe = regexp.MustCompile(`"(` + akJSStrBody + `)"`)

// akCDNPathRe matches the two 32-hex segments the CDN token signs.
var akCDNPathRe = regexp.MustCompile(`(?i)/([a-f0-9]{32})/([a-f0-9]{32})/`)

// akHexRe validates hex-escape digits while decoding JS strings.
const akHexDigits = "0123456789abcdefABCDEF"

// akXORPrefix is the known plaintext of the wrapped payload (every
// obfuscated bundle's initializer begins with it): the repeating XOR
// key is recovered from this prefix, never by executing the script.
const akXORPrefix = "(function(){function "

// akReservedPercent is the set decodeURI leaves escaped (JS's
// decodeURIComponent was NOT used by the obfuscator — reserved URI
// punctuation stays percent-encoded in the payload).
const akReservedPercent = ";/?:@&=+$,#"

// akParamTTL bounds the cached player parameters (the bundle is
// versioned by its script URL; the TTL matches the AniVault-Scraper's
// megacloud key cache).
const akParamTTL = 15 * time.Minute

// akPlayerParams is the megaplay player's public crypto material: the
// AES-256-CBC key/IV decrypting the getSources enc blob, the HMAC
// secret signing the resulting CDN URL and that token's lifetime.
type akPlayerParams struct {
	key    string
	iv     string
	secret string
	ttl    int
}

// AniKoto is the anikototv.to provider (EN streaming, PR104) — a
// HiAnime/Zoro-style clone written against the live site,
// characterized 2026-09-25:
//
//   - Search: GET /filter?keyword= renders 30 server-side result cards
//     per page (#list-items .item); a junk query answers the same
//     shell with zero cards — an empty list, not an error.
//   - Episodes: the watch page carries the anime id (data-id), and
//     GET /ajax/episode/list/{id} answers the site's JSON envelope
//     {"status":200,"result":"<html>"} whose HTML lists the episodes
//     as li[data-html] anchors carrying per-episode server blobs
//     (data-ids, base64), MAL id and timestamps.
//   - Dubs: GET /ajax/server/list?servers={data-ids} answers the same
//     envelope with two server groups — SUB and DUB (data-type) — each
//     holding ~3 named servers (Vidstream-2, HD-1, HD-2) as
//     li[data-link-id] rows. The listing fans out lazily: episodes
//     arrive with empty embeds and the session hydrates them through
//     the DubsHydrator capability (the kickassanime pattern).
//   - Streams: GET /ajax/server?get={link-id} resolves a server to
//     {"result":{"url":…,"skip_data":…}} where url is a megaplay.buzz
//     player page. The player's e1 bundle is XOR-obfuscated; the
//     provider statically unpacks its string table (no JS execution),
//     reads the AES key/IV + HMAC secret, decrypts the getSources enc
//     blob and HMAC-signs the resulting CDN URL — the exact chain
//     verified live 2026-09-25 against Black Lagoon: The Second
//     Barrage (playable 1080/720/480 master manifest).
//
// Known walls, typed per the no-silent-failure policy:
//
//   - The site answers malformed AJAX with HTTP 200 and the error
//     INSIDE the envelope ({"status":500,"result":"Bad request"} — the
//     /api/search path the controller probed is such a decoy: it
//     rejects every parameter). The provider surfaces the envelope
//     status, never the HTTP one.
//   - The skip_data (intro/outro ranges) the resolver hands out has no
//     slot in the stream contract — documented, out of scope.
type AniKoto struct {
	Base
	// mu guards the player-parameter cache.
	mu sync.Mutex
	// params cache: keyed by the script URL that produced it, valid
	// for akParamTTL.
	params     *akPlayerParams
	paramsFrom string
	paramsAt   time.Time
}

// newAniKoto builds the provider against baseURL. anikototv.to
// answers plain client requests (verified live through the netclient
// fingerprint), so the shared netclient is kept: per-provider cookie
// jar (the AJAX endpoints tolerate the anonymous session the watch
// page establishes), status mapping and the CF ladder wiring.
func newAniKoto(baseURL string, http *netclient.Client) *AniKoto {
	return &AniKoto{
		Base: Base{
			id:          "anikoto",
			name:        "AniKoto",
			baseURL:     baseURL,
			sourceType:  contracts.SourceTypeBoth,
			contentLang: "en",
			http:        http,
		},
	}
}

// NamePreference implements contracts.NamePreferenceProvider: the
// catalog indexes romaji/English titles only — a Cyrillic query is
// guaranteed-zero.
func (p *AniKoto) NamePreference() contracts.NamePreference { return contracts.NamePrefLatin }

// Search fetches the /filter page and scrapes the result cards
// [LIVE-VERIFIED 2026-09-25: GET keyword=black lagoon → HTTP 200, 30
// rendered cards; junk query → 200 with an empty #list-items].
func (p *AniKoto) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	form := url.Values{"keyword": {query}}
	resp, err := p.http.Get(ctx, p.baseURL+"/filter?"+form.Encode(), nil)
	if err != nil {
		return nil, err
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(resp.Body)))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, resp.StatusCode,
			fmt.Errorf("parse search page: %w", err))
	}

	results := make([]contracts.SearchResult, 0, 30)
	doc.Find("#list-items .item").Each(func(_ int, card *goquery.Selection) {
		href, _ := card.Find("a[href]").First().Attr("href")
		title := strings.TrimSpace(card.Find(".info .b1 a.name.d-title").First().Text())
		if href == "" || title == "" {
			return
		}

		poster := ""
		if img := card.Find(".ani.poster.tip img[src]").First(); img.Length() > 0 {
			if src, ok := img.Attr("src"); ok {
				poster = normalizeProtocolURL(src)
			}
		}

		results = append(results, contracts.SearchResult{
			Title:    title,
			URL:      akWatchSuffixRe.ReplaceAllString(href, ""),
			SourceID: p.ID(),
			Poster:   poster,
		})
	})
	return results, nil
}

// GetEpisodes lists the episode table for the anime at animeURL
// [LIVE-VERIFIED 2026-09-25 on a 12-episode series]. The watch page's
// data-id feeds the AJAX episode list; every li[data-html] anchor
// becomes one episode whose RawID composes
// "{ep data-id}:{data-ids}" — everything FetchDubs and the dub menu
// need without a second page fetch. The dub list itself hydrates
// lazily (see FetchDubs).
func (p *AniKoto) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	resp, err := p.http.Get(ctx, animeURL, nil)
	if err != nil {
		return nil, err
	}
	m := akDataIDRe.FindSubmatch(resp.Body)
	if m == nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("watch page carries no anime data-id: %w", contracts.ErrNotFound))
	}

	result, err := p.akAJAX(ctx, contracts.OpGetEpisodes,
		p.baseURL+"/ajax/episode/list/"+string(m[1])+"?vrf=", p.baseURL+"/")
	if err != nil {
		return nil, err
	}
	var html string
	if err := json.Unmarshal(result, &html); err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
			fmt.Errorf("episode list result is not HTML: %w", err))
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
			fmt.Errorf("parse episode list: %w", err))
	}

	episodes := make([]contracts.Episode, 0, 12)
	doc.Find("li[data-html]").Each(func(_ int, li *goquery.Selection) {
		a := li.Find("a[data-id]").First()
		num, _ := a.Attr("data-num")
		epID, _ := a.Attr("data-id")
		ids, _ := a.Attr("data-ids")
		if num == "" || epID == "" || ids == "" {
			return
		}
		episodes = append(episodes, contracts.Episode{
			Num:   num,
			Title: strings.TrimSpace(li.AttrOr("title", "")),
			RawID: epID + ":" + ids,
		})
	})
	if len(episodes) == 0 {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
			fmt.Errorf("episode list carries no episode anchors: %w", contracts.ErrNotFound))
	}
	return episodes, nil
}

// FetchDubs implements contracts.DubsHydrator (the session hydrates
// eagerly per episode). The episode's data-ids blob feeds the server
// list [LIVE-VERIFIED 2026-09-25: SUB and DUB groups with three
// servers each]; every link-id becomes a value under its group so
// ResolveStream can walk a group's servers in order.
func (p *AniKoto) FetchDubs(ctx context.Context, episode *contracts.Episode) (*contracts.Episode, error) {
	_, ids, found := strings.Cut(episode.RawID, ":")
	if !found || ids == "" {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
			fmt.Errorf("%w: episode RawID %q is not {ep-id}:{data-ids}", contracts.ErrInvalidInput, episode.RawID))
	}

	result, err := p.akAJAX(ctx, contracts.OpGetEpisodes,
		p.baseURL+"/ajax/server/list?servers="+url.QueryEscape(ids), p.baseURL+"/")
	if err != nil {
		return nil, err
	}
	var html string
	if err := json.Unmarshal(result, &html); err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
			fmt.Errorf("server list result is not HTML: %w", err))
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
			fmt.Errorf("parse server list: %w", err))
	}

	embeds := map[string][]string{}
	doc.Find(".servers .type[data-type]").Each(func(_ int, group *goquery.Selection) {
		dub := strings.ToUpper(strings.TrimSpace(group.AttrOr("data-type", "")))
		if dub != akDubSub && dub != akDubDub {
			return
		}
		group.Find("li[data-link-id]").Each(func(_ int, li *goquery.Selection) {
			linkID := strings.TrimSpace(li.AttrOr("data-link-id", ""))
			if linkID == "" {
				return
			}
			embeds[dub] = append(embeds[dub], linkID)
		})
	})
	if len(embeds) == 0 {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
			fmt.Errorf("server list carries no SUB/DUB groups: %w", contracts.ErrExtractFailed))
	}

	episode.RawEmbeds = embeds
	return episode, nil
}

// ResolveStream resolves one dub's servers: each link-id walks the
// chain server resolver → megaplay page → player bundle → getSources →
// master playlist, stopping at the first server that yields variants.
// A dub the episode does not carry is a caller bug (typed
// ErrInvalidInput); every server failing is the typed extract wall
// with the first server's cause attached.
func (p *AniKoto) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	stream := contracts.MediaStream{
		DubName: dubID,
		Links:   map[string]contracts.VideoSource{},
	}

	linkIDs, ok := episode.RawEmbeds[dubID]
	if !ok || len(linkIDs) == 0 {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
			fmt.Errorf("%w: episode %s carries no dub %q", contracts.ErrInvalidInput, episode.Num, dubID))
	}

	var firstErr error
	for _, linkID := range linkIDs {
		result, err := p.akAJAX(ctx, contracts.OpResolveStream,
			p.baseURL+"/ajax/server?get="+url.QueryEscape(linkID), p.baseURL+"/")
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		var payload struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(result, &payload); err != nil || payload.URL == "" {
			if firstErr == nil {
				if err == nil {
					err = fmt.Errorf("stream resolver returned no url")
				}
				firstErr = err
			}
			continue
		}

		links, err := p.akResolveMegaplay(ctx, payload.URL)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		stream.Links = links
		return stream, nil
	}

	if firstErr == nil {
		firstErr = contracts.ErrExtractFailed
	}
	return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
		fmt.Errorf("%w: episode %s dub %q: %d server(s) dead: %w",
			contracts.ErrExtractFailed, episode.Num, dubID, len(linkIDs), firstErr))
}

// akResolveMegaplay walks the megaplay embed chain [LIVE-VERIFIED
// 2026-09-25]: the embed page carries the player div (data-id) and the
// e1 player bundle; the bundle's static unpack yields the AES
// key/IV + HMAC secret; getSources answers an enc blob decrypting to
// the CDN manifest URL, which must carry the HMAC token. The manifest
// splits into per-quality variant links (Referer = the megaplay
// origin — the CDN checks it on playback).
func (p *AniKoto) akResolveMegaplay(ctx context.Context, embedURL string) (map[string]contracts.VideoSource, error) {
	parsed, err := url.Parse(embedURL)
	if err != nil {
		return nil, fmt.Errorf("%w: embed url %q: %w", contracts.ErrExtractFailed, embedURL, err)
	}
	origin := parsed.Scheme + "://" + parsed.Host
	originReferer := origin + "/"

	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     embedURL,
		Headers: map[string]string{"Referer": p.baseURL + "/"},
		Op:      contracts.OpResolveStream,
	})
	if err != nil {
		return nil, err
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(resp.Body)))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, resp.StatusCode,
			fmt.Errorf("parse embed page: %w", err))
	}
	player := doc.Find("#megaplay-player[data-id]").First()
	dataID, ok := player.Attr("data-id")
	if !ok || dataID == "" {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, resp.StatusCode,
			fmt.Errorf("%w: embed page carries no #megaplay-player data-id", contracts.ErrExtractFailed))
	}
	scriptSrc := ""
	doc.Find("script[src]").Each(func(_ int, s *goquery.Selection) {
		if scriptSrc == "" && strings.Contains(s.AttrOr("src", ""), "e1-player") {
			scriptSrc = s.AttrOr("src", "")
		}
	})
	if scriptSrc == "" {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, resp.StatusCode,
			fmt.Errorf("%w: embed page carries no e1-player bundle", contracts.ErrExtractFailed))
	}
	scriptRef, err := url.Parse(scriptSrc)
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
			fmt.Errorf("player script src %q: %w", scriptSrc, err))
	}
	scriptURL := parsed.ResolveReference(scriptRef).String()

	params, err := p.akParamsFor(ctx, scriptURL, originReferer)
	if err != nil {
		return nil, err
	}

	xhr := map[string]string{
		"X-Requested-With": "XMLHttpRequest",
		"Referer":          originReferer,
	}
	sources, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     origin + "/stream/getSourcesNew?id=" + url.QueryEscape(dataID),
		Headers: xhr,
		Op:      contracts.OpResolveStream,
	})
	if err != nil {
		return nil, err
	}
	var payload struct {
		Enc string `json:"enc"`
	}
	if err := json.Unmarshal(sources.Body, &payload); err != nil || payload.Enc == "" {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, sources.StatusCode,
			fmt.Errorf("%w: getSources returned no enc blob", contracts.ErrExtractFailed))
	}
	file, err := akDecryptSource(payload.Enc, params.key, params.iv)
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
			fmt.Errorf("%w: decrypt sources: %w", contracts.ErrExtractFailed, err))
	}

	manifest := akSignedURL(file, params.secret, params.ttl, time.Now())
	playlist, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     manifest,
		Headers: map[string]string{"Referer": originReferer},
		Op:      contracts.OpResolveStream,
	})
	if err != nil {
		return nil, err
	}

	if variants, isVariant := parseMasterPlaylist(string(playlist.Body), manifest); isVariant {
		links := make(map[string]contracts.VideoSource, len(variants))
		for _, v := range variants {
			links[v.height] = contracts.VideoSource{
				URL:     v.uri,
				Quality: v.height,
				Type:    "m3u8",
				Headers: map[string]string{"Referer": originReferer},
			}
		}
		return links, nil
	}
	return map[string]contracts.VideoSource{
		"1080": {
			URL:     manifest,
			Quality: "1080",
			Type:    "m3u8",
			Headers: map[string]string{"Referer": originReferer},
		},
	}, nil
}

// akParamsFor returns the player crypto parameters for the bundle at
// scriptURL, fetching and unpacking it on cache miss (the parameters
// are pinned to the script version, so the cache keys on the URL).
func (p *AniKoto) akParamsFor(ctx context.Context, scriptURL, referer string) (*akPlayerParams, error) {
	p.mu.Lock()
	if p.params != nil && p.paramsFrom == scriptURL && time.Since(p.paramsAt) < akParamTTL {
		params := p.params
		p.mu.Unlock()
		return params, nil
	}
	p.mu.Unlock()

	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     scriptURL,
		Headers: map[string]string{"Referer": referer},
		Op:      contracts.OpResolveStream,
	})
	if err != nil {
		return nil, err
	}
	params, err := akPlayerParameters(string(resp.Body))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, resp.StatusCode, err)
	}

	p.mu.Lock()
	p.params = params
	p.paramsFrom = scriptURL
	p.paramsAt = time.Now()
	p.mu.Unlock()
	return params, nil
}

// akAJAX performs one XMLHttpRequest against the site and unwraps the
// {"status":N,"result":...} envelope. The site answers malformed calls
// with HTTP 200 and the error INSIDE the envelope
// ({"status":500,"result":"Bad request"} — live-verified): a status
// ≥ 400 surfaces as the typed extract wall quoting both.
func (p *AniKoto) akAJAX(ctx context.Context, op, url, referer string) (json.RawMessage, error) {
	resp, err := p.http.Do(ctx, netclient.Request{
		Method: "GET",
		URL:    url,
		Headers: map[string]string{
			"X-Requested-With": "XMLHttpRequest",
			"Referer":          referer,
		},
		Op: op,
	})
	if err != nil {
		return nil, err
	}

	var env struct {
		Status int             `json:"status"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return nil, contracts.WrapProvider(p.ID(), op, resp.StatusCode,
			fmt.Errorf("%w: ajax %s answered non-JSON: %w", contracts.ErrExtractFailed, url, err))
	}
	if env.Status >= 400 {
		msg := strings.TrimSpace(string(env.Result))
		return nil, contracts.WrapProvider(p.ID(), op, env.Status,
			fmt.Errorf("%w: ajax %s answered envelope status %d: %s",
				contracts.ErrExtractFailed, url, env.Status, msg))
	}
	return env.Result, nil
}

// akPlayerParameters recovers the player's crypto parameters from the
// bundle source: a plaintext bundle (AES-CBC already visible) is read
// directly, an obfuscated one is statically unpacked first — the XOR
// key comes from the known payload prefix and the string table is
// identified by its AES-CBC member, so no JavaScript ever executes.
func akPlayerParameters(script string) (*akPlayerParams, error) {
	if !strings.Contains(script, "AES-CBC") {
		unpacked, err := akUnpackPlayerStrings(script)
		if err != nil {
			return nil, fmt.Errorf("%w: player bundle: %w", contracts.ErrExtractFailed, err)
		}
		script = unpacked
	}

	for _, loc := range akPlayerParamRe.FindAllStringSubmatchIndex(script, -1) {
		nearby := script[loc[1]:]
		if len(nearby) > 12000 {
			nearby = nearby[:12000]
		}
		if !strings.Contains(nearby, "AES-CBC") || !strings.Contains(nearby, "HMAC") {
			continue
		}
		key, err := akJSString(string(script[loc[2]:loc[3]]))
		if err != nil {
			continue
		}
		iv, err := akJSString(string(script[loc[4]:loc[5]]))
		if err != nil {
			continue
		}
		secret, err := akJSString(string(script[loc[6]:loc[7]]))
		if err != nil {
			continue
		}
		ttl, err := strconv.Atoi(script[loc[8]:loc[9]])
		if err != nil {
			continue
		}
		return &akPlayerParams{key: key, iv: iv, secret: secret, ttl: ttl}, nil
	}
	return nil, fmt.Errorf("%w: player crypto parameters changed", contracts.ErrExtractFailed)
}

// akUnpackPlayerStrings statically decodes the player's XOR wrapper
// and string table (the AniVault reference implementation's algorithm,
// never executing anything): recover the repeating XOR key from the
// known payload prefix, decode the initializer, find the string-table
// array whose entries XOR-mask onto the crypto identifiers, and
// substitute the lookups back into the bundle source.
func akUnpackPlayerStrings(script string) (string, error) {
	wrapperMatch := akReturnEvalRe.FindStringSubmatch(script)
	objectMatch := akObjectDeclRe.FindStringSubmatch(script)
	if wrapperMatch == nil || objectMatch == nil {
		return "", fmt.Errorf("%w: unrecognized obfuscated player wrapper", contracts.ErrExtractFailed)
	}
	wrapper, err := akJSString(wrapperMatch[1])
	if err != nil {
		return "", err
	}
	payloadMatch := akPayloadTailRe.FindStringSubmatch(wrapper)
	if payloadMatch == nil {
		return "", fmt.Errorf("%w: obfuscated player payload missing", contracts.ErrExtractFailed)
	}
	payload, err := akJSString(payloadMatch[1])
	if err != nil {
		return "", err
	}
	payload = akDecodeURI(payload)

	// XOR key recovery from the known prefix: the key repeats with a
	// period that must divide the whole derived key stream.
	known := make([]int, len(akXORPrefix))
	for i := range len(akXORPrefix) {
		known[i] = int(payload[i]) ^ int(akXORPrefix[i])
	}
	var decoded string
	found := false
	for size := 1; size <= len(known)/2; size++ {
		periodic := true
		for i, v := range known {
			if v != known[i%size] {
				periodic = false
				break
			}
		}
		if !periodic {
			continue
		}
		buf := make([]byte, len(payload))
		for i := range payload {
			buf[i] = byte(known[i%size]&0xFF) ^ payload[i]
		}
		candidate := string(buf)
		if strings.HasPrefix(candidate, akXORPrefix) && strings.HasSuffix(strings.TrimRight(candidate, " \t\r\n"), "})") {
			decoded = candidate
			found = true
			break
		}
	}
	if !found {
		return "", fmt.Errorf("%w: player XOR wrapper format changed", contracts.ErrExtractFailed)
	}

	// Locate the string table: an array whose 7-char entries XOR-mask
	// onto "AES-CBC" and whose unmasked siblings cover the WebCrypto
	// identifiers.
	var table []string
	for _, array := range akStringArrayRe.FindAllStringSubmatch(decoded, -1) {
		entries := make([]string, 0, 32)
		for _, lit := range akJSLiteralRe.FindAllStringSubmatch(array[1], -1) {
			v, err := akJSString(lit[1])
			if err != nil {
				continue
			}
			entries = append(entries, v)
		}
		for _, entry := range entries {
			if len(entry) != len("AES-CBC") {
				continue
			}
			mask := entry[0] ^ 'A'
			unmasked := make([]byte, len(entry))
			for i := range len(entry) {
				unmasked[i] = entry[i] ^ mask
			}
			if string(unmasked) != "AES-CBC" {
				continue
			}
			candidate := make([]string, len(entries))
			for i, v := range entries {
				buf := make([]byte, len(v))
				for j := range len(v) {
					buf[j] = v[j] ^ mask
				}
				candidate[i] = string(buf)
			}
			if akAllPresent(candidate, "HMAC", "SHA-256", "importKey", "decrypt") {
				table = candidate
				break
			}
		}
		if table != nil {
			break
		}
	}
	if table == nil {
		return "", fmt.Errorf("%w: player crypto string table could not be decoded", contracts.ErrExtractFailed)
	}

	// Substitute the object's table lookups — ONLY the declared
	// obfuscation object's `name.fn(N)` calls, exactly like the
	// reference implementation pins them.
	lookupRe, err := regexp.Compile(`\b` + regexp.QuoteMeta(objectMatch[1]) + `\.[\w$]+\((\d+)\)`)
	if err != nil {
		return "", fmt.Errorf("%w: object name %q: %w", contracts.ErrExtractFailed, objectMatch[1], err)
	}
	return lookupRe.ReplaceAllStringFunc(script, func(call string) string {
		m := lookupRe.FindStringSubmatch(call)
		idx, err := strconv.Atoi(m[1])
		if err != nil || idx >= len(table) {
			return call
		}
		quoted, err := json.Marshal(table[idx])
		if err != nil {
			return call
		}
		return strings.Replace(call, m[0], string(quoted), 1)
	}), nil
}

// akAllPresent reports whether every needle is in the table.
func akAllPresent(table []string, needles ...string) bool {
	for _, n := range needles {
		ok := false
		for _, v := range table {
			if v == n {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// akJSString decodes a double-quoted JS string literal's CONTENT as
// data: \xNN and \uNNNN escapes, JS-only escape sequences, and the
// obfuscator's literal \r\n pair (dropped). The input excludes the
// surrounding quotes. The error return keeps the call sites uniform
// (a malformed literal contributes nothing rather than panicking).
func akJSString(content string) (string, error) {
	return unquoteJSBody(content), nil
}

// unquoteJSBody performs the escape decoding of akJSString.
func unquoteJSBody(content string) string {
	var b strings.Builder
	b.Grow(len(content))
	for i := 0; i < len(content); i++ {
		c := content[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(content) {
			break
		}
		switch e := content[i]; {
		case e == 'x' && i+2 < len(content) && akIsHex(content[i+1]) && akIsHex(content[i+2]):
			v, _ := strconv.ParseUint(content[i+1:i+3], 16, 8)
			b.WriteByte(byte(v))
			i += 2
		case e == 'u' && i+4 < len(content) && akIsHex(content[i+1]) && akIsHex(content[i+2]) && akIsHex(content[i+3]) && akIsHex(content[i+4]):
			v, _ := strconv.ParseUint(content[i+1:i+5], 16, 16)
			b.WriteRune(rune(v))
			i += 4
		case e == '\r' && i+1 < len(content) && content[i+1] == '\n':
			i++
		case e == '\n' || e == '\r':
			// line continuation
		default:
			switch e {
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'v':
				b.WriteByte('\v')
			case '0':
				b.WriteByte(0)
			default:
				b.WriteByte(e)
			}
		}
	}
	return b.String()
}

func akIsHex(c byte) bool { return strings.IndexByte(akHexDigits, c) >= 0 }

// akDecodeURI reproduces the obfuscator's decodeURI pass: %XX
// sequences whose decoded byte is reserved URI punctuation stay
// escaped, everything else decodes (multi-byte UTF-8 sequences pass
// through byte-exact).
func akDecodeURI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && akIsHex(s[i+1]) && akIsHex(s[i+2]) {
			v, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
			if err == nil {
				if strings.IndexByte(akReservedPercent, byte(v)) >= 0 {
					b.WriteByte(s[i])
					b.WriteByte(s[i+1])
					b.WriteByte(s[i+2])
				} else {
					b.WriteByte(byte(v))
				}
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// akDecryptSource decrypts the getSources enc blob: AES-256-CBC with
// the UTF-8 key truncated/zero-padded to 32 bytes (the JS
// TextEncoder semantics), zero-padded 16-byte IV, URL-safe base64 and
// PKCS7. The decrypted JSON carries the manifest URL either as
// {"file": …} or as a list of such objects.
func akDecryptSource(encoded, key, iv string) (string, error) {
	kb := make([]byte, 32)
	copy(kb, key)
	ib := make([]byte, 16)
	copy(ib, iv)

	if pad := len(encoded) % 4; pad != 0 {
		encoded += strings.Repeat("=", 4-pad)
	}
	ct, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decode enc: %w", err)
	}
	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return "", fmt.Errorf("ciphertext length %d is not a block multiple", len(ct))
	}
	block, err := aes.NewCipher(kb)
	if err != nil {
		return "", err
	}
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, ib).CryptBlocks(pt, ct)

	n := int(pt[len(pt)-1])
	if n == 0 || n > aes.BlockSize || n > len(pt) {
		return "", fmt.Errorf("invalid PKCS7 padding")
	}
	for _, b := range pt[len(pt)-n:] {
		if int(b) != n {
			return "", fmt.Errorf("invalid PKCS7 padding bytes")
		}
	}

	var single struct {
		File string `json:"file"`
	}
	if err := json.Unmarshal(pt[:len(pt)-n], &single); err == nil && single.File != "" {
		return single.File, nil
	}
	var list []map[string]any
	if err := json.Unmarshal(pt[:len(pt)-n], &list); err == nil {
		for _, item := range list {
			if f, ok := item["file"].(string); ok && f != "" {
				return f, nil
			}
		}
	}
	return "", fmt.Errorf("decrypted sources carry no file")
}

// akSignedURL reproduces the player's CDN URL signing: the two 32-hex
// path segments form the message "{expires}|{h1}/{h2}" (lowercase,
// expires = now + ttl), HMAC-SHA256'd with the player secret; the
// token is b64url(message) + "." + b64url(mac). URLs already carrying
// a token — and URLs without the hex segments — pass through
// unchanged.
func akSignedURL(rawURL, secret string, ttl int, now time.Time) string {
	if strings.Contains(rawURL, "token=") {
		return rawURL
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	m := akCDNPathRe.FindStringSubmatch(parsed.Path)
	if m == nil {
		return rawURL
	}
	message := fmt.Sprintf("%d|%s/%s", now.Unix()+int64(ttl), strings.ToLower(m[1]), strings.ToLower(m[2]))
	sep := "&"
	if !strings.Contains(rawURL, "?") {
		sep = "?"
	}
	return rawURL + sep + "token=" + akTokenMessageB64(message) + "." + akTokenSignatureB64(secret, message)
}

// akTokenMessageB64 encodes the signing message exactly as the player
// does (URL-safe base64, no padding).
func akTokenMessageB64(message string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(message))
}

// akTokenSignatureB64 HMACs the raw message bytes with the secret and
// encodes the MAC the same way.
func akTokenSignatureB64(secret, message string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(message))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// akFirstEpisodeRef extracts the first episode anchor of an episodes
// result page (test helper reading the real fixture the way
// GetEpisodes parses it).
func akFirstEpisodeRef(html string) (epID, ids string, ok bool) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return "", "", false
	}
	a := doc.Find("li[data-html] a[data-id]").First()
	epID = a.AttrOr("data-id", "")
	ids = a.AttrOr("data-ids", "")
	return epID, ids, epID != "" && ids != ""
}
