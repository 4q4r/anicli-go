package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/PuerkitoBio/goquery"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// DreamCastBase is the site root (anicli-py anicli/providers/
// dreamcast.py:22).
const DreamCastBase = "https://dreamerscast.com"

// dreamcastPlayerjsRe captures the obfuscated playlist blob off the
// inline player bootstrap (dreamcast.py:60).
var dreamcastPlayerjsRe = regexp.MustCompile(`new Playerjs\("(.*?)"\)`)

// dreamcastCryptKeyRe extracts the crypto key from the unpacked playerjs
// (dreamcast.py:133): the raw file bytes carry a backslash before the
// opening quote and backslashes after the final '='. Re-verified against
// the LIVE library 2026-09-18: Playerjs 20.7.1 ships the byte-identical
// escaped-quote shape (u:\'#1…=\\\'), so the frozen pattern still pins
// the baked-in key of dreamerscast.com's /js/playerjs.min.js.
var dreamcastCryptKeyRe = regexp.MustCompile(`u:\s*\\\s*['"]([^=]+=[\\]+)\s*['"]`)

// unpack helpers mirror the Python packer regexp set (dreamcast.py:151,
// 159, 163, 177).
var (
	dreamcastUnpackPayloadRe = regexp.MustCompile(`'(.*?[^\\])',`)
	dreamcastUnpackDigitsRe  = regexp.MustCompile(`(\d+),`)
	dreamcastUnpackKeysRe    = regexp.MustCompile(`'(.*?[^\\])'\.split`)
	dreamcastUnpackTokenRe   = regexp.MustCompile(`\b\w+\b`)
)

// dreamcastUnpackStartMarker anchors the (p,a,c,k,e,d) packer payload
// (dreamcast.py:140).
const dreamcastUnpackStartMarker = "return p}('"

// dreamcastABC is the scrambled alphabet of the playerjs salt codec
// (dreamcast.py:182).
const dreamcastABC = "ABCDEFGHIJKLMabcdefghijklmNOPQRSTUVWXYZnopqrstuvwxyz"

// dreamcastOY is the fixed pepper operand (dreamcast.py:231).
const dreamcastOY = "xx???x=xx?x??="

// DreamCast is the port of anicli-py anicli/providers/dreamcast.py: a
// form-POST JSON search, and episodes decoded out of an obfuscated
// Playerjs playlist (packer + salt/pepper crypto chain, ported verbatim
// below).
//
// Revived live 2026-09-18 (PR51) against the rebranded site's own
// domain (dreamerscast.com — the base the Python original already
// carried). The 20.7.1 library ships the SAME key shape and crypto
// constants; what changed is behavioral and typed here:
//   - the player blob marker is "#2" (the library's junk-strip path;
//     the decode math is marker-agnostic after the 2-char strip);
//   - film releases carry a single-string playlist file (one episode);
//   - license-blocked releases carry a …/dash/block,… string file — a
//     typed ErrGeoBlocked, not a silent [];
//   - decode/markup failures are typed ErrExtractFailed: the frozen
//     Python-parity silence masked the whole site drift until the
//     provider looked dead (task ruling, PR51);
//   - stream links keep the site Referer (task ruling, PR5).
type DreamCast struct {
	Base
}

// newDreamCast builds the provider against baseURL. The Python original
// sends no extra headers (dreamcast.py defines none).
func newDreamCast(baseURL string, http *netclient.Client) *DreamCast {
	return &DreamCast{Base: Base{
		id:          "dreamcast",
		name:        "DreamCast",
		baseURL:     baseURL,
		sourceType:  contracts.SourceTypeBoth,
		contentLang: "ru",
		http:        http,
	}}
}

// dreamcastSearch mirrors the fields consumed by dreamcast.py:34-47.
type dreamcastSearch struct {
	Releases []struct {
		Russian  *string `json:"russian"`
		Original *string `json:"original"`
		URL      *string `json:"url"`
		Image    string  `json:"image"`
	} `json:"releases"`
}

