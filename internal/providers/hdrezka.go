package providers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// HDRezkaBase is the site root [LIVE-VERIFIED 2026-09-19].
const HDRezkaBase = "https://hdrezka-home.tv"

// hdrezkaAnubisMarker detects the Anubis proof-of-work challenge page:
// every path of hdrezka-home.tv is fronted by Anubis 1.25.0
// (TecharoHQ), so the first request of any operation (and any later
// one after the auth cookie expires) is answered with this page
// instead of the real content. The upstream anicli-api reference has
// no anti-bot handling and CRASHES against the live site (its httpx
// client receives the challenge for the anime page and the init-script
// selector IndexErrors); this port solves the gate in pure Go.
const hdrezkaAnubisMarker = `<script id="anubis_challenge"`

// hdrezkaMaxPoWIterations bounds the proof-of-work search. The
// observed difficulty is 2 (leading zero hex digits — ~10² hashes);
// difficulty 8 would still fit the cap (~4·10⁹ is out, 1.6·10⁷ hashes
// ≈ seconds). Anything beyond is treated as a hostile gate.
const hdrezkaMaxPoWIterations = 1 << 24

// hdrezkaAnubisChallengeRe extracts the challenge JSON blob.
var hdrezkaAnubisChallengeRe = regexp.MustCompile(
	`(?s)<script id="anubis_challenge" type="application/json">(.*?)</script>`)

// hdrezkaInitScriptRe extracts id and default translator_id from the
// player bootstrap call sof.tv.initCDN{Series,Movies}Events(id, tr, …)
// (hdrezka_parser.py:174-185).
var (
	hdrezkaInitIDRe = regexp.MustCompile(`initCDN(?:Series|Movies)Events\(\s*(\d+)`)
	hdrezkaInitTrRe = regexp.MustCompile(`initCDN(?:Series|Movies)Events\(\s*\d+\s*,\s*(\d+)`)
	// hdrezkaQualityRe ports the quality label scan "\[.*?(\d+).*?\]"
	// — the first number inside the "[1080p Ultra]" prefix.
	hdrezkaQualityRe = regexp.MustCompile(`\[.*?(\d+).*?\]`)
)

// HDRezka is the hdrezka-home.tv provider (RU anime section of the
// rezka catalog). Port of the frozen anicli-api hdrezka source
// (anicli_api/source/hdrezka.py + parsers/hdrezka_parser.py)
// re-verified live 2026-09-19, plus the pure-Go Anubis proof-of-work
// ladder the site added on top of every path — see hdrezkaAnubisMarker.
// Dubs are the site's translators (одноголосые included); the player
// is hdrezka's own CDN (up to 1080).
type HDRezka struct {
	Base
}

// newHDRezka builds the provider against baseURL. The site answers
// plain desktop-UA requests behind the Anubis gate, so no extra
// headers are wired here.
func newHDRezka(baseURL string, http *netclient.Client) *HDRezka {
	return &HDRezka{Base: Base{
		id:          "hdrezka",
		name:        "HDRezka",
		baseURL:     baseURL,
		sourceType:  contracts.SourceTypeBoth,
		contentLang: "ru",
		http:        http,
	}}
}

// hdrezkaStreamPayload is the per-(episode, translator) request the
// resolve step replays as the get_cdn_series form. It rides inside
// Episode.RawEmbeds as JSON (the animevost pattern) — the extra
// page_url field never reaches the wire, it only restores the Referer
// the site's own player sends.
type hdrezkaStreamPayload struct {
	ID           string `json:"id"`
	TranslatorID string `json:"translator_id"`
	Season       string `json:"season,omitempty"`
	Episode      string `json:"episode,omitempty"`
	Favs         string `json:"favs"`
	Action       string `json:"action"`
	PageURL      string `json:"page_url,omitempty"`
}

// decodeHDRezkaStreamPayload decodes the RawEmbeds payload.
func decodeHDRezkaStreamPayload(raw string) (hdrezkaStreamPayload, error) {
	var payload hdrezkaStreamPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return payload, fmt.Errorf("decode stream payload: %w", err)
	}
	return payload, nil
}

// hdrezkaRequest carries the parts of one outgoing request so the
// Anubis ladder can replay it with a fresh body reader after the gate
// clears (an io.Reader is consumed by the first attempt).
type hdrezkaRequest struct {
	method  string
	url     string
	headers map[string]string
	body    string
	op      string
}

