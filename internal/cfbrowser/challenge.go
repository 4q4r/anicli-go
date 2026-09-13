package cfbrowser

import (
	"strings"
	"time"
)

// Cloudflare challenge fingerprints. Title markers are matched
// case-insensitively; body markers are plain substrings of the
// challenge markup Cloudflare ships.
var (
	challengeTitleMarkers = []string{
		"just a moment",
		"attention required",
		"checking your browser",
		"verify you are human",
		"доступ ограничен", // RU Cloudflare interstitial
	}
	challengeBodyMarkers = []string{
		"challenge-platform",
		"cf-chl",
		"cf_chl",
		"cf-turnstile",
		"turnstile.js",
		"_cf_chl_opt",
		"cf-mitigated",
	}
)

// Cookie is one harvested browser cookie, transport-agnostic so the
// store JSON stays stable while netclient adapts it to its own jar.
type Cookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain,omitempty"`
	Path   string `json:"path,omitempty"`
}

// Clearance is the result of one challenge solve: the cookie set (at
// minimum cf_clearance for the solved host), the browser's
// User-Agent and Accept-Language that MUST accompany the cookies on
// every replayed request, and the harvest timestamp feeding the TTL.
type Clearance struct {
	Cookies        []Cookie  `json:"cookies"`
	UserAgent      string    `json:"user_agent"`
	AcceptLanguage string    `json:"accept_language,omitempty"`
	Obtained       time.Time `json:"obtained"`
}

// HasCFClearance reports whether the cookie set carries cf_clearance.
func (c Clearance) HasCFClearance() bool {
	for _, ck := range c.Cookies {
		if ck.Name == "cf_clearance" {
			return true
		}
	}
	return false
}

// IsChallengePage reports whether a page (title + body markup) looks
// like a Cloudflare challenge interstitial. Deliberately cheap
// substring matching: it runs inside the solve poll loop.
func IsChallengePage(title, body string) bool {
	tl := strings.ToLower(title)
	for _, m := range challengeTitleMarkers {
		if strings.Contains(tl, m) {
			return true
		}
	}
	for _, m := range challengeBodyMarkers {
		if strings.Contains(body, m) {
			return true
		}
	}
	return false
}

// SolvedState reports whether the solve loop may stop: the
// cf_clearance cookie is present, or the page renders content that is
// neither challenge-titled nor carries challenge markup.
func SolvedState(title, body string, hasCFClearanceCookie bool) bool {
	if hasCFClearanceCookie {
		return true
	}
	return !IsChallengePage(title, body)
}
