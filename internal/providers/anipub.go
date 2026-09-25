package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/crypto"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// AniPub production endpoints [LIVE-VERIFIED 2026-09-25, anonymous on
// every leg]. The catalog API rides the APEX host: www.anipub.xyz 301s
// onto it and api.anipub.xyz is a static GitHub Pages site (its 404
// page was the controller's «HTML instead of JSON» probe hit — no API
// lives there despite the README's wording). The Express source
// (github.com/AnimePub/AniPub backend/) pins the real routes:
//
//	GET /api/searchAll/<query>  — paged name search (≥3 chars), 20/page
//	GET /v1/api/details/<id>    — the release doc incl. the ep stream links
//	GET /api/getAll             — total catalog size
//
// The stream leg hops twice off-site: each ep link is the site's own
// /video/<n>/<sub|dub> player page whose single iframe embeds a
// megaplay.buzz stream page; that page's data-id feeds the same-origin
// /stream/getSourcesNew endpoint, whose enc payload decrypts (static
// AES-256-CBC key/iv lifted from megaplay's newclient.min.js) to the
// master.m3u8. The megaplay CDN 403s playback WITHOUT a Referer on the
// stream origin, so the source carries it.
const (
	// AniPubBase is the catalog API root (the apex host).
	AniPubBase = "https://anipub.xyz"
	// AniPubMegaplayBase is the live stream-embed origin; production
	// only — the provider follows whatever origin the video page's
	// iframe carries (megaplay's client fetches getSourcesNew
	// same-origin), so a VIDEOAPI change rides through. The constant
	// documents the probed shape and feeds no request.
	AniPubMegaplayBase = "https://megaplay.buzz"
)

// AniPub serves the anipub.xyz EN catalog (8438 releases at probe
// time). Not a port of a frozen anicli-py source (anitokyo/animiku
// precedent): written from the live API and the site's own open-source
// backend, capture-verified 2026-09-25. No credentials — the validkey
// middleware guarding /api/info calls next() on a missing key (the
// rejection branch is commented out in the source), and search plus
// /v1/api/details are unguarded outright.
//
// Content: every title carries Sub and Dub stream variants behind the
// same megaplay file id — the site's own player toggles them via the
// changeStreamType rewrite (type=(sub|dub) or the /video/<n>/<lang>
// path segment), so both dubs are emitted per episode and resolved
// through the same chain with their lang suffix.
type AniPub struct {
	Base
}

// anipubSmokeQuery is the declared live probe (PR51 mechanism): the
// shared RU probe «черная лагуна» misses this EN-only name index (the
// search is a MongoDB $regex over the Latin Name field), so the
// provider speaks for itself. «cowboy bebop» surfaces 3 releases
// (live-verified 2026-09-25), cheap for the whole-surface smoke.
const anipubSmokeQuery = "cowboy bebop"

// anipubIframeRe digs the player iframe src out of an /video player
// page. The src value is UNQUOTED in the captured markup
// (`<iframe src=https://megaplay.buzz/... ` — the first real iframe
// element; the Cloudflare bootstrap rides script, not markup).
var anipubIframeRe = regexp.MustCompile(`(?i)<iframe[^>]+src=("([^"]*)"|[^>\s]+)`)

// anipubFileIDRe digs the stream file id out of a megaplay stream page
// (data-id="104085"; realid/mediaid are other namespaces).
var anipubFileIDRe = regexp.MustCompile(`data-id="([0-9]+)"`)

// anipubTypeRe and anipubPathLangRe port the site's changeStreamType
// link grammar (app.js): the type= query form and the /sub|/dub path
// form, first match only like the JS String.replace.
var (
	anipubTypeRe     = regexp.MustCompile(`(?i)type=(sub|dub)`)
	anipubPathLangRe = regexp.MustCompile(`(?i)/(sub|dub)`)
)

// anipubChangeLang ports the site's changeStreamType (app.js): both
// stream-link spellings carry their lang — the type= query form (the
// doc-level gogo link) and the /video/<n>/<lang> path form (every
// captured ep link). No match returns the link unchanged.
func anipubChangeLang(link, lang string) string {
	if strings.Contains(link, "type=") {
		if loc := anipubTypeRe.FindStringIndex(link); loc != nil {
			return link[:loc[0]] + "type=" + lang + link[loc[1]:]
		}
		return link
	}
	if loc := anipubPathLangRe.FindStringIndex(link); loc != nil {
		return link[:loc[0]] + "/" + lang + link[loc[1]:]
	}
	return link
}

