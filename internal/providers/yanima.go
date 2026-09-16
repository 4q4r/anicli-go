package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// This file hosts the yanima.space provider (PR33; PR34 full-episode
// resolve). It is NOT a Python-tree port: yanima.space is a
// Russian-dub aggregator (Next.js SPA behind a Mitelis DDoS-Mitigation
// wall) characterized from user-supplied DevTools curl captures on
// 2026-09-14 — see the ".sdd/ledger.md" YANIMA.SPACE PROTOCOL DOSSIER.
// Wire-shape facts marked [LIVE-CAPTURED] come from those captures;
// the ep1-free/ep2+-walled split and the S3-gated-by-JWT-only facts
// are [LIVE-VERIFIED protocol, controller 2026-09-13]. The SOURCE row
// shape (resolution object + src) is live-verified; the response-body
// wrapper keys remain bounded-tolerant hypotheses pinned by the
// fixtures (disclosed in .sdd/pr33-report.md and .sdd/pr34-report.md).
//
// Anime IDs are Shikimori IDs (/play/{shikimori_id}). The site has no
// characterized search or episodes endpoint yet, so Search and
// GetEpisodes fail loud with typed errors and the resolve path —
// fed by raw_embeds carrying yanima page URLs — is the feature.

// YanimaBase is the site root [LIVE-CAPTURED 2026-09-14].
const YanimaBase = "https://yanima.space"

// YanimaStreamReferer is the Referer the S3 HLS server expects on
// manifest playback requests [LIVE-CAPTURED 2026-09-14].
const YanimaStreamReferer = "https://yanima.space/"

// Yanima is the yanima.space provider: multiple RU dubs per episode,
// per-dub quality/resolution variants up to 4K (resolution=2160).
type Yanima struct {
	Base

	// ddoSP1/ddoSP2 are the Mitelis-wall cookies (mit_ck_p1 =
	// base64(IP|UserAgent|OK|hash), mit_ck_p2 = encrypted challenge
	// token) copied from the browser by the user — same workflow as
	// the kodik token. Every API request answers 403 without them.
	// session is the optional YAA_SESS_ID cookie for authenticated
	// content.
	ddoSP1  string
	ddoSP2  string
	session string

	// ymMu guards ymTokens: the first ym_ JWT each anime successfully
	// manifests with, kept across ResolveStream calls. The
	// s3.yanima.space gate is that JWT alone [LIVE-VERIFIED protocol,
	// controller 2026-09-13: ep1 4K plays free and the subscription
	// check sits on the MANIFEST endpoint, not on S3 file access], so
	// a token minted for a free episode is the bypass credential when
	// a later episode's manifest hits the subscription wall.
	ymMu     sync.Mutex
	ymTokens map[string]string

	// s3Host is the hostname of the yanima S3 storage [dossier:
	// s3.yanima.space]. It decides which SOURCE src rows are native
	// (JWT-gated) versus foreign embeds (Kodik et al., credential-free):
	// the ym_ JWT is a bearer credential and is only ever sent to this
	// host. Injectable like the API baseURL; tests point it at their
	// fake.
	s3Host string
}

// YanimaS3Host is the default s3.yanima.space storage hostname
// [LIVE-CAPTURED 2026-09-14].
const YanimaS3Host = "s3.yanima.space"

// newYanima builds the provider against baseURL with the user's wall
// cookies. The cookies are opaque browser-issued values and are sent
// verbatim.
func newYanima(baseURL, ddoSP1, ddoSP2, session string, http *netclient.Client) *Yanima {
	return &Yanima{
		Base: Base{
			id:          "yanima",
			name:        "Yanima",
			baseURL:     baseURL,
			sourceType:  contracts.SourceTypeBoth,
			contentLang: "ru",
			http:        http,
		},
		ddoSP1:  ddoSP1,
		ddoSP2:  ddoSP2,
		session: session,
		s3Host:  YanimaS3Host,

		ymTokens: map[string]string{},
	}
}

// Search is a typed stub [dossier 2026-09-14: SEARCH endpoint NOT YET
// FOUND]: yanima has no characterized search API, so discovery goes
// through the catalog flow (the user picks the title from the
// Shikimori list). Fails loud instead of answering an empty result
// set that would look like "no matches".
func (p *Yanima) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, 0,
		fmt.Errorf("%w: yanima search is not implemented: pick the title via the catalog flow; only the resolve path is supported until a search endpoint is characterized",
			contracts.ErrInvalidInput))
}

