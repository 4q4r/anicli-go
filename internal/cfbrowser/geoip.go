package cfbrowser

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// geoEndpoint is the best-effort geo lookup answering two lines:
// timezone and country code (http://ip-api.com/line/).
const geoEndpoint = "http://ip-api.com/line/?fields=timezone,countryCode"

// geoTimeout bounds the whole lookup: it runs before solves, so it
// must stay cheap.
const geoTimeout = 5 * time.Second

// countryLocales maps the country codes anicli's audience actually
// hits to BCP-47 locales; unknown codes fall back to en-US. Extend on
// demand — a full CLDR map is not worth the weight here.
var countryLocales = map[string]string{
	"RU": "ru-RU", "BY": "ru-RU", "KZ": "ru-RU",
	"US": "en-US", "GB": "en-GB",
	"JP": "ja-JP",
	"DE": "de-DE",
	"FR": "fr-FR",
	"UA": "uk-UA",
}

// LocaleForCountry renders the browser --lang value for a country
// code (unknown → en-US).
func LocaleForCountry(countryCode string) string {
	if loc, ok := countryLocales[strings.ToUpper(strings.TrimSpace(countryCode))]; ok {
		return loc
	}
	return "en-US"
}

// geoInfo is one successful lookup.
type geoInfo struct {
	Timezone    string
	CountryCode string
}

// resolveGeo asks the line endpoint for the egress IP's timezone and
// country, through the same proxy the browser will use (empty proxy =
// direct). The endpoint URL is injectable for tests. Malformed
// payloads yield zero info without an error — geo alignment is
// strictly best-effort; transport failures surface as errors the
// caller logs and ignores.
func resolveGeo(ctx context.Context, proxyURL, endpoint string, hc *http.Client) (geoInfo, error) {
	if endpoint == "" {
		endpoint = geoEndpoint
	}
	if hc == nil {
		hc = &http.Client{Timeout: geoTimeout}
	}
	if proxyURL != "" {
		pu, err := url.Parse(proxyURL)
		if err != nil {
			return geoInfo{}, fmt.Errorf("cfbrowser: parse proxy %q: %w", proxyURL, err)
		}
		transport := &http.Transport{Proxy: http.ProxyURL(pu)}
		hc = &http.Client{Transport: transport, Timeout: geoTimeout}
	}

	ctx, cancel := context.WithTimeout(ctx, geoTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return geoInfo{}, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return geoInfo{}, fmt.Errorf("cfbrowser: geo lookup: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return geoInfo{}, fmt.Errorf("cfbrowser: geo lookup: status %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return geoInfo{}, fmt.Errorf("cfbrowser: geo lookup body: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) < 2 {
		return geoInfo{}, nil
	}
	tz, cc := strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1])
	// Plausibility: timezone has a continent/city shape, country is
	// two letters; anything else is treated as no data.
	if !strings.Contains(tz, "/") || len(cc) != 2 {
		return geoInfo{}, nil
	}
	return geoInfo{Timezone: tz, CountryCode: strings.ToUpper(cc)}, nil
}