// dreamcastPlaylist mirrors the playlist envelope: `file` is an episode
// array for series and a single URL string for films and the
// license-blocked placeholder (both shapes are live — see the fixtures),
// so it stays raw until GetEpisodes interprets it.
type dreamcastPlaylist struct {
	Label string          `json:"label"`
	File  json.RawMessage `json:"file"`
}

// dreamcastPlaylistEntry is one series episode of the playlist
// (dreamcast.py:79-85).
type dreamcastPlaylistEntry struct {
	Title *string `json:"title"`
	File  string  `json:"file"`
}

// Search POSTs the filter form to the site root (anicli-py
// dreamcast.py:25-48). Divergence from Python (task ruling, PR51): a
// non-JSON answer is protocol drift and fails typed — the Python bare
// except returned [], which masked site drift as an empty catalog.
func (p *DreamCast) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	form := url.Values{}
	form.Set("search", query)
	form.Set("status", "")
	form.Set("pageSize", "16")
	form.Set("pageNumber", "1")

	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "POST",
		URL:     p.baseURL + "/",
		Headers: formContentType,
		Body:    strings.NewReader(form.Encode()),
		Op:      contracts.OpSearch,
	})
	if err != nil {
		return nil, err
	}

	var data dreamcastSearch
	if jsonErr := json.Unmarshal(resp.Body, &data); jsonErr != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, resp.StatusCode,
			fmt.Errorf("decode search response (%w): %w", contracts.ErrExtractFailed, jsonErr))
	}

	results := make([]contracts.SearchResult, 0, len(data.Releases))
	for _, item := range data.Releases {
		title := firstNonEmpty(deref(item.Russian), deref(item.Original))
		link := deref(item.URL)
		if link != "" && !strings.HasPrefix(link, "http") {
			link = p.baseURL + link
		}
		results = append(results, contracts.SearchResult{
			Title:    title,
			URL:      link,
			SourceID: p.ID(),
			Poster:   item.Image,
		})
	}
	return results, nil
}

// GetEpisodes decodes the obfuscated Playerjs playlist of the anime page
// (port of dreamcast.py:50-88). The page carries the "#2"-marked blob
// plus the /js/playerjs script URL; the library holds the baked-in key
// the decoder needs. Divergence from Python (task ruling, PR51): every
// silent-empty path is typed — missing markers and decode failures are
// ErrExtractFailed, the license-blocked placeholder playlist is
// ErrGeoBlocked, film releases surface as ONE episode.
func (p *DreamCast) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	resp, err := p.http.Do(ctx, netclient.Request{
		Method: "GET",
		URL:    animeURL,
		Op:     contracts.OpGetEpisodes,
	})
	if err != nil {
		return nil, err
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(resp.Body))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("parse anime page: %w", err))
	}

	jsEncoded := ""
	playerJSURL := ""
	doc.Find("script").Each(func(_ int, s *goquery.Selection) {
		if txt := s.Text(); strings.Contains(txt, `new Playerjs("`) {
			if match := dreamcastPlayerjsRe.FindStringSubmatch(txt); match != nil {
				jsEncoded = match[1]
			}
		}
		if src, ok := s.Attr("src"); ok && strings.Contains(src, "/js/playerjs") {
			playerJSURL = src
		}
	})

	if jsEncoded == "" || playerJSURL == "" {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("no Playerjs blob or player script on the page: %w", contracts.ErrExtractFailed))
	}
	if !strings.HasPrefix(playerJSURL, "http") {
		playerJSURL = p.baseURL + playerJSURL
	}

	jsResp, err := p.http.Do(ctx, netclient.Request{
		Method: "GET",
		URL:    playerJSURL,
		Op:     contracts.OpGetEpisodes,
	})
	if err != nil {
		return nil, err
	}

	playlist, err := decodePlaylist(string(jsResp.Body), jsEncoded)
	if err != nil {
		// Python _decode_playlist swallowed every exception into an
		// empty playlist (dreamcast.py:75-88, 124-125) — the silence
		// that hid the 2026 drift. Typed now.
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, jsResp.StatusCode,
			fmt.Errorf("decode playlist (%w): %w", contracts.ErrExtractFailed, err))
	}

	// Interpret the playlist `file` shape (live behavior):
	//   array  → the series episode list;
	//   string → ONE film episode, unless it is the license-blocked
	//            placeholder (…/dash/block,… or label "<id>-block").
	var entries []dreamcastPlaylistEntry
	if err := json.Unmarshal(playlist.File, &entries); err == nil {
		return p.episodesFromEntries(entries), nil
	}
	var single string
	if serr := json.Unmarshal(playlist.File, &single); serr != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, jsResp.StatusCode,
			fmt.Errorf("playlist file: neither episode array nor string url: %w", contracts.ErrExtractFailed))
	}
	if dreamcastIsBlocked(playlist.Label, single) {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, jsResp.StatusCode, contracts.ErrGeoBlocked)
	}
	return p.episodesFromEntries([]dreamcastPlaylistEntry{{File: single}}), nil
}