func (r hdrezkaRequest) withBody() netclient.Request {
	req := netclient.Request{
		Method:  r.method,
		URL:     r.url,
		Headers: r.headers,
		Op:      r.op,
	}
	if r.body != "" {
		req.Body = strings.NewReader(r.body)
	}
	return req
}

// do issues the request through the provider client; an Anubis
// challenge answer is solved (sha256 proof-of-work + pass-challenge
// round-trip, cookies carried by the provider jar) and the ORIGINAL
// request is retried exactly once. A still-challenged retry fails loud
// with ErrProvider403 — the gate refused us even after an honest solve.
func (p *HDRezka) do(ctx context.Context, r hdrezkaRequest) (*netclient.Response, error) {
	resp, err := p.http.Do(ctx, r.withBody())
	if err != nil {
		return nil, err
	}
	if !bytes.Contains(resp.Body, []byte(hdrezkaAnubisMarker)) {
		return resp, nil
	}
	if err := p.passAnubisGate(ctx, resp.Body, r.url); err != nil {
		return nil, contracts.WrapProvider(p.ID(), r.op, resp.StatusCode, err)
	}
	retry, err := p.http.Do(ctx, r.withBody())
	if err != nil {
		return nil, err
	}
	if bytes.Contains(retry.Body, []byte(hdrezkaAnubisMarker)) {
		return nil, contracts.WrapProvider(p.ID(), r.op, retry.StatusCode,
			fmt.Errorf("%w: anubis gate did not clear after a valid proof-of-work solve",
				contracts.ErrProvider403))
	}
	return retry, nil
}

// hdrezkaAnubisChallenge mirrors the anubis_challenge JSON the gate
// embeds in the page.
type hdrezkaAnubisChallenge struct {
	Rules struct {
		Algorithm  string `json:"algorithm"`
		Difficulty int    `json:"difficulty"`
	} `json:"rules"`
	Challenge struct {
		ID         string `json:"id"`
		RandomData string `json:"randomData"`
		Method     string `json:"method"`
	} `json:"challenge"`
}

// solveHDRezkaAnubis ports the worker contract of Anubis 1.25
// (sha256-purejs.mjs + lib/challenge/proofofwork): the nonce N makes
// hex(sha256(randomData + strconv.Itoa(N))) start with `difficulty`
// zero hex digits. "fast" and "slow" share the same sha256 validation
// server-side (both register the same Impl); anything else fails loud.
func solveHDRezkaAnubis(challenge hdrezkaAnubisChallenge) (int, string, error) {
	switch challenge.Rules.Algorithm {
	case "fast", "slow":
		// same sha256 proof-of-work on both (anubis lib/challenge:
		// chall.Register("fast"/"slow", same Impl))
	default:
		return 0, "", fmt.Errorf("%w: unsupported anubis algorithm %q",
			contracts.ErrProvider403, challenge.Rules.Algorithm)
	}
	prefix := strings.Repeat("0", challenge.Rules.Difficulty)
	for nonce := 0; nonce <= hdrezkaMaxPoWIterations; nonce++ {
		sum := sha256.Sum256([]byte(challenge.Challenge.RandomData + strconv.Itoa(nonce)))
		digest := hex.EncodeToString(sum[:])
		if strings.HasPrefix(digest, prefix) {
			return nonce, digest, nil
		}
	}
	return 0, "", fmt.Errorf("%w: anubis proof-of-work exceeded %d iterations (difficulty %d)",
		contracts.ErrProvider403, hdrezkaMaxPoWIterations, challenge.Rules.Difficulty)
}

// passAnubisGate solves the challenge and completes the pass-challenge
// round-trip; the auth cookie lands in the provider's cookie jar.
// redir must be a same-host parseable URI (the server rejects anything
// else) — the original absolute URL satisfies both. elapsedTime is an
// honest measurement of the solve; the server only logs it.
func (p *HDRezka) passAnubisGate(ctx context.Context, body []byte, originalURL string) error {
	m := hdrezkaAnubisChallengeRe.FindSubmatch(body)
	if m == nil {
		return fmt.Errorf("%w: anubis challenge page without challenge JSON", contracts.ErrProvider403)
	}
	var challenge hdrezkaAnubisChallenge
	if err := json.Unmarshal(m[1], &challenge); err != nil {
		return fmt.Errorf("%w: decode anubis challenge: %w", contracts.ErrProvider403, err)
	}
	start := time.Now()
	nonce, digest, err := solveHDRezkaAnubis(challenge)
	if err != nil {
		return err
	}
	passURL := fmt.Sprintf("%s/.within.website/x/cmd/anubis/api/pass-challenge?id=%s&response=%s&nonce=%d&redir=%s&elapsedTime=%d",
		p.baseURL,
		challenge.Challenge.ID,
		digest,
		nonce,
		originalURL,
		time.Since(start).Milliseconds(),
	)
	_, err = p.http.Do(ctx, netclient.Request{
		Method: "GET",
		URL:    passURL,
		Op:     "anubis_gate",
		Headers: map[string]string{
			"Referer": originalURL,
		},
	})
	if err != nil {
		return fmt.Errorf("%w: anubis pass-challenge request: %w", contracts.ErrProvider403, err)
	}
	return nil
}