// GetEpisodes is a typed stub [dossier 2026-09-14: EPISODES endpoint
// NOT YET FOUND] — marked for future characterization. Episodes that
// carry yanima embeds arrive from other sources via raw_embeds.
func (p *Yanima) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
		fmt.Errorf("%w: yanima episodes endpoint is not characterized yet",
			contracts.ErrInvalidInput))
}

// ResolveStream resolves the best available quality for one dub
// [LIVE-CAPTURED endpoint routes 2026-09-14; LIVE-VERIFIED fallback
// protocol, controller 2026-09-13]:
//
//  1. the yanima embed URL is parsed into (animeID, episode[, pinned
//     quality/resolution]);
//  2. GET /api/anime/v1/player/source/{id}/{ep}/{dub}?fetchKodik=true
//     lists the dub's quality/resolution variants plus direct m3u8
//     src URLs (Kodik embeds, native S3 rows);
//  3. GET /api/anime/v2/player/manifest/{id}/{ep}/{quality}/{resolution}
//     yields the m3u8 URL plus a ym_ JWT per variant — FREE on episode
//     1, subscription-walled from episode 2 on;
//  4. every manifested variant becomes a VideoSource carrying the ym_
//     Cookie and the site Referer — both load-bearing on the
//     s3.yanima.space HLS server — and successful tokens are cached
//     per anime;
//  5. when the manifest wall swallowed every variant (episode 2+),
//     the SOURCE-disclosed native S3 src URLs are probed with the
//     cached ym_ token (the S3 gate is the JWT, not the manifest);
//  6. if the probes are rejected too, the remaining SOURCE src rows
//     (Kodik et al., credential-free) become the links.
//
// A pinned pair in the embed URL skips the SOURCE discovery call and
// stays manifest-only (the user asked for that exact variant). Only
// server-disclosed URLs are used: no videoId guessing against S3.
// When every candidate fails the error is ErrAllCandidatesFailed
// (never a silent empty stream).
func (p *Yanima) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	stream := contracts.MediaStream{
		DubName: dubID,
		Links:   map[string]contracts.VideoSource{},
	}

	if p.ddoSP1 == "" || p.ddoSP2 == "" {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
			fmt.Errorf("%w: yanima: DDoS cookies not configured (providers.yanima.ddoS_p1/ddoS_p2)",
				contracts.ErrInvalidInput))
	}

	embeds := episode.RawEmbeds[dubID]
	if len(embeds) == 0 {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
			fmt.Errorf("%w: no yanima embed for dub %q", contracts.ErrInvalidInput, dubID))
	}
	ref, err := parseYanimaEmbed(embeds)
	if err != nil {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0, err)
	}

	pinned := ref.quality != "" && ref.resolution != ""
	variants := []yanimaVariant{{quality: ref.quality, resolution: ref.resolution}}
	if !pinned {
		variants, err = p.fetchSource(ctx, ref, dubID)
		if err != nil {
			return stream, err
		}
	}

	// Stage 1 — MANIFEST per variant: free episodes resolve here with
	// the full quality ladder up to 4K.
	for _, variant := range variants {
		res := variant.resolution
		if _, done := stream.Links[res]; done {
			continue // first variant per resolution wins (document order)
		}
		manifest, err := p.fetchManifest(ctx, ref, variant)
		if err != nil {
			continue // sibling variants stay candidates
		}
		p.cacheToken(ref.animeID, manifest.token)
		stream.Links[res] = contracts.VideoSource{
			URL:     manifest.url,
			Quality: res,
			Type:    "m3u8",
			Headers: map[string]string{
				"Cookie":  p.streamCookie(manifest.token),
				"Referer": YanimaStreamReferer,
			},
		}
	}

	// Stage 2 — native bypass: the manifest wall is server-side and
	// episode-scoped, but S3 file access answers to the ym_ JWT alone.
	// A token minted by an earlier free manifest (episode 1) is tried
	// against the SOURCE-disclosed native S3 URLs.
	if len(stream.Links) == 0 && !pinned {
		if token := p.cachedToken(ref.animeID); token != "" {
			for _, variant := range variants {
				res := variant.resolution
				if variant.src == "" || !p.nativeSrc(variant.src) {
					continue
				}
				if _, done := stream.Links[res]; done {
					continue
				}
				if !p.probeNative(ctx, variant.src, token) {
					continue // claims enforced / dead URL: try the next row
				}
				stream.Links[res] = contracts.VideoSource{
					URL:     variant.src,
					Quality: res,
					Type:    "m3u8",
					Headers: map[string]string{
						"Cookie":  p.streamCookie(token),
						"Referer": YanimaStreamReferer,
					},
				}
			}
		}
	}

	// Stage 3 — credential-free fallback: the SOURCE endpoint serves
	// Kodik m3u8 links for every episode regardless of the wall.
	if len(stream.Links) == 0 && !pinned {
		for _, variant := range variants {
			res := variant.resolution
			if variant.src == "" || p.nativeSrc(variant.src) {
				continue // native rows without a token would 403 in the player
			}
			if _, done := stream.Links[res]; done {
				continue
			}
			stream.Links[res] = contracts.VideoSource{
				URL:     variant.src,
				Quality: res,
				Type:    "m3u8",
			}
		}
	}

	if len(stream.Links) == 0 {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
			fmt.Errorf("%w: every yanima manifest failed for anime %s episode %s",
				contracts.ErrAllCandidatesFailed, ref.animeID, ref.episode))
	}
	return stream, nil
}