// dreamcastIsBlocked reports the license-blocked placeholder playlist:
// the site serves a single string file pointing at a /dash/block,/
// rendition labelled "<id>-block" (live: release 111 Bleach TYBW).
func dreamcastIsBlocked(label, file string) bool {
	return strings.Contains(file, "/block,") || strings.HasSuffix(label, "-block")
}

// episodesFromEntries maps playlist entries to contracts episodes
// (dreamcast.py:92-99): 1-based num and index default titles, the raw
// file string carried verbatim for ResolveStream. A missing title on a
// single-entry playlist renders the kodik-movie convention "Фильм".
func (p *DreamCast) episodesFromEntries(entries []dreamcastPlaylistEntry) []contracts.Episode {
	episodes := make([]contracts.Episode, 0, len(entries))
	for i, item := range entries {
		// Python enumerate(..., 1): 1-based num, default title uses the
		// 1-based index too.
		num := strconv.Itoa(i + 1)
		title := fmt.Sprintf("Episode %d", i+1)
		if item.Title != nil {
			title = *item.Title
		} else if len(entries) == 1 {
			title = "Фильм"
		}
		episodes = append(episodes, contracts.Episode{
			Num:   num,
			Title: title,
			RawID: num,
			RawEmbeds: map[string][]string{
				"DreamCast": {item.File},
			},
		})
	}
	return episodes
}

// ResolveStream splits the episode file field and keeps http(s) media
// URLs ending in .m3u8 or .mpd (port of dreamcast.py:90-99): one 1080
// slot, the last match winning.
//
// Divergences (task rulings): the resolved VideoSource carries the site
// root as its Referer so mpv can play the CDN link (PR5; the Python
// original left stream headers empty), and trailing separators are
// trimmed per token — the live playlist joins the dash/hls manifests
// with " or " and the vod URLs THEMSELVES carry commas (the
// _,1080,720,low, quality path), so the Python blanket comma-split
// destroyed every live link.
func (p *DreamCast) ResolveStream(_ context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	stream := contracts.MediaStream{
		DubName: dubID,
		Links:   map[string]contracts.VideoSource{},
	}

	embeds := episode.RawEmbeds[dubID]
	if len(embeds) == 0 || embeds[0] == "" {
		return stream, nil
	}

	// The live playlist joins the dash/hls manifests with " or "; each
	// part is whitespace-tokenized, trailing separators trimmed (the
	// legacy ", "-joined lists), URLs kept verbatim — their quality
	// path carries meaningful commas (…_,1080,720,low,aac,.mp4.urlset).
	for _, part := range strings.Split(embeds[0], " or ") {
		for _, u := range strings.Fields(part) {
			u = strings.TrimRight(u, ",")
			if strings.HasPrefix(u, "http") &&
				(strings.HasSuffix(u, ".m3u8") || strings.HasSuffix(u, ".mpd")) {
				stream.Links["1080"] = contracts.VideoSource{
					URL:     u,
					Quality: "1080",
					Headers: map[string]string{"Referer": p.baseURL},
				}
			}
		}
	}
	return stream, nil
}

