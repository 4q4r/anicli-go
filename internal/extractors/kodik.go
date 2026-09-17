package extractors

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// kodikExtractor ports KodikExtractor (extractors.py:63-208), the kodik
// player reached from the kodik provider embeds and anilib's Kodik
// players.
//
// Divergence from Python (test-enabling, prod-equivalent): Python
// hardcodes https:// when absolutizing the player JS and building the
// API URL (extractors.py:97-108); this port derives the scheme from the
// embed URL. Production kodik embeds are https, so the wire behavior is
// identical, while tests can drive the flow over httptest.
type kodikExtractor struct {
	http *netclient.Client
}

// kodik param keys scraped off the player page (extractors.py:165-169).
var kodikParamKeys = []string{
	"domain", "d_sign", "pd", "pd_sign", "ref", "ref_sign",
	"type", "hash", "id", "player_js_path",
}

// kodikPlayerJSRe scrapes the app JS path when no var declares it
// (extractors.py:88).
var kodikPlayerJSRe = regexp.MustCompile(`src=["']([^"']+/assets/js/app\.[^"']+\.js)["']`)

// kodikAjaxAtobRe finds the base64 api path inside the app JS ajax call
// (extractors.py:101).
var kodikAjaxAtobRe = regexp.MustCompile(`\$\.ajax[^)]+atob\(["'](\w+=*)["']\)`)

// Name identifies the extractor.
func (e *kodikExtractor) Name() string { return "kodik" }

// Matches ports the Python URL gate (extractors.py:67).
func (e *kodikExtractor) Matches(u string) bool {
	return strings.Contains(u, "kodik") || strings.Contains(u, "aniqit")
}

// Extract resolves a kodik player page into quality-keyed sources.
func (e *kodikExtractor) Extract(ctx context.Context, rawURL string) (map[string]contracts.VideoSource, error) {
	shape := func(reason string) error {
		return fmt.Errorf("extractor:kodik: %w: %s", contracts.ErrExtractFailed, reason)
	}

	rawURL = normalizeProtocolRelative(rawURL)

	resp, err := e.http.Get(ctx, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("extractor:kodik: %w", err)
	}
	html := string(resp.Body)

	params := e.scrapeParams(html)
	if params["hash"] == "" || params["id"] == "" {
		return nil, shape("player page carries no hash/id (extractors.py:77-78)")
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, shape(fmt.Sprintf("unparseable embed url: %v", err))
	}
	base := parsed.Scheme + "://" + parsed.Host

	// 1. API path from the player JS, /ftor fallback (extractors.py:84-105).
	apiPath := "/ftor"
	jsPath := params["player_js_path"]
	if jsPath == "" {
		if m := kodikPlayerJSRe.FindStringSubmatch(html); m != nil {
			jsPath = m[1]
		}
	}
	if jsPath != "" {
		if !strings.HasPrefix(jsPath, "http") {
			if strings.HasPrefix(jsPath, "//") {
				jsPath = "https:" + jsPath
			} else {
				jsPath = base + jsPath
			}
		}
		// Python wraps this whole block in its own try/except (swallow to
		// the /ftor default); failures here are best-effort only.
		if jsResp, jsErr := e.http.Get(ctx, jsPath, nil); jsErr == nil {
			if m := kodikAjaxAtobRe.FindStringSubmatch(string(jsResp.Body)); m != nil {
				if dec, decErr := base64.StdEncoding.DecodeString(m[1]); decErr == nil {
					apiPath = string(dec)
				}
			}
		}
	}

	// 2. Signed API POST (extractors.py:107-132).
	form := url.Values{}
	form.Set("d", firstNonEmptyStr(params["domain"], parsed.Host))
	form.Set("d_sign", params["d_sign"])
	form.Set("pd", params["pd"])
	form.Set("pd_sign", params["pd_sign"])
	form.Set("ref", params["ref"])
	form.Set("ref_sign", params["ref_sign"])
	form.Set("type", params["type"])
	form.Set("hash", params["hash"])
	form.Set("id", params["id"])
	form.Set("bad_user", "false")
	form.Set("info", "{}")
	form.Set("cdn_is_working", "true")

	apiResp, err := e.http.PostForm(ctx, base+apiPath, form, map[string]string{
		"Origin":           base,
		"Referer":          rawURL,
		"Accept":           "application/json, text/javascript, */*; q=0.01",
		"X-Requested-With": "XMLHttpRequest",
	})
	if err != nil {
		return nil, fmt.Errorf("extractor:kodik: %w", err)
	}

	var data struct {
		Links map[string][]struct {
			Src string `json:"src"`
		} `json:"links"`
	}
	if err := json.Unmarshal(apiResp.Body, &data); err != nil {
		return nil, shape(fmt.Sprintf("links json: %v", err))
	}

	results := map[string]contracts.VideoSource{}
	for qualityStr, sources := range data.Links {
		if len(sources) == 0 || sources[0].Src == "" {
			continue // Python: falsy/empty first src (extractors.py:139-144)
		}
		link := kodikDecode(sources[0].Src)
		// 720 links hiding a 480 file get renamed (extractors.py:150-151).
		if qualityStr == "720" {
			link = strings.ReplaceAll(link, "/480.mp4", "/720.mp4")
		}
		digits := digitsOnly(qualityStr)
		q, convErr := strconv.Atoi(digits)
		if convErr != nil {
			continue // Python ValueError: pass
		}
		results[strconv.Itoa(q)] = contracts.VideoSource{URL: link, Quality: strconv.Itoa(q)}
	}
	return results, nil
}

