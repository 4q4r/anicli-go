package extractors

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/crypto"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// kwikReferer is the Referer kwik requires on the token POST
// (extractors.py:626 comment: "headers MUST include Referer:
// https://kwik.cx/").
const kwikReferer = "https://kwik.cx/"

// kwikParamsRe scrapes the packed p,a,c,k,e,d call parameters off the
// kwik embed page (extractors.py:604):
// \("(\w+)",\d+,"(\w+)",(\d+),(\d+),\d+\) → payload, alphabet, v1, v2.
var kwikParamsRe = regexp.MustCompile(`\("(\w+)",\d+,"(\w+)",(\d+),(\d+),\d+\)`)

var (
	kwikActionRe = regexp.MustCompile(`action="(.+?)"`)
	kwikTokenRe  = regexp.MustCompile(`value="(.+?)"`)
)

// kwikExtractor ports KwikExtractor (extractors.py:588-650), the player
// animepahe embeds. The frozen Python original scraped the embed page,
// decrypted the packed params and extracted the POST form but returned {}
// — its NetworkClient could not surface the redirect Location of the
// token POST. This port completes the flow the Python comments describe:
// the POST is issued with redirects followed and the final (post-redirect)
// URL — an .m3u8 playlist or .mp4 file — is the media link.
type kwikExtractor struct {
	http *netclient.Client
}

// Name identifies the extractor in typed errors and the factory order.
func (e *kwikExtractor) Name() string { return "kwik" }

// Matches ports the Python URL gate (extractors.py:594).
func (e *kwikExtractor) Matches(u string) bool { return strings.Contains(u, "kwik") }

// Extract resolves a kwik.cx embed URL to its direct media link.
func (e *kwikExtractor) Extract(ctx context.Context, url string) (map[string]contracts.VideoSource, error) {
	shape := func(reason string) error {
		return fmt.Errorf("extractor:kwik: %w: %s", contracts.ErrExtractFailed, reason)
	}

	// 1. Embed page with the animepahe Referer (extractors.py:600).
	// PR49: the site's serving origin is animepahe.pw today (animepahe.com
	// 301s to it; see providers.AnimePaheBase) — a browser session sends
	// .pw as the Referer, so the embed fetch does the same. Live kwik
	// verification is currently impossible: kwik.cx WAF-blocks this
	// network even for real browsers (2026-09-18).
	resp, err := e.http.Get(ctx, url, map[string]string{"Referer": "https://animepahe.pw"})
	if err != nil {
		return nil, fmt.Errorf("extractor:kwik: %w", err)
	}

	// 2. Packed parameters (extractors.py:604-608).
	m := kwikParamsRe.FindStringSubmatch(string(resp.Body))
	if m == nil {
		return nil, shape("packed params not found on the embed page")
	}
	v1, err1 := strconv.Atoi(m[3])
	v2, err2 := strconv.Atoi(m[4])
	if err1 != nil || err2 != nil {
		return nil, shape("non-numeric packer offsets")
	}

	// 3. Decrypt (extractors.py:610-612; base conversion lives in
	// internal/crypto, oracle-verified there).
	decrypted, err := crypto.KwikDecrypt(m[1], m[2], v1, v2)
	if err != nil {
		return nil, shape(fmt.Sprintf("kwik_decrypt: %v", err))
	}

	// 4. POST form action + token (extractors.py:614-620). The form field
	// is the Laravel CSRF name "_token" (kwik runs Laravel); the Python
	// stub never reached the POST, so this is the one parameter the frozen
	// original leaves unpinned — see the fixture provenance notes.
	action := kwikActionRe.FindStringSubmatch(decrypted)
	token := kwikTokenRe.FindStringSubmatch(decrypted)
	if action == nil || token == nil {
		return nil, shape("decrypted payload carries no action/value form")
	}

	// 5. Token POST: the 302 Location is the media URL. The netclient
	// follows the redirect (headers persist), so the final URL of the
	// last response is the link (extractors.py:622-646 comments).
	// The POST body cost is one CSRF field (tens of bytes), so the
	// netclient's retry buffering of request bodies is effectively free (F34).
	form := map[string][]string{"_token": {token[1]}}
	post, err := e.http.PostForm(ctx, action[1], form, map[string]string{"Referer": kwikReferer})
	if err != nil {
		return nil, fmt.Errorf("extractor:kwik: %w", err)
	}
	if post.FinalURL == "" || post.FinalURL == action[1] {
		return nil, shape("token POST did not redirect to a media URL")
	}

	mediaType := "mp4"
	if strings.HasSuffix(post.FinalURL, ".m3u8") {
		mediaType = "m3u8"
	}
	// Python never chose a quality (the stub returned {}); single-quality
	// extractors upstream (dood, streamtape, aniboom) key their result as
	// 1080 — kwik follows that convention.
	return map[string]contracts.VideoSource{
		"1080": {
			URL:     post.FinalURL,
			Quality: "1080",
			Type:    mediaType,
			Headers: map[string]string{"Referer": kwikReferer},
		},
	}, nil
}