// SmokeQuery reports the provider-specific live smoke probe (PR51):
// the catalog is the team's OWN dubs under strict prefix search, so
// the shared probes (черная лагуна / black lagoon) never surface.
// "мао" is a stable single-hit release — and deliberately NOT the
// catalog's top «блич» hit (Bleach TYBW), which is license-blocked.
func (p *DreamCast) SmokeQuery() string { return "мао" }

// decodePlaylist ports _decode_playlist (dreamcast.py:101-125): unpack
// the playerjs, decode the salt key JSON, strip the bk0-bk4 junk parts
// off the blob and base64+percent-decode what remains.
func decodePlaylist(playerJS, encoded string) (*dreamcastPlaylist, error) {
	cryptCodes, err := getCryptCodes(playerJS)
	if err != nil {
		return nil, err
	}
	decodedKey, err := dreamDecode(cryptCodes)
	if err != nil {
		return nil, err
	}

	var v map[string]any
	if err := json.Unmarshal([]byte(decodedKey), &v); err != nil {
		return nil, fmt.Errorf("decode crypt key json: %w", err)
	}
	// Python forces v["file3_separator"] = "//" after loading, so the
	// separator below is a constant regardless of the key payload.
	const separator = "//"

	aStr := ""
	if len(encoded) >= 2 {
		aStr = encoded[2:] // two leading marker bytes are dropped
	}
	for i := 4; i >= 0; i-- {
		valRaw, ok := v["bk"+strconv.Itoa(i)]
		if !ok || valRaw == nil {
			continue
		}
		val, ok := valRaw.(string)
		if !ok {
			// Python builds junk via quote(non-string) → TypeError → the
			// whole decode fails; mirrored here.
			return nil, fmt.Errorf("bk%d: non-string value %T", i, valRaw)
		}
		if val == "" || val == "undefined" {
			continue
		}
		junk := separator + b64eURLParams(val)
		aStr = strings.ReplaceAll(aStr, junk, "")
	}

	decodedJSON, err := b64dURLParams(aStr)
	if err != nil {
		return nil, err
	}

	var playlist dreamcastPlaylist
	if err := json.Unmarshal([]byte(decodedJSON), &playlist); err != nil {
		return nil, fmt.Errorf("decode playlist json: %w", err)
	}
	return &playlist, nil
}

// getCryptCodes ports _get_crypt_codes (dreamcast.py:129-136).
func getCryptCodes(playerjsPacked string) (string, error) {
	unpacked := unpackPlayerJS(playerjsPacked)
	match := dreamcastCryptKeyRe.FindStringSubmatch(unpacked)
	if match == nil {
		return "", errors.New("crypt keys not found")
	}
	return match[1], nil
}