// yanimaRef is the parsed yanima embed URL: the anime (Shikimori) ID,
// the episode and — when the embed URL pins them — an exact
// quality/resolution pair.
type yanimaRef struct {
	animeID    string
	episode    string
	quality    string
	resolution string
}

// parseYanimaEmbed extracts the resolve reference from the episode's
// yanima page URLs, trying every candidate until one parses. Accepted
// shapes (dossier routes /play/{id} and /anime/{id}): the episode via
// the ?episode= query or a third path segment; the optional
// ?quality=&resolution= pins. Anything not served by a *.yanima.space
// host is rejected — embeds from other providers must never reach the
// yanima API.
func parseYanimaEmbed(embeds []string) (yanimaRef, error) {
	for _, raw := range embeds {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		host := u.Hostname()
		if host != "yanima.space" && !strings.HasSuffix(host, ".yanima.space") {
			continue
		}
		segments := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(segments) < 2 || (segments[0] != "play" && segments[0] != "anime") {
			continue
		}
		if segments[1] == "" {
			continue
		}
		episodeNum := u.Query().Get("episode")
		if episodeNum == "" && len(segments) >= 3 {
			episodeNum = segments[2]
		}
		if episodeNum == "" {
			continue
		}
		return yanimaRef{
			animeID:    segments[1],
			episode:    episodeNum,
			quality:    u.Query().Get("quality"),
			resolution: u.Query().Get("resolution"),
		}, nil
	}
	return yanimaRef{}, fmt.Errorf("%w: no yanima play/anime URL with an episode among %d embed(s)",
		contracts.ErrExtractFailed, len(embeds))
}

// yanimaVariant is one SOURCE quality/resolution row.
type yanimaVariant struct {
	quality    string
	resolution string
	// src is the SOURCE-disclosed direct m3u8 (native S3 or Kodik);
	// "" marks a manifest-only row.
	src string
}

// fetchSource calls the SOURCE endpoint for the dub's variants
// [LIVE-CAPTURED 2026-09-14: route + fetchKodik=true + Accept */*;
// the DDoS cookies are mandatory or the wall answers 403].
func (p *Yanima) fetchSource(ctx context.Context, ref yanimaRef, dub string) ([]yanimaVariant, error) {
	endpoint := fmt.Sprintf("%s/api/anime/v1/player/source/%s/%s/%s?fetchKodik=true",
		p.baseURL, ref.animeID, ref.episode, url.PathEscape(dub))

	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     endpoint,
		Headers: p.apiHeaders(),
		Op:      contracts.OpResolveStream,
	})
	if err != nil {
		return nil, err // netclient already maps 403 → ErrProvider403 etc.
	}

	variants, err := parseYanimaVariants(resp.Body)
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, resp.StatusCode, err)
	}
	return variants, nil
}