// scrapeParams ports _extract_params (extractors.py:163-179): regex
// shapes per key, first hit wins. The 2026-09 kodik redesign renamed
// the params object to `vInfo` (python's `videoInfo\.` regex misses it
// too — site-side change breaking both implementations); the shape is
// matched here additively.
func (e *kodikExtractor) scrapeParams(html string) map[string]string {
	params := map[string]string{}
	for _, key := range kodikParamKeys {
		q := regexp.QuoteMeta(key)
		for _, pattern := range []string{
			`var\s+` + q + `\s*=\s*["']([^"']+)["']`,
			`videoInfo\.` + q + `\s*=\s*["']([^"']+)["']`,
			`vInfo\.` + q + `\s*=\s*["']([^"']+)["']`,
			`["']` + q + `["']\s*:\s*["']([^"']+)["']`,
		} {
			if m := regexp.MustCompile(pattern).FindStringSubmatch(html); m != nil {
				params[key] = m[1]
				break
			}
		}
	}
	return params
}

// kodikRot18 ports _decrypt_url's caesar shift (extractors.py:181-190):
// letters shift forward by 18, everything else passes through.
func kodikRot18(s string) string {
	b := []byte(s)
	for i, c := range b {
		switch {
		case c >= 'A' && c <= 'Z':
			b[i] = (c-'A'+18)%26 + 'A'
		case c >= 'a' && c <= 'z':
			b[i] = (c-'a'+18)%26 + 'a'
		}
	}
	return string(b)
}

// kodikB64Decode mirrors python base64.b64decode(validate=False) with
// its legacy leniency (oracle-verified 2026-09-13): bytes outside the
// standard alphabet are discarded, and excess '=' padding is tolerated —
// b'x=' + '==' == 'x===' still decodes, while Go's StdEncoding is
// strict. Cutting at the first '=' and re-padding to a quad multiple
// reproduces the observable Python behavior for every padding shape.
func kodikB64Decode(s string) ([]byte, error) {
	filtered := make([]byte, 0, len(s))
	for i := range len(s) {
		c := s[i]
		switch {
		case c == '=' || c == '+' || c == '/',
			c >= 'A' && c <= 'Z',
			c >= 'a' && c <= 'z',
			c >= '0' && c <= '9':
			filtered = append(filtered, c)
		}
	}
	cut := string(filtered)
	if i := strings.IndexByte(cut, '='); i >= 0 {
		cut = cut[:i]
	}
	if m := len(cut) % 4; m != 0 {
		cut += strings.Repeat("=", 4-m)
	}
	return base64.StdEncoding.DecodeString(cut)
}

// kodikDecode ports _decode (extractors.py:192-208): bare .m3u8 srcs
// pass through (absolutized), everything else is ROT-18 + base64 with
// "==" padding top-up; decode failures return the original string.
func kodikDecode(src string) string {
	if strings.HasSuffix(src, ".m3u8") {
		if strings.HasPrefix(src, "http") {
			return src
		}
		return "https:" + src
	}
	b64 := kodikRot18(src)
	if !strings.HasSuffix(b64, "==") {
		b64 += "=="
	}
	decoded, err := kodikB64Decode(b64)
	if err != nil {
		return src
	}
	s := string(decoded)
	// Bug-compatible with Python: both "//..."-prefixed and non-http
	// payloads get the bare "https:" prefix (extractors.py:204-206).
	if strings.HasPrefix(s, "//") || !strings.HasPrefix(s, "http") {
		return "https:" + s
	}
	return s
}
