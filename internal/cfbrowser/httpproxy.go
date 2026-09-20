package cfbrowser

// [cf] proxy transport (PR80): the proxy for cfbrowser DOWNLOAD and
// UPDATE network traffic — free-channel GitHub fetches, pro version/
// download calls, license checks and update checks. Deliberate
// boundaries: it never touches the stealth browser's own page traffic
// (site access stays on the browser's per-run wiring) and never
// touches netclient/provider traffic (that is network.proxy_url).

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/proxy"
)

// proxySchemes are the schemes the download transport supports.
var proxySchemes = []string{"http", "https", "socks5", "socks5h"}

// ValidateProxyScheme checks a [cf] proxy value at config load:
// empty means direct and is always valid; otherwise the URL must
// parse and carry a supported scheme.
func ValidateProxyScheme(proxyURL string) error {
	if proxyURL == "" {
		return nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return fmt.Errorf("[cf] proxy: %q не разбирается как URL: %w", proxyURL, err)
	}
	for _, s := range proxySchemes {
		if u.Scheme == s {
			return nil
		}
	}
	return fmt.Errorf("[cf] proxy: схема %q не поддерживается (допустимо: http, https, socks5, socks5h)", u.Scheme)
}

// DownloadHTTPClient builds the transport for download/update traffic:
// an empty proxyURL is the direct connection (regression-pinned);
// http/https proxies ride http.ProxyURL; socks5/socks5h dial through
// golang.org/x/net/proxy. An invalid value is an error — config load
// already validates, so this is the API-user's fail-loud backstop.
func DownloadHTTPClient(proxyURL string, timeout time.Duration) (*http.Client, error) {
	if proxyURL == "" {
		return &http.Client{Timeout: timeout}, nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("cfbrowser: parse [cf] proxy %q: %w", proxyURL, err)
	}
	switch u.Scheme {
	case "http", "https":
		return &http.Client{
			Timeout:   timeout,
			Transport: &http.Transport{Proxy: http.ProxyURL(u)},
		}, nil
	case "socks5", "socks5h":
		dialer, derr := proxy.FromURL(u, proxy.Direct)
		if derr != nil {
			return nil, fmt.Errorf("cfbrowser: [cf] proxy socks dialer: %w", derr)
		}
		ctxDialer, ok := dialer.(proxy.ContextDialer)
		if !ok {
			return nil, fmt.Errorf("cfbrowser: [cf] proxy socks dialer lacks DialContext")
		}
		return &http.Client{
			Timeout:   timeout,
			Transport: &http.Transport{DialContext: ctxDialer.DialContext},
		}, nil
	default:
		return nil, fmt.Errorf("cfbrowser: [cf] proxy: схема %q не поддерживается (допустимо: http, https, socks5, socks5h)", u.Scheme)
	}
}