// unpackPlayerJS ports _unpack_playerjs (dreamcast.py:138-179): a
// (p,a,c,k,e,d) packer unpacker. Every parse failure returns the input
// unchanged, mirroring the Python except-block.
//
// Divergence: Python \w is Unicode-aware while Go \w is ASCII; packer
// payloads are ASCII, so the observable behavior matches.
func unpackPlayerJS(packed string) string {
	idx := strings.Index(packed, dreamcastUnpackStartMarker)
	if idx == -1 {
		return packed // maybe not packed
	}
	start := idx + len(dreamcastUnpackStartMarker) - 1
	if start >= len(packed) || len(packed) < 2 {
		return packed
	}
	payload := packed[start : len(packed)-1]

	pm := dreamcastUnpackPayloadRe.FindStringSubmatch(payload)
	if pm == nil {
		return packed
	}
	p := pm[1]
	rest := payload[len(pm[0]):]

	// Python re.findall over a single-group pattern yields the capture
	// ("62"), not the full match ("62,") — submatch extraction mirrors it.
	digitMatches := dreamcastUnpackDigitsRe.FindAllStringSubmatch(rest, -1)
	if len(digitMatches) < 2 {
		return packed // Python IndexError → except → packed
	}
	a, errA := strconv.Atoi(digitMatches[0][1])
	c, errC := strconv.Atoi(digitMatches[1][1])
	if errA != nil || errC != nil || a <= 0 {
		return packed
	}

	km := dreamcastUnpackKeysRe.FindStringSubmatch(rest)
	if km == nil {
		return packed // Python AttributeError → except → packed
	}
	k := strings.Split(km[1], "|")
	if c > len(k) {
		return packed // Python IndexError → except → packed
	}

	const base36 = "0123456789abcdefghijklmnopqrstuvwxyz"
	var e func(n int) string
	e = func(n int) string {
		head := ""
		if n >= a {
			head = e(n / a)
		}
		r := n % a
		if r > 35 {
			// Python chr(n%a+29): the packer radix keeps n%a ≤ 61, so the
			// rune stays inside 'A'..'Z'.
			return head + string(rune(r+29)) //nolint:gosec // bounded by the radix (≤ 61+29)
		}
		return head + string(base36[r])
	}

	d := map[string]string{}
	for cc := c; cc > 0; cc-- {
		key := e(cc - 1)
		v := k[cc-1]
		if v == "" {
			v = key
		}
		d[key] = v
	}

	return dreamcastUnpackTokenRe.ReplaceAllStringFunc(p, func(tok string) string {
		if v, ok := d[tok]; ok {
			return v
		}
		return tok
	})
}

// dreamDecode ports _decode (dreamcast.py:181-189): "#1" applies the
// pepper rotation before the salt codec, "#0" only the salt codec,
// anything else passes through.
func dreamDecode(x string) (string, error) {
	saltABC := dreamcastABC + "0123456789+/="
	switch {
	case strings.HasPrefix(x, "#1"):
		peppered, err := dreamPepper(x[2:], -1)
		if err != nil {
			return "", err
		}
		return dreamSaltD(peppered, saltABC)
	case strings.HasPrefix(x, "#0"):
		return dreamSaltD(x[2:], saltABC)
	}
	return x, nil
}

// dreamPepper ports _pepper (dreamcast.py:230-242). With the fixed
// operand sugar=14 and n=-1 the rotation index is int((−14+26/2)*2)=24;
// out-of-range indexes clamp like Python slicing (defensive only, the
// value above is the sole reachable one).
func dreamPepper(s string, n int) (string, error) {
	// Python s.replace("+", "#").replace("#", "+"): a verbatim no-op
	// swap (kept for fidelity).
	s = strings.ReplaceAll(strings.ReplaceAll(s, "+", "#"), "#", "+")

	sugar, err := dreamSugar(dreamcastOY)
	if err != nil {
		return "", err
	}
	af := float64(sugar) * float64(n)
	if n < 0 {
		af += 26.0 // len(_ABC)/2, Python float division
	}
	idx := int(af * 2)

	size := len(dreamcastABC)
	var r string
	switch {
	case idx >= size:
		r = dreamcastABC
	case idx < 0:
		shift := size + idx
		if shift < 0 {
			shift = 0
		}
		r = dreamcastABC[shift:] + dreamcastABC[:shift]
	default:
		r = dreamcastABC[idx:] + dreamcastABC[:idx]
	}

	var b strings.Builder
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			pos := strings.IndexRune(dreamcastABC, c)
			b.WriteRune(rune(r[pos]))
			continue
		}
		b.WriteRune(c)
	}
	return b.String(), nil
}

// dreamSugar ports _sugar (dreamcast.py:244-252): the fixed "="-split
// binary decoder. Only the constant dreamcastOY is ever passed (yielding
// 14); the error paths mirror Python's int()/chr() ValueErrors bubbling
// into a failed decode.
func dreamSugar(x string) (int, error) {
	parts := strings.Split(x, "=")
	out := make([]byte, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			out = append(out, 0)
			continue
		}
		var bits strings.Builder
		for i := range part {
			if part[i] == 'x' {
				bits.WriteByte('1')
			} else {
				bits.WriteByte('0')
			}
		}
		v, err := strconv.ParseInt(bits.String(), 2, 64)
		if err != nil || v > 255 {
			return 0, fmt.Errorf("sugar decode %q: out of range", part)
		}
		out = append(out, byte(v)) //nolint:gosec // guarded by the v>255 check above
	}
	if len(out) == 0 {
		return 0, nil
	}
	n, err := strconv.Atoi(string(out[:len(out)-1]))
	if err != nil {
		return 0, fmt.Errorf("sugar digits: %w", err)
	}
	return n, nil
}