// newAniPub builds the provider against baseURL (the catalog API apex).
func newAniPub(baseURL string, http *netclient.Client) *AniPub {
	return &AniPub{Base: Base{
		id:          "anipub",
		name:        "AniPub",
		baseURL:     baseURL,
		sourceType:  contracts.SourceTypeBoth,
		contentLang: "en",
		http:        http,
	}}
}

// SmokeQuery reports the provider-specific live probe; see
// anipubSmokeQuery.
func (p *AniPub) SmokeQuery() string { return anipubSmokeQuery }

// NamePreference implements contracts.NamePreferenceProvider (PR42):
// the search index matches the Latin Name field only — Cyrillic
// queries are guaranteed-zero.
func (p *AniPub) NamePreference() contracts.NamePreference {
	return contracts.NamePrefLatin
}

// anipubSearchItem mirrors the fields the search parse consumes from
// /api/searchAll's AniData rows ( MALScore/RatingsNum/DescripTion are
// pinned in the capture but feed nothing — unknown fields dropped).
type anipubSearchItem struct {
	ID        json.Number `json:"_id"`
	Name      string      `json:"Name"`
	ImagePath string      `json:"ImagePath"`
}

// anipubSearchResponse is the searchAll envelope. The no-hit body is
// {"found":false} (same shape the ≤3-char guard returns); the hit body
// is {"currentPage":N,"AniData":[…]}.
type anipubSearchResponse struct {
	Found       *bool              `json:"found"`
	AniData     []anipubSearchItem `json:"AniData"`
	CurrentPage json.Number        `json:"currentPage"`
}

// Search queries the paged name search. The query is ONE path segment:
// url.PathEscape percent-encodes it (spaces as %20 — the form the live
// server answers; the "/" escape keeps multi-segment queries inside the
// :query param). Zero hits and short queries both arrive as
// {"found":false} — a clean empty result, not an error.
func (p *AniPub) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	searchURL := p.baseURL + "/api/searchAll/" + url.PathEscape(query)

	resp, err := p.http.Get(ctx, searchURL, nil)
	if err != nil {
		return nil, err
	}

	var parsed anipubSearchResponse
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, resp.StatusCode,
			fmt.Errorf("decode search response: %w", err))
	}

	results := make([]contracts.SearchResult, 0, len(parsed.AniData))
	for _, item := range parsed.AniData {
		results = append(results, contracts.SearchResult{
			Title:    item.Name,
			URL:      item.ID.String(),
			SourceID: p.ID(),
			Poster:   item.ImagePath,
		})
	}
	return results, nil
}

// anipubDetailsResponse mirrors /v1/api/details: the local doc with the
// ep array. Each episode carries its stream link as "src=<url>" (the
// schema's _id/name/title fields ride empty in the captures); the
// numbering is the array order. TV releases fill ep[]; MOVIES and
// specials leave ep[] EMPTY and carry the stream on the doc-level
// link field instead (live-verified: Cowboy Bebop: The Movie 1387 and
// Session XX 7152 — the README's "link": "src=URL" example shape).
type anipubDetailsResponse struct {
	Local struct {
		ID   json.Number `json:"_id"`
		Link string      `json:"link"`
		Ep   []struct {
			Link string `json:"link"`
		} `json:"ep"`
	} `json:"local"`
}

// anipubStreamRef is one parsed ep link: the player-page URL with its
// leading "src=" stripped and the numeric stream id.
type anipubStreamRef struct {
	videoURL string
	streamID string
}

// GetEpisodes fetches the release doc and expands the ep array into
// (Sub, Dub) embed rows [LIVE-VERIFIED 2026-09-25 on Cowboy Bebop (25
// eps, /sub links) and Naruto (219 eps, /dub links)]. The catalog link
// carries one lang flavor; both rows emit per the site's own player
// contract (changeStreamType toggles either spelling), the catalog
// flavor preserved on its own row. Movies and specials (empty ep[],
// doc-level link — Cowboy Bebop: The Movie shape) surface as one
// synthetic episode. The 404 unknown-id shape maps to
// contracts.ErrNotFound by the netclient status routing; a body with no
// parseable links is the typed not-found too.
func (p *AniPub) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	detailsURL := p.baseURL + "/v1/api/details/" + url.PathEscape(animeURL)

	resp, err := p.http.Get(ctx, detailsURL, nil)
	if err != nil {
		return nil, err
	}

	var parsed anipubDetailsResponse
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("decode details response: %w", err))
	}

	episodes := make([]contracts.Episode, 0, len(parsed.Local.Ep))
	for i, ep := range parsed.Local.Ep {
		ref, ok := anipubParseStreamRef(p.baseURL, ep.Link)
		if !ok {
			continue // a linkless slot contributes nothing (anitokyo precedent)
		}
		episodes = append(episodes, contracts.Episode{
			Num:   strconv.Itoa(i + 1),
			RawID: ref.streamID,
			RawEmbeds: map[string][]string{
				"Sub": {anipubChangeLang(ref.videoURL, "sub")},
				"Dub": {anipubChangeLang(ref.videoURL, "dub")},
			},
		})
	}
	if len(episodes) == 0 {
		// Movie/special shape: no ep array, the stream on the doc
		// link — one synthetic episode (live-verified 2026-09-25).
		if ref, ok := anipubParseStreamRef(p.baseURL, parsed.Local.Link); ok {
			episodes = append(episodes, contracts.Episode{
				Num:   "1",
				RawID: ref.streamID,
				RawEmbeds: map[string][]string{
					"Sub": {anipubChangeLang(ref.videoURL, "sub")},
					"Dub": {anipubChangeLang(ref.videoURL, "dub")},
				},
			})
		}
	}
	if len(episodes) == 0 {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("%w: details for %s carry no episode links", contracts.ErrNotFound, animeURL))
	}
	return episodes, nil
}