// hdrezkaPage is the parsed anime page: everything the episode/dub
// generation needs (hdrezka_parser.py PageAnimeType, minus season_box
// which the reference parses but never consumes).
type hdrezkaPage struct {
	title      string
	poster     string
	favs       string
	id         string
	translID   string
	translList []hdrezkaTranslator
	episodes   []hdrezkaEpisodeItem
}

type hdrezkaTranslator struct {
	title string
	id    string
}

type hdrezkaEpisodeItem struct {
	id        string
	seasonID  string
	episodeID string
	title     string
}

// parseHDRezkaPage scrapes an /animation/ page [LIVE-VERIFIED].
func parseHDRezkaPage(body []byte) (*hdrezkaPage, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("parse anime page: %w", err)
	}
	page := &hdrezkaPage{}
	page.title = strings.TrimSpace(doc.Find(".b-post__title h1").First().Text())
	if img := doc.Find("img[data-caption-title][src]").First(); img.Length() > 0 {
		page.poster, _ = img.Attr("src")
	}
	if favs := doc.Find("input#ctrl_favs[value]").First(); favs.Length() > 0 {
		page.favs, _ = favs.Attr("value")
	}

	// The init script: the first <script> mentioning the CDN bootstrap
	// call — id and default translator_id live in its argument list.
	var initScript string
	doc.Find("script").Each(func(_ int, s *goquery.Selection) {
		if initScript != "" {
			return
		}
		text := s.Text()
		if strings.Contains(text, "sof.tv.initCDNSeriesEvents") || strings.Contains(text, "sof.tv.initCDNMoviesEvents") {
			initScript = text
		}
	})
	if initScript != "" {
		if mm := hdrezkaInitIDRe.FindStringSubmatch(initScript); mm != nil {
			page.id = mm[1]
		}
		if mm := hdrezkaInitTrRe.FindStringSubmatch(initScript); mm != nil {
			page.translID = mm[1]
		}
	}

	doc.Find("#translators-list > li.b-translator__item").Each(func(_ int, li *goquery.Selection) {
		title, _ := li.Attr("title")
		id, _ := li.Attr("data-translator_id")
		page.translList = append(page.translList, hdrezkaTranslator{title: title, id: id})
	})

	doc.Find(".b-simple_episodes__list .b-simple_episode__item").Each(func(_ int, li *goquery.Selection) {
		id, _ := li.Attr("data-id")
		season, _ := li.Attr("data-season_id")
		episode, _ := li.Attr("data-episode_id")
		page.episodes = append(page.episodes, hdrezkaEpisodeItem{
			id:        id,
			seasonID:  season,
			episodeID: episode,
			title:     strings.TrimSpace(li.Text()),
		})
	})
	return page, nil
}

// dubs returns the dub set of a page: the translator list, or the
// single site default "hdrezka" when the page carries no translators
// (hdrezka.py _get_series_sources/_get_movie_sources).
func (pg *hdrezkaPage) dubs() []hdrezkaTranslator {
	if len(pg.translList) > 0 {
		return pg.translList
	}
	return []hdrezkaTranslator{{title: "hdrezka", id: pg.translID}}
}

// payloadFor builds the stream payload of one (episode, dub) pair.
// Series replays id/translator_id/season/episode/favs with
// action=get_stream; movies replay id/translator_id/favs with
// action=get_movie (hdrezka.py HdrezkaApiPayloadSeries/Movie).
func (pg *hdrezkaPage) payloadFor(animeURL string, ep hdrezkaEpisodeItem, dub hdrezkaTranslator) hdrezkaStreamPayload {
	if len(pg.episodes) == 0 {
		return hdrezkaStreamPayload{
			ID:           pg.id,
			TranslatorID: dub.id,
			Favs:         pg.favs,
			Action:       "get_movie",
			PageURL:      animeURL,
		}
	}
	return hdrezkaStreamPayload{
		ID:           ep.id,
		TranslatorID: dub.id,
		Season:       ep.seasonID,
		Episode:      ep.episodeID,
		Favs:         pg.favs,
		Action:       "get_stream",
		PageURL:      animeURL,
	}
}