// fetchManifest calls the MANIFEST endpoint for one variant and
// returns the m3u8 URL plus the ym_ JWT for the S3 stream.
func (p *Yanima) fetchManifest(ctx context.Context, ref yanimaRef, variant yanimaVariant) (yanimaManifest, error) {
	endpoint := fmt.Sprintf("%s/api/anime/v2/player/manifest/%s/%s/%s/%s",
		p.baseURL, ref.animeID, ref.episode, variant.quality, variant.resolution)

	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     endpoint,
		Headers: p.apiHeaders(),
		Op:      contracts.OpResolveStream,
	})
	if err != nil {
		return yanimaManifest{}, err
	}

	manifest, err := parseYanimaManifest(resp.Body)
	if err != nil {
		return yanimaManifest{}, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, resp.StatusCode, err)
	}
	return manifest, nil
}

// apiHeaders renders the headers every API request carries
// [LIVE-CAPTURED 2026-09-14]: the DDoS cookies (plus the optional
// session) and Accept */*.
func (p *Yanima) apiHeaders() map[string]string {
	cookie := "mit_ck_p1=" + p.ddoSP1 + "; mit_ck_p2=" + p.ddoSP2
	if p.session != "" {
		cookie += "; YAA_SESS_ID=" + p.session
	}
	return map[string]string{
		"Cookie": cookie,
		"Accept": "*/*",
	}
}

// streamCookie renders the Cookie the s3.yanima.space server expects:
// the ym_ JWT (plus the optional session alongside, mirroring the
// manifest path).
func (p *Yanima) streamCookie(token string) string {
	cookie := "ym_=" + token
	if p.session != "" {
		cookie += "; YAA_SESS_ID=" + p.session
	}
	return cookie
}

// cacheToken remembers the anime's first successfully manifested ym_
// JWT (first wins — the JWT is per-anime/per-episode, but the S3 gate
// checks the credential, not which episode minted it).
func (p *Yanima) cacheToken(animeID, token string) {
	p.ymMu.Lock()
	defer p.ymMu.Unlock()
	if _, ok := p.ymTokens[animeID]; !ok {
		p.ymTokens[animeID] = token
	}
}

// cachedToken returns the remembered ym_ JWT for the anime, "" when
// none was minted yet this session.
func (p *Yanima) cachedToken(animeID string) string {
	p.ymMu.Lock()
	defer p.ymMu.Unlock()
	return p.ymTokens[animeID]
}

// probeNative checks one native S3 src URL against the cached ym_
// credential with a lightweight GET. Any error (403 wall, 404 drift,
// transport) marks the candidate dead — unverified links must never
// reach the player.
func (p *Yanima) probeNative(ctx context.Context, src, token string) bool {
	_, err := p.http.Do(ctx, netclient.Request{
		Method: "GET",
		URL:    src,
		Headers: map[string]string{
			"Cookie":  p.streamCookie(token),
			"Referer": YanimaStreamReferer,
		},
		Op: contracts.OpResolveStream,
	})
	return err == nil
}

// nativeSrc reports whether src points at the yanima S3 storage. Native
// streams are gated by the ym_ JWT; foreign srcs (Kodik et al.) play
// without credentials — and must never receive the JWT.
func (p *Yanima) nativeSrc(src string) bool {
	u, err := url.Parse(src)
	if err != nil {
		return false
	}
	// Hostname() strips the port (dossier pins s3.yanima.space:9000).
	return u.Hostname() == p.s3Host
}

// yanimaManifest is one MANIFEST answer: the HLS playlist URL and the
// ym_ JWT the S3 server expects as a Cookie.
type yanimaManifest struct {
	url   string
	token string
}

// yanimaVariantKeys are the wrapper keys the SOURCE variant list is
// looked under (the canonical fixture pins {"sources": [...]}). The
// row shape itself is live-verified (controller protocol 2026-09-13:
// {source, priority, resolution: {name, numeric}, src}); the wrapper
// stayed a tolerant list since the envelope around the array was not
// captured verbatim.
var yanimaVariantKeys = []string{"sources", "variants", "qualities", "data", "results"}