// dreamSaltD ports _salt_d (dreamcast.py:191-208): a custom-alphabet
// base64 decoder over 4-char quanta where alphabet index 64 ("=") pads.
// A trailing partial quantum or invalid UTF-8 fails the decode — Python
// crashes into the caller's except-block there, producing an empty
// playlist, so failing here mirrors the observable outcome.
func dreamSaltD(e, keyStr string) (string, error) {
	filtered := make([]byte, 0, len(e))
	for i := range e {
		if strings.IndexByte(keyStr, e[i]) >= 0 {
			filtered = append(filtered, e[i])
		}
	}

	var t []byte
	for f := 0; f < len(filtered); f += 4 {
		if f+4 > len(filtered) {
			return "", errors.New("salt decode: partial quantum")
		}
		s := strings.IndexByte(keyStr, filtered[f])
		o := strings.IndexByte(keyStr, filtered[f+1])
		u := strings.IndexByte(keyStr, filtered[f+2])
		a := strings.IndexByte(keyStr, filtered[f+3])

		n := (s << 2) | (o >> 4)
		r := ((o & 15) << 4) | (u >> 2)
		i := ((u & 3) << 6) | a
		// Every operand is a ≤64 alphabet index, so all three values fit
		// a byte by construction (i only emits when a != 64).
		t = append(t, byte(n)) //nolint:gosec // 6-bit group arithmetic, ≤ 255
		if u != 64 {
			t = append(t, byte(r)) //nolint:gosec // 6-bit group arithmetic, ≤ 255
		}
		if a != 64 {
			t = append(t, byte(i)) //nolint:gosec // 6-bit group arithmetic, ≤ 255
		}
	}

	// Python's hand-rolled _utf8_decode produces garbage on invalid
	// sequences that json.loads then rejects; failing here is the twin.
	if !utf8.Valid(t) {
		return "", errors.New("salt decode: invalid utf-8")
	}
	return string(t), nil
}

// b64eURLParams ports _b64e_url_params (dreamcast.py:254-255):
// base64(urllib.parse.quote(s)). pyQuote is the exact quote() twin
// (safe="/"), so it is reused here.
func b64eURLParams(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(pyQuote(s)))
}

// b64dURLParams ports _b64d_url_params (dreamcast.py:257-258):
// unquote(base64.b64decode(s)). Python's b64decode(validate=False)
// discards non-alphabet bytes before the padding check, and unquote
// never fails (invalid escapes stay literal, invalid UTF-8 becomes
// U+FFFD) — both behaviors are mirrored.
func b64dURLParams(s string) (string, error) {
	filtered := make([]byte, 0, len(s))
	for i := range s {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '+' || c == '/' || c == '=' {
			filtered = append(filtered, c)
		}
	}
	if rem := len(filtered) % 4; rem != 0 {
		filtered = append(filtered, bytes.Repeat([]byte("="), 4-rem)...)
	}
	raw, err := base64.StdEncoding.DecodeString(string(filtered))
	if err != nil {
		return "", fmt.Errorf("b64 decode: %w", err)
	}
	return pyUnquoteTolerant(string(raw)), nil
}

// pyUnquoteTolerant mirrors urllib.parse.unquote(errors="replace"):
// percent-escapes decode (invalid escapes stay literal via the
// PathUnescape fallback) and invalid UTF-8 becomes U+FFFD.
func pyUnquoteTolerant(s string) string {
	decoded, err := url.PathUnescape(s)
	if err != nil {
		decoded = s
	}
	return strings.ToValidUTF8(decoded, "\uFFFD")
}