// Search GETs the DLE search listing and keeps ONLY the cards whose
// data-url carries /animation/ [LIVE-VERIFIED 2026-09-19: search
// "наруто" → HTTP 200, 25 /animation/ cards; the site mixes films and
// TV series into the same listing]. Result titles are composed like
// the reference: card title + " " + span.info text (hdrezka.py
// f"{data['title']} {data['season']}") — a trailing space when the
// card carries no info line. The query is percent-encoded with pyQuote
// (spaces %20); the server does the matching.
func (p *HDRezka) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	resp, err := p.do(ctx, hdrezkaRequest{
		method: "GET",
		url:    p.baseURL + "/search/?do=search&subaction=search&q=" + pyQuote(query),
		op:     contracts.OpSearch,
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
	doc.Find(".b-content__inline_item").FilterFunction(func(_ int, item *goquery.Selection) bool {
		dataURL, ok := item.Attr("data-url")
		return ok && strings.Contains(dataURL, "/animation/")
	}).Each(func(_ int, item *goquery.Selection) {
		link := item.Find(".b-content__inline_item-link a").First()
		if link.Length() == 0 {
			return
		}
		url, _ := link.Attr("href")
		if url == "" {
			return
		}
		// Reference composition, verbatim: span.info is NOT stripped
		// (hdrezka_parser.py _parse_season returns the raw text).
		title := strings.TrimSpace(link.Text())
		season := item.Find("span.info").First().Text()
		poster := ""
		if img := item.Find("img[src]").First(); img.Length() > 0 {
			poster, _ = img.Attr("src")
		}
		results = append(results, contracts.SearchResult{
			Title:    title + " " + season,
			URL:      url,
			SourceID: p.ID(),
			Poster:   poster,
		})
	})
	return results, nil
}

// GetEpisodes scrapes the anime page [LIVE-VERIFIED 2026-09-19]:
// translator items become the dub set, SSR episode items become
// episodes carrying one payload per dub, and a page WITHOUT episode
// items is a movie — one synthetic episode whose payloads carry the
// get_movie fields (hdrezka.py Anime.get_episodes). Every payload is
// self-sufficient: ResolveStream never refetches the page.
func (p *HDRezka) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	resp, err := p.do(ctx, hdrezkaRequest{
		method: "GET",
		url:    animeURL,
		op:     contracts.OpGetEpisodes,
	})
	if err != nil {
		return nil, err
	}
	page, err := parseHDRezkaPage(resp.Body)
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode, err)
	}
	if page.favs == "" || page.id == "" {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("%w: anime page without the favs token or player id (not an /animation/ page?)",
				contracts.ErrExtractFailed))
	}

	dubs := page.dubs()
	if len(page.episodes) == 0 {
		// Movie: one synthetic episode (reference: data_id = page id,
		// season/episode 0, is_movie).
		embeds := make(map[string][]string, len(dubs))
		for _, dub := range dubs {
			payload := page.payloadFor(animeURL, hdrezkaEpisodeItem{}, dub)
			raw, err := json.Marshal(payload)
			if err != nil {
				return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
					fmt.Errorf("encode movie payload: %w", err))
			}
			embeds[dub.title] = []string{string(raw)}
		}
		return []contracts.Episode{{
			Num:       "1",
			Title:     page.title,
			RawID:     page.id,
			RawEmbeds: embeds,
		}}, nil
	}

	episodes := make([]contracts.Episode, 0, len(page.episodes))
	for _, ep := range page.episodes {
		embeds := make(map[string][]string, len(dubs))
		for _, dub := range dubs {
			payload := page.payloadFor(animeURL, ep, dub)
			raw, err := json.Marshal(payload)
			if err != nil {
				return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
					fmt.Errorf("encode payload for episode %s: %w", ep.episodeID, err))
			}
			embeds[dub.title] = []string{string(raw)}
		}
		episodes = append(episodes, contracts.Episode{
			Num:       ep.episodeID,
			Title:     ep.title,
			RawID:     ep.episodeID,
			RawEmbeds: embeds,
		})
	}
	return episodes, nil
}