// yanimaManifestURLKeys / yanimaManifestTokenKeys are the field names
// the MANIFEST answer is read from (same shape hypothesis; the
// canonical fixture pins {"url", "token"}).
var (
	yanimaManifestURLKeys   = []string{"url", "file", "manifest", "m3u8", "src"}
	yanimaManifestTokenKeys = []string{"token", "ym_", "jwt"}
)

// parseYanimaVariants extracts the quality/resolution rows from a
// SOURCE payload: a top-level array, or the variant list under a known
// wrapper key. Rows without a resolution are skipped; a payload with
// no resolvable row fails loud (ErrExtractFailed) — a shape drift must
// never masquerade as an empty-but-successful stream.
func parseYanimaVariants(raw []byte) ([]yanimaVariant, error) {
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("%w: yanima source payload is not JSON: %w", contracts.ErrExtractFailed, err)
	}

	items := yanimaArrayField(decoded)
	if items == nil {
		return nil, fmt.Errorf("%w: yanima source payload has no variant list", contracts.ErrExtractFailed)
	}

	variants := make([]yanimaVariant, 0, len(items))
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		resolution := yanimaResolutionLabel(obj["resolution"])
		if resolution == "" {
			continue
		}
		quality := yanimaStringField(obj["quality"])
		if quality == "" {
			quality = resolution
		}
		variants = append(variants, yanimaVariant{
			quality:    quality,
			resolution: resolution,
			src:        yanimaStringField(obj["src"]),
		})
	}
	if len(variants) == 0 {
		return nil, fmt.Errorf("%w: yanima source payload carries no resolution variants", contracts.ErrExtractFailed)
	}
	return variants, nil
}

// yanimaArrayField finds the variant array: a top-level array, an
// array under a known wrapper key, or the first array value of a flat
// object.
func yanimaArrayField(decoded any) []any {
	switch v := decoded.(type) {
	case []any:
		return v
	case map[string]any:
		for _, key := range yanimaVariantKeys {
			if arr, ok := v[key].([]any); ok {
				return arr
			}
		}
		for _, val := range v {
			if arr, ok := val.([]any); ok {
				return arr
			}
		}
	}
	return nil
}

// parseYanimaManifest extracts the m3u8 URL and the ym_ JWT from a
// MANIFEST payload. Either field missing fails loud.
func parseYanimaManifest(raw []byte) (yanimaManifest, error) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return yanimaManifest{}, fmt.Errorf("%w: yanima manifest payload is not a JSON object: %w",
			contracts.ErrExtractFailed, err)
	}

	var manifest yanimaManifest
	for _, key := range yanimaManifestURLKeys {
		if v := yanimaStringField(obj[key]); v != "" {
			manifest.url = v
			break
		}
	}
	for _, key := range yanimaManifestTokenKeys {
		if v := yanimaStringField(obj[key]); v != "" {
			manifest.token = v
			break
		}
	}
	if manifest.url == "" {
		return yanimaManifest{}, fmt.Errorf("%w: yanima manifest payload has no stream URL", contracts.ErrExtractFailed)
	}
	if manifest.token == "" {
		return yanimaManifest{}, fmt.Errorf("%w: yanima manifest payload has no ym_ token", contracts.ErrExtractFailed)
	}
	return manifest, nil
}

// yanimaResolutionLabel reads a SOURCE row's resolution field. The
// live-verified row shape (controller protocol 2026-09-13) carries an
// object {name, numeric}; earlier captures and compact payloads carry
// a bare string/number. numeric wins, then name, then the bare scalar;
// "" marks the row unusable.
func yanimaResolutionLabel(v any) string {
	switch n := v.(type) {
	case map[string]any:
		if num := yanimaStringField(n["numeric"]); num != "" {
			return num
		}
		return yanimaStringField(n["name"])
	default:
		return yanimaStringField(v)
	}
}

// yanimaStringField renders a decoded JSON scalar as the wire string:
// strings pass through, numbers keep their literal form ("2160" stays
// "2160", not "2160e+00"), everything else yields "".
func yanimaStringField(v any) string {
	switch n := v.(type) {
	case string:
		return n
	case float64:
		return strconv.FormatFloat(n, 'f', -1, 64)
	}
	return ""
}