// anipubParseStreamRef strips the "src=" wrapper and pulls the numeric
// stream id off the /video/<n>/<lang> player URL. The host normalizes
// onto the provider base — the catalog data carries both the apex and
// the www host (live-verified), and both serve the identical player
// page (the anikado host-normalization precedent). "" and foreign
// shapes parse as not-ok.
func anipubParseStreamRef(baseURL, link string) (anipubStreamRef, bool) {
	raw, ok := strings.CutPrefix(link, "src=")
	if !ok || raw == "" {
		return anipubStreamRef{}, false
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return anipubStreamRef{}, false
	}
	segs := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segs) != 3 || segs[0] != "video" || segs[1] == "" || segs[2] == "" {
		return anipubStreamRef{}, false
	}
	return anipubStreamRef{videoURL: baseURL + parsed.Path, streamID: segs[1]}, true
}

// anipubSourcesResponse is the getSourcesNew envelope: the enc payload
// (base64url AES-256-CBC) plus tracks/intro/outro the player consumes
// and this provider drops.
type anipubSourcesResponse struct {
	Enc string `json:"enc"`
}

// megaplay AES-256-CBC parameters, lifted verbatim from
// megaplay.buzz/lib/newclient.min.js (SegmentDecrypt key import +
// decryptToken IV; live-verified 2026-09-25 against the enc captures —
// both decrypt to the pinned master.m3u8 URLs). The key string is
// zero-padded to the 32-byte AES-256 length exactly like the JS
// (new Uint8Array(32).set(key.subarray(0, 32))).
var (
	anipubAESKey = append([]byte("i?LMTAx0Q6,:}50U"), make([]byte, 16)...)
	anipubAESIV  = []byte("W0;27ToaUpl_P%'c")
)