// ResolveStream replays the dub's payload as the get_cdn_series POST
// [LIVE-VERIFIED wire shape via the site player JS; reference: ts in
// SECONDS minus 40]. The response url field is the comma-separated
// "[Qp (Ultra)?]URL or URL" list — parsed into quality-keyed sources
// (first URL per quality wins the map key; type mp4/m3u8 by
// extension; Referer restored from the page). success:false fails
// loud with the server message; the REAL url:false shape (the site
// serves pages but withholds stream links from stream-refusing exits
// — geo/premium decision, reproduced by the upstream reference)
// fails loud with ErrGeoBlocked where the reference CRASHES
// (bool.split).
func (p *HDRezka) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	stream := contracts.MediaStream{
		DubName: dubID,
		Links:   map[string]contracts.VideoSource{},
	}

	embeds := episode.RawEmbeds[dubID]
	if len(embeds) == 0 || embeds[0] == "" {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
			fmt.Errorf("%w: no stream payload for dub %q", contracts.ErrNotFound, dubID))
	}
	payload, err := decodeHDRezkaStreamPayload(embeds[0])
	if err != nil {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0, err)
	}

	form := url.Values{}
	form.Set("id", payload.ID)
	form.Set("translator_id", payload.TranslatorID)
	form.Set("favs", payload.Favs)
	form.Set("action", payload.Action)
	if payload.Season != "" || payload.Episode != "" {
		form.Set("season", payload.Season)
		form.Set("episode", payload.Episode)
	}

	// Reference cache-buster: int(time()-40) SECONDS (the site JS sends
	// millis; the server ignores it — the reference value is ported).
	ts := strconv.FormatInt(time.Now().Unix()-40, 10)
	resp, err := p.do(ctx, hdrezkaRequest{
		method: "POST",
		url:    p.baseURL + "/ajax/get_cdn_series/?t=" + ts,
		headers: map[string]string{
			"Accept":           "application/json, text/javascript, */*; q=0.01",
			"Content-Type":     "application/x-www-form-urlencoded; charset=UTF-8",
			"X-Requested-With": "XMLHttpRequest",
			"Origin":           p.baseURL,
			"Referer":          payload.PageURL,
		},
		body: form.Encode(),
		op:   contracts.OpResolveStream,
	})
	if err != nil {
		return stream, err
	}

	var cdn struct {
		Success bool            `json:"success"`
		Message string          `json:"message"`
		URL     json.RawMessage `json:"url"`
	}
	if err := json.Unmarshal(resp.Body, &cdn); err != nil {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, resp.StatusCode,
			fmt.Errorf("decode get_cdn_series response: %w", err))
	}
	if !cdn.Success {
		msg := cdn.Message
		if msg == "" {
			msg = "get_cdn_series refused without a message"
		}
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, resp.StatusCode,
			fmt.Errorf("%w: %s", contracts.ErrExtractFailed, msg))
	}

	var rawURLs string
	if err := json.Unmarshal(cdn.URL, &rawURLs); err != nil {
		// url:false (or null) — the site answered success but serves no
		// links: the stream-refusing-exit shape [LIVE-VERIFIED].
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, resp.StatusCode,
			fmt.Errorf("%w: site served no stream links (geo/premium refusal)", contracts.ErrGeoBlocked))
	}
	if rawURLs == "" {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, resp.StatusCode,
			fmt.Errorf("%w: site served no stream links (geo/premium refusal)", contracts.ErrGeoBlocked))
	}

	referer := payload.PageURL
	if referer == "" {
		referer = p.baseURL
	}
	for _, part := range strings.Split(rawURLs, ",") {
		qm := hdrezkaQualityRe.FindStringSubmatch(part)
		if qm == nil {
			continue
		}
		idx := strings.Index(part, "]")
		if idx < 0 || idx+1 > len(part) {
			continue
		}
		urlPart := part[idx+1:]
		// " or " alternates (hls/mp4 twins): the first URL per quality
		// wins the quality-keyed map (Go contract divergence from the
		// reference's list — documented at the type).
		url := strings.TrimSpace(strings.Split(urlPart, " or ")[0])
		if url == "" {
			continue
		}
		quality := qm[1]
		if _, exists := stream.Links[quality]; exists {
			continue
		}
		kind := "m3u8"
		if strings.HasSuffix(url, ".mp4") {
			kind = "mp4"
		}
		stream.Links[quality] = contracts.VideoSource{
			URL:     url,
			Quality: quality,
			Type:    kind,
			Headers: map[string]string{"Referer": referer},
		}
	}
	if len(stream.Links) == 0 {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, resp.StatusCode,
			fmt.Errorf("%w: no parsable stream urls in %q", contracts.ErrExtractFailed, rawURLs))
	}
	return stream, nil
}