// ResolveStream resolves one dub's stream through the three-hop chain:
// the /video player page, its megaplay iframe (origin followed — the
// getSourcesNew endpoint is same-origin by construction), the enc
// payload decrypt. A dub the episode does not carry is the typed
// not-found; a playerless page, a foreign lang suffix or a missing enc
// payload are the typed extract wall.
func (p *AniPub) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	stream := contracts.MediaStream{
		DubName: dubID,
		Links:   map[string]contracts.VideoSource{},
	}

	refs, ok := episode.RawEmbeds[dubID]
	if !ok || len(refs) == 0 {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
			fmt.Errorf("%w: episode %s carries no dub %q", contracts.ErrNotFound, episode.Num, dubID))
	}

	// Hop 1: the site's own player page (the catalog link's flavor).
	videoPage, err := p.http.Get(ctx, refs[0], nil)
	if err != nil {
		return stream, err
	}
	embedURL, ok := anipubIframeSrc(videoPage.Body)
	if !ok {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, videoPage.StatusCode,
			fmt.Errorf("%w: /video page has no player iframe (%s)", contracts.ErrExtractFailed, refs[0]))
	}

	// Hop 2: the megaplay stream page → the numeric file id. The lang
	// suffix decides the getSourcesNew type param.
	parsed, err := url.Parse(embedURL)
	if err != nil {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, videoPage.StatusCode,
			fmt.Errorf("parse embed url %s: %w", embedURL, err))
	}
	origin := parsed.Scheme + "://" + parsed.Host
	lang, ok := anipubLangSuffix(parsed.Path)
	if !ok {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, videoPage.StatusCode,
			fmt.Errorf("%w: embed url %s carries no sub/dub suffix", contracts.ErrExtractFailed, embedURL))
	}
	// Hop 2: the megaplay stream page → the numeric file id. The lang
	// suffix decides the getSourcesNew type param. Megaplay
	// hotlink-gates the page on the embedding site's Referer —
	// without it the page is its Error shell, no data-id
	// (live-verified 2026-09-25).
	streamPage, err := p.http.Get(ctx, embedURL, map[string]string{
		"Referer": p.baseURL + "/",
	})
	if err != nil {
		return stream, err
	}
	fileID, ok := anipubFileID(streamPage.Body)
	if !ok {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, streamPage.StatusCode,
			fmt.Errorf("%w: stream page has no data-id (%s)", contracts.ErrExtractFailed, embedURL))
	}

	// Hop 3: same-origin getSourcesNew (the client fetches it
	// relative), decrypt, done. s=bcdn is the CDN selector the player
	// appends; without it the endpoint answers track-only, no enc.
	sourcesURL := origin + "/stream/getSourcesNew?id=" + url.QueryEscape(fileID) +
		"&type=" + lang + "&s=bcdn"
	sourcesResp, err := p.http.Get(ctx, sourcesURL, map[string]string{
		"Referer":          embedURL,
		"X-Requested-With": "XMLHttpRequest",
	})
	if err != nil {
		return stream, err
	}
	var sources anipubSourcesResponse
	if err := json.Unmarshal(sourcesResp.Body, &sources); err != nil {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, sourcesResp.StatusCode,
			fmt.Errorf("decode getSourcesNew response: %w", err))
	}
	manifest, err := anipubDecryptSources(sources.Enc)
	if err != nil {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, sourcesResp.StatusCode, err)
	}

	// The CDN 403s playback without the stream-origin Referer
	// (live-verified), so it rides on the source like kickassanime's.
	stream.Links["auto"] = contracts.VideoSource{
		URL:     manifest,
		Quality: "auto",
		Type:    "m3u8",
		Headers: map[string]string{"Referer": origin + "/"},
	}
	return stream, nil
}

// anipubIframeSrc extracts the first player iframe src off a /video
// page (unquoted or quoted). ok=false when the page carries none.
func anipubIframeSrc(body []byte) (string, bool) {
	m := anipubIframeRe.FindSubmatch(body)
	if m == nil {
		return "", false
	}
	src := string(m[1])
	if len(src) >= 2 && src[0] == '"' && src[len(src)-1] == '"' {
		src = src[1 : len(src)-1]
	}
	if src == "" {
		return "", false
	}
	return src, true
}

// anipubFileID extracts the stream page's data-id. ok=false when the
// page carries none.
func anipubFileID(body []byte) (string, bool) {
	m := anipubFileIDRe.FindSubmatch(body)
	if m == nil {
		return "", false
	}
	return string(m[1]), true
}

// anipubLangSuffix reports the trailing path segment when it is the
// sub/dub selector (the /video/<n>/<lang> and /stream/s-2/<id>/<lang>
// shapes).
func anipubLangSuffix(path string) (string, bool) {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	if len(segs) == 0 {
		return "", false
	}
	last := segs[len(segs)-1]
	if last == "sub" || last == "dub" {
		return last, true
	}
	return "", false
}

// anipubDecryptSources decrypts the getSourcesNew enc payload: base64url
// → AES-256-CBC (static key/iv) → {"file": <master.m3u8>} [VERIFIED
// against both live captures 2026-09-25]. An empty payload (the
// track-only answer a bad CDN selector or unknown type gets) is the
// typed extract wall.
func anipubDecryptSources(enc string) (string, error) {
	if enc == "" {
		return "", fmt.Errorf("%w: getSourcesNew carries no enc payload", contracts.ErrExtractFailed)
	}
	// base64url → base64 for the shared std-encoding decrypt helper.
	std := strings.NewReplacer("-", "+", "_", "/").Replace(enc)
	std += strings.Repeat("=", (4-len(std)%4)%4)
	plain, err := crypto.AESDecrypt(std, anipubAESKey, anipubAESIV)
	if err != nil {
		return "", fmt.Errorf("%w: decrypt enc payload: %w", contracts.ErrExtractFailed, err)
	}
	var decoded struct {
		File string `json:"file"`
	}
	if err := json.Unmarshal([]byte(plain), &decoded); err != nil {
		return "", fmt.Errorf("%w: decode decrypted sources: %w", contracts.ErrExtractFailed, err)
	}
	if decoded.File == "" {
		return "", fmt.Errorf("%w: decrypted sources carry no file", contracts.ErrExtractFailed)
	}
	return decoded.File, nil
}
